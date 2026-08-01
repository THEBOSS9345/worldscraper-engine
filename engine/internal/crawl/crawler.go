// Package crawl is the crawl orchestrator: a polite, per-host scheduler in
// front of a large worker pool, with batched persistence behind it.
//
// Shape of the pipeline:
//
//	Pebble frontier -> refill -> per-host queues -> scheduler -> workers
//	                                                               |
//	           Pebble (frontier + spool) <- collector <- results <-+
//	                     SQLite (aggregates)
//
// The scheduler is the only thing that decides *when* a host may be hit again,
// which keeps politeness in one place regardless of how many workers exist.
package crawl

import (
	"context"
	"errors"
	"net/url"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"worldscraper/engine/internal/classify"
	"worldscraper/engine/internal/config"
	"worldscraper/engine/internal/discover"
	"worldscraper/engine/internal/extract"
	"worldscraper/engine/internal/fetch"
	"worldscraper/engine/internal/geoip"
	"worldscraper/engine/internal/kv"
	"worldscraper/engine/internal/meta"
	"worldscraper/engine/internal/metrics"
	"worldscraper/engine/internal/robots"
	"worldscraper/engine/internal/urlx"
)

const (
	// A host that keeps failing is parked rather than retried forever.
	maxHostFails = 12
	// Documents are indexed with at most this much body text.
	maxBodyText = 24 << 10
	// Below this queue depth the engine starts pulling recrawl work early so
	// the crawl never goes idle.
	lowWaterMark = 2000
)

type hostState int

const (
	hostNeedRobots hostState = iota
	hostFetchingRobots
	hostReady
	hostParked
)

type hostEntry struct {
	name     string
	queue    []kv.Leased
	nextAt   time.Time
	inflight int
	state    hostState
	rules    *robots.Rules
	delay    time.Duration
	fails    int
	inRing   bool
}

type job struct {
	robots bool
	host   string
	item   kv.Leased
}

type result struct {
	comp     kv.Completion
	ev       metrics.Event
	host     string
	site     string
	category string
	lang     string
	country  string
	status   int
	bytes    int64
	ok       bool
	links    int
}

// Crawler owns the running crawl.
type Crawler struct {
	kvdb   *kv.DB
	metadb *meta.DB
	met    *metrics.M
	geo    *geoip.DB // nil when no GeoIP database is installed
	disc   *discover.Discoverer

	cfg    atomic.Pointer[config.Config]
	client atomic.Pointer[fetch.Client]

	hostsMu  sync.Mutex
	hosts    map[string]*hostEntry
	ring     []string
	ringPos  int
	buffered int

	work    chan job
	results chan result

	seedsMu sync.Mutex
	seeds   []string

	runMu   sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool

	uaToken   string
	lastDry   atomic.Int64
	recrawled atomic.Int64
}

// New builds a crawler. Start must be called to begin work. geo may be nil,
// in which case crawled pages carry no location.
func New(k *kv.DB, m *meta.DB, met *metrics.M, geo *geoip.DB, cfg config.Config, seeds []string) *Crawler {
	c := &Crawler{
		kvdb:   k,
		metadb: m,
		met:    met,
		geo:    geo,
		hosts:  make(map[string]*hostEntry),
		seeds:  seeds,
	}
	c.applyConfig(cfg)
	c.disc = discover.New(
		k,
		func() *fetch.Client { return c.client.Load() },
		func() string { return productToken(c.cfg.Load().UserAgent) },
		logf,
	)
	return c
}

// Config returns a copy of the live configuration.
func (c *Crawler) Config() config.Config { return *c.cfg.Load() }

// applyConfig stores config and rebuilds the HTTP client to match.
func (c *Crawler) applyConfig(cfg config.Config) {
	cfg.Sanitize()
	c.cfg.Store(&cfg)
	c.uaToken = productToken(cfg.UserAgent)
	c.client.Store(fetch.New(fetch.Options{
		UserAgent:       cfg.UserAgent,
		Timeout:         time.Duration(cfg.RequestTimeoutMS) * time.Millisecond,
		MaxBytes:        cfg.MaxPageBytes,
		MaxRedirects:    cfg.FollowRedirects,
		InsecureTLS:     cfg.InsecureTLS,
		MaxConnsPerHost: cfg.PerHostBurst + 1,
	}))
}

