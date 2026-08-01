// Package discover feeds the frontier brand-new URLs from external sources so a
// crawl keeps meeting websites it has never seen, instead of cycling forever
// over the same reachable set.
//
// A plain "harvest links from what we crawled" loop is a closed graph: once the
// seed set's link neighbourhood is exhausted, discovery stops. Discovery breaks
// that by periodically asking small, polite external sources that each point at
// many sites the frontier has not met yet:
//
//   - Hacker News story submissions (fresh outbound links, endlessly new),
//   - the daily Tranco top-domain list (a fresh ranking churns daily),
//   - any user-supplied RSS/Atom/OPML/TXT/CSV feed.
//
// Every candidate URL is handed to the frontier's Enqueue, which dedupes
// against the permanent seen-set, so re-discovered URLs cost nothing.
//
// Politeness: each source host's robots.txt is consulted (and cached for 24h)
// before any request, and each source makes at most a handful of requests per
// cycle, with a generous cycle interval.
package discover

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"worldscraper/engine/internal/config"
	"worldscraper/engine/internal/extract"
	"worldscraper/engine/internal/fetch"
	"worldscraper/engine/internal/kv"
	"worldscraper/engine/internal/robots"
	"worldscraper/engine/internal/urlx"
)

const (
	// The Tranco daily list is final the morning after a date, so the crawl
	// always asks for "yesterday": the freshest list that is guaranteed to
	// exist.
	trancoMetaURL = "https://tranco-list.eu/daily_list_id?date=%s"
	trancoListURL = "https://tranco-list.eu/download/%s/%d"
	// topMillion is the size of the standard list; the walk wraps at this.
	topMillion = 1000000

	// Persisted discovery cursor names (kv m/ space).
	keyTrancoID   = "discoverTrancoID"
	keyTrancoRank = "discoverTrancoRank"
)

// Source is one discovery input: a polite feed of candidate URLs.
type Source interface {
	// Name identifies the source in logs.
	Name() string
	// MaxBytes bounds the body size discovery is willing to read.
	MaxBytes() int64
	// Collect fetches the source and returns candidate URLs, at most limit of
	// them. allow is a robots.txt gate that must be consulted before each
	// request to a new URL.
	Collect(
		ctx context.Context,
		cl *fetch.Client,
		allow func(ctx context.Context, rawURL string) (bool, error),
		limit int,
	) ([]string, error)
}

// ------------------------------------------------------------------ sources --

// hnSearchURL is the Algolia-backed Hacker News search API: a public JSON
// endpoint that returns the newest story submissions, each of which links out
// to an external site. One request yields up to 100 fresh URLs.
const hnSearchURL = "https://hn.algolia.com/api/v1/search_by_date?tags=story&hitsPerPage=100"

// HackerNews offers the outbound links of the newest story submissions.
type HackerNews struct{}

func (HackerNews) Name() string { return "hackernews" }

func (HackerNews) MaxBytes() int64 { return 8 << 20 }

type hnResponse struct {
	Hits []struct {
		URL string `json:"url"`
	} `json:"hits"`
}

