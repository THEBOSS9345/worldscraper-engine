// Package config holds the tunable runtime settings for the crawl engine.
//
// The configuration lives in the SQLite database (settings table) so that it
// survives restarts and can be edited live from the desktop UI.
package config

import (
	"runtime"
	"strings"
)

// Config is the full set of knobs the engine exposes.
type Config struct {
	// Crawl shape
	Workers        int `json:"workers"`        // global concurrent fetches
	PerHostDelayMS int `json:"perHostDelayMs"` // politeness gap between hits on one host
	PerHostBurst   int `json:"perHostBurst"`   // concurrent fetches allowed per host
	MaxDepth       int `json:"maxDepth"`       // link depth from a seed; -1 = unlimited
	MaxLinksPerDoc int `json:"maxLinksPerDoc"` // cap on links harvested from one page

	// Fetch behaviour
	RequestTimeoutMS int    `json:"requestTimeoutMs"`
	MaxPageBytes     int64  `json:"maxPageBytes"`
	UserAgent        string `json:"userAgent"`
	RespectRobots    bool   `json:"respectRobots"`
	FollowRedirects  int    `json:"followRedirects"`
	// InsecureTLS ignores certificate errors. The crawler only reads public
	// pages and never sends credentials, and a large share of the web has
	// broken or expired certificates, so this defaults on for coverage.
	InsecureTLS bool `json:"insecureTls"`

	// Never-ending crawl behaviour
	RecrawlAfterHours int `json:"recrawlAfterHours"` // re-queue pages older than this
	ReseedWhenDry     bool `json:"reseedWhenDry"`    // re-inject seed list if frontier empties

	// Autonomous discovery: polls external sources so the crawl keeps meeting
	// websites it has never seen instead of cycling over the same reachable
	// set. Sources are Wikipedia random articles, the daily Tranco top-domain
	// list, and any extra RSS/OPML/TXT feeds in DiscoverySources.
	DiscoveryEnabled     bool     `json:"discoveryEnabled"`     // poll sources on a timer
	DiscoveryIntervalMin int      `json:"discoveryIntervalMin"` // minutes between source polls
	DiscoveryMaxPerCycle int      `json:"discoveryMaxPerCycle"` // cap on URLs injected per cycle
	DiscoverySources     []string `json:"discoverySources"`     // extra feed/CSV URLs, one per entry

	// Content policy
	CrawlAdult bool `json:"crawlAdult"` // fetch hosts classified as adult
	OnlyHTML   bool `json:"onlyHtml"`   // skip non-HTML content types

	// Runtime state
	Paused bool `json:"paused"`
}

// Default returns a sane starting configuration for a desktop machine.
func Default() Config {
	// Deliberately not "as many as possible": this runs behind an interactive
	// window, and starving the UI of CPU to gain throughput is a bad trade.
	// Raise it in Settings if you want the machine fully committed.
	workers := runtime.NumCPU() * 3
	if workers < 16 {
		workers = 16
	}
	if workers > 96 {
		workers = 96
	}
	return Config{
		Workers:           workers,
		PerHostDelayMS:    1200,
		PerHostBurst:      1,
		MaxDepth:          -1,
		MaxLinksPerDoc:    150,
		RequestTimeoutMS:  15000,
		MaxPageBytes:      3 << 20, // 3 MiB
		UserAgent:         "WorldScraperBot/0.1 (+https://github.com/worldscraper; desktop research crawler)",
		RespectRobots:     true,
		FollowRedirects:   5,
		InsecureTLS:       true,
		RecrawlAfterHours: 24 * 14,
		ReseedWhenDry:     true,
		DiscoveryEnabled:     true,
		DiscoveryIntervalMin: 30,
		DiscoveryMaxPerCycle: 500,
		CrawlAdult:           true,
		OnlyHTML:             true,
		Paused:               false,
	}
}

// Sanitize clamps user-supplied values into ranges the engine can survive.
func (c *Config) Sanitize() {
	clampInt(&c.Workers, 1, 1024)
	clampInt(&c.PerHostDelayMS, 0, 600000)
	clampInt(&c.PerHostBurst, 1, 16)
	clampInt(&c.MaxLinksPerDoc, 1, 5000)
	clampInt(&c.RequestTimeoutMS, 1000, 120000)
	clampInt(&c.FollowRedirects, 0, 20)
	clampInt(&c.RecrawlAfterHours, 1, 24*365*5)
	clampInt(&c.DiscoveryIntervalMin, 5, 1440)
	clampInt(&c.DiscoveryMaxPerCycle, 10, 10000)
	kept := c.DiscoverySources[:0]
	for _, s := range c.DiscoverySources {
		s = strings.TrimSpace(s)
		if s != "" {
			kept = append(kept, s)
		}
	}
	c.DiscoverySources = kept
	if len(c.DiscoverySources) > 50 {
		c.DiscoverySources = c.DiscoverySources[:50]
	}
	if c.MaxDepth < -1 {
		c.MaxDepth = -1
	}
	if c.MaxPageBytes < 4096 {
		c.MaxPageBytes = 4096
	}
	if c.MaxPageBytes > 64<<20 {
		c.MaxPageBytes = 64 << 20
	}
	if c.UserAgent == "" {
		c.UserAgent = Default().UserAgent
	}
}

func clampInt(v *int, lo, hi int) {
	if *v < lo {
		*v = lo
	}
	if *v > hi {
		*v = hi
	}
}
