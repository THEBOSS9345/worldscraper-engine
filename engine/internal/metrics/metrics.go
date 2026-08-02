// Package metrics keeps the live, in-memory view of the crawl that the
// dashboard streams: per-second throughput, latency, and a ring buffer of the
// most recent pages so the UI can animate them as they land.
package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// Event is one completed fetch, as shown in the live feed.
type Event struct {
	TS       int64  `json:"ts"`
	URL      string `json:"url"`
	Host     string `json:"host"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Lang     string `json:"lang"`
	Status   int    `json:"status"`
	Bytes    int64  `json:"bytes"`
	Latency  int    `json:"latencyMs"`
	Depth    int    `json:"depth"`
	Links    int    `json:"links"`
	OK       bool   `json:"ok"`
	Err      string `json:"err,omitempty"`

	// Geolocation of the server, present only when a GeoIP database is
	// installed. HasGeo distinguishes "no database" from "coordinates 0,0".
	IP      string  `json:"ip,omitempty"`
	Lat     float64 `json:"lat,omitempty"`
	Lon     float64 `json:"lon,omitempty"`
	Country string  `json:"country,omitempty"`
	City    string  `json:"city,omitempty"`
	HasGeo  bool    `json:"hasGeo,omitempty"`
}

const (
	windowSecs  = 120 // rolling throughput window
	recentLimit = 400 // live feed ring size
)

type slot struct {
	ts      int64
	pages   int64
	errors  int64
	bytes   int64
	latency int64
	latN    int64
}

// M is the concurrent metrics collector.
type M struct {
	mu     sync.Mutex
	slots  [windowSecs]slot
	recent []Event
	head   int
	filled int

	inflight  atomic.Int64
	hostsLive atomic.Int64
	deduped   atomic.Int64
	startedAt time.Time

	seq atomic.Uint64
}

// New creates a collector.
func New() *M {
	return &M{
		recent:    make([]Event, recentLimit),
		startedAt: time.Now(),
	}
}

// IncInflight/DecInflight track fetches currently in progress.
func (m *M) IncInflight() { m.inflight.Add(1) }

// DecInflight decrements the in-flight fetch count.
func (m *M) DecInflight() { m.inflight.Add(-1) }

// SetHostsLive records how many hosts currently hold queued work.
func (m *M) SetHostsLive(n int64) { m.hostsLive.Store(n) }

// AddDedup counts one document suppressed as a near duplicate.
func (m *M) AddDedup() { m.deduped.Add(1) }

// Deduped returns the total number of near-duplicate documents dropped.
func (m *M) Deduped() int64 { return m.deduped.Load() }

// Record files one completed fetch.
func (m *M) Record(ev Event) {
	now := time.Now().Unix()
	if ev.TS == 0 {
		ev.TS = now
	}

	m.mu.Lock()
	s := &m.slots[now%windowSecs]
	if s.ts != now {
		*s = slot{ts: now}
	}
	if ev.OK {
		s.pages++
		s.bytes += ev.Bytes
		s.latency += int64(ev.Latency)
		s.latN++
	} else {
		s.errors++
	}

	m.recent[m.head] = ev
	m.head = (m.head + 1) % recentLimit
	if m.filled < recentLimit {
		m.filled++
	}
	m.mu.Unlock()

	m.seq.Add(1)
}

// Rates is throughput measured over several windows.
type Rates struct {
	PagesPerSec   float64 `json:"pagesPerSec"`
	BytesPerSec   float64 `json:"bytesPerSec"`
	ErrorsPerSec  float64 `json:"errorsPerSec"`
	PagesPerMin   float64 `json:"pagesPerMin"`
	Deduped       int64   `json:"deduped"`
	AvgLatencyMs  float64 `json:"avgLatencyMs"`
	SuccessRate   float64 `json:"successRate"`
	Inflight      int64   `json:"inflight"`
	HostsLive     int64   `json:"hostsLive"`
	UptimeSeconds int64   `json:"uptimeSeconds"`
}

// Rates computes current throughput. `fast` seconds drives the responsive
// numbers; the minute figures always use 60s.
func (m *M) Rates(fast int) Rates {
	if fast <= 0 || fast > windowSecs {
		fast = 10
	}
	now := time.Now().Unix()

	var (
		fPages, fErr, fBytes int64
		mPages, mErr         int64
		latSum, latN         int64
	)

	m.mu.Lock()
	for i := 0; i < windowSecs; i++ {
		s := m.slots[i]
		if s.ts == 0 {
			continue
		}
		age := now - s.ts
		// Skip the current, still-accumulating second so rates do not dip.
		if age < 1 || age > 60 {
			continue
		}
		mPages += s.pages
		mErr += s.errors
		latSum += s.latency
		latN += s.latN
		if age <= int64(fast) {
			fPages += s.pages
			fErr += s.errors
			fBytes += s.bytes
		}
	}
	m.mu.Unlock()

	r := Rates{
		PagesPerSec:   float64(fPages) / float64(fast),
		ErrorsPerSec:  float64(fErr) / float64(fast),
		BytesPerSec:   float64(fBytes) / float64(fast),
		PagesPerMin:   float64(mPages),
		Deduped:       m.deduped.Load(),
		Inflight:      m.inflight.Load(),
		HostsLive:     m.hostsLive.Load(),
		UptimeSeconds: int64(time.Since(m.startedAt).Seconds()),
	}
	if latN > 0 {
		r.AvgLatencyMs = float64(latSum) / float64(latN)
	}
	if tot := mPages + mErr; tot > 0 {
		r.SuccessRate = float64(mPages) / float64(tot)
	}
	return r
}

// Recent returns up to n of the most recent events, newest first.
func (m *M) Recent(n int) []Event {
	if n <= 0 || n > recentLimit {
		n = 60
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.filled == 0 {
		return []Event{}
	}
	if n > m.filled {
		n = m.filled
	}
	out := make([]Event, 0, n)
	idx := m.head - 1
	for i := 0; i < n; i++ {
		if idx < 0 {
			idx += recentLimit
		}
		out = append(out, m.recent[idx])
		idx--
	}
	return out
}

// Seq is a monotonic counter the SSE layer uses to detect new activity.
func (m *M) Seq() uint64 { return m.seq.Load() }

// Spark returns the last n seconds of page throughput, oldest first, for the
// dashboard's high-resolution sparkline.
func (m *M) Spark(n int) []int64 {
	if n <= 0 || n > windowSecs {
		n = 60
	}
	now := time.Now().Unix()
	out := make([]int64, n)

	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < n; i++ {
		ts := now - int64(n-1-i)
		s := m.slots[((ts%windowSecs)+windowSecs)%windowSecs]
		if s.ts == ts {
			out[i] = s.pages
		}
	}
	return out
}
