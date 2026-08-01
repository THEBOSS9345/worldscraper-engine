// Package meta is the small, structured half of the storage split: settings,
// per-host aggregates and the per-minute metrics series that back the
// dashboard charts.
//
// Everything here is bounded and low-write-rate, which is exactly what SQLite
// is good at. The unbounded firehose lives in package kv instead.
package meta

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS hosts (
  host       TEXT    PRIMARY KEY,
  site       TEXT    NOT NULL DEFAULT '',
  pages      INTEGER NOT NULL DEFAULT 0,
  errors     INTEGER NOT NULL DEFAULT 0,
  bytes      INTEGER NOT NULL DEFAULT 0,
  category   TEXT    NOT NULL DEFAULT 'other',
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS hosts_pages_idx ON hosts(pages DESC);

CREATE TABLE IF NOT EXISTS categories (
  category TEXT PRIMARY KEY,
  pages    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS statuses (
  code INTEGER PRIMARY KEY,
  n    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS langs (
  lang  TEXT PRIMARY KEY,
  pages INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

-- Populated only when a GeoIP database is installed.
CREATE TABLE IF NOT EXISTS countries (
  country TEXT PRIMARY KEY,
  pages   INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS metrics_minute (
  ts     INTEGER PRIMARY KEY,
  pages  INTEGER NOT NULL DEFAULT 0,
  bytes  INTEGER NOT NULL DEFAULT 0,
  errors INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS totals (
  k TEXT PRIMARY KEY,
  v INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
`

// DB holds a single-writer connection plus a small read pool.
type DB struct {
	w  *sql.DB
	r  *sql.DB
	mu sync.Mutex // serializes multi-statement write transactions
}

// Open initialises the metadata database at path.
func Open(path string) (*DB, error) {
	dsn := path + "?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=temp_store(memory)&_pragma=cache_size(-32000)"

	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open meta writer: %w", err)
	}
	// SQLite allows exactly one writer; making that explicit avoids lock churn.
	w.SetMaxOpenConns(1)
	w.SetConnMaxLifetime(0)

	if _, err := w.Exec(schema); err != nil {
		w.Close()
		return nil, fmt.Errorf("apply meta schema: %w", err)
	}

	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("open meta reader: %w", err)
	}
	r.SetMaxOpenConns(4)

	return &DB{w: w, r: r}, nil
}

// Close shuts both handles down.
func (d *DB) Close() error {
	e1 := d.w.Close()
	if err := d.r.Close(); err != nil {
		return err
	}
	return e1
}

// ------------------------------------------------------------------ deltas --

// HostDelta accumulates one host's contribution within a flush window.
type HostDelta struct {
	Site     string
	Category string
	Pages    int64
	Errors   int64
	Bytes    int64
	LastSeen int64
}

// Delta is a batch of aggregate changes applied in a single transaction.
type Delta struct {
	Hosts      map[string]*HostDelta
	Categories map[string]int64
	Statuses   map[int]int64
	Langs      map[string]int64
	Countries  map[string]int64

	Pages  int64
	Bytes  int64
	Errors int64
	Links  int64
	Fetch  int64 // total fetch attempts

	Minute int64 // unix seconds truncated to the minute
}

// NewDelta returns an empty delta bucket.
func NewDelta() *Delta {
	return &Delta{
		Hosts:      make(map[string]*HostDelta),
		Categories: make(map[string]int64),
		Statuses:   make(map[int]int64),
		Langs:      make(map[string]int64),
		Countries:  make(map[string]int64),
	}
}

// Empty reports whether there is nothing to write.
func (d *Delta) Empty() bool {
	return d.Pages == 0 && d.Errors == 0 && d.Links == 0 && d.Fetch == 0 && len(d.Hosts) == 0
}

// Host returns (creating if needed) the accumulator for a host.
func (d *Delta) Host(host string) *HostDelta {
	h, ok := d.Hosts[host]
	if !ok {
		h = &HostDelta{}
		d.Hosts[host] = h
	}
	return h
}

// Apply writes a delta atomically.
func (d *DB) Apply(dl *Delta) error {
	if dl == nil || dl.Empty() {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()

	hostStmt, err := tx.Prepare(`
		INSERT INTO hosts(host, site, pages, errors, bytes, category, first_seen, last_seen)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(host) DO UPDATE SET
			pages     = pages  + excluded.pages,
			errors    = errors + excluded.errors,
			bytes     = bytes  + excluded.bytes,
			site      = CASE WHEN excluded.site      != '' THEN excluded.site      ELSE hosts.site END,
			category  = CASE WHEN excluded.category  != '' THEN excluded.category  ELSE hosts.category END,
			last_seen = MAX(hosts.last_seen, excluded.last_seen)`)
	if err != nil {
		return err
	}
	defer hostStmt.Close()

	for host, h := range dl.Hosts {
		last := h.LastSeen
		if last == 0 {
			last = now
		}
		if _, err := hostStmt.Exec(host, h.Site, h.Pages, h.Errors, h.Bytes, h.Category, now, last); err != nil {
			return err
		}
	}

	if err := bumpMap(tx, `INSERT INTO categories(category, pages) VALUES(?, ?)
		ON CONFLICT(category) DO UPDATE SET pages = pages + excluded.pages`, dl.Categories); err != nil {
		return err
	}
	if err := bumpMap(tx, `INSERT INTO langs(lang, pages) VALUES(?, ?)
		ON CONFLICT(lang) DO UPDATE SET pages = pages + excluded.pages`, dl.Langs); err != nil {
		return err
	}
	if err := bumpMap(tx, `INSERT INTO countries(country, pages) VALUES(?, ?)
		ON CONFLICT(country) DO UPDATE SET pages = pages + excluded.pages`, dl.Countries); err != nil {
		return err
	}
	if len(dl.Statuses) > 0 {
		st, err := tx.Prepare(`INSERT INTO statuses(code, n) VALUES(?, ?)
			ON CONFLICT(code) DO UPDATE SET n = n + excluded.n`)
		if err != nil {
			return err
		}
		for code, n := range dl.Statuses {
			if _, err := st.Exec(code, n); err != nil {
				st.Close()
				return err
			}
		}
		st.Close()
	}

	totals := map[string]int64{
		"pages": dl.Pages, "bytes": dl.Bytes, "errors": dl.Errors,
		"links": dl.Links, "fetches": dl.Fetch,
	}
	if err := bumpMap(tx, `INSERT INTO totals(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = v + excluded.v`, totals); err != nil {
		return err
	}

	minute := dl.Minute
	if minute == 0 {
		minute = now - now%60
	}
	if _, err := tx.Exec(`INSERT INTO metrics_minute(ts, pages, bytes, errors) VALUES(?, ?, ?, ?)
		ON CONFLICT(ts) DO UPDATE SET
			pages  = pages  + excluded.pages,
			bytes  = bytes  + excluded.bytes,
			errors = errors + excluded.errors`,
		minute, dl.Pages, dl.Bytes, dl.Errors); err != nil {
		return err
	}

	return tx.Commit()
}

func bumpMap[K comparable](tx *sql.Tx, q string, m map[K]int64) error {
	if len(m) == 0 {
		return nil
	}
	st, err := tx.Prepare(q)
	if err != nil {
		return err
	}
	defer st.Close()
	for k, v := range m {
		if v == 0 {
			continue
		}
		if _, err := st.Exec(k, v); err != nil {
			return err
		}
	}
	return nil
}

// ----------------------------------------------------------------- queries --

// HostRow is one row of the top-hosts leaderboard.
type HostRow struct {
	Host     string `json:"host"`
	Site     string `json:"site"`
	Pages    int64  `json:"pages"`
	Errors   int64  `json:"errors"`
	Bytes    int64  `json:"bytes"`
	Category string `json:"category"`
	LastSeen int64  `json:"lastSeen"`
}

// TopHosts returns the most-crawled hosts.
func (d *DB) TopHosts(n int) ([]HostRow, error) {
	rows, err := d.r.Query(`SELECT host, site, pages, errors, bytes, category, last_seen
		FROM hosts ORDER BY pages DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HostRow{}
	for rows.Next() {
		var h HostRow
		if err := rows.Scan(&h.Host, &h.Site, &h.Pages, &h.Errors, &h.Bytes, &h.Category, &h.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// CountRow is a generic label/value pair.
type CountRow struct {
	Label string `json:"label"`
	N     int64  `json:"n"`
}

// Categories returns page counts per category, most common first.
func (d *DB) Categories() ([]CountRow, error) {
	return d.counts(`SELECT category, pages FROM categories WHERE pages > 0 ORDER BY pages DESC`)
}

// Langs returns page counts per detected language.
func (d *DB) Langs(n int) ([]CountRow, error) {
	return d.counts(`SELECT lang, pages FROM langs WHERE lang != '' AND pages > 0
		ORDER BY pages DESC LIMIT ` + itoa(n))
}

// Countries returns page counts per server country. Empty without a GeoIP
// database installed.
func (d *DB) Countries(n int) ([]CountRow, error) {
	return d.counts(`SELECT country, pages FROM countries WHERE country != '' AND pages > 0
		ORDER BY pages DESC LIMIT ` + itoa(n))
}

// Statuses returns HTTP response code counts.
func (d *DB) Statuses() ([]CountRow, error) {
	return d.counts(`SELECT CAST(code AS TEXT), n FROM statuses WHERE n > 0 ORDER BY n DESC`)
}

func (d *DB) counts(q string) ([]CountRow, error) {
	rows, err := d.r.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CountRow{}
	for rows.Next() {
		var c CountRow
		if err := rows.Scan(&c.Label, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Totals returns the lifetime counters.
func (d *DB) Totals() (map[string]int64, error) {
	rows, err := d.r.Query(`SELECT k, v FROM totals`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, k := range []string{"pages", "bytes", "errors", "links", "fetches"} {
		if _, ok := out[k]; !ok {
			out[k] = 0
		}
	}
	return out, nil
}

// HostCount is the number of distinct hosts touched.
func (d *DB) HostCount() (int64, error) {
	var n int64
	err := d.r.QueryRow(`SELECT count(*) FROM hosts`).Scan(&n)
	return n, err
}

// MinutePoint is one sample of the throughput series.
type MinutePoint struct {
	TS     int64 `json:"ts"`
	Pages  int64 `json:"pages"`
	Bytes  int64 `json:"bytes"`
	Errors int64 `json:"errors"`
}

// Series returns the last n minutes of throughput, oldest first, with gaps
// filled so the chart has a continuous time axis.
func (d *DB) Series(minutes int) ([]MinutePoint, error) {
	if minutes <= 0 || minutes > 10080 {
		minutes = 120
	}
	now := time.Now().Unix()
	now -= now % 60
	from := now - int64(minutes-1)*60

	rows, err := d.r.Query(`SELECT ts, pages, bytes, errors FROM metrics_minute
		WHERE ts >= ? ORDER BY ts`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	found := make(map[int64]MinutePoint, minutes)
	for rows.Next() {
		var p MinutePoint
		if err := rows.Scan(&p.TS, &p.Pages, &p.Bytes, &p.Errors); err != nil {
			return nil, err
		}
		found[p.TS] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]MinutePoint, 0, minutes)
	for ts := from; ts <= now; ts += 60 {
		if p, ok := found[ts]; ok {
			out = append(out, p)
		} else {
			out = append(out, MinutePoint{TS: ts})
		}
	}
	return out, nil
}

// ResetStats zeroes every aggregate while leaving settings untouched.
func (d *DB) ResetStats() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, table := range []string{"hosts", "categories", "statuses", "langs", "countries", "metrics_minute", "totals"} {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Vacuum reclaims file space after a large delete.
func (d *DB) Vacuum() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.w.Exec("VACUUM")
	return err
}

// PruneMetrics drops throughput samples older than the retention window.
func (d *DB) PruneMetrics(keepDays int) error {
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour).Unix()
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.w.Exec(`DELETE FROM metrics_minute WHERE ts < ?`, cutoff)
	return err
}

// ---------------------------------------------------------------- settings --

// GetJSON reads a JSON-encoded setting into v. ok is false when unset.
func (d *DB) GetJSON(k string, v any) (bool, error) {
	var raw string
	err := d.r.QueryRow(`SELECT v FROM settings WHERE k = ?`, k).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return false, err
	}
	return true, nil
}

// PutJSON stores a JSON-encoded setting.
func (d *DB) PutJSON(k string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err = d.w.Exec(`INSERT INTO settings(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, string(raw))
	return err
}

func itoa(n int) string {
	if n <= 0 {
		n = 10
	}
	return strings.TrimSpace(fmt.Sprintf("%d", n))
}