// SetConfig swaps configuration in. Changing the worker count restarts the
// pool, because pool size is fixed for the lifetime of a run.
func (c *Crawler) SetConfig(cfg config.Config) error {
	cfg.Sanitize()
	old := c.cfg.Load()
	needRestart := old.Workers != cfg.Workers

	if err := c.metadb.PutJSON("config", cfg); err != nil {
		return err
	}
	c.applyConfig(cfg)

	if needRestart && c.isRunning() {
		c.Stop()
		return c.Start()
	}
	return nil
}

// SetSeeds replaces the seed list used when the frontier runs dry.
func (c *Crawler) SetSeeds(seeds []string) {
	c.seedsMu.Lock()
	c.seeds = seeds
	c.seedsMu.Unlock()
}

// AddSeeds normalizes and enqueues URLs at depth 0.
func (c *Crawler) AddSeeds(raw []string) (int, error) {
	items := make([]kv.Item, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" || strings.HasPrefix(r, "#") {
			continue
		}
		if !strings.Contains(r, "://") {
			r = "https://" + r
		}
		n, ok := urlx.Normalize(r)
		if !ok {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		items = append(items, kv.Item{URL: n, Host: urlx.Host(n), Depth: 0})
	}
	if len(items) == 0 {
		return 0, nil
	}
	return c.kvdb.Enqueue(items)
}

func (c *Crawler) isRunning() bool {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	return c.running
}

// Start launches the scheduler, worker pool, collector and maintenance loops.
func (c *Crawler) Start() error {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.running {
		return nil
	}

	// Anything left in-flight from a previous process goes back on the queue.
	if n, err := c.kvdb.SweepLeases(); err != nil {
		return err
	} else if n > 0 {
		logf("recovered %d in-flight URLs from previous run", n)
	}

	cfg := c.cfg.Load()
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.work = make(chan job, cfg.Workers*2)
	c.results = make(chan result, 8192)
	c.running = true

	for i := 0; i < cfg.Workers; i++ {
		c.wg.Add(1)
		go c.worker(ctx)
	}
	c.wg.Add(3)
	go c.scheduler(ctx)
	go c.collector(ctx)
	go c.maintenance(ctx)

	logf("crawler started: %d workers, %dms per-host delay", cfg.Workers, cfg.PerHostDelayMS)
	return nil
}

// Stop halts the crawl and waits for in-flight work to drain.
func (c *Crawler) Stop() {
	c.runMu.Lock()
	if !c.running {
		c.runMu.Unlock()
		return
	}
	c.running = false
	cancel := c.cancel
	c.runMu.Unlock()

	cancel()
	c.wg.Wait()

	// Return everything still buffered in memory to the durable frontier.
	c.hostsMu.Lock()
	var back []kv.Item
	for _, e := range c.hosts {
		for _, l := range e.queue {
			back = append(back, l.Item)
		}
		e.queue = nil
	}
	c.hosts = make(map[string]*hostEntry)
	c.ring = nil
	c.ringPos = 0
	c.buffered = 0
	c.hostsMu.Unlock()

	if len(back) > 0 {
		if err := c.kvdb.Requeue(back); err != nil {
			logf("requeue on stop: %v", err)
		}
	}
	c.kvdb.PersistCounters()
	_ = c.kvdb.Flush()
	logf("crawler stopped")
}

// ------------------------------------------------------------- scheduling --

