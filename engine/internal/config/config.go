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

	// Per-domain budget: hosts that exceed their daily cap stop feeding new
	// work (harvested links, discovery, recrawls) until the day rolls over, so
	// one giant site cannot eat the whole queue.
	PerHostDailyCap int `json:"perHostDailyCap"` // max fetches per host per day; 0 = unlimited

	// Storage pruning: search-index documents older than this many days are
	// deleted (0 = keep everything). Applied automatically by the indexer.
	PruneOlderThanDays int `json:"pruneOlderThanDays"`

	// DedupNearDuplicates suppresses syndicated copies of an already-indexed
	// page by comparing a SimHash fingerprint recorded when the page was last
	// fetched. Copies still get crawled (harvesting their links); only the
	// document handed to the search index is dropped.
	DedupNearDuplicates bool `json:"dedupNearDuplicates"`

	// Autonomous discovery: polls external sources so the crawl keeps meeting
	// websites it has never seen instead of cycling over the same reachable
	// set. Sources are Wikipedia random articles, the daily Tranco top-domain
	// list, and any extra RSS/OPML/TXT feeds in DiscoverySources.
	DiscoveryEnabled          bool     `json:"discoveryEnabled"`           // poll sources on a timer
	DiscoveryIntervalMin      int      `json:"discoveryIntervalMin"`       // minutes between source polls
	DiscoveryMaxPerCycle      int      `json:"discoveryMaxPerCycle"`       // cap on URLs injected per cycle
	DiscoverySources          []string `json:"discoverySources"`           // extra feed/CSV URLs, one per entry
	DiscoverySitemapEnabled   bool     `json:"discoverySitemapEnabled"`    // scan top hosts' robots.txt for sitemaps
	DiscoverySitemapHosts     int      `json:"discoverySitemapHosts"`      // how many top hosts to scan per cycle
	DiscoveryMaxSitemapFetches int     `json:"discoveryMaxSitemapFetches"` // cap on sitemap URL fetches per cycle

	// Notifications: alert the user (webhook, Discord, Telegram) when the
	// crawl stalls, the error rate spikes, or disk usage crosses a threshold.
	NotifyEnabled         bool   `json:"notifyEnabled"`
	NotifyWebhook         string `json:"notifyWebhook"`       // generic JSON POST target
	NotifyDiscord         string `json:"notifyDiscord"`       // Discord webhook URL
	NotifyTelegramBot     string `json:"notifyTelegramBot"`   // Telegram bot token
	NotifyTelegramChat    string `json:"notifyTelegramChat"`  // Telegram chat id
	NotifyOnStall         bool   `json:"notifyOnStall"`       // no successful fetch for 10 minutes
	NotifyOnErrors        bool   `json:"notifyOnErrors"`      // >50% of recent fetches failing
	NotifyOnDisk          bool   `json:"notifyOnDisk"`        // data dir exceeds threshold
	NotifyDiskThresholdMB int    `json:"notifyDiskThresholdMB"`

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
		// A current, real browser UA: sites that hard-block non-browser
		// fingerprints otherwise 403 before robots.txt is ever consulted.
		UserAgent:         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		RespectRobots:     true,
		FollowRedirects:   5,
		InsecureTLS:       false,
		RecrawlAfterHours: 24 * 14,
		ReseedWhenDry:     true,
		PerHostDailyCap:   5000,
		PruneOlderThanDays: 0,
		DedupNearDuplicates: true,
		DiscoveryEnabled:             true,
		DiscoveryIntervalMin:          30,
		DiscoveryMaxPerCycle:          500,
		DiscoverySitemapEnabled:       true,
		DiscoverySitemapHosts:         50,
		DiscoveryMaxSitemapFetches:    200,
		NotifyWebhook:                 "",
		NotifyEnabled:                 false,
		NotifyOnStall:                 true,
		NotifyOnErrors:                true,
		NotifyOnDisk:                  true,
		NotifyDiskThresholdMB:         20480,
		CrawlAdult:                    true,
		OnlyHTML:                      true,
		Paused:                        false,
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
	clampInt(&c.PerHostDailyCap, 0, 1000000)
	if c.PruneOlderThanDays < 0 {
		c.PruneOlderThanDays = 0
	}
	clampInt(&c.DiscoverySitemapHosts, 0, 1000)
	clampInt(&c.DiscoveryMaxSitemapFetches, 0, 5000)
	clampInt(&c.NotifyDiskThresholdMB, 0, 1024*1024)
	c.NotifyWebhook = strings.TrimSpace(c.NotifyWebhook)
	c.NotifyDiscord = strings.TrimSpace(c.NotifyDiscord)
	c.NotifyTelegramBot = strings.TrimSpace(c.NotifyTelegramBot)
	c.NotifyTelegramChat = strings.TrimSpace(c.NotifyTelegramChat)
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