func (h HackerNews) Collect(
	ctx context.Context,
	cl *fetch.Client,
	allow func(ctx context.Context, rawURL string) (bool, error),
	limit int,
) ([]string, error) {
	ok, err := allow(ctx, hnSearchURL)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("blocked by robots.txt")
	}

	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	resp, err := cl.Get(fctx, hnSearchURL, false)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", hnSearchURL, err)
	}
	if resp.Status < 200 || resp.Status >= 300 || len(resp.Body) == 0 {
		return nil, fmt.Errorf("fetch %s: status %d", hnSearchURL, resp.Status)
	}

	var j hnResponse
	if err := json.Unmarshal(resp.Body, &j); err != nil {
		return nil, fmt.Errorf("parse %s: %w", hnSearchURL, err)
	}

	var out []string
	seen := make(map[string]struct{}, limit)
	for _, hit := range j.Hits {
		if hit.URL == "" {
			continue
		}
		if _, dup := seen[hit.URL]; dup {
			continue
		}
		seen[hit.URL] = struct{}{}
		out = append(out, hit.URL)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Tranco walks the daily top-domain list, resuming each cycle where it left
// off, so a fresh domain is offered to the frontier every cycle for the whole
// million. The cursor (list id + rank) survives restarts in the kv store.
type Tranco struct {
	kv *kv.DB

	mu   sync.Mutex
	id   string
	rank int
}

func (t *Tranco) Name() string { return "tranco-top1m" }

func (t *Tranco) MaxBytes() int64 { return 24 << 20 }

func (t *Tranco) Collect(
	ctx context.Context,
	cl *fetch.Client,
	allow func(ctx context.Context, rawURL string) (bool, error),
	limit int,
) ([]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id == "" {
		t.id = t.kv.GetString(keyTrancoID)
		t.rank = int(t.kv.Counter(keyTrancoRank))
	}

	date := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	metaURL := fmt.Sprintf(trancoMetaURL, date)
	if ok, err := allow(ctx, metaURL); err != nil {
		return nil, err
	} else if !ok {
		return nil, errors.New("blocked by robots.txt")
	}

	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	resp, err := cl.Get(fctx, metaURL, false)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", metaURL, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("tranco metadata: HTTP %d", resp.Status)
	}
	id := strings.TrimSpace(string(resp.Body))
	if id == "" {
		return nil, errors.New("tranco metadata: empty list id")
	}

	if id != t.id {
		// A new day's list: restart the walk from the top. Domains already
		// crawled are filtered out for free by the frontier's seen-set.
		t.id = id
		t.rank = 0
		_ = t.kv.SetString(keyTrancoID, id)
		_ = t.kv.SetCounter(keyTrancoRank, 0)
	}

	if t.rank >= topMillion {
		t.rank = 0
	}
	want := limit
	if t.rank+want > topMillion {
		want = topMillion - t.rank
	}
	if want <= 0 {
		return nil, nil
	}

	listURL := fmt.Sprintf(trancoListURL, id, t.rank+want)
	if ok, err := allow(ctx, listURL); err != nil {
		return nil, err
	} else if !ok {
		return nil, errors.New("blocked by robots.txt")
	}

	fctx, cancel = context.WithTimeout(ctx, 120*time.Second)
	listResp, err := cl.Get(fctx, listURL, false)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", listURL, err)
	}
	if listResp.Status != 200 {
		return nil, fmt.Errorf("tranco list: HTTP %d", listResp.Status)
	}

	body := listResp.Body
	if isZip(body) {
		if body, err = unzipFirst(body); err != nil {
			return nil, fmt.Errorf("tranco list zip: %w", err)
		}
	}

	domains := parseTopCSV(body, t.rank, want)
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		out = append(out, "https://"+d+"/")
	}

	t.rank += len(domains)
	if t.rank >= topMillion {
		t.rank = 0
	}
	_ = t.kv.SetCounter(keyTrancoRank, uint64(t.rank))
	return out, nil
}

// parseTopCSV reads "rank,domain" rows and returns the domains ranked in
// (lo, lo+want], i.e. the window after the persisted cursor.
func parseTopCSV(body []byte, lo, want int) []string {
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if len(out) >= want {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		i := strings.IndexByte(line, ',')
		if i <= 0 {
			continue
		}
		rank, err := strconv.Atoi(line[:i])
		if err != nil || rank <= lo {
			continue
		}
		d := strings.ToLower(strings.TrimSpace(line[i+1:]))
		if d == "" || !strings.Contains(d, ".") {
			continue
		}
		out = append(out, d)
	}
	return out
}

// Feed fetches one user-supplied URL and extracts candidate links from it,
// handling RSS/Atom/OPML XML, HTML pages and plain text or CSV URL lists.
type Feed struct {
	URL string
}

func (f Feed) Name() string { return "feed:" + f.URL }

func (f Feed) MaxBytes() int64 { return 24 << 20 }

func (f Feed) Collect(
	ctx context.Context,
	cl *fetch.Client,
	allow func(ctx context.Context, rawURL string) (bool, error),
	limit int,
) ([]string, error) {
	ok, err := allow(ctx, f.URL)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("blocked by robots.txt")
	}

	fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	resp, err := cl.Get(fctx, f.URL, false)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", f.URL, err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, fmt.Errorf("feed: HTTP %d", resp.Status)
	}

	body := resp.Body
	if isZip(body) {
		if body, err = unzipFirst(body); err != nil {
			return nil, fmt.Errorf("feed zip: %w", err)
		}
	}

	base, err := url.Parse(f.URL)
	if err != nil {
		base, _ = url.Parse("https://localhost/")
	}
	return parseFeed(body, resp.ContentType, base, limit), nil
}

// ---------------------------------------------------------------- parsing ---

var plainURLRe = regexp.MustCompile("https?://[^\\s\"<>`]+")