func (c *Crawler) scheduler(ctx context.Context) {
	defer c.wg.Done()

	dispatch := time.NewTicker(10 * time.Millisecond)
	defer dispatch.Stop()
	refill := time.NewTicker(250 * time.Millisecond)
	defer refill.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-refill.C:
			if c.cfg.Load().Paused {
				continue
			}
			c.refill()

		case <-dispatch.C:
			if c.cfg.Load().Paused {
				continue
			}
			jobs, drops := c.pick(256)
			for _, d := range drops {
				select {
				case c.results <- d:
				case <-ctx.Done():
					return
				}
			}
			for _, j := range jobs {
				select {
				case c.work <- j:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// refill tops the in-memory per-host queues up from the durable frontier.
func (c *Crawler) refill() {
	cfg := c.cfg.Load()
	target := cfg.Workers * 64
	if target < 4000 {
		target = 4000
	}
	if target > 60000 {
		target = 60000
	}

	c.hostsMu.Lock()
	have := c.buffered
	c.hostsMu.Unlock()

	if have >= target {
		return
	}
	items, err := c.kvdb.Lease(target - have)
	if err != nil {
		logf("lease: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}

	c.hostsMu.Lock()
	for _, it := range items {
		host := it.Host
		if host == "" {
			host = urlx.Host(it.URL)
			it.Host = host
		}
		if host == "" {
			continue
		}
		e := c.hostLocked(host)
		e.queue = append(e.queue, it)
		c.buffered++
		if !e.inRing {
			e.inRing = true
			c.ring = append(c.ring, host)
		}
	}
	live := int64(len(c.ring))
	c.hostsMu.Unlock()
	c.met.SetHostsLive(live)
}

// hostLocked fetches or creates a host entry. Caller holds hostsMu.
func (c *Crawler) hostLocked(host string) *hostEntry {
	e, ok := c.hosts[host]
	if !ok {
		e = &hostEntry{name: host, state: hostNeedRobots}
		c.hosts[host] = e
	}
	return e
}

// pick walks the host ring and returns work that is eligible right now.
// Nothing here blocks, so politeness decisions stay cheap.
func (c *Crawler) pick(max int) ([]job, []result) {
	cfg := c.cfg.Load()
	now := time.Now()

	var jobs []job
	var drops []result

	c.hostsMu.Lock()
	defer c.hostsMu.Unlock()

	n := len(c.ring)
	if n == 0 {
		return nil, nil
	}

	scanned := 0
	for scanned < n && len(jobs) < max {
		if c.ringPos >= len(c.ring) {
			c.ringPos = 0
		}
		host := c.ring[c.ringPos]
		e := c.hosts[host]
		scanned++

		if e == nil || len(e.queue) == 0 {
			c.removeFromRingLocked(c.ringPos)
			if e != nil {
				e.inRing = false
			}
			n = len(c.ring)
			continue
		}

		// A host that has failed repeatedly gets its queue discarded so the
		// crawl does not spend its life retrying a dead server.
		if e.fails >= maxHostFails {
			for _, l := range e.queue {
				drops = append(drops, result{
					comp: kv.Completion{Lease: l.Lease, URL: l.URL, State: kv.StateDead, DoneAt: now.Unix()},
					host: host,
					ev: metrics.Event{
						TS: now.Unix(), URL: l.URL, Host: host,
						OK: false, Err: "host unreachable",
					},
				})
			}
			c.buffered -= len(e.queue)
			e.queue = nil
			e.state = hostParked
			c.removeFromRingLocked(c.ringPos)
			e.inRing = false
			n = len(c.ring)
			continue
		}

		if now.Before(e.nextAt) || e.inflight >= cfg.PerHostBurst {
			c.ringPos++
			continue
		}

		switch e.state {
		case hostNeedRobots:
			if !cfg.RespectRobots {
				e.rules = robots.AllowAllRules()
				e.state = hostReady
				continue // re-evaluate this host on the next pass
			}
			e.state = hostFetchingRobots
			e.inflight++
			jobs = append(jobs, job{robots: true, host: host})

		case hostFetchingRobots, hostParked:
			c.ringPos++

		case hostReady:
			l := e.queue[0]
			e.queue = e.queue[1:]
			c.buffered--

			if skip, reason := c.shouldSkip(cfg, e, l.URL); skip {
				drops = append(drops, result{
					comp: kv.Completion{Lease: l.Lease, URL: l.URL, State: kv.StateDead, DoneAt: now.Unix()},
					host: host,
					ev: metrics.Event{
						TS: now.Unix(), URL: l.URL, Host: host, OK: false, Err: reason,
					},
				})
				continue
			}

			e.inflight++
			e.nextAt = now.Add(c.hostDelay(cfg, e))
			jobs = append(jobs, job{host: host, item: l})
			c.ringPos++
		}
	}

	return jobs, drops
}

// removeFromRingLocked deletes ring[i] with an O(1) swap. Caller holds hostsMu.
func (c *Crawler) removeFromRingLocked(i int) {
	last := len(c.ring) - 1
	c.ring[i] = c.ring[last]
	c.ring = c.ring[:last]
	if c.ringPos > last {
		c.ringPos = 0
	}
}

// shouldSkip applies robots and content policy before a fetch is dispatched.
func (c *Crawler) shouldSkip(cfg *config.Config, e *hostEntry, rawURL string) (bool, string) {
	if !cfg.CrawlAdult && classify.HostLooksAdult(e.name) {
		return true, "adult content disabled"
	}
	if cfg.RespectRobots && e.rules != nil {
		u, err := url.Parse(rawURL)
		if err != nil {
			return true, "unparseable url"
		}
		p := u.EscapedPath()
		if p == "" {
			p = "/"
		}
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
		if !e.rules.Allowed(p) {
			return true, "blocked by robots.txt"
		}
	}
	return false, ""
}

// hostDelay is the politeness gap: the larger of the configured delay and any
// Crawl-delay the host published.
func (c *Crawler) hostDelay(cfg *config.Config, e *hostEntry) time.Duration {
	d := time.Duration(cfg.PerHostDelayMS) * time.Millisecond
	if e.rules != nil && e.rules.CrawlDelay > d {
		d = e.rules.CrawlDelay
	}
	if e.fails > 0 {
		// Exponential backoff, capped, so a flaky host degrades gracefully.
		backoff := time.Duration(1<<uint(min(e.fails, 6))) * time.Second
		if backoff > d {
			d = backoff
		}
	}
	return d
}

// hostDone releases one in-flight slot for a host.
func (c *Crawler) hostDone(host string, failed bool) {
	c.hostsMu.Lock()
	defer c.hostsMu.Unlock()
	e := c.hosts[host]
	if e == nil {
		return
	}
	if e.inflight > 0 {
		e.inflight--
	}
	if failed {
		e.fails++
	} else if e.fails > 0 {
		e.fails = 0
	}
}

// ---------------------------------------------------------------- workers --

func (c *Crawler) worker(ctx context.Context) {
	defer c.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-c.work:
			if !ok {
				return
			}
			if j.robots {
				c.fetchRobots(ctx, j.host)
				continue
			}
			r := c.fetchPage(ctx, j)
			select {
			case c.results <- r:
			case <-ctx.Done():
				return
			}
		}
	}
}

// fetchRobots resolves a host's robots.txt, using the cache when it is fresh.
func (c *Crawler) fetchRobots(ctx context.Context, host string) {
	cfg := c.cfg.Load()
	var rules *robots.Rules

	if rec, ok := c.kvdb.GetRobots(host); ok &&
		time.Since(time.Unix(rec.FetchedAt, 0)) < 24*time.Hour {
		if rec.OK {
			rules = robots.Parse(rec.Body, c.uaToken)
		} else {
			rules = robots.AllowAllRules()
		}
	} else {
		rules = c.loadRobots(ctx, host, cfg)
	}

	c.hostsMu.Lock()
	if e := c.hosts[host]; e != nil {
		e.rules = rules
		e.state = hostReady
		if e.inflight > 0 {
			e.inflight--
		}
		// Respect Crawl-delay from the very first request.
		if d := c.hostDelay(cfg, e); d > 0 {
			e.nextAt = time.Now().Add(d)
		}
	}
	c.hostsMu.Unlock()
}

func (c *Crawler) loadRobots(ctx context.Context, host string, cfg *config.Config) *robots.Rules {
	client := c.client.Load()
	rec := kv.Robots{FetchedAt: time.Now().Unix()}
	rules := robots.AllowAllRules()

	for _, scheme := range []string{"https", "http"} {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := client.Get(rctx, scheme+"://"+host+"/robots.txt", false)
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
			rules = robots.Parse(body, c.uaToken)
		case resp.Status >= 500:
			// Server trouble: back off rather than assume permission.
			rec.OK = false
			rules = robots.DenyAll()
		default:
			// 404 and friends mean "no restrictions published".
			rec.OK = true
		}
		break
	}

	if err := c.kvdb.PutRobots(host, rec); err != nil {
		logf("cache robots for %s: %v", host, err)
	}
	return rules
}

// fetchPage performs one page fetch and turns it into a persistable result.
func (c *Crawler) fetchPage(ctx context.Context, j job) result {
	cfg := c.cfg.Load()
	client := c.client.Load()
	item := j.item
	now := time.Now()

	r := result{
		host: j.host,
		site: urlx.RegistrableSuffix(j.host),
		comp: kv.Completion{Lease: item.Lease, URL: item.URL, DoneAt: now.Unix()},
		ev: metrics.Event{
			TS: now.Unix(), URL: item.URL, Host: j.host, Depth: item.Depth,
		},
	}

	c.met.IncInflight()
	fctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.RequestTimeoutMS)*time.Millisecond)
	resp, err := client.Get(fctx, item.URL, cfg.OnlyHTML)
	cancel()
	c.met.DecInflight()

	if err != nil {
		failed := !errors.Is(err, context.Canceled)
		c.hostDone(j.host, failed)
		r.comp.State = kv.StateDead
		r.ev.Err = shortErr(err)
		r.ev.Latency = int(time.Since(now).Milliseconds())
		return r
	}
	c.hostDone(j.host, resp.Status >= 500)

	r.status = resp.Status
	r.bytes = resp.Bytes
	r.ev.Status = resp.Status
	r.ev.Bytes = resp.Bytes
	r.ev.Latency = int(resp.Latency.Milliseconds())
	r.ev.IP = resp.RemoteIP

	// Locate the server we just talked to. Cached per host, so this is a map
	// hit for all but the first page of each site.
	if loc, ok := c.geo.Lookup(j.host, resp.RemoteIP); ok {
		r.ev.Lat, r.ev.Lon = loc.Lat, loc.Lon
		r.ev.Country, r.ev.City = loc.Country, loc.City
		r.ev.HasGeo = true
		r.country = loc.Country
	}

	// Rate limiting and transient server errors are worth one more attempt.
	if resp.Status == 429 || resp.Status == 503 {
		r.comp.Retry = true
		r.comp.Item = item.Item
		r.ev.Err = "throttled"
		c.penalize(j.host, 30*time.Second)
		return r
	}

	r.comp.State = kv.StateDone
	if resp.Status < 200 || resp.Status >= 300 || len(resp.Body) == 0 {
		r.ev.Err = statusText(resp.Status)
		return r
	}
	if cfg.OnlyHTML && !fetch.IsHTML(resp.ContentType) {
		r.ev.Err = "non-html"
		return r
	}

	base, err := url.Parse(resp.URL)
	if err != nil {
		r.ev.Err = "bad final url"
		return r
	}

	doc := extract.Parse(resp.Body, resp.ContentType, base, extract.Limits{
		MaxLinks: cfg.MaxLinksPerDoc,
		MaxText:  maxBodyText,
	})

	cat := classify.Of(j.host, base.Path, doc.Title, doc.Description)
	r.category = cat
	r.lang = doc.Lang
	r.ok = true

	r.ev.OK = true
	r.ev.Title = doc.Title
	r.ev.Category = cat
	r.ev.Lang = doc.Lang

	if !doc.NoIndex {
		r.comp.Doc = &kv.Doc{
			URL: item.URL, Host: j.host, Site: r.site,
			Title: doc.Title, Description: doc.Description, Body: doc.Text,
			Lang: doc.Lang, Category: cat, Status: resp.Status,
			Bytes: resp.Bytes, Depth: item.Depth, FetchedAt: now.Unix(),
		}
	}

	r.comp.Links = c.harvest(cfg, doc.Links, item.Depth)
	r.links = len(r.comp.Links)
	r.ev.Links = r.links
	return r
}

