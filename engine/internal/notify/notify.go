// Package notify watches the crawl for trouble and posts alerts to the
// configured webhook, Discord webhook, or Telegram bot. It is deliberately
// small: the engine already tracks everything it needs in metrics and status.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"worldscraper/engine/internal/config"
)

// Kind is the type of alert sent.
type Kind string

const (
	KindStall     Kind = "stall"
	KindErrorSpike Kind = "errorSpike"
	KindDisk      Kind = "disk"
	KindTest      Kind = "test"
)

// Info is the runtime data the notifier inspects on each sweep. The API server
// supplies it from its own components so the package stays decoupled.
type Info struct {
	Rates  Rates
	Status Status
	DiskMB int64
}

// Rates is the slice of the metrics collector the notifier needs.
type Rates struct {
	PagesPerSec  float64
	ErrorsPerSec float64
	SuccessRate  float64
	PagesPerMin  float64
}

// Status is the slice of the crawler status the notifier needs.
type Status struct {
	Running bool
	Paused  bool
	Workers int
}

// Notifier posts alerts when thresholds are crossed, with per-kind cooldowns
// so a persistent problem is not a chat spammer.
type Notifier struct {
	cfg func() config.Config
	inf func() Info
	now func() time.Time

	client *http.Client

	mu      sync.Mutex
	lastSent map[Kind]time.Time
	lastOK   time.Time
	started  time.Time
}

// New creates a notifier. cfg returns the live configuration, inf the current
// runtime snapshot. Both are called fresh on every sweep.
func New(cfg func() config.Config, inf func() Info) *Notifier {
	now := time.Now()
	return &Notifier{
		cfg:      cfg,
		inf:      inf,
		now:      time.Now,
		client:   &http.Client{Timeout: 15 * time.Second},
		lastSent: make(map[Kind]time.Time),
		started:  now,
	}
}

// Run sweeps once a minute until the context is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := n.check(); err != nil {
				log.Printf("[notify] %v", err)
			}
		}
	}
}

// check evaluates each enabled alert and fires those that are due.
func (n *Notifier) check() error {
	cfg := n.cfg()
	if !cfg.NotifyEnabled {
		return nil
	}
	inf := n.inf()

	now := n.now()
	// A page that landed any time within the last 10 minutes proves the crawl
	// is alive. Track the last one seen.
	if inf.Rates.PagesPerSec > 0 || inf.Rates.PagesPerMin > 0 {
		n.mu.Lock()
		n.lastOK = now
		n.mu.Unlock()
	}

	// Give the crawler time to warm up before calling it stalled.
	warm := time.Since(n.started) > 10*time.Minute

	if cfg.NotifyOnStall && warm && inf.Status.Running && !inf.Status.Paused {
		n.mu.Lock()
		idle := now.Sub(n.lastOK)
		n.mu.Unlock()
		if idle > 10*time.Minute {
			return n.send(KindStall, fmt.Sprintf(
				"Crawl may be stalled: no page fetched for %s (%.0f pages in the last minute).",
				idle.Round(time.Minute), inf.Rates.PagesPerMin))
		}
	}

	if cfg.NotifyOnErrors {
		// Error spike: more than half of recent fetches failed and there was
		// enough activity that the ratio means something.
		total := inf.Rates.PagesPerMin + inf.Rates.ErrorsPerSec*60
		if total >= 5 && inf.Rates.SuccessRate > 0 && inf.Rates.SuccessRate < 0.5 {
			return n.send(KindErrorSpike, fmt.Sprintf(
				"Error rate spike: %.0f%% of the last minute's fetches failed (%.0f errors, %.0f pages).",
				(1-inf.Rates.SuccessRate)*100, inf.Rates.ErrorsPerSec*60, inf.Rates.PagesPerMin))
		}
	}

	if cfg.NotifyOnDisk && cfg.NotifyDiskThresholdMB > 0 && inf.DiskMB > int64(cfg.NotifyDiskThresholdMB) {
		return n.send(KindDisk, fmt.Sprintf(
			"Disk usage high: data directory is %.1f GB, over the %.1f GB threshold.",
			float64(inf.DiskMB)/1024, float64(cfg.NotifyDiskThresholdMB)/1024))
	}
	return nil
}

// Test fires an immediate alert so the user can confirm their endpoints work.
func (n *Notifier) Test() error {
	return n.send(KindTest, "WorldScraper test alert: your notification endpoints are working.")
}

// send delivers an alert to every configured channel, honouring cooldowns for
// everything except the manual test.
func (n *Notifier) send(kind Kind, message string) error {
	cfg := n.cfg()
	if kind != KindTest {
		n.mu.Lock()
		if last, ok := n.lastSent[kind]; ok && n.now().Sub(last) < time.Hour {
			n.mu.Unlock()
			return nil
		}
		n.lastSent[kind] = n.now()
		n.mu.Unlock()
	}

	inf := n.inf()
	payload := map[string]any{
		"app":       "WorldScraper",
		"type":      string(kind),
		"message":   message,
		"timestamp": n.now().Unix(),
		"status": map[string]any{
			"running":  inf.Status.Running,
			"paused":   inf.Status.Paused,
			"workers":  inf.Status.Workers,
			"pagesPerSec": inf.Rates.PagesPerSec,
			"diskMB":   inf.DiskMB,
		},
	}

	var targets int
	var errs []error
	if cfg.NotifyWebhook != "" {
		targets++
		if err := postJSON(n.client, cfg.NotifyWebhook, payload); err != nil {
			errs = append(errs, fmt.Errorf("webhook: %w", err))
		}
	}
	if cfg.NotifyDiscord != "" {
		targets++
		// Discord expects {content: "..."}.
		if err := postJSON(n.client, cfg.NotifyDiscord, map[string]any{"content": message}); err != nil {
			errs = append(errs, fmt.Errorf("discord: %w", err))
		}
	}
	if cfg.NotifyTelegramBot != "" && cfg.NotifyTelegramChat != "" {
		targets++
		url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.NotifyTelegramBot)
		if err := postJSON(n.client, url, map[string]any{
			"chat_id": cfg.NotifyTelegramChat, "text": "🤖 " + message,
		}); err != nil {
			errs = append(errs, fmt.Errorf("telegram: %w", err))
		}
	}

	if targets == 0 {
		return fmt.Errorf("no notification endpoint configured")
	}
	if len(errs) > 0 {
		log.Printf("[notify] %s alert delivery failed: %v", kind, errs)
		return nil
	}
	log.Printf("[notify] %s alert sent to %d endpoint(s)", kind, targets)
	return nil
}

func postJSON(client *http.Client, url string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "WorldScraper/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("endpoint returned %s", resp.Status)
	}
	return nil
}
