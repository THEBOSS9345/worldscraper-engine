# WorldScraper

A desktop application that crawls the open web continuously and builds your own
private search engine out of what it finds — with a live operations dashboard
showing what is being fetched, from where, and how fast.

Everything runs locally. Nothing is uploaded anywhere.

---

## What it is

| Layer | Technology | Why |
|---|---|---|
| Crawl engine | **Go** | Goroutines make tens of thousands of concurrent fetches cheap, and it cross-compiles to a single static binary. |
| Frontier / dedupe | **Pebble** (pure Go LSM) | The URL queue and seen-set grow to billions of keys. An LSM tree absorbs that write volume; a single-writer SQL database would become the bottleneck. |
| Search index | **Tantivy** (Rust) | A real Lucene-class engine: BM25 ranking, fast field sorting, sub-100 ms queries at hundreds of millions of documents. |
| Aggregates / settings | **SQLite** | Small, bounded, low-write-rate data — host stats, per-minute metrics, configuration. Exactly what SQLite is best at. |
| Desktop shell + UI | **Tauri 2 (Rust)** + TypeScript | Native window, tray, autostart, ~10 MB shell instead of a bundled browser. |

The split is deliberate: the three storage engines each handle the workload they
are actually good at, rather than forcing one database to do all three jobs.

### How the pieces talk

```
                    ┌──────────────────────────────────────┐
                    │  wsengine.exe  (Go, headless daemon) │
   seeds ──────────▶│                                      │
                    │  scheduler ─▶ worker pool ─▶ fetch    │
                    │      ▲              │                │
                    │      │              ▼                │
                    │   Pebble ◀──── extract + classify    │
                    │   frontier          │                │
                    │   + doc spool       ▼                │
                    │                  SQLite (stats)      │
                    └────────┬──────────────────┬──────────┘
                             │ HTTP + SSE       │ spool pull/ack
                             ▼                  ▼
                    ┌──────────────────────────────────────┐
                    │  WorldScraper.exe  (Rust / Tauri)    │
                    │                                      │
                    │  run-or-adopt ──▶ the engine keeps   │
                    │                  crawling when this  │
                    │  ingest loop ──▶ Tantivy index       │
                    │  dashboard (WebView2)                │
                    └──────────────────────────────────────┘
```

The engine is a **standalone daemon**: it writes its port and API token to
`runtime.json`, and the shell either adopts that running instance or starts a
fresh one. Quitting the app never stops the engine — it keeps crawling, and the
next launch simply reconnects to it.

The document handoff is **pull-based and acknowledged after commit**: the engine
keeps a crawled page in its spool until Tantivy confirms it is durably indexed.
If either process dies mid-batch, the batch replays. Re-indexing is idempotent
because documents are deleted by URL term before insert.

---

## Running it

### Prerequisites

- Go 1.22+
- Rust (MSVC toolchain on Windows)
- Node 18+
- WebView2 runtime (preinstalled on Windows 11)

### Development

```bash
cd engine && go build -o wsengine.exe ./cmd/wsengine
```

```bash
cd app && npm install && npm run tauri dev
```

The shell finds the engine binary automatically — from `src-tauri/binaries/` in a
packaged build, or from `../engine/` in a dev tree.

### Portable build (no installer)

```bash
cd app && npm run build:portable
```

Builds the engine, the front end and the app, then stages `dist-portable/`:

```
dist-portable/
  WorldScraper.exe   ~13 MB
  wsengine.exe       ~24 MB
  README.txt
```

Run `WorldScraper.exe`; it starts the engine itself (or adopts one left running
by a previous session). Keep the two files together — the app looks for the
engine next to its own executable. Nothing is written into the folder; data
lives in `%APPDATA%\WorldScraper`.

Closing the window hides to the tray. **Quitting the app does not stop the
crawl** — the engine daemon keeps running and the next launch reconnects to it.
Use **Control → Stop engine** to stop it for good, or **Start engine** to bring
it back.