// harvest filters extracted links into new frontier items.
func (c *Crawler) harvest(cfg *config.Config, links []string, depth int) []kv.Item {
	if cfg.MaxDepth >= 0 && depth >= cfg.MaxDepth {
		return nil
	}
	out := make([]kv.Item, 0, len(links))
	for _, l := range links {
		h := urlx.Host(l)
		if h == "" {
			continue
		}
		if !cfg.CrawlAdult && classify.HostLooksAdult(h) {
			continue
		}
		out = append(out, kv.Item{URL: l, Host: h, Depth: depth + 1})
	}
	return out
}

// penalize pushes a host's next allowed fetch further into the future.
func (c *Crawler) penalize(host string, d time.Duration) {
	c.hostsMu.Lock()
	defer c.hostsMu.Unlock()
	if e := c.hosts[host]; e != nil {
		if t := time.Now().Add(d); t.After(e.nextAt) {
			e.nextAt = t
		}
	}
}

// -------------------------------------------------------------- collector --

func (c *Crawler) collector(ctx context.Context) {
	defer c.wg.Done()

	const maxBatch = 512
	batch := make([]result, 0, maxBatch)
	flush := time.NewTicker(500 * time.Millisecond)
	defer flush.Stop()

	for {
		select {
		case <-ctx.Done():
			// Drain whatever is already queued so no work is lost on shutdown.
			for {
				select {
				case r := <-c.results:
					batch = append(batch, r)
					if len(batch) >= maxBatch {
						c.persist(batch)
						batch = batch[:0]
					}
					continue
				default:
				}
				break
			}
			c.persist(batch)
			return

		case r := <-c.results:
			batch = append(batch, r)
			if len(batch) >= maxBatch {
				c.persist(batch)
				batch = batch[:0]
			}

		case <-flush.C:
			if len(batch) > 0 {
				c.persist(batch)
				batch = batch[:0]
			}
		}
	}
}

