// Command wsengine is the headless crawl daemon.
//
// It owns the frontier, the worker pool and the aggregate statistics, and
// exposes everything over loopback HTTP. It runs independently of the desktop
// shell: the shell adopts a running instance (via runtime.json) or starts one,
// and quitting the shell never stops it. The shell drains its document spool
// into the search index while it is open.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "embed"

	"worldscraper/engine/internal/api"
	"worldscraper/engine/internal/config"
	"worldscraper/engine/internal/crawl"
	"worldscraper/engine/internal/geoip"
	"worldscraper/engine/internal/kv"
	"worldscraper/engine/internal/meta"
	"worldscraper/engine/internal/metrics"
)

//go:embed seeds.txt
var defaultSeeds string

// runtimeInfo is written to the data directory so the desktop shell can find
// the engine even when it chose its own port.
type runtimeInfo struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Token   string `json:"token"`
	Started int64  `json:"started"`
	Version string `json:"version"`
}

const version = "0.1.0"

func main() {
	var (
		dataDir   = flag.String("data", defaultDataDir(), "data directory")
		listen    = flag.String("listen", "127.0.0.1:8787", "listen address; port 0 picks a free one")
		token     = flag.String("token", "", "shared token required on API requests")
		seedsFile = flag.String("seeds", "", "optional seed list file, one URL per line")
		paused    = flag.Bool("paused", false, "start with crawling paused")
		parentPID = flag.Int("parent-pid", 0, "optional: exit when this process disappears")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("")

	if err := run(*dataDir, *listen, *token, *seedsFile, *paused, *parentPID); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(dataDir, listen, token, seedsFile string, paused bool, parentPID int) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	log.Printf("[engine] worldscraper engine %s", version)
	log.Printf("[engine] data directory: %s", dataDir)

	kvdb, err := kv.Open(filepath.Join(dataDir, "frontier"))
	if err != nil {
		return fmt.Errorf("open frontier store: %w", err)
	}
	defer kvdb.Close()

	metadb, err := meta.Open(filepath.Join(dataDir, "meta.db"))
	if err != nil {
		return fmt.Errorf("open meta store: %w", err)
	}
	defer metadb.Close()

	// Persisted settings win over defaults; the -paused flag wins over both.
	cfg := config.Default()
	if _, err := metadb.GetJSON("config", &cfg); err != nil {
		log.Printf("[engine] could not read saved config, using defaults: %v", err)
		cfg = config.Default()
	}
	cfg.Sanitize()
	if paused {
		cfg.Paused = true
	}
	if err := metadb.PutJSON("config", cfg); err != nil {
		log.Printf("[engine] could not persist config: %v", err)
	}

	seeds := loadSeeds(seedsFile)
	log.Printf("[engine] %d seed URLs available", len(seeds))

	// Geolocation is optional: without a user-supplied database the crawler
	// runs unchanged and the dashboard says positions are approximate.
	geo, err := geoip.Open(dataDir)
	if err != nil {
		log.Printf("[engine] geoip database present but unusable: %v", err)
		geo = nil
	}
	if geo.Available() {
		log.Printf("[engine] geolocation enabled: %s", filepath.Base(geo.Path()))
	} else {
		log.Printf("[engine] no geoip database in %s; globe positions will be approximate", dataDir)
	}
	defer geo.Close()

	met := metrics.New()
	crawler := crawl.New(kvdb, metadb, met, geo, cfg, seeds)

	// First run (or a fully drained frontier) needs the seed list injected.
	if kvdb.PendingApprox() == 0 {
		n, err := crawler.AddSeeds(seeds)
		if err != nil {
			log.Printf("[engine] seeding: %v", err)
		} else if n > 0 {
			log.Printf("[engine] seeded %d new URLs", n)
		}
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listen, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	info := runtimeInfo{
		PID: os.Getpid(), Port: port, Token: token,
		Started: time.Now().Unix(), Version: version,
	}
	infoPath := filepath.Join(dataDir, "runtime.json")
	if err := writeRuntimeInfo(infoPath, info); err != nil {
		log.Printf("[engine] could not write runtime info: %v", err)
	}
	defer os.Remove(infoPath)

	// Created early so the "shutdown" control action can cancel it: the engine
	// is a standalone daemon now, and the only way to make it exit is a signal,
	// a killed parent it was explicitly told to watch, or this cancel.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	apisrv := api.New(kvdb, metadb, met, crawler, geo, token)
	apisrv.OnShutdown = stop

	srv := &http.Server{
		Handler:      apisrv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE streams stay open indefinitely
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("[engine] api listening on http://127.0.0.1:%d", port)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[engine] http server stopped: %v", err)
		}
	}()

	if err := crawler.Start(); err != nil {
		return fmt.Errorf("start crawler: %w", err)
	}
	if cfg.Paused {
		log.Printf("[engine] started in paused state")
	}

	if parentPID > 0 {
		go watchParent(ctx, parentPID, stop)
	}

	<-ctx.Done()
	log.Printf("[engine] shutting down")

	crawler.Stop()

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("[engine] http shutdown: %v", err)
	}
	log.Printf("[engine] stopped cleanly")
	return nil
}

// watchParent exits the engine if the supervising desktop app disappears, so a
// killed shell never leaves an orphaned crawler running.
func watchParent(ctx context.Context, pid int, stop context.CancelFunc) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !processAlive(pid) {
				log.Printf("[engine] supervisor %d exited, shutting down", pid)
				stop()
				return
			}
		}
	}
}

// loadSeeds reads the seed list, preferring an explicit file over the built-in
// list, and merging both when a file is given.
func loadSeeds(path string) []string {
	seeds := parseSeeds(defaultSeeds)
	if path == "" {
		return seeds
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[engine] could not read seed file %s: %v", path, err)
		return seeds
	}
	extra := parseSeeds(string(raw))
	seen := make(map[string]struct{}, len(seeds)+len(extra))
	out := make([]string, 0, len(seeds)+len(extra))
	for _, s := range append(extra, seeds...) {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func parseSeeds(body string) []string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if !strings.Contains(l, "://") {
			l = "https://" + l
		}
		out = append(out, l)
	}
	return out
}

func writeRuntimeInfo(path string, info runtimeInfo) error {
	raw, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func defaultDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "WorldScraper")
	}
	return filepath.Join(".", "worldscraper-data")
}
