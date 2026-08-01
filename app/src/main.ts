/** WorldScraper dashboard entry point. */

import "./styles.css";

import {
  addSeeds,
  connectEngine,
  control,
  dataDir,
  engineLogs,
  engineStart,
  engineStop,
  getConfig,
  getHosts,
  getStats,
  indexStats,
  openLiveFeed,
  search,
  setConfig,
  type Aggregates,
  type ControlAction,
  type CrawlEvent,
  type EngineConfig,
  type EngineInfo,
  type Hit,
  type HostRow,
  type IndexStats,
  type Snapshot,
} from "./api";
import { AreaChart, bucketStatuses, compactNumber, formatBytes, Sparkline } from "./charts";
import { Globe } from "./globe";

// Fixed category→hue assignment. Only the six most common categories get a
// hue; everything else is neutral. Hues are never cycled, and the category
// name is always shown as text so color is never the only signal.
const CATEGORY_COLOR: Record<string, string> = {
  news: "var(--cat-1)",
  docs: "var(--cat-2)",
  code: "var(--cat-3)",
  wiki: "var(--cat-4)",
  social: "var(--cat-5)",
  academic: "var(--cat-6)",
};
const catColor = (c: string) => CATEGORY_COLOR[c] ?? "var(--cat-other)";

const MAX_FEED_ROWS = 120;

// ---------------------------------------------------------------- utilities --

const $ = <T extends HTMLElement = HTMLElement>(sel: string, root: ParentNode = document) => {
  const el = root.querySelector<T>(sel);
  if (!el) throw new Error(`missing element: ${sel}`);
  return el;
};

const esc = (s: string) =>
  s.replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c] ?? c,
  );