// persist commits one batch to both stores and publishes the live events.
func (c *Crawler) persist(batch []result) {
	if len(batch) == 0 {
		return
	}

	comps := make([]kv.Completion, 0, len(batch))
	delta := meta.NewDelta()
	now := time.Now().Unix()
	delta.Minute = now - now%60

	for i := range batch {
		r := &batch[i]
		comps = append(comps, r.comp)

		delta.Fetch++
		hd := delta.Host(r.host)
		hd.LastSeen = now
		if r.site != "" {
			hd.Site = r.site
		}
		if r.ok {
			delta.Pages++
			delta.Bytes += r.bytes
			delta.Links += int64(r.links)
			hd.Pages++
			hd.Bytes += r.bytes
			if r.category != "" {
				hd.Category = r.category
				delta.Categories[r.category]++
			}
			if r.lang != "" {
				delta.Langs[r.lang]++
			}
			if r.country != "" {
				delta.Countries[r.country]++
			}
		} else {
			delta.Errors++
			hd.Errors++
		}
		if r.status > 0 {
			delta.Statuses[r.status]++
		}
		c.met.Record(r.ev)
	}

	if _, err := c.kvdb.Complete(comps); err != nil {
		logf("commit frontier batch: %v", err)
	}
	if err := c.metadb.Apply(delta); err != nil {
		logf("commit aggregates: %v", err)
	}
}