### Packaging an installer

```bash
cd app && npm run tauri build
```

Produces an NSIS installer under `app/src-tauri/target/release/bundle/`. Copy a
fresh `engine/wsengine.exe` into `app/src-tauri/binaries/` first; it ships as a
bundled resource.

### Running the engine on its own

The crawler is a normal headless daemon and does not need the UI — this is the
same process the app uses:

```bash
./wsengine.exe -data ./mydata -listen 127.0.0.1:8787
```

Then `curl http://127.0.0.1:8787/api/stats`. Omitting `-token` disables API
authentication, which is only appropriate for local development. It runs until
it receives a signal, its `-parent-pid` disappears (if given), or a
`POST /api/control` with action `shutdown` arrives.

---

## How the crawl never stops

Four mechanisms keep it running indefinitely:

1. **Recrawl scheduling.** Every finished URL is written to a time-ordered index.
   A sweeper pulls pages older than the configured recrawl age back into the
   queue, so the frontier refills itself.
2. **Autonomous discovery.** Every cycle (default 30 minutes) the engine asks
   small, polite external sources for brand-new sites and offers the frontier
   ones it has never seen — so the crawl keeps meeting new ground instead of
   cycling forever over the same reachable set. Built-in sources are Hacker News
   story submissions (fresh outbound links) and the daily Tranco top-domain
   list, walked from rank 1 through the whole million; you can add your own
   RSS/Atom/OPML/TXT/CSV feeds in Settings. Each source host's robots.txt is
   respected, the Tranco cursor survives restarts, and a **Discover sites**
   button on the Control tab runs one cycle on demand.
3. **Reseeding.** If the queue still runs dry, the seed list is re-injected
   (throttled to once every five minutes).
4. **Daemon independence.** The engine is a detached process that owns its own
   lifetime. The shell adopts it or starts it, health-checks it and restarts it
   with backoff if it dies while the app is open — but closing the app never
   stops it, so the crawl survives the app being gone for as long as the machine
   is up.

Closing the window hides to the tray; the crawl keeps going. Even quitting the
app from the tray leaves the engine running. Enable launch-at-login in Settings
to survive reboots.

---

## Runtime control

Everything is adjustable from the **Control** tab while the crawl is running —
nothing requires a restart:

| Control | Effect |
|---|---|
| Workers | Concurrent fetches across all hosts. Applied live; the pool restarts in place. |
| Per-host delay | Politeness gap between two hits on one host. |
| Parallel per host | Simultaneous requests allowed to a single host. |
| Pause / Resume | Stops dispatching without tearing anything down. |
| Reseed now | Re-injects the seed list immediately. |
| Discover sites | Runs one discovery cycle now, queuing any new sites it finds. |
| Free memory | Flushes buffers and returns unused heap to the OS. |
| Compact storage | Reclaims disk left behind by deletions; runs in the background. |
| Clear robots cache | Forces robots.txt to be re-fetched. |
| Clear queue | Empties the backlog; crawled pages are kept. |
| Reset statistics | Zeroes the dashboard aggregates. |
| Forget crawled URLs | Clears the seen-set so everything becomes eligible again. |

The panel also shows live engine heap and RSS, so the effect of freeing memory
is visible rather than assumed.

The **Start engine** / **Stop engine** buttons manage the daemon itself: stop
halts the whole process (crawling pauses until you start it again or reopen the
app), start brings it back. The crawl keeps running on its own, so neither is
needed for normal use.

---

## Politeness

Broad crawling is only sustainable if it is well-behaved, and a crawler that gets
blocked stops being useful. Defaults:

- `robots.txt` is fetched, cached for 24 h, and honoured, including `Crawl-delay`
- one request per host at a time, ≥ 1.2 s apart
- `429` and `503` back the host off for 30 s and retry once
- repeatedly failing hosts are parked rather than retried forever
- responses are size-capped, and non-HTML is abandoned after the headers