func parseFeed(body []byte, contentType string, base *url.URL, limit int) []string {
	var out []string
	seen := make(map[string]struct{}, limit)
	add := func(raw string) {
		if len(out) >= limit {
			return
		}
		raw = strings.TrimSpace(raw)
		if n, ok := urlx.Normalize(raw); ok {
			if _, dup := seen[n]; dup {
				return
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}

	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	lower := strings.ToLower(strings.TrimSpace(string(head)))

	// XML feeds first (RSS, Atom, OPML): cheap and precise.
	if len(body) > 0 && body[0] == '<' &&
		!strings.HasPrefix(lower, "<html") && !strings.HasPrefix(lower, "<!doctype html") {
		if parsed := parseXMLFeed(body, add); parsed {
			return out
		}
	}

	// HTML: hand it to the crawler's own extractor.
	if strings.Contains(lower, "<a ") || strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<!doctype") {
		doc := extract.Parse(body, contentType, base, extract.Limits{
			MaxLinks: 1500,
			MaxText:  24 << 10,
		})
		for _, l := range doc.Links {
			add(l)
		}
		return out
	}

	// Plain text / CSV: find bare URLs.
	for _, m := range plainURLRe.FindAllString(string(body), limit) {
		add(strings.TrimRight(m, ".,;:!?)]}>'\""))
	}
	return out
}

type opmlOutline struct {
	URL      string         `xml:"url,attr"`
	XMLURL   string         `xml:"xmlUrl,attr"`
	HTMLURL  string         `xml:"htmlUrl,attr"`
	Children []opmlOutline  `xml:"outline"`
}

type feedDoc struct {
	Channel struct {
		Items []struct {
			Link string `xml:"link"`
		} `xml:"item"`
	} `xml:"channel"`
	Entries []struct {
		Links []struct {
			Href string `xml:"href,attr"`
			Text string `xml:",chardata"`
		} `xml:"link"`
	} `xml:"entry"`
	Links []struct {
		Href string `xml:"href,attr"`
		Text string `xml:",chardata"`
	} `xml:"link"`
	Outlines []opmlOutline `xml:"outline"`
}

func parseXMLFeed(body []byte, add func(string)) bool {
	var doc feedDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return false
	}

	for _, it := range doc.Channel.Items {
		add(it.Link)
	}
	for _, en := range doc.Entries {
		for _, l := range en.Links {
			if l.Href != "" {
				add(l.Href)
			} else {
				add(l.Text)
			}
		}
	}
	for _, l := range doc.Links {
		if l.Href != "" {
			add(l.Href)
		} else {
			add(l.Text)
		}
	}
	var walk func([]opmlOutline)
	walk = func(os []opmlOutline) {
		for _, o := range os {
			add(o.URL)
			add(o.XMLURL)
			add(o.HTMLURL)
			walk(o.Children)
		}
	}
	walk(doc.Outlines)
	return true
}

// isZip reports whether the body is a ZIP archive (PK\x03\x04).
func isZip(b []byte) bool {
	return len(b) > 4 && b[0] == 'P' && b[1] == 'K' && b[2] == 0x03 && b[3] == 0x04
}

// unzipFirst returns the first non-directory entry of a zip archive.
func unzipFirst(b []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(rc, 64<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, errors.New("archive contains no files")
}

// ------------------------------------------------------------------ runner ---

// Discoverer polls a set of sources on a slow timer and enqueues the URLs they
// surface. It owns the robots.txt cache for source hosts and the lifetime
// counters surfaced in the dashboard.
type Discoverer struct {
	kvdb   *kv.DB
	client func() *fetch.Client
	token  func() string
	logf   func(string, ...any)

	robotMu sync.Mutex
	robots  map[string]*robotEntry

	runMu    sync.Mutex
	lastRun  atomic.Int64
	lastErr  atomic.Value // string
	injected atomic.Int64
}

type robotEntry struct {
	rules     *robots.Rules
	fetchedAt int64
}

// New builds a Discoverer. client and token are closures so live config
// changes (user agent, timeouts) apply to discovery fetches too.
func New(k *kv.DB, client func() *fetch.Client, token func() string, logf func(string, ...any)) *Discoverer {
	d := &Discoverer{
		kvdb:   k,
		client: client,
		token:  token,
		logf:   logf,
		robots: make(map[string]*robotEntry),
	}
	d.lastErr.Store("")
	return d
}

// Run executes one discovery cycle. It does nothing when discovery is
// disabled, unless force is set (the dashboard's "Discover sites" button).
// It returns how many genuinely new URLs were queued.
func (d *Discoverer) Run(ctx context.Context, cfg config.Config, force bool) (int, error) {
	if !cfg.DiscoveryEnabled && !force {
		return 0, nil
	}

	d.runMu.Lock()
	defer d.runMu.Unlock()

	limit := cfg.DiscoveryMaxPerCycle
	var items []kv.Item
	seen := make(map[string]struct{}, limit)
	var firstErr error

	for _, src := range d.sources(cfg) {
		if limit <= 0 {
			break
		}
		allow := func(ctx context.Context, rawURL string) (bool, error) {
			return d.allowed(ctx, rawURL)
		}
		urls, err := src.Collect(ctx, d.client(), allow, limit)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", src.Name(), err)
			}
			d.logf("discovery: %s: %v", src.Name(), err)
			continue
		}
		for _, u := range urls {
			if _, dup := seen[u]; dup {
				continue
			}
			seen[u] = struct{}{}
			items = append(items, kv.Item{URL: u, Host: urlx.Host(u), Depth: 0})
			if len(items) >= cfg.DiscoveryMaxPerCycle {
				break
			}
		}
		limit = cfg.DiscoveryMaxPerCycle - len(items)
	}

	added := 0
	if len(items) > 0 {
		var err error
		added, err = d.kvdb.Enqueue(items)
		if err != nil {
			d.logf("discovery: enqueue: %v", err)
		}
	}

	d.lastRun.Store(time.Now().Unix())
	if firstErr != nil {
		d.lastErr.Store(firstErr.Error())
	} else {
		d.lastErr.Store("")
	}
	d.injected.Add(int64(added))

	d.logf("discovery: cycle finished, %d candidates, %d new URLs queued", len(items), added)
	return added, nil
}