// ------------------------------------------------------------ maintenance --

// maintenance is what makes the crawl a 24/7 process rather than a one-shot
// job: it recycles finished URLs back into the frontier, re-injects seeds when
// everything drains, polls the discovery sources, and keeps the stores tidy.
func (c *Crawler) maintenance(ctx context.Context) {
	defer c.wg.Done()

	recrawl := time.NewTicker(20 * time.Second)
	defer recrawl.Stop()
	housekeeping := time.NewTicker(60 * time.Second)
	defer housekeeping.Stop()
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()
	// Discovery runs on a 1-minute beat so a config change to the interval is
	// picked up without restarting the ticker; the source poll itself only
	// fires when the configured interval has actually elapsed.
	discBeat := time.NewTicker(time.Minute)
	defer discBeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-recrawl.C:
			if !c.cfg.Load().Paused {
				c.topUpFrontier()
			}

		case <-discBeat.C:
			cfg := c.cfg.Load()
			if !cfg.Paused && cfg.DiscoveryEnabled {
				interval := time.Duration(cfg.DiscoveryIntervalMin) * time.Minute
				if time.Since(time.Unix(c.disc.LastRun(), 0)) >= interval {
					c.runDiscovery(ctx)
				}
			}

		case <-housekeeping.C:
			c.kvdb.PersistCounters()
			if err := c.kvdb.Flush(); err != nil {
				logf("pebble flush: %v", err)
			}

		case <-prune.C:
			if err := c.metadb.PruneMetrics(30); err != nil {
				logf("prune metrics: %v", err)
			}
		}
	}
}

// runDiscovery executes one source poll with a bounded runtime.
func (c *Crawler) runDiscovery(parent context.Context) {
	cfg := c.cfg.Load()
	dctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if _, err := c.disc.Run(dctx, *cfg, false); err != nil {
		logf("discovery: %v", err)
	}
}