const clockFmt = new Intl.DateTimeFormat(undefined, {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

function relativeTime(unix: number): string {
  const diff = Date.now() / 1000 - unix;
  if (diff < 60) return `${Math.max(0, Math.round(diff))}s ago`;
  if (diff < 3600) return `${Math.round(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.round(diff / 3600)}h ago`;
  return `${Math.round(diff / 86400)}d ago`;
}

// Intl knows every region name, so no country table is needed in the UI.
const regionNames = (() => {
  try {
    return new Intl.DisplayNames(undefined, { type: "region" });
  } catch {
    return null;
  }
})();

function countryName(code: string): string {
  if (!code) return "unknown";
  try {
    return regionNames?.of(code.toUpperCase()) ?? code;
  } catch {
    return code;
  }
}

function duration(seconds: number): string {
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m ${Math.floor(seconds % 60)}s`;
}

let toastTimer: number | undefined;
function toast(message: string, bad = false): void {
  const el = $("#toast");
  el.textContent = message;
  el.classList.toggle("toast--bad", bad);
  el.classList.add("toast--on");
  if (toastTimer) window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => el.classList.remove("toast--on"), 3200);
}

/** Splits a query into the bare words worth highlighting. */
function queryTerms(query: string): string[] {
  return query
    .toLowerCase()
    .split(/\s+/)
    .map((t) => t.replace(/[^\p{L}\p{N}]/gu, ""))
    .filter((t) => t.length > 1);
}

/** Wraps query terms found in text with <mark>, on already-escaped output. */
function highlight(text: string, query: string): string {
  const safe = esc(text);
  const terms = queryTerms(query);
  if (terms.length === 0) return safe;

  const pattern = terms.map((t) => t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|");
  return safe.replace(new RegExp(`(${pattern})`, "gi"), "<mark>$1</mark>");
}

/**
 * Builds the passage shown under a result.
 *
 * Rather than always printing the first N characters, this centres the window
 * on the first query term that actually occurs — which is what makes a result
 * list scannable, because you see *why* the page matched.
 */
function snippetFor(h: Hit, query: string): string {
  const source = [h.description, h.excerpt].filter(Boolean).join(" — ");
  if (!source) return "";

  const terms = queryTerms(query);
  const WINDOW = 260;
  if (terms.length === 0 || source.length <= WINDOW) return source.slice(0, WINDOW);

  const haystack = source.toLowerCase();
  let hitAt = -1;
  for (const t of terms) {
    const i = haystack.indexOf(t);
    if (i >= 0 && (hitAt < 0 || i < hitAt)) hitAt = i;
  }
  if (hitAt < 0) return source.slice(0, WINDOW);

  let start = Math.max(0, hitAt - Math.floor(WINDOW / 3));
  let end = Math.min(source.length, start + WINDOW);

  // Snap to word boundaries so the passage does not begin mid-word.
  if (start > 0) {
    const space = source.indexOf(" ", start);
    if (space > 0 && space - start < 24) start = space + 1;
  }
  if (end < source.length) {
    const space = source.lastIndexOf(" ", end);
    if (space > start && end - space < 24) end = space;
  }

  return (start > 0 ? "… " : "") + source.slice(start, end).trim() + (end < source.length ? " …" : "");
}

/** Renders a URL as a readable breadcrumb, the way a search engine does. */
function breadcrumb(rawUrl: string): string {
  try {
    const u = new URL(rawUrl);
    const parts = u.pathname.split("/").filter(Boolean).slice(0, 4);
    const decoded = parts.map((p) => {
      try {
        return decodeURIComponent(p);
      } catch {
        return p;
      }
    });
    return [u.host, ...decoded].join(" › ");
  } catch {
    return rawUrl;
  }
}

// -------------------------------------------------------------------- shell --

document.querySelector<HTMLDivElement>("#app")!.innerHTML = `
  <div class="shell">
    <div class="brand">
      <div class="brand__mark"></div>
      <div>
        <div class="brand__name">WorldScraper</div>
        <div class="brand__sub">PLANETARY INDEX</div>
      </div>
    </div>

    <header class="topbar">
      <span class="pill"><span class="dot" id="engine-dot"></span><span id="engine-state">connecting…</span></span>
      <span class="pill" id="pill-rate">— pages/s</span>
      <span class="pill" id="pill-indexed">— indexed</span>
      <div class="topbar__spacer"></div>
      <button class="btn btn--ghost" id="btn-pause">Pause</button>
      <span class="pill" id="pill-clock">--:--:--</span>
    </header>

    <nav class="nav">
      <button class="nav__item" data-view="overview" aria-current="page"><span class="nav__icon">◎</span> Overview</button>
      <button class="nav__item" data-view="search"><span class="nav__icon">⌕</span> Search</button>
      <button class="nav__item" data-view="control"><span class="nav__icon">⌁</span> Control</button>
      <button class="nav__item" data-view="hosts"><span class="nav__icon">▤</span> Hosts</button>
      <button class="nav__item" data-view="settings"><span class="nav__icon">⚙</span> Settings</button>
      <div class="nav__foot" id="nav-foot">—</div>
    </nav>

    <main class="main">
      <!-- ------------------------------------------------------ overview -->
      <section class="view view--active" id="view-overview">
        <div class="grid">
          <div class="c12">
            <div class="tiles" id="tiles"></div>
          </div>

          <div class="panel c5">
            <div class="panel__head">
              <span class="panel__title" id="globe-title">Live crawl surface</span>
              <span class="panel__note" id="globe-note">—</span>
            </div>
            <div class="chart" style="height:330px"><canvas id="globe"></canvas></div>
            <div class="legend">
              <span class="legend__item"><span class="legend__swatch" style="background:#6edc7d"></span>page fetched</span>
              <span class="legend__item"><span class="legend__swatch" style="background:#ff7d92"></span>fetch failed</span>
              <span class="legend__item"><span class="legend__swatch" style="background:#ff4fdf"></span>link discovered</span>
            </div>
            <div class="panel__note" id="globe-legend-note" style="margin-top:8px">—</div>
          </div>

          <div class="panel c7">
            <div class="panel__head">
              <span class="panel__title">Pages crawled per minute</span>
              <span class="panel__note">last 2 hours</span>
            </div>
            <div class="chart" style="height:200px">
              <canvas id="chart-throughput"></canvas>
              <div class="tooltip" id="tip-throughput"></div>
            </div>
            <div class="panel__head" style="margin:18px 0 8px">
              <span class="panel__title">Fetches per second</span>
              <span class="panel__note">last 60 seconds</span>
            </div>
            <div class="chart" style="height:70px"><canvas id="chart-spark"></canvas></div>
          </div>

          <div class="panel c4">
            <div class="panel__head"><span class="panel__title">Content categories</span></div>
            <div class="ranks" id="ranks-categories"></div>
          </div>

          <div class="panel c4">
            <div class="panel__head"><span class="panel__title">Response classes</span></div>
            <div class="ranks" id="ranks-statuses"></div>
          </div>

          <div class="panel c4">
            <div class="panel__head"><span class="panel__title">Languages</span></div>
            <div class="ranks" id="ranks-langs"></div>
          </div>

          <div class="panel c4">
            <div class="panel__head">
              <span class="panel__title">Server countries</span>
              <span class="panel__note" id="geo-note">—</span>
            </div>
            <div class="ranks" id="ranks-countries"></div>
          </div>

          <div class="panel c5">
            <div class="panel__head">
              <span class="panel__title">Most-crawled hosts</span>
              <span class="panel__note">click to search</span>
            </div>
            <div class="ranks" id="ranks-hosts"></div>
          </div>

          <div class="panel c7">
            <div class="panel__head">
              <span class="panel__title">Live feed</span>
              <span class="panel__note" id="feed-note">—</span>
            </div>
            <div class="feed" id="feed"></div>
          </div>
        </div>
      </section>

      <!-- -------------------------------------------------------- search -->
      <section class="view" id="view-search">
        <div class="panel">
          <div class="search__bar">
            <input class="input" id="q" type="search" placeholder="Search everything the crawler has indexed…" autocomplete="off" spellcheck="false" />
            <select class="input" id="sort" style="flex:0 0 160px">
              <option value="relevance">Best match</option>
              <option value="recent">Most recent</option>
            </select>
            <button class="btn" id="btn-search">Search</button>
          </div>
          <div class="chips" id="filters"></div>
          <div id="search-summary" class="panel__note" style="margin-bottom:10px"></div>
          <div class="results" id="results">
            <div class="empty">
              <div class="empty__big">⌕</div>
              Search the index built from your own crawl.<br />
              Results come from Tantivy with BM25 ranking — no network requests are made.
            </div>
          </div>
          <div style="display:flex;gap:10px;justify-content:center;margin-top:16px">
            <button class="btn btn--ghost" id="btn-prev" disabled>← Previous</button>
            <button class="btn btn--ghost" id="btn-next" disabled>Next →</button>
          </div>
        </div>
      </section>

      <!-- ------------------------------------------------------- control -->
      <section class="view" id="view-control">
        <div class="grid">
          <div class="panel c6">
            <div class="panel__head">
              <span class="panel__title">Live crawl controls</span>
              <span class="panel__note" id="ctl-state">—</span>
            </div>

            <div class="form">
              <div class="field">
                <div class="field__row">
                  <label class="field__label" for="ctl-workers" style="flex:1">Concurrent workers</label>
                  <output class="rank__value" id="ctl-workers-out">—</output>
                </div>
                <input id="ctl-workers" type="range" min="1" max="512" step="1" />
                <span class="field__hint">Simultaneous fetches across all hosts. Applied live; the pool restarts.</span>
              </div>

              <div class="field">
                <div class="field__row">
                  <label class="field__label" for="ctl-delay" style="flex:1">Per-host delay</label>
                  <output class="rank__value" id="ctl-delay-out">—</output>
                </div>
                <input id="ctl-delay" type="range" min="0" max="10000" step="100" />
                <span class="field__hint">Politeness gap between two hits on the same host. Lower is faster and ruder.</span>
              </div>

              <div class="field">
                <div class="field__row">
                  <label class="field__label" for="ctl-burst" style="flex:1">Parallel requests per host</label>
                  <output class="rank__value" id="ctl-burst-out">—</output>
                </div>
                <input id="ctl-burst" type="range" min="1" max="8" step="1" />
              </div>

              <div class="field__row" style="flex-wrap:wrap;gap:8px">
                <button class="btn" id="ctl-apply">Apply</button>
                <button class="btn btn--ghost" id="ctl-pause">Pause</button>
                <button class="btn btn--ghost" id="ctl-reseed">Reseed now</button>
                <button class="btn btn--ghost" id="ctl-discover">Discover sites</button>
                <button class="btn btn--ghost" id="ctl-start-engine">Start engine</button>
                <button class="btn btn--danger" id="ctl-stop">Stop engine</button>
              </div>
            </div>
          </div>

          <div class="panel c6">
            <div class="panel__head"><span class="panel__title">Runtime state</span></div>
            <div class="ranks" id="ctl-stats"></div>
            <span class="field__hint" id="ctl-discovery" style="display:block;margin-top:10px"></span>

            <div class="panel__head" style="margin:24px 0 12px">
              <span class="panel__title">Maintenance</span>
            </div>
            <div class="field__row" style="flex-wrap:wrap;gap:8px">
              <button class="btn btn--ghost" id="ctl-free">Free memory</button>
              <button class="btn btn--ghost" id="ctl-compact">Compact storage</button>
              <button class="btn btn--ghost" id="ctl-robots">Clear robots cache</button>
            </div>
            <span class="field__hint" style="display:block;margin-top:10px">
              Free memory returns unused heap to the OS. Compaction reclaims disk left by
              deletions and can take minutes on a large store; it runs in the background.
            </span>

            <div class="panel__head" style="margin:24px 0 12px">
              <span class="panel__title">Destructive</span>
            </div>
            <div class="field__row" style="flex-wrap:wrap;gap:8px">
              <button class="btn btn--danger" id="ctl-clear-queue">Clear queue</button>
              <button class="btn btn--danger" id="ctl-reset-stats">Reset statistics</button>
              <button class="btn btn--danger" id="ctl-clear-seen">Forget all crawled URLs</button>
            </div>
            <span class="field__hint" style="display:block;margin-top:10px">
              Clearing the queue empties the backlog but keeps what has been crawled.
              Forgetting crawled URLs makes the whole web eligible again — the search
              index is left intact, but everything will be re-fetched.
            </span>
          </div>
        </div>
      </section>

      <!-- --------------------------------------------------------- hosts -->
      <section class="view" id="view-hosts">
        <div class="panel">
          <div class="panel__head">
            <span class="panel__title">Hosts discovered</span>
            <span class="panel__note" id="hosts-note">—</span>
          </div>
          <div style="max-height:calc(100vh - 220px);overflow:auto">
            <table class="table">
              <thead>
                <tr>
                  <th>Host</th><th>Category</th>
                  <th class="num">Pages</th><th class="num">Errors</th>
                  <th class="num">Data</th><th class="num">Last seen</th>
                </tr>
              </thead>
              <tbody id="hosts-body"></tbody>
            </table>
          </div>
        </div>
      </section>

      <!-- ------------------------------------------------------ settings -->
      <section class="view" id="view-settings">
        <div class="grid">
          <div class="panel c6">
            <div class="panel__head"><span class="panel__title">Crawl configuration</span></div>
            <div class="form" id="config-form">
              <div class="field">
                <label class="field__label" for="cfg-workers">Concurrent workers</label>
                <input id="cfg-workers" type="number" min="1" max="1024" />
                <span class="field__hint">Total simultaneous fetches across all hosts. Changing this restarts the worker pool.</span>
              </div>
              <div class="field">
                <label class="field__label" for="cfg-delay">Per-host delay (ms)</label>
                <input id="cfg-delay" type="number" min="0" max="600000" />
                <span class="field__hint">Minimum gap between two requests to the same host. A host's own Crawl-delay wins if it is longer.</span>
              </div>
              <div class="field">
                <label class="field__label" for="cfg-depth">Maximum link depth</label>
                <input id="cfg-depth" type="number" min="-1" />
                <span class="field__hint">Hops from a seed. −1 crawls without a depth limit.</span>
              </div>
              <div class="field">
                <label class="field__label" for="cfg-recrawl">Recrawl after (hours)</label>
                <input id="cfg-recrawl" type="number" min="1" />
                <span class="field__hint">Finished pages return to the queue after this long, which is what keeps the crawl running indefinitely.</span>
              </div>
              <div class="field">
                <label class="field__label" for="cfg-ua">User agent</label>
                <input id="cfg-ua" type="text" />
              </div>
              <div class="field">
                <label class="switch">
                  <input type="checkbox" id="cfg-robots" /><span class="switch__track"></span>
                  <span class="field__label">Respect robots.txt</span>
                </label>
                <span class="field__hint">Honours each site's published crawling rules. Turning this off will get your IP blocked by many hosts.</span>
              </div>
              <div class="field">
                <label class="switch">
                  <input type="checkbox" id="cfg-adult" /><span class="switch__track"></span>
                  <span class="field__label">Crawl adult content</span>
                </label>
                <span class="field__hint">When off, hosts classified as adult are skipped and their links are not followed.</span>
              </div>
              <div class="field">
                <label class="switch">
                  <input type="checkbox" id="cfg-html" /><span class="switch__track"></span>
                  <span class="field__label">HTML only</span>
                </label>
                <span class="field__hint">Skips non-HTML responses after reading their headers.</span>
              </div>

              <div class="panel__head" style="margin:24px 0 12px"><span class="panel__title">Site discovery</span></div>
              <div class="field">
                <label class="switch">
                  <input type="checkbox" id="cfg-discovery" /><span class="switch__track"></span>
                  <span class="field__label">Find new sites automatically</span>
                </label>
                <span class="field__hint">
                  The engine keeps offering the frontier sites it has never seen, so the crawl never runs out of
                  new ground. Built-in sources: Hacker News submissions and the daily Tranco top-domain list.
                  The button on the Control page runs one cycle on demand.
                </span>
              </div>
              <div class="field">
                <label class="field__label" for="cfg-disc-interval">Run every (minutes)</label>
                <input id="cfg-disc-interval" type="number" min="5" max="1440" />
                <span class="field__hint">A cycle adds up to the cap below, politely, from each source.</span>
              </div>
              <div class="field">
                <label class="field__label" for="cfg-disc-cap">New sites per cycle (max)</label>
                <input id="cfg-disc-cap" type="number" min="10" max="10000" />
              </div>
              <div class="field">
                <label class="field__label" for="cfg-disc-sources">Extra feeds</label>
                <textarea id="cfg-disc-sources" placeholder="https://example.com/feed.xml&#10;https://blog.example.com"></textarea>
                <span class="field__hint">One per line: RSS/Atom feeds, OPML files, or plain lists of URLs (TXT/CSV). Each is read for links to enqueue.</span>
              </div>
              <div class="field__row">
                <button class="btn" id="btn-save-config">Save configuration</button>
                <button class="btn btn--danger" id="btn-stop">Stop crawler</button>
              </div>
            </div>
          </div>

          <div class="panel c6">
            <div class="panel__head"><span class="panel__title">Seed URLs</span></div>
            <div class="form">
              <div class="field">
                <label class="field__label" for="seeds">Add starting points</label>
                <textarea id="seeds" placeholder="example.com&#10;https://news.example.org/section&#10;another-site.net"></textarea>
                <span class="field__hint">One per line. Already-seen URLs are ignored, so pasting the same list twice is harmless.</span>
              </div>
              <div class="field__row"><button class="btn" id="btn-add-seeds">Add to frontier</button></div>
            </div>

            <div class="panel__head" style="margin:24px 0 12px"><span class="panel__title">Storage</span></div>
            <div class="ranks" id="storage"></div>

            <div class="panel__head" style="margin:24px 0 12px">
              <span class="panel__title">Engine log</span>
              <span class="panel__note" id="log-note">—</span>
            </div>
            <div class="log" id="log">—</div>
          </div>
        </div>
      </section>
    </main>
  </div>
  <div class="toast" id="toast"></div>
`;

// ---------------------------------------------------------------- app state --

let latest: Snapshot | null = null;
let config: EngineConfig | null = null;
let idxStats: IndexStats | null = null;
let feedRows: CrawlEvent[] = [];
let activeView = "overview";

const globe = new Globe($<HTMLCanvasElement>("#globe"));
const throughput = new AreaChart(
  $<HTMLCanvasElement>("#chart-throughput"),
  $("#tip-throughput"),
  "pages",
);
const spark = new Sparkline($<HTMLCanvasElement>("#chart-spark"));

// ------------------------------------------------------------------- render --

/** Tile definitions, in display order. Built once; only the text changes. */
const TILE_SPECS: { key: string; label: string; accent: boolean }[] = [
  { key: "pages", label: "Pages crawled", accent: true },
  { key: "indexed", label: "Indexed & searchable", accent: true },
  { key: "hosts", label: "Hosts reached", accent: false },
  { key: "rate", label: "Throughput", accent: true },
  { key: "queue", label: "Frontier queue", accent: false },
  { key: "bytes", label: "Data fetched", accent: false },
  { key: "success", label: "Success rate", accent: false },
  { key: "latency", label: "Avg latency", accent: false },
  { key: "spool", label: "Awaiting index", accent: false },
  { key: "uptime", label: "Uptime", accent: false },
];

const tileNodes = new Map<string, { value: HTMLElement; sub: HTMLElement }>();

function buildTiles(): void {
  const host = $("#tiles");
  host.innerHTML = TILE_SPECS.map(
    (t) => `
      <div class="tile">
        <div class="tile__label">${esc(t.label)}</div>
        <div class="tile__value ${t.accent ? "tile__value--accent" : ""}" data-v="${t.key}">—</div>
        <div class="tile__sub" data-s="${t.key}">—</div>
      </div>`,
  ).join("");

  for (const t of TILE_SPECS) {
    tileNodes.set(t.key, {
      value: $(`[data-v="${t.key}"]`, host),
      sub: $(`[data-s="${t.key}"]`, host),
    });
  }
}

/**
 * Updates the stat tiles in place.
 *
 * This runs twice a second off the live feed. Rebuilding the tiles' innerHTML
 * at that rate re-parses and re-lays-out the whole row every tick, which is
 * enough to make the window feel sluggish while a crawl is saturating the CPU.
 * Writing textContent on existing nodes avoids all of that.
 */
function renderTiles(): void {
  if (!latest) return;
  if (tileNodes.size === 0) buildTiles();

  const { rates, status, frontier, agg } = latest;
  const totals = agg.totals ?? {};

  const values: Record<string, [string, string]> = {
    pages: [compactNumber(totals.pages ?? 0), `${compactNumber(totals.links ?? 0)} links found`],
    indexed: [compactNumber(idxStats?.docs ?? 0), `${idxStats?.segments ?? 0} segments`],
    hosts: [compactNumber(agg.hostCount ?? 0), `${status.hostsLive} active now`],
    rate: [rates.pagesPerSec.toFixed(1), "pages / second"],
    queue: [compactNumber(frontier.pending + status.buffered), `${status.buffered} in memory`],
    bytes: [formatBytes(totals.bytes ?? 0), `${formatBytes(rates.bytesPerSec)}/s`],
    success: [`${(rates.successRate * 100).toFixed(1)}%`, `${compactNumber(totals.errors ?? 0)} errors`],
    latency: [Math.round(rates.avgLatencyMs).toString(), "milliseconds"],
    spool: [compactNumber(frontier.spoolDepth), idxStats?.indexing ? "indexing now" : "idle"],
    uptime: [duration(rates.uptimeSeconds), `${status.workers} workers`],
  };

  for (const [key, [value, sub]] of Object.entries(values)) {
    const node = tileNodes.get(key);
    if (!node) continue;
    if (node.value.textContent !== value) node.value.textContent = value;
    if (node.sub.textContent !== sub) node.sub.textContent = sub;
  }
}

/** Signature of the last render, so unchanged panels are left alone. */
const rankSignatures = new Map<string, string>();
/** Containers that already have a delegated click handler. */
const rankDelegated = new Set<string>();

/**
 * Ranked single-hue bars. Magnitude is the job, so one hue plus direct labels.
 *
 * Clicks are handled by delegation on the container rather than per row: rows
 * are replaced whenever the data changes, and a listener bound to a row that is
 * about to be swapped out is exactly how a click ends up doing nothing.
 */
function renderRanks(
  target: string,
  rows: { label: string; n: number; color?: string }[],
  opts: { onClick?: (label: string) => void; max?: number } = {},
): void {
  const el = $(target);
  const limit = opts.max ?? 8;
  const shown = rows.slice(0, limit);

  const signature = shown.map((r) => `${r.label}:${r.n}`).join("|");
  if (rankSignatures.get(target) === signature) return;
  rankSignatures.set(target, signature);

  if (shown.length === 0) {
    el.innerHTML = `<div class="empty" style="padding:22px">no data yet</div>`;
    return;
  }
  const peak = Math.max(...shown.map((r) => r.n), 1);

  el.innerHTML = shown
    .map(
      (r) => `
      <div class="rank" data-label="${esc(r.label)}" ${opts.onClick ? 'role="button" tabindex="0"' : ""}>
        <span class="rank__label">${esc(r.label)}</span>
        <span class="rank__value">${compactNumber(r.n)}</span>
        <span class="rank__track">
          <span class="rank__fill" style="width:${(r.n / peak) * 100}%;background:${r.color ?? "var(--mag)"}"></span>
        </span>
      </div>`,
    )
    .join("");

  if (opts.onClick && !rankDelegated.has(target)) {
    rankDelegated.add(target);
    const fire = (e: Event) => {
      const row = (e.target as HTMLElement | null)?.closest<HTMLElement>(".rank");
      const label = row?.dataset.label;
      if (label) opts.onClick?.(label);
    };
    el.addEventListener("click", fire);
    el.addEventListener("keydown", (e) => {
      const key = (e as KeyboardEvent).key;
      if (key === "Enter" || key === " ") {
        e.preventDefault();
        fire(e);
      }
    });
  }
}

function renderAggregates(agg: Aggregates): void {
  renderRanks("#ranks-categories", agg.categories.map((c) => ({ label: c.label, n: c.n })), { max: 9 });
  renderRanks("#ranks-langs", agg.langs.map((c) => ({ label: c.label, n: c.n })), { max: 8 });

  renderRanks(
    "#ranks-statuses",
    bucketStatuses(agg.statuses).map((b) => ({ label: b.label, n: b.n, color: b.color })),
    { max: 4 },
  );

  const geoOn = agg.geo?.enabled ?? false;
  renderRanks(
    "#ranks-countries",
    (agg.countries ?? []).map((c) => ({ label: countryName(c.label), n: c.n })),
    { max: 9 },
  );
  $("#geo-note").textContent = geoOn ? (agg.geo.database ?? "GeoIP") : "no GeoIP database";
  $("#globe-title").textContent = geoOn ? "Live crawl surface" : "Live crawl activity";
  $("#globe-legend-note").textContent = geoOn
    ? "positions are real server locations"
    : "positions are approximate — add a GeoIP database for real locations";

  renderRanks(
    "#ranks-hosts",
    agg.topHosts.map((h) => ({ label: h.host, n: h.pages })),
    {
      max: 10,
      onClick: (host) => {
        switchView("search");
        $<HTMLInputElement>("#q").value = "";
        hostFilter = host;
        renderFilters();
        runSearch(0);
      },
    },
  );

  throughput.setData(agg.series.map((p) => ({ ts: p.ts, value: p.pages })));
}

function feedRowHTML(e: CrawlEvent): string {
  const statusClass = !e.ok ? "bad" : e.status >= 400 ? "warn" : "ok";
  const statusText = e.status > 0 ? String(e.status) : "ERR";
  const detail = e.ok
    ? `${e.links} links · ${e.latencyMs}ms`
    : esc(e.err ?? "failed");

  let path = "";
  try {
    const u = new URL(e.url);
    path = u.pathname === "/" ? "" : u.pathname;
  } catch {
    path = "";
  }

  return `
    <div class="feed__row feed__row--${e.ok ? "ok" : "bad"}" title="${esc(e.url)}">
      <span class="feed__time">${clockFmt.format(new Date(e.ts * 1000)).slice(0, 8)}</span>
      <span class="feed__status feed__status--${statusClass}">${statusText}</span>
      <span class="feed__url"><b>${esc(e.host)}</b>${esc(path)}</span>
      <span class="feed__meta">
        ${e.category ? `<span class="tag" style="color:${catColor(e.category)}">${esc(e.category)}</span> ` : ""}
        ${detail}
      </span>
    </div>`;
}

function renderFeedFull(): void {
  $("#feed").innerHTML = feedRows.map(feedRowHTML).join("");
  $("#feed-note").textContent = `${feedRows.length} most recent`;
}

/**
 * Adds newly crawled pages to the top of the live feed.
 *
 * `events` must be newest-first. Rows are prepended as real nodes rather than
 * re-rendering the list, so existing rows keep their identity — a full
 * innerHTML rewrite would replay every row's entry animation on every tick and
 * read as the panel flickering instead of scrolling.
 */
function pushFeed(events: CrawlEvent[]): void {
  if (events.length === 0) return;
  feedRows = [...events, ...feedRows].slice(0, MAX_FEED_ROWS);

  for (const e of events) {
    const geo = e.hasGeo && (e.lat !== 0 || e.lon !== 0) ? { lat: e.lat!, lon: e.lon! } : null;
    globe.hit(e.host, e.ok, geo);
  }

  if (activeView !== "overview") return;

  const feed = $("#feed");
  if (feed.childElementCount === 0) {
    renderFeedFull();
    return;
  }

  const staged = document.createElement("div");
  staged.innerHTML = events.map(feedRowHTML).join("");
  const nodes = Array.from(staged.children);

  // Keep the reader's place if they have scrolled away from the top.
  const pinnedToTop = feed.scrollTop < 8;
  const heightBefore = feed.scrollHeight;

  // Prepend back-to-front so events[0] ends up topmost.
  for (let i = nodes.length - 1; i >= 0; i--) feed.prepend(nodes[i]);
  while (feed.childElementCount > MAX_FEED_ROWS) feed.lastElementChild?.remove();

  if (!pinnedToTop) feed.scrollTop += feed.scrollHeight - heightBefore;
  $("#feed-note").textContent = `${feedRows.length} most recent`;
}

function renderTopbar(): void {
  if (!latest) return;
  const { rates, status } = latest;
  $("#pill-rate").textContent = `${rates.pagesPerSec.toFixed(1)} pages/s · ${rates.inflight} in flight`;
  $("#pill-indexed").textContent = `${compactNumber(idxStats?.docs ?? 0)} indexed`;
  $("#globe-note").textContent = `${status.hostsLive} hosts active`;

  const btn = $<HTMLButtonElement>("#btn-pause");
  btn.textContent = status.paused ? "Resume" : "Pause";
}

function setEngineState(connected: boolean, note?: string): void {
  const dot = $("#engine-dot");
  dot.className = "dot " + (connected ? "dot--live" : "dot--down");
  $("#engine-state").textContent = note ?? (connected ? "engine online" : "engine offline");
}

// -------------------------------------------------------------------- views --

function switchView(name: string): void {
  activeView = name;
  document.querySelectorAll<HTMLElement>(".view").forEach((v) => {
    v.classList.toggle("view--active", v.id === `view-${name}`);
  });
  document.querySelectorAll<HTMLElement>(".nav__item").forEach((b) => {
    if (b.dataset.view === name) b.setAttribute("aria-current", "page");
    else b.removeAttribute("aria-current");
  });

  if (name === "overview") {
    globe.resize();
    globe.start();
    throughput.render();
    renderFeedFull();
  } else {
    globe.stop();
  }

  if (name === "hosts") void loadHosts();
  if (name === "control") void loadControl();
  if (name === "settings") void loadSettings();
  if (name === "search") $<HTMLInputElement>("#q").focus();
}

document.querySelectorAll<HTMLElement>(".nav__item").forEach((b) => {
  b.addEventListener("click", () => switchView(b.dataset.view ?? "overview"));
});

// ------------------------------------------------------------------- search --

let categoryFilter: string | null = null;
let hostFilter: string | null = null;
let searchOffset = 0;
let lastTotal = 0;
const PAGE_SIZE = 25;

function renderFilters(): void {
  const cats = latest?.agg.categories ?? [];
  const chips: string[] = [
    `<button class="chip" data-cat="" aria-pressed="${categoryFilter === null}">All categories</button>`,
    ...cats
      .slice(0, 10)
      .map(
        (c) =>
          `<button class="chip" data-cat="${esc(c.label)}" aria-pressed="${categoryFilter === c.label}">${esc(c.label)} <span style="color:var(--ink-3)">${compactNumber(c.n)}</span></button>`,
      ),
  ];
  if (hostFilter) {
    chips.push(
      `<button class="chip" id="chip-host" aria-pressed="true">host: ${esc(hostFilter)} ✕</button>`,
    );
  }
  const el = $("#filters");
  el.innerHTML = chips.join("");

  el.querySelectorAll<HTMLElement>("[data-cat]").forEach((chip) => {
    chip.addEventListener("click", () => {
      categoryFilter = chip.dataset.cat ? chip.dataset.cat : null;
      renderFilters();
      runSearch(0);
    });
  });
  const hostChip = el.querySelector("#chip-host");
  hostChip?.addEventListener("click", () => {
    hostFilter = null;
    renderFilters();
    runSearch(0);
  });
}

function resultHTML(h: Hit, query: string): string {
  return `
    <div class="result" data-url="${esc(h.url)}" role="link" tabindex="0">
      <div class="result__url">${esc(breadcrumb(h.url))}</div>
      <div class="result__title">${highlight(h.title || h.host, query)}</div>
      <div class="result__snippet">${highlight(snippetFor(h, query), query)}</div>
      <div class="result__meta">
        <span class="tag" style="color:${catColor(h.category)}">${esc(h.category || "other")}</span>
        ${h.lang ? `<span>${esc(h.lang)}</span>` : ""}
        <span>${formatBytes(h.bytes)}</span>
        <span>crawled ${relativeTime(h.fetchedAt)}</span>
      </div>
    </div>`;
}

async function runSearch(offset: number): Promise<void> {
  const query = $<HTMLInputElement>("#q").value.trim();
  const sort = $<HTMLSelectElement>("#sort").value as "relevance" | "recent";

  if (!query && !categoryFilter && !hostFilter) {
    $("#results").innerHTML = `
      <div class="empty">
        <div class="empty__big">⌕</div>
        Type a query, or pick a category to browse what has been indexed.
      </div>`;
    $("#search-summary").textContent = "";
    updatePager(0, 0);
    return;
  }

  searchOffset = offset;
  try {
    const res = await search({
      query,
      limit: PAGE_SIZE,
      offset,
      category: categoryFilter,
      host: hostFilter,
      sort,
    });
    lastTotal = res.total;

    const summary =
      `About ${res.total.toLocaleString()} result${res.total === 1 ? "" : "s"} (${res.tookMs} ms)` +
      (res.total > 0 ? ` · showing ${offset + 1}–${offset + res.hits.length}` : "");
    $("#search-summary").innerHTML = res.relaxed
      ? `${esc(summary)} — <span style="color:var(--warn-ink)">no page contained every word, so these match some of them</span>`
      : esc(summary);

    if (res.hits.length === 0) {
      $("#results").innerHTML = `
        <div class="empty">
          <div class="empty__big">∅</div>
          Nothing matched.<br />
          The index only contains pages this crawler has already fetched — give it time, or add seeds.
        </div>`;
    } else {
      $("#results").innerHTML = res.hits.map((h) => resultHTML(h, query)).join("");
      $("#results")
        .querySelectorAll<HTMLElement>(".result")
        .forEach((row) => {
          const open = () => {
            const url = row.dataset.url;
            if (url) void openExternal(url);
          };
          row.addEventListener("click", open);
          row.addEventListener("keydown", (e) => {
            const key = (e as KeyboardEvent).key;
            if (key === "Enter" || key === " ") {
              e.preventDefault();
              open();
            }
          });
        });
    }
    updatePager(offset, res.total);
  } catch (err) {
    toast(`Search failed: ${err}`, true);
  }
}

function updatePager(offset: number, total: number): void {
  $<HTMLButtonElement>("#btn-prev").disabled = offset <= 0;
  $<HTMLButtonElement>("#btn-next").disabled = offset + PAGE_SIZE >= total;
}

async function openExternal(url: string): Promise<void> {
  try {
    const { openUrl } = await import("@tauri-apps/plugin-opener");
    await openUrl(url);
  } catch {
    toast("Could not open link", true);
  }
}

$("#btn-search").addEventListener("click", () => void runSearch(0));
$("#sort").addEventListener("change", () => void runSearch(0));

// Search as you type. 220 ms is long enough that a fast typist issues one
// query per word rather than one per keystroke.
let typeTimer: number | undefined;
$("#q").addEventListener("input", () => {
  if (typeTimer) window.clearTimeout(typeTimer);
  typeTimer = window.setTimeout(() => void runSearch(0), 220);
});
$("#q").addEventListener("keydown", (e) => {
  const key = (e as KeyboardEvent).key;
  if (key === "Enter") {
    if (typeTimer) window.clearTimeout(typeTimer);
    void runSearch(0);
  } else if (key === "Escape") {
    $<HTMLInputElement>("#q").value = "";
    void runSearch(0);
  }
});

document.addEventListener("keydown", (e) => {
  const target = e.target as HTMLElement | null;
  const typing = target && /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName);

  // "/" jumps to search from anywhere, as it does in most search UIs.
  if (e.key === "/" && !typing) {
    e.preventDefault();
    switchView("search");
    $<HTMLInputElement>("#q").focus();
    return;
  }
});

$("#btn-prev").addEventListener("click", () => void runSearch(Math.max(0, searchOffset - PAGE_SIZE)));
$("#btn-next").addEventListener("click", () => {
  if (searchOffset + PAGE_SIZE < lastTotal) void runSearch(searchOffset + PAGE_SIZE);
});

// ------------------------------------------------------------------ control --

/** Wires a range input to its numeric readout. */
function bindSlider(id: string, format: (v: number) => string): void {
  const input = $<HTMLInputElement>(`#${id}`);
  const out = $(`#${id}-out`);
  const sync = () => {
    out.textContent = format(Number(input.value));
  };
  input.addEventListener("input", sync);
  sync();
}

bindSlider("ctl-workers", (v) => `${v}`);
bindSlider("ctl-delay", (v) => (v === 0 ? "no delay" : `${v} ms`));
bindSlider("ctl-burst", (v) => `${v}`);

async function loadControl(): Promise<void> {
  try {
    config = await getConfig();
    $<HTMLInputElement>("#ctl-workers").value = String(config.workers);
    $<HTMLInputElement>("#ctl-delay").value = String(config.perHostDelayMs);
    $<HTMLInputElement>("#ctl-burst").value = String(config.perHostBurst);
    $("#ctl-workers-out").textContent = String(config.workers);
    $("#ctl-delay-out").textContent =
      config.perHostDelayMs === 0 ? "no delay" : `${config.perHostDelayMs} ms`;
    $("#ctl-burst-out").textContent = String(config.perHostBurst);
  } catch (err) {
    toast(`Could not read configuration: ${err}`, true);
  }
  renderControlStats();
}

function renderControlStats(): void {
  if (!latest) return;
  const { rates, status, frontier } = latest;

  $("#ctl-state").textContent = status.paused
    ? "paused"
    : status.running
      ? `running · ${rates.pagesPerSec.toFixed(1)} pages/s`
      : "stopped";
  $<HTMLButtonElement>("#ctl-pause").textContent = status.paused ? "Resume" : "Pause";

  const disc = status.discoveryLastRun
    ? `last ${new Date(status.discoveryLastRun * 1000).toLocaleTimeString()}`
    : "never run";
  const discErr = status.discoveryLastErr ? ` · ${status.discoveryLastErr}` : "";
  $("#ctl-discovery").textContent =
    status.discovered > 0 || status.discoveryLastRun > 0
      ? `Discovery — ${status.discovered.toLocaleString()} new sites found, ${disc}${discErr}`
      : "";

  renderRanks("#ctl-stats", [
    { label: `Queued URLs — ${(frontier.pending + status.buffered).toLocaleString()}`, n: frontier.pending + status.buffered },
    { label: `In memory — ${status.buffered.toLocaleString()}`, n: status.buffered },
    { label: `Awaiting index — ${frontier.spoolDepth.toLocaleString()}`, n: frontier.spoolDepth },
    { label: `Engine heap — ${frontier.heapMb.toFixed(0)} MB`, n: Math.round(frontier.heapMb) },
    { label: `Engine RSS — ${frontier.rssMb.toFixed(0)} MB`, n: Math.round(frontier.rssMb) },
    { label: `Frontier on disk — ${formatBytes(frontier.diskBytes)}`, n: frontier.diskBytes },
    { label: `Sites discovered — ${status.discovered.toLocaleString()}`, n: status.discovered },
  ], { max: 7 });
}

/** Runs a control action with consistent feedback and button locking. */
async function runAction(button: HTMLButtonElement, action: ControlAction): Promise<void> {
  const original = button.textContent;
  button.disabled = true;
  button.textContent = "Working…";
  try {
    const res = await control(action);
    toast(res.message || "Done");
    if (latest) latest.status = res.status;
    renderControlStats();
  } catch (err) {
    toast(`${action} failed: ${err}`, true);
  } finally {
    button.disabled = false;
    button.textContent = original;
  }
}

function wireAction(id: string, action: ControlAction, confirmText?: string): void {
  const button = $<HTMLButtonElement>(`#${id}`);
  button.addEventListener("click", () => {
    if (confirmText && !window.confirm(confirmText)) return;
    void runAction(button, action);
  });
}

wireAction("ctl-reseed", "reseed");
wireAction("ctl-discover", "discover");
wireAction("ctl-free", "freeMemory");
wireAction("ctl-compact", "compact");
wireAction("ctl-robots", "clearRobots");
wireAction("ctl-clear-queue", "clearQueue", "Empty the frontier queue? Crawled pages are kept.");
wireAction("ctl-reset-stats", "resetStats", "Reset all dashboard statistics to zero?");
wireAction(
  "ctl-clear-seen",
  "clearSeen",
  "Forget every crawled URL? The whole web becomes eligible again and everything will be re-fetched.",
);

/** Runs an async callback with consistent button locking and error handling. */
async function runEngineAction(
  button: HTMLButtonElement,
  fn: () => Promise<void>,
): Promise<void> {
  const original = button.textContent;
  button.disabled = true;
  button.textContent = "Working…";
  try {
    await fn();
  } catch (err) {
    toast(`${err}`, true);
  } finally {
    button.disabled = false;
    button.textContent = original;
  }
}

// "Stop engine" stops the entire engine process; "Start engine" brings it back.
// The crawl keeps running on its own, so neither is needed for normal use.
$("#ctl-stop").addEventListener("click", () => {
  if (
    !window.confirm(
      "Stop the engine process? The crawl halts until you click Start or reopen the app.",
    )
  )
    return;
  void runEngineAction($<HTMLButtonElement>("#ctl-stop"), async () => {
    await engineStop();
    setEngineState(false, "engine stopped");
    toast("Engine stopped");
  });
});
$("#ctl-start-engine").addEventListener("click", () => {
  void runEngineAction($<HTMLButtonElement>("#ctl-start-engine"), async () => {
    const info = await engineStart();
    setEngineState(true, "engine starting…");
    toast(`Engine starting (port ${info.port})`);
    window.setTimeout(() => void refreshIndexStats(), 2000);
  });
});

$("#ctl-pause").addEventListener("click", () => {
  const button = $<HTMLButtonElement>("#ctl-pause");
  void runAction(button, latest?.status.paused ? "resume" : "pause");
});

$("#ctl-apply").addEventListener("click", async () => {
  const button = $<HTMLButtonElement>("#ctl-apply");
  button.disabled = true;
  try {
    config = await setConfig({
      workers: Number($<HTMLInputElement>("#ctl-workers").value),
      perHostDelayMs: Number($<HTMLInputElement>("#ctl-delay").value),
      perHostBurst: Number($<HTMLInputElement>("#ctl-burst").value),
    });
    toast(`Applied: ${config.workers} workers, ${config.perHostDelayMs} ms delay`);
  } catch (err) {
    toast(`Could not apply settings: ${err}`, true);
  } finally {
    button.disabled = false;
  }
});

// -------------------------------------------------------------------- hosts --

async function loadHosts(): Promise<void> {
  try {
    const rows: HostRow[] = await getHosts(300);
    $("#hosts-note").textContent = `${rows.length} shown`;
    $("#hosts-body").innerHTML = rows
      .map(
        (h) => `
        <tr>
          <td>${esc(h.host)}</td>
          <td><span class="tag" style="color:${catColor(h.category)}">${esc(h.category)}</span></td>
          <td class="num">${h.pages.toLocaleString()}</td>
          <td class="num">${h.errors.toLocaleString()}</td>
          <td class="num">${formatBytes(h.bytes)}</td>
          <td class="num">${relativeTime(h.lastSeen)}</td>
        </tr>`,
      )
      .join("");
  } catch (err) {
    toast(`Could not load hosts: ${err}`, true);
  }
}

// ----------------------------------------------------------------- settings --

async function loadSettings(): Promise<void> {
  try {
    config = await getConfig();
    $<HTMLInputElement>("#cfg-workers").value = String(config.workers);
    $<HTMLInputElement>("#cfg-delay").value = String(config.perHostDelayMs);
    $<HTMLInputElement>("#cfg-depth").value = String(config.maxDepth);
    $<HTMLInputElement>("#cfg-recrawl").value = String(config.recrawlAfterHours);
    $<HTMLInputElement>("#cfg-ua").value = config.userAgent;
    $<HTMLInputElement>("#cfg-robots").checked = config.respectRobots;
    $<HTMLInputElement>("#cfg-adult").checked = config.crawlAdult;
    $<HTMLInputElement>("#cfg-html").checked = config.onlyHtml;
    $<HTMLInputElement>("#cfg-discovery").checked = config.discoveryEnabled;
    $<HTMLInputElement>("#cfg-disc-interval").value = String(config.discoveryIntervalMin);
    $<HTMLInputElement>("#cfg-disc-cap").value = String(config.discoveryMaxPerCycle);
    $<HTMLTextAreaElement>("#cfg-disc-sources").value = (config.discoverySources ?? []).join("\n");
  } catch (err) {
    toast(`Could not load configuration: ${err}`, true);
  }

  try {
    const logs = await engineLogs();
    $("#log").textContent = logs.slice(-160).join("\n") || "—";
    $("#log-note").textContent = `${logs.length} lines`;
    const logEl = $("#log");
    logEl.scrollTop = logEl.scrollHeight;
  } catch {
    /* log panel is best-effort */
  }

  const frontierBytes = latest?.frontier.diskBytes ?? 0;
  const indexBytes = idxStats?.diskBytes ?? 0;
  renderRanks("#storage", [
    { label: `Frontier store (Pebble) — ${formatBytes(frontierBytes)}`, n: frontierBytes },
    { label: `Search index (Tantivy) — ${formatBytes(indexBytes)}`, n: indexBytes },
  ]);
}

$("#btn-save-config").addEventListener("click", async () => {
  try {
    const patch: Partial<EngineConfig> = {
      workers: Number($<HTMLInputElement>("#cfg-workers").value),
      perHostDelayMs: Number($<HTMLInputElement>("#cfg-delay").value),
      maxDepth: Number($<HTMLInputElement>("#cfg-depth").value),
      recrawlAfterHours: Number($<HTMLInputElement>("#cfg-recrawl").value),
      userAgent: $<HTMLInputElement>("#cfg-ua").value,
      respectRobots: $<HTMLInputElement>("#cfg-robots").checked,
      crawlAdult: $<HTMLInputElement>("#cfg-adult").checked,
      onlyHtml: $<HTMLInputElement>("#cfg-html").checked,
      discoveryEnabled: $<HTMLInputElement>("#cfg-discovery").checked,
      discoveryIntervalMin: Number($<HTMLInputElement>("#cfg-disc-interval").value),
      discoveryMaxPerCycle: Number($<HTMLInputElement>("#cfg-disc-cap").value),
      discoverySources: $<HTMLTextAreaElement>("#cfg-disc-sources").value
        .split(/\r?\n/)
        .map((s) => s.trim())
        .filter(Boolean),
    };
    config = await setConfig(patch);
    toast("Configuration saved");
  } catch (err) {
    toast(`Save failed: ${err}`, true);
  }
});

$("#btn-add-seeds").addEventListener("click", async () => {
  const box = $<HTMLTextAreaElement>("#seeds");
  const urls = box.value.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);
  if (urls.length === 0) {
    toast("Nothing to add", true);
    return;
  }
  try {
    const res = await addSeeds(urls);
    toast(`Queued ${res.added} new URL${res.added === 1 ? "" : "s"} of ${res.submitted}`);
    box.value = "";
  } catch (err) {
    toast(`Could not add seeds: ${err}`, true);
  }
});

$("#btn-stop").addEventListener("click", async () => {
  try {
    await control("stop");
    toast("Crawler stopped");
  } catch (err) {
    toast(`Stop failed: ${err}`, true);
  }
});

$("#btn-pause").addEventListener("click", async () => {
  const paused = latest?.status.paused ?? false;
  try {
    await control(paused ? "resume" : "pause");
    toast(paused ? "Crawling resumed" : "Crawling paused");
  } catch (err) {
    toast(`Could not change state: ${err}`, true);
  }
});

// ------------------------------------------------------------------ startup --

function startClock(): void {
  const tick = () => {
    $("#pill-clock").textContent = clockFmt.format(new Date());
  };
  tick();
  window.setInterval(tick, 1000);
}

async function refreshIndexStats(): Promise<void> {
  try {
    idxStats = await indexStats();
    renderTopbar();
    renderTiles();
  } catch {
    /* index may not be ready during startup */
  }
}

let resizeTimer: number | undefined;
window.addEventListener("resize", () => {
  if (resizeTimer) window.clearTimeout(resizeTimer);
  resizeTimer = window.setTimeout(() => {
    if (activeView === "overview") {
      globe.resize();
      throughput.render();
    }
  }, 140);
});

async function boot(): Promise<void> {
  startClock();
  setEngineState(false, "starting engine…");

  // If the engine cannot be brought up within the boot window (missing
  // binary, slow start, port contention) keep the UI alive and show offline;
  // the live feed re-checks periodically and comes alive when the engine is
  // reachable again.
  let info: EngineInfo | null = null;
  try {
    info = await connectEngine();
  } catch (err) {
    setEngineState(false, "engine unavailable");
    toast(`Engine did not start: ${err}`, true);
  }

  if (info) {
    setEngineState(true);
    $("#nav-foot").textContent = `engine :${info.port}\n${await dataDir().catch(() => info.dataDir)}`;
  } else {
    $("#nav-foot").textContent = "engine :offline";
  }

  try {
    if (info) {
      latest = await getStats();
      renderTopbar();
      renderTiles();
      renderAggregates(latest.agg);
      renderFilters();
      pushFeed(latest.recent);
    }
  } catch (err) {
    toast(`Could not load statistics: ${err}`, true);
  }

  globe.resize();
  globe.start();

  openLiveFeed({
    onState: (connected) => setEngineState(connected),
    onSnapshot: (s) => {
      latest = s;
      renderTopbar();
      renderTiles();
      renderAggregates(s.agg);
      renderFilters();
      pushFeed(s.recent);
      spark.render(s.spark);
    },
    onTick: (t) => {
      if (latest) {
        latest.rates = t.rates;
        latest.status = t.status;
        latest.frontier = t.frontier;
      }
      renderTopbar();
      renderTiles();
      if (activeView === "overview") spark.render(t.spark);
      if (activeView === "control") renderControlStats();
      // Already newest-first, which is the order pushFeed expects.
      pushFeed(t.events);
    },
    onAgg: (agg) => {
      if (latest) latest.agg = agg;
      renderAggregates(agg);
    },
  });

  void refreshIndexStats();
  window.setInterval(() => void refreshIndexStats(), 3000);
}

void boot();
