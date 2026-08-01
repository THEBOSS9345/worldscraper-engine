// Package api exposes the engine over loopback HTTP: dashboard statistics, a
// server-sent-events live feed, crawl control, and the spool endpoints the
// Tantivy indexer drains.
//
// The listener is bound to 127.0.0.1 and, when a token is configured, every
// request must carry it. That matters because any web page the user visits can
// otherwise reach a localhost port.
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"worldscraper/engine/internal/config"
	"worldscraper/engine/internal/crawl"
	"worldscraper/engine/internal/geoip"
	"worldscraper/engine/internal/kv"
	"worldscraper/engine/internal/meta"
	"worldscraper/engine/internal/metrics"
)

// Server wires the engine's components to HTTP handlers.
type Server struct {
	kvdb    *kv.DB
	metadb  *meta.DB
	met     *metrics.M
	crawler *crawl.Crawler
	geo     *geoip.DB
	token   string

	aggMu   sync.Mutex
	aggVal  *Aggregates
	aggTime time.Time

	// OnShutdown, when set, is invoked after a "shutdown" control action so the
	// process can wind down cleanly (the engine is a daemon and this is the
	// only in-process way to ask it to exit).
	OnShutdown func()
}

// New creates the API server. geo may be nil.
func New(k *kv.DB, m *meta.DB, met *metrics.M, c *crawl.Crawler, geo *geoip.DB, token string) *Server {
	return &Server{kvdb: k, metadb: m, met: met, crawler: c, geo: geo, token: token}
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/stats", s.auth(s.stats))
	mux.HandleFunc("GET /api/live", s.auth(s.live))
	mux.HandleFunc("GET /api/recent", s.auth(s.recent))
	mux.HandleFunc("GET /api/hosts", s.auth(s.hosts))
	mux.HandleFunc("GET /api/config", s.auth(s.getConfig))
	mux.HandleFunc("POST /api/config", s.auth(s.setConfig))
	mux.HandleFunc("POST /api/control", s.auth(s.control))
	mux.HandleFunc("POST /api/seeds", s.auth(s.addSeeds))
	mux.HandleFunc("GET /api/spool", s.auth(s.spoolRead))
	mux.HandleFunc("POST /api/spool/ack", s.auth(s.spoolAck))

	return cors(mux)
}

// ------------------------------------------------------------ middleware ---

// auth rejects requests without the shared token. An empty token disables the
// check, which is only intended for development.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next(w, r)
			return
		}
		got := r.Header.Get("X-WS-Token")
		if got == "" {
			// EventSource cannot set headers, so the SSE endpoint also accepts
			// the token as a query parameter.
			got = r.URL.Query().Get("token")
		}
		if got != s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-WS-Token")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --------------------------------------------------------------- handlers --

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "service": "wsengine"})
}

// Aggregates are the slower-moving numbers behind the dashboard panels.
type Aggregates struct {
	Totals     map[string]int64   `json:"totals"`
	Hosts      int64              `json:"hostCount"`
	Categories []meta.CountRow    `json:"categories"`
	Langs      []meta.CountRow    `json:"langs"`
	Statuses   []meta.CountRow    `json:"statuses"`
	Countries  []meta.CountRow    `json:"countries"`
	TopHosts   []meta.HostRow     `json:"topHosts"`
	Series     []meta.MinutePoint `json:"series"`
	Geo        GeoInfo            `json:"geo"`
}

// GeoInfo tells the UI whether locations are real or approximated.
type GeoInfo struct {
	Enabled  bool   `json:"enabled"`
	Database string `json:"database,omitempty"`
}

// aggregates caches the SQLite rollups briefly so a 2 Hz live feed does not
// re-run them on every tick.
func (s *Server) aggregates(seriesMinutes int) *Aggregates {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()

	if s.aggVal != nil && time.Since(s.aggTime) < 2*time.Second {
		return s.aggVal
	}

	a := &Aggregates{}
	var err error
	if a.Totals, err = s.metadb.Totals(); err != nil {
		log.Printf("[api] totals: %v", err)
		a.Totals = map[string]int64{}
	}
	if a.Hosts, err = s.metadb.HostCount(); err != nil {
		log.Printf("[api] host count: %v", err)
	}
	if a.Categories, err = s.metadb.Categories(); err != nil {
		log.Printf("[api] categories: %v", err)
	}
	if a.Langs, err = s.metadb.Langs(12); err != nil {
		log.Printf("[api] langs: %v", err)
	}
	if a.Statuses, err = s.metadb.Statuses(); err != nil {
		log.Printf("[api] statuses: %v", err)
	}
	if a.Countries, err = s.metadb.Countries(30); err != nil {
		log.Printf("[api] countries: %v", err)
	}
	a.Geo = GeoInfo{Enabled: s.geo.Available(), Database: filepath.Base(s.geo.Path())}
	if a.TopHosts, err = s.metadb.TopHosts(25); err != nil {
		log.Printf("[api] top hosts: %v", err)
	}
	if a.Series, err = s.metadb.Series(seriesMinutes); err != nil {
		log.Printf("[api] series: %v", err)
	}

	s.aggVal, s.aggTime = a, time.Now()
	return a
}