// topUpFrontier keeps work available. Normally it only recycles pages older
// than the configured recrawl age; when the queue is nearly empty it lowers
// that bar and re-injects seeds so the crawler never sits idle.
func (c *Crawler) topUpFrontier() {
	cfg := c.cfg.Load()

	// Work already leased into the per-host queues still counts as frontier
	// depth; ignoring it makes the engine think it is dry on every pass.
	c.hostsMu.Lock()
	buffered := int64(c.buffered)
	c.hostsMu.Unlock()
	pending := c.kvdb.PendingApprox() + buffered

	maxAge := time.Duration(cfg.RecrawlAfterHours) * time.Hour
	batch := 5000
	if pending < lowWaterMark {
		// Running dry: accept anything crawled more than an hour ago.
		if maxAge > time.Hour {
			maxAge = time.Hour
		}
		batch = 20000
	} else if pending > 200000 {
		return // plenty of work already queued
	}

	cutoff := time.Now().Add(-maxAge).Unix()
	items, err := c.kvdb.DueForRecrawl(cutoff, batch)
	if err != nil {
		logf("recrawl sweep: %v", err)
		return
	}
	if len(items) > 0 {
		if err := c.kvdb.Requeue(items); err != nil {
			logf("requeue recrawl: %v", err)
			return
		}
		c.recrawled.Add(int64(len(items)))
		logf("recrawl: re-queued %d pages", len(items))
		return
	}

	// Nothing due and the queue is low: fall back to the seed list.
	if pending < lowWaterMark && cfg.ReseedWhenDry {
		last := c.lastDry.Load()
		if time.Now().Unix()-last < 300 {
			return
		}
		c.lastDry.Store(time.Now().Unix())

		c.seedsMu.Lock()
		seeds := append([]string(nil), c.seeds...)
		c.seedsMu.Unlock()

		items := make([]kv.Item, 0, len(seeds))
		for _, s := range seeds {
			if n, ok := urlx.Normalize(s); ok {
				items = append(items, kv.Item{URL: n, Host: urlx.Host(n), Depth: 0})
			}
		}
		if len(items) > 0 {
			if err := c.kvdb.Requeue(items); err != nil {
				logf("reseed: %v", err)
				return
			}
			logf("frontier dry: re-injected %d seeds", len(items))
		}
	}
}

// ------------------------------------------------------- runtime operations --

// DropBuffered discards the in-memory per-host queues, returning the URLs to
// the durable frontier. Used by the maintenance actions so they operate on a
// consistent view rather than racing the scheduler.
func (c *Crawler) DropBuffered(requeue bool) int {
	c.hostsMu.Lock()
	var back []kv.Item
	for _, e := range c.hosts {
		for _, l := range e.queue {
			back = append(back, l.Item)
		}
		e.queue = nil
		e.inRing = false
	}
	c.hosts = make(map[string]*hostEntry)
	c.ring = nil
	c.ringPos = 0
	c.buffered = 0
	c.hostsMu.Unlock()

	if requeue && len(back) > 0 {
		if err := c.kvdb.Requeue(back); err != nil {
			logf("requeue on drop: %v", err)
		}
	}
	return len(back)
}

// ClearQueue empties both the in-memory buffers and the durable frontier.
func (c *Crawler) ClearQueue() error {
	c.DropBuffered(false)
	if err := c.kvdb.ClearQueue(); err != nil {
		return err
	}
	c.DropBuffered(false) // anything the scheduler leased mid-flight
	logf("frontier queue cleared")
	return nil
}

// ReseedNow injects the seed list immediately, regardless of queue depth.
func (c *Crawler) ReseedNow() (int, error) {
	c.seedsMu.Lock()
	seeds := append([]string(nil), c.seeds...)
	c.seedsMu.Unlock()

	items := make([]kv.Item, 0, len(seeds))
	for _, s := range seeds {
		if n, ok := urlx.Normalize(s); ok {
			items = append(items, kv.Item{URL: n, Host: urlx.Host(n), Depth: 0})
		}
	}
	if len(items) == 0 {
		return 0, nil
	}
	if err := c.kvdb.Requeue(items); err != nil {
		return 0, err
	}
	logf("reseeded %d URLs on request", len(items))
	return len(items), nil
}

// ForgetHosts drops the in-memory host table, which releases the robots cache
// and per-host scheduling state. Queued URLs are preserved.
func (c *Crawler) ForgetHosts() int {
	return c.DropBuffered(true)
}