// sources assembles the source set for one cycle: the two built-ins plus every
// user-configured feed.
func (d *Discoverer) sources(cfg config.Config) []Source {
	ss := []Source{
		HackerNews{},
		&Tranco{kv: d.kvdb},
	}
	for _, raw := range cfg.DiscoverySources {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		if _, ok := urlx.Normalize(raw); ok {
			ss = append(ss, Feed{URL: raw})
		}
	}
	return ss
}

// allowed checks a URL against its host's robots.txt, caching rules for 24h.
func (d *Discoverer) allowed(ctx context.Context, rawURL string) (bool, error) {
	host := urlx.Host(rawURL)
	if host == "" {
		return true, nil
	}
	path := requestPath(rawURL)

	d.robotMu.Lock()
	e := d.robots[host]
	d.robotMu.Unlock()
	if e != nil && time.Since(time.Unix(e.fetchedAt, 0)) < 24*time.Hour {
		return e.rules.Allowed(path), nil
	}

	if rec, ok := d.kvdb.GetRobots(host); ok && time.Since(time.Unix(rec.FetchedAt, 0)) < 24*time.Hour {
		var rules *robots.Rules
		if rec.OK {
			rules = robots.Parse(rec.Body, d.token())
		} else {
			rules = robots.AllowAllRules()
		}
		d.cacheRobots(host, rules)
		return rules.Allowed(path), nil
	}

	return d.fetchRobots(ctx, host).Allowed(path), nil
}

// fetchRobots mirrors the crawler's politeness: 4xx means "no restrictions",
// 5xx means back off, and a network failure falls back to permissive so a
// transient outage does not silently kill discovery.
func (d *Discoverer) fetchRobots(ctx context.Context, host string) *robots.Rules {
	cl := d.client()
	rec := kv.Robots{FetchedAt: time.Now().Unix()}
	rules := robots.AllowAllRules()

	for _, scheme := range []string{"https", "http"} {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := cl.Get(rctx, scheme+"://"+host+"/robots.txt", false)
		cancel()
		if err != nil {
			continue
		}
		switch {
		case resp.Status == 200:
			body := string(resp.Body)
			if len(body) > 512<<10 {
				body = body[:512<<10]
			}
			rec.Body, rec.OK = body, true
			rules = robots.Parse(body, d.token())
		case resp.Status >= 500:
			rec.OK = false
			rules = robots.DenyAll()
		default:
			rec.OK = true
		}
		break
	}

	if err := d.kvdb.PutRobots(host, rec); err != nil {
		d.logf("discovery: cache robots for %s: %v", host, err)
	}
	d.cacheRobots(host, rules)
	return rules
}

func (d *Discoverer) cacheRobots(host string, rules *robots.Rules) {
	d.robotMu.Lock()
	d.robots[host] = &robotEntry{rules: rules, fetchedAt: time.Now().Unix()}
	d.robotMu.Unlock()
}

// LastRun returns the unix time of the most recent cycle (0 if never).
func (d *Discoverer) LastRun() int64 { return d.lastRun.Load() }

// Stats reports lifetime counters for the dashboard.
func (d *Discoverer) Stats() (injected int64, lastRun int64, lastErr string) {
	s := d.lastErr.Load()
	if s == nil {
		s = ""
	}
	return d.injected.Load(), d.lastRun.Load(), s.(string)
}

// Reset clears the lifetime counter (used by "reset statistics").
func (d *Discoverer) Reset() { d.injected.Store(0) }

// requestPath returns the escaped path-plus-query used for robots matching.
func requestPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "/"
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}