// Snapshot is the full dashboard payload.
type Snapshot struct {
	Rates      metrics.Rates    `json:"rates"`
	Status     crawl.Status     `json:"status"`
	Frontier   FrontierStats    `json:"frontier"`
	Aggregates *Aggregates      `json:"agg"`
	Spark      []int64          `json:"spark"`
	Recent     []metrics.Event  `json:"recent"`
	Now        int64            `json:"now"`
}

// FrontierStats describes queue and index-handoff depth.
type FrontierStats struct {
	Pending     int64   `json:"pending"`
	SpoolDepth  uint64  `json:"spoolDepth"`
	SpoolCursor uint64  `json:"spoolCursor"`
	DiskBytes   uint64  `json:"diskBytes"`
	HeapMB      float64 `json:"heapMb"`
	RssMB       float64 `json:"rssMb"`
}

func (s *Server) frontierStats() FrontierStats {
	heap, sys := s.crawler.MemStats()
	return FrontierStats{
		Pending:     s.kvdb.PendingApprox(),
		SpoolDepth:  s.kvdb.SpoolDepth(),
		SpoolCursor: s.kvdb.SpoolCursor(),
		DiskBytes:   s.kvdb.DiskUsage(),
		HeapMB:      heap,
		RssMB:       sys,
	}
}

func (s *Server) snapshot(seriesMinutes, recentN int) *Snapshot {
	return &Snapshot{
		Rates:    s.met.Rates(10),
		Status:   s.crawler.Status(),
		Frontier: s.frontierStats(),
		Aggregates: s.aggregates(seriesMinutes),
		Spark:      s.met.Spark(60),
		Recent:     s.met.Recent(recentN),
		Now:        time.Now().Unix(),
	}
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.snapshot(intParam(r, "minutes", 120), intParam(r, "recent", 40)))
}

func (s *Server) recent(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.met.Recent(intParam(r, "n", 60)))
}

func (s *Server) hosts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.metadb.TopHosts(intParam(r, "n", 50))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, rows)
}

// live streams the dashboard payload over server-sent events. Fast-moving
// numbers go out twice a second; the heavier rollups every three seconds.
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	lastSeq := s.met.Seq()
	// Prime the client with a full snapshot so the UI is populated instantly.
	if err := sendEvent(w, "snapshot", s.snapshot(120, 60)); err != nil {
		return
	}
	flusher.Flush()

	aggEvery := 6 // ticks between full rollup pushes
	n := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n++

			seq := s.met.Seq()
			var fresh []metrics.Event
			if seq > lastSeq {
				delta := int(seq - lastSeq)
				if delta > 200 {
					delta = 200
				}
				fresh = s.met.Recent(delta)
				lastSeq = seq
			}

			payload := map[string]any{
				"rates":    s.met.Rates(10),
				"status":   s.crawler.Status(),
				"frontier": s.frontierStats(),
				"spark":    s.met.Spark(60),
				"events":   fresh,
				"now":      time.Now().Unix(),
			}
			if err := sendEvent(w, "tick", payload); err != nil {
				return
			}

			if n%aggEvery == 0 {
				if err := sendEvent(w, "agg", s.aggregates(120)); err != nil {
					return
				}
			}
			flusher.Flush()
		}
	}
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.crawler.Config())
}