// FreeMemory returns unused heap to the operating system and flushes the
// stores. Reported RSS falls noticeably after this on a long-running crawl.
func (c *Crawler) FreeMemory() {
	c.DropBuffered(true)
	c.kvdb.PersistCounters()
	if err := c.kvdb.Flush(); err != nil {
		logf("flush: %v", err)
	}
	runtime.GC()
	debug.FreeOSMemory()
	logf("released memory back to the OS")
}

// Compact rewrites both stores to reclaim deleted space. Slow; run detached.
func (c *Crawler) Compact() error {
	if err := c.kvdb.Compact(); err != nil {
		return err
	}
	return c.metadb.Vacuum()
}

// ResetStats zeroes the dashboard aggregates.
func (c *Crawler) ResetStats() error { return c.metadb.ResetStats() }

// ClearSeen forgets every visited URL so the crawl can start over.
func (c *Crawler) ClearSeen() error {
	if err := c.kvdb.ClearSeen(); err != nil {
		return err
	}
	logf("seen-set cleared: previously crawled URLs are eligible again")
	return nil
}

// ClearRobots drops the cached robots.txt files.
func (c *Crawler) ClearRobots() error {
	c.DropBuffered(true)
	return c.kvdb.ClearRobots()
}

// MemStats reports current process memory use for the dashboard.
func (c *Crawler) MemStats() (heapMB, sysMB float64) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1 << 20), float64(m.Sys) / (1 << 20)
}

// ------------------------------------------------------------------ status --

// Status is the scheduler's own view of itself, surfaced in the API.
type Status struct {
	Running   bool  `json:"running"`
	Paused    bool  `json:"paused"`
	Workers   int   `json:"workers"`
	Buffered  int   `json:"buffered"`
	HostsLive int   `json:"hostsLive"`
	HostsSeen int   `json:"hostsSeen"`
	Recrawled int64 `json:"recrawled"`

	// Discovery telemetry.
	Discovered       int64  `json:"discovered"`
	DiscoveryLastRun int64  `json:"discoveryLastRun"`
	DiscoveryLastErr string `json:"discoveryLastErr,omitempty"`
}

// Status reports live scheduler state.
func (c *Crawler) Status() Status {
	c.hostsMu.Lock()
	buffered, live, seen := c.buffered, len(c.ring), len(c.hosts)
	c.hostsMu.Unlock()

	cfg := c.cfg.Load()
	discovered, lastRun, lastErr := c.disc.Stats()
	return Status{
		Running:          c.isRunning(),
		Paused:           cfg.Paused,
		Workers:          cfg.Workers,
		Buffered:         buffered,
		HostsLive:        live,
		HostsSeen:        seen,
		Recrawled:        c.recrawled.Load(),
		Discovered:       discovered,
		DiscoveryLastRun: lastRun,
		DiscoveryLastErr: lastErr,
	}
}

// DiscoverNow runs a discovery cycle immediately, bypassing the interval gate.
// Used by the dashboard's "Discover sites" action.
func (c *Crawler) DiscoverNow() (int, error) {
	cfg := c.cfg.Load()
	if cfg.Paused {
		return 0, errors.New("crawler is paused")
	}
	dctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return c.disc.Run(dctx, *cfg, true)
}

// ResetDiscoveryCounters clears the dashboard discovery telemetry.
func (c *Crawler) ResetDiscoveryCounters() { c.disc.Reset() }

// ------------------------------------------------------------------ helpers --

// productToken reduces a User-Agent string to the token robots.txt matches on.
func productToken(ua string) string {
	ua = strings.ToLower(strings.TrimSpace(ua))
	if i := strings.IndexAny(ua, "/ ("); i > 0 {
		ua = ua[:i]
	}
	if ua == "" {
		return "worldscraperbot"
	}
	return ua
}

func shortErr(err error) string {
	s := err.Error()
	// net/http errors embed the whole URL, which is noise in a live feed.
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		s = s[i+2:]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func statusText(code int) string {
	switch {
	case code == 0:
		return "no response"
	case code >= 500:
		return "server error"
	case code == 404:
		return "not found"
	case code >= 400:
		return "client error"
	case code >= 300:
		return "redirect"
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