All of these are adjustable in Settings. Turning robots off will get you blocked
by a large share of the web.

TLS certificate errors are ignored by default (`insecureTls`), because a
meaningful fraction of the web has broken certificates and the crawler only reads
public pages and never sends credentials.

---

## Geolocation (optional)

By default the globe places each host at a hash-derived point — stable per host,
but **not** geographic, and the panel says so.

Drop a MaxMind or DB-IP database into the data directory and it becomes real:

```
%APPDATA%\WorldScraper\GeoLite2-City.mmdb
```

Accepted filenames: `geoip.mmdb`, `GeoLite2-City.mmdb`, `GeoIP2-City.mmdb`,
`dbip-city-lite.mmdb`, or the `-Country` equivalents. Restart the app; the log
prints `geolocation enabled`.

The database is never bundled, because GeoLite2 carries its own licence and
requires you to accept MaxMind's terms. With one installed you also get the
**Server countries** panel; without one, everything works exactly as before.

No extra network requests are made either way — the server's IP comes from the
connection the crawler already opened, and lookups are cached per host.

---

## Search

Ranking is BM25 over title, description, body and host, with the title weighted
5×, then adjusted by a bounded quality multiplier that prefers shallow, clean
URLs over deep generated pages. Beyond that:

- **All terms first, any term as a fallback.** A long query that matches nothing
  strictly is retried loosely, and the UI says when that happened.
- **Contextual snippets.** The passage shown is centred on the first query term
  that actually occurs, not the first N characters of the page.
- **Search as you type**, with `/` to focus the box from anywhere.
- Filters for category, host and language; sort by relevance or recency.

Only a 1,200-character excerpt is stored per document for display. Matching still
uses the full extracted text, which keeps the index small enough to scale.

---

## Storage layout

Everything lives in one directory (`%APPDATA%\WorldScraper` on Windows):

```
frontier/            Pebble — URL queue, seen-set, recrawl index, robots cache, doc spool
index/               Tantivy — the searchable index
meta.db              SQLite — host stats, category/language/country/status counts, metrics
runtime.json         the engine's current port and API token
GeoLite2-City.mmdb   optional, user-supplied (see Geolocation)
```

To reset the crawl, quit the app and delete the directory.

---

## Security notes

The engine's HTTP API binds to `127.0.0.1` only and requires a token that is
minted fresh on every launch and passed to the engine on the command line. This
matters because any web page in any browser can reach `localhost` — without the
token, a visited page could drive your crawler.

---

## Project layout

```
engine/                     Go crawl daemon
  cmd/wsengine/             entry point, seed list
  internal/crawl/           scheduler, worker pool, 24/7 maintenance
  internal/kv/              Pebble frontier, spool, recrawl index
  internal/meta/            SQLite aggregates and settings
  internal/fetch/           HTTP client
  internal/extract/         streaming HTML extraction
  internal/robots/          robots.txt parser
  internal/classify/        category heuristics
  internal/api/             HTTP + SSE API
  internal/geoip/           optional IP -> location lookup
app/
  src/                      dashboard (TypeScript, canvas charts, globe)
  src-tauri/src/engine.rs   run-or-adopt engine daemon supervisor
  src-tauri/src/indexer.rs  Tantivy index + spool ingest loop
  src-tauri/examples/query.rs  CLI diagnostic for the live index
  tools/make-icon.mjs       generates the app icon
tools/capture-window.ps1    screenshots the running window
```

---

## Known limitations

- Category classification is keyword heuristics, not a model. It is good enough
  for dashboard breakdowns and filtering; it is not a moderation system.
- Without a GeoIP database the globe places hosts at a hash-derived point. It is
  an activity display, not geography, and the panel labels it as such.
- Language detection uses the page's declared `lang` first and falls back to a
  script-range guess.
- Search stores a 400-character excerpt per document rather than the full body, to
  keep the index small at scale. Matching still uses the full extracted text.