func (s *Server) setConfig(w http.ResponseWriter, r *http.Request) {
	// Start from the live config so a partial body only changes what it names.
	cfg := s.crawler.Config()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&cfg); err != nil {
		http.Error(w, "bad config: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.crawler.SetConfig(cfg); err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, s.crawler.Config())
}

func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	cfg := s.crawler.Config()
	message := ""

	switch body.Action {
	case "start":
		cfg.Paused = false
		if err := s.crawler.SetConfig(cfg); err != nil {
			httpError(w, err)
			return
		}
		if err := s.crawler.Start(); err != nil {
			httpError(w, err)
			return
		}
		message = "crawler started"
	case "stop":
		s.crawler.Stop()
		message = "crawler stopped"

	case "shutdown":
		// Stop crawling and then ask the process to exit. The shell's supervisor
		// treats this as an explicit stop and will not restart the daemon until
		// told to, so the exit is clean and the runtime.json handoff disappears.
		s.crawler.Stop()
		message = "engine shutting down"
		if s.OnShutdown != nil {
			s.OnShutdown()
		}
	case "pause":
		cfg.Paused = true
		if err := s.crawler.SetConfig(cfg); err != nil {
			httpError(w, err)
			return
		}
		message = "crawling paused"
	case "resume":
		cfg.Paused = false
		if err := s.crawler.SetConfig(cfg); err != nil {
			httpError(w, err)
			return
		}
		if err := s.crawler.Start(); err != nil {
			httpError(w, err)
			return
		}
		message = "crawling resumed"

	case "clearQueue":
		if err := s.crawler.ClearQueue(); err != nil {
			httpError(w, err)
			return
		}
		message = "frontier queue cleared"

	case "reseed":
		n, err := s.crawler.ReseedNow()
		if err != nil {
			httpError(w, err)
			return
		}
		message = fmt.Sprintf("re-queued %d seed URLs", n)

	case "discover":
		// The cycle does up to a few network fetches and can take a couple of
		// minutes, so it runs detached and reports through the live feed's
		// discovery telemetry rather than holding the request open.
		go func() {
			if n, err := s.crawler.DiscoverNow(); err != nil {
				log.Printf("[api] discover: %v", err)
			} else {
				log.Printf("[api] discover: %d new URLs queued", n)
			}
		}()
		message = "discovery cycle started in the background"

	case "freeMemory":
		s.crawler.FreeMemory()
		heap, sys := s.crawler.MemStats()
		message = fmt.Sprintf("memory released (heap %.0f MB, rss %.0f MB)", heap, sys)

	case "clearRobots":
		if err := s.crawler.ClearRobots(); err != nil {
			httpError(w, err)
			return
		}
		message = "robots.txt cache cleared"

	case "resetStats":
		if err := s.crawler.ResetStats(); err != nil {
			httpError(w, err)
			return
		}
		s.crawler.ResetDiscoveryCounters()
		s.invalidateAggregates()
		message = "statistics reset"

	case "clearSeen":
		if err := s.crawler.ClearSeen(); err != nil {
			httpError(w, err)
			return
		}
		message = "seen-set cleared: every known URL may be crawled again"

	case "compact":
		// Compaction can run for minutes on a large store; do not hold the
		// request open for it.
		go func() {
			if err := s.crawler.Compact(); err != nil {
				log.Printf("[api] compact: %v", err)
			} else {
				log.Printf("[api] store compaction finished")
			}
		}()
		message = "compaction started in the background"

	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"status":  s.crawler.Status(),
		"message": message,
	})
}

// invalidateAggregates forces the next stats read to hit the database.
func (s *Server) invalidateAggregates() {
	s.aggMu.Lock()
	s.aggVal = nil
	s.aggMu.Unlock()
}

func (s *Server) addSeeds(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	n, err := s.crawler.AddSeeds(body.URLs)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, map[string]any{"added": n, "submitted": len(body.URLs)})
}

// --------------------------------------------------------------- indexing --

// spoolRead hands documents to the indexer. Documents stay in the spool until
// the indexer acknowledges them, so a crash on either side replays instead of
// dropping pages.
func (s *Server) spoolRead(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	n := intParam(r, "n", 500)
	if n > 5000 {
		n = 5000
	}

	docs, last, err := s.kvdb.SpoolRead(after, n)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"docs":  docs,
		"last":  last,
		"depth": s.kvdb.SpoolDepth(),
	})
}

func (s *Server) spoolAck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.kvdb.SpoolAck(body.Seq); err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, map[string]any{"cursor": s.kvdb.SpoolCursor(), "depth": s.kvdb.SpoolDepth()})
}

// ---------------------------------------------------------------- helpers --

func sendEvent(w http.ResponseWriter, event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("event: " + event + "\ndata: ")); err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n\n"))
	return err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[api] encode: %v", err)
	}
}

func httpError(w http.ResponseWriter, err error) {
	log.Printf("[api] %v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// ConfigFrom re-exports the config type for handlers that need it.
type ConfigFrom = config.Config
