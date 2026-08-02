// Package kv is the crawl-scale key/value layer, backed by Pebble.
//
// It owns everything that grows without bound: the URL frontier, the permanent
// "already seen" set, the recrawl schedule, the robots.txt cache and the
// document spool that the Tantivy indexer drains.
//
// Key space
//
//	u/<url>                 url state (seen-set + recrawl source of truth)
//	q/<pri><rnd><seq>       pending queue, ascending scan order = crawl order
//	l/<seq>                 leased / in-flight, swept back into q/ on startup
//	t/<doneAt><url>         recrawl time index
//	d/<seq>                 spooled documents awaiting indexing
//	r/<host>                robots.txt cache
//	m/<name>                persisted counters
package kv

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/bloom"
)

// URL states stored under the u/ prefix.
const (
	StatePending = 0
	StateDone    = 1
	StateDead    = 2
)

type urlState struct {
	S      byte  `json:"s"`
	DoneAt int64 `json:"d,omitempty"`
}

// Item is one unit of crawl work.
type Item struct {
	URL   string `json:"u"`
	Host  string `json:"h"`
	Depth int    `json:"d"`
}

// Leased is an Item plus the lease key needed to release it.
type Leased struct {
	Item
	Lease uint64 `json:"-"`
}

// Doc is a crawled document handed to the search indexer.
type Doc struct {
	Seq         uint64 `json:"seq"`
	URL         string `json:"url"`
	Host        string `json:"host"`
	Site        string `json:"site"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Lang        string `json:"lang"`
	Category    string `json:"category"`
	Status      int    `json:"status"`
	Bytes       int64  `json:"bytes"`
	Depth       int    `json:"depth"`
	FetchedAt   int64  `json:"fetchedAt"`
	// Fp is a 64-bit SimHash of the page text. The indexer uses it to detect
	// near-duplicate pages and index only the first of each cluster. Zero means
	// the page had no extractable text.
	Fp uint64 `json:"fp,omitempty"`
}

// Completion reports the outcome of one leased item.
type Completion struct {
	Lease  uint64
	URL    string
	State  byte
	DoneAt int64
	Doc    *Doc // non-nil when the page should be indexed
	Links  []Item

	// Retry releases the lease and puts Item straight back on the queue,
	// leaving the URL's seen-state untouched. Used for 429/503 responses.
	Retry bool
	Item  Item
}

// Robots is a cached robots.txt fetch.
type Robots struct {
	Body      string `json:"b"`
	FetchedAt int64  `json:"f"`
	OK        bool   `json:"o"`
}

// DB wraps a Pebble instance with crawler-shaped operations.
type DB struct {
	p   *pebble.DB
	dir string

	seq       atomic.Uint64 // monotonic key sequence
	spoolTail atomic.Uint64 // highest spool seq written
	spoolHead atomic.Uint64 // highest spool seq acknowledged by the indexer
	pending   atomic.Int64  // approximate depth of the q/ queue

	rndMu sync.Mutex
	rnd   *rand.Rand
}

// Open creates or reopens the crawl store at dir.
func Open(dir string) (*DB, error) {
	opts := &pebble.Options{
		// A crawler is write-dominated with random point lookups for dedupe,
		// so favour a big memtable and aggressive bloom filtering.
		MemTableSize:                64 << 20,
		MemTableStopWritesThreshold: 8,
		CompactionConcurrencyRange:  func() (int, int) { return 1, 4 },
		L0CompactionThreshold:       4,
		L0StopWritesThreshold:       1000,
	}
	for i := range opts.Levels {
		l := &opts.Levels[i]
		l.BlockSize = 32 << 10
		l.IndexBlockSize = 256 << 10
		l.FilterPolicy = bloom.FilterPolicy(10)
		l.FilterType = pebble.TableFilter
		if i == 0 {
			opts.TargetFileSizes[i] = 8 << 20
		} else {
			opts.TargetFileSizes[i] = opts.TargetFileSizes[i-1] * 2
		}
	}
	opts.Cache = pebble.NewCache(256 << 20)
	defer opts.Cache.Unref()

	p, err := pebble.Open(dir, opts)
	if err != nil {
		return nil, fmt.Errorf("open pebble: %w", err)
	}

	d := &DB{p: p, dir: dir, rnd: rand.New(rand.NewSource(rand.Int63()))}
	d.seq.Store(d.counter("seq"))
	d.spoolTail.Store(d.counter("spoolTail"))
	d.spoolHead.Store(d.counter("spoolHead"))
	d.pending.Store(int64(d.counter("pending")))
	return d, nil
}

// Close persists counters and shuts Pebble down cleanly.
func (d *DB) Close() error {
	d.PersistCounters()
	return d.p.Close()
}

// PersistCounters flushes the in-memory counters to disk.
func (d *DB) PersistCounters() {
	b := d.p.NewBatch()
	defer b.Close()
	setCounter(b, "seq", d.seq.Load())
	setCounter(b, "spoolTail", d.spoolTail.Load())
	setCounter(b, "spoolHead", d.spoolHead.Load())
	p := d.pending.Load()
	if p < 0 {
		p = 0
	}
	setCounter(b, "pending", uint64(p))
	_ = d.p.Apply(b, pebble.NoSync)
}

// Flush forces a memtable flush; used on a timer so an OS-level crash loses
// only a few seconds of frontier writes.
func (d *DB) Flush() error { return d.p.Flush() }

// ------------------------------------------------------------------ metakeys --

// SetString persists an arbitrary string under m/<name>. Used for small
// cross-restart values such as discovery cursors.
func (d *DB) SetString(name, v string) error {
	if v == "" {
		return d.p.Delete(counterKey(name), pebble.NoSync)
	}
	return d.p.Set(counterKey(name), []byte(v), pebble.NoSync)
}

// GetString reads a string previously written with SetString.
func (d *DB) GetString(name string) string {
	v, closer, err := d.p.Get(counterKey(name))
	if err != nil {
		return ""
	}
	defer closer.Close()
	return string(v)
}

// SetCounter persists an arbitrary uint64 counter under m/<name>.
func (d *DB) SetCounter(name string, v uint64) error {
	b := d.p.NewBatch()
	defer b.Close()
	setCounter(b, name, v)
	return d.p.Apply(b, pebble.NoSync)
}

// Counter reads an arbitrary counter written with SetCounter.
func (d *DB) Counter(name string) uint64 { return d.counter(name) }

// ---------------------------------------------------------------- frontier --

// Enqueue adds items that have never been seen before. It returns how many
// were genuinely new.
func (d *DB) Enqueue(items []Item) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	b := d.p.NewBatch()
	defer b.Close()

	added := 0
	for _, it := range items {
		if it.URL == "" {
			continue
		}
		uk := key('u', it.URL)
		if _, closer, err := d.p.Get(uk); err == nil {
			closer.Close()
			continue // already seen
		} else if !errors.Is(err, pebble.ErrNotFound) {
			return added, err
		}
		if err := d.pushLocked(b, it); err != nil {
			return added, err
		}
		st, _ := json.Marshal(urlState{S: StatePending})
		if err := b.Set(uk, st, nil); err != nil {
			return added, err
		}
		added++
	}
	if added == 0 {
		return 0, nil
	}
	if err := d.p.Apply(b, pebble.NoSync); err != nil {
		return 0, err
	}
	d.pending.Add(int64(added))
	return added, nil
}

// Requeue puts a known URL back into the pending queue (used by recrawl).
// Already-seen work is pinned to the back of the queue so fresh URLs win.
func (d *DB) Requeue(items []Item) error {
	if len(items) == 0 {
		return nil
	}
	b := d.p.NewBatch()
	defer b.Close()
	for _, it := range items {
		if err := d.pushLockedAt(b, it, 255); err != nil {
			return err
		}
		st, _ := json.Marshal(urlState{S: StatePending})
		if err := b.Set(key('u', it.URL), st, nil); err != nil {
			return err
		}
	}
	if err := d.p.Apply(b, pebble.NoSync); err != nil {
		return err
	}
	d.pending.Add(int64(len(items)))
	return nil
}

// pushLocked writes one queue entry for a fresh URL. Priority is crawl depth so
// the crawl is breadth-first, and the random bytes shuffle same-depth URLs so a
// single page's outlinks do not arrive as one host-dominated run.
func (d *DB) pushLocked(b *pebble.Batch, it Item) error {
	pri := byte(255)
	if it.Depth >= 0 && it.Depth < 255 {
		pri = byte(it.Depth)
	}
	return d.pushLockedAt(b, it, pri)
}

// pushLockedAt writes one queue entry at an explicit priority. Queue keys are
// q/<priority><random><seq> with ascending order leased first, so lower bytes
// win. Fresh URLs sort by depth; anything for a URL that has already been seen
// (recrawls, retries, and the startup sweep of in-flight leases) is pinned to
// 255 so brand-new domains are always fetched before already-known ones.
func (d *DB) pushLockedAt(b *pebble.Batch, it Item, pri byte) error {
	d.rndMu.Lock()
	jitter := uint16(d.rnd.Intn(1 << 16))
	d.rndMu.Unlock()

	k := make([]byte, 2+1+2+8)
	k[0], k[1] = 'q', '/'
	k[2] = pri
	binary.BigEndian.PutUint16(k[3:], jitter)
	binary.BigEndian.PutUint64(k[5:], d.seq.Add(1))

	v, err := json.Marshal(it)
	if err != nil {
		return err
	}
	return b.Set(k, v, nil)
}

// Lease removes up to n items from the queue and parks them under l/ so a
// crash cannot silently drop them.
func (d *DB) Lease(n int) ([]Leased, error) {
	if n <= 0 {
		return nil, nil
	}
	iter, err := d.p.NewIter(prefixBounds('q'))
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	b := d.p.NewBatch()
	defer b.Close()

	out := make([]Leased, 0, n)
	for iter.First(); iter.Valid() && len(out) < n; iter.Next() {
		var it Item
		if err := json.Unmarshal(iter.Value(), &it); err != nil {
			// Unreadable entry: drop it rather than wedging the queue.
			_ = b.Delete(append([]byte{}, iter.Key()...), nil)
			continue
		}
		lease := d.seq.Add(1)
		lv, err := json.Marshal(it)
		if err != nil {
			continue
		}
		if err := b.Set(seqKey('l', lease), lv, nil); err != nil {
			return nil, err
		}
		if err := b.Delete(append([]byte{}, iter.Key()...), nil); err != nil {
			return nil, err
		}
		out = append(out, Leased{Item: it, Lease: lease})
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := d.p.Apply(b, pebble.NoSync); err != nil {
		return nil, err
	}
	d.pending.Add(-int64(len(out)))
	return out, nil
}

// Complete applies a batch of crawl outcomes atomically: it releases leases,
// records final URL states, schedules recrawls, spools documents for indexing
// and enqueues newly discovered links.
func (d *DB) Complete(cs []Completion) (newLinks int, err error) {
	if len(cs) == 0 {
		return 0, nil
	}
	b := d.p.NewBatch()
	defer b.Close()

	pendingAdd := 0
	for _, c := range cs {
		if err := b.Delete(seqKey('l', c.Lease), nil); err != nil {
			return 0, err
		}
		if c.Retry {
			// Leave the seen-state as pending and re-queue the same item.
			// Failed work is already known, so it waits behind fresh URLs.
			if err := d.pushLockedAt(b, c.Item, 255); err != nil {
				return 0, err
			}
			pendingAdd++
			continue
		}
		st, _ := json.Marshal(urlState{S: c.State, DoneAt: c.DoneAt})
		if err := b.Set(key('u', c.URL), st, nil); err != nil {
			return 0, err
		}
		if c.State == StateDone {
			// Time-ordered index so the recrawl sweeper never scans the whole
			// seen-set looking for stale pages.
			if err := b.Set(timeKey(c.DoneAt, c.URL), nil, nil); err != nil {
				return 0, err
			}
		}
		if c.Doc != nil {
			seq := d.spoolTail.Add(1)
			c.Doc.Seq = seq
			dv, err := json.Marshal(c.Doc)
			if err != nil {
				return 0, err
			}
			if err := b.Set(seqKey('d', seq), dv, nil); err != nil {
				return 0, err
			}
		}
		for _, ln := range c.Links {
			if ln.URL == "" {
				continue
			}
			uk := key('u', ln.URL)
			if _, closer, gerr := d.p.Get(uk); gerr == nil {
				closer.Close()
				continue
			} else if !errors.Is(gerr, pebble.ErrNotFound) {
				return 0, gerr
			}
			if err := d.pushLocked(b, ln); err != nil {
				return 0, err
			}
			us, _ := json.Marshal(urlState{S: StatePending})
			if err := b.Set(uk, us, nil); err != nil {
				return 0, err
			}
			pendingAdd++
		}
	}
	if err := d.p.Apply(b, pebble.NoSync); err != nil {
		return 0, err
	}
	d.pending.Add(int64(pendingAdd))
	return pendingAdd, nil
}

// SweepLeases moves every in-flight entry back to pending. Called once at
// startup so an unclean shutdown never loses queued work.
func (d *DB) SweepLeases() (int, error) {
	iter, err := d.p.NewIter(prefixBounds('l'))
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	b := d.p.NewBatch()
	defer b.Close()

	n := 0
	for iter.First(); iter.Valid(); iter.Next() {
		var it Item
		if err := json.Unmarshal(iter.Value(), &it); err == nil {
			if err := d.pushLockedAt(b, it, 255); err != nil {
				return n, err
			}
			n++
		}
		if err := b.Delete(append([]byte{}, iter.Key()...), nil); err != nil {
			return n, err
		}
	}
	if err := iter.Error(); err != nil {
		return n, err
	}
	if b.Empty() {
		return 0, nil
	}
	if err := d.p.Apply(b, pebble.Sync); err != nil {
		return 0, err
	}
	d.pending.Add(int64(n))
	return n, nil
}

// DueForRecrawl returns pages finished before `before`, and removes them from
// the recrawl index so they are handed out only once.
func (d *DB) DueForRecrawl(before int64, n int) ([]Item, error) {
	lo := timeKey(0, "")
	hi := timeKey(before, "")
	iter, err := d.p.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	b := d.p.NewBatch()
	defer b.Close()

	out := make([]Item, 0, n)
	for iter.First(); iter.Valid() && len(out) < n; iter.Next() {
		k := iter.Key()
		if len(k) <= 10 {
			continue
		}
		u := string(k[10:])
		out = append(out, Item{URL: u, Host: hostOf(u), Depth: 0})
		if err := b.Delete(append([]byte{}, k...), nil); err != nil {
			return nil, err
		}
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := d.p.Apply(b, pebble.NoSync); err != nil {
		return nil, err
	}
	return out, nil
}

// ClearQueue drops every pending and in-flight frontier entry.
//
// The seen-set is left intact, so already-crawled URLs are not re-fetched; this
// empties the backlog without discarding what has been done.
func (d *DB) ClearQueue() error {
	if err := d.p.DeleteRange([]byte{'q', '/'}, prefixEnd('q'), pebble.Sync); err != nil {
		return err
	}
	if err := d.p.DeleteRange([]byte{'l', '/'}, prefixEnd('l'), pebble.Sync); err != nil {
		return err
	}
	d.pending.Store(0)
	d.PersistCounters()
	return nil
}

// ClearSeen forgets every URL the crawler has visited, so the whole frontier can
// be crawled again from scratch. Destructive: this is the "start over" button.
func (d *DB) ClearSeen() error {
	for _, p := range []byte{'u', 't'} {
		if err := d.p.DeleteRange([]byte{p, '/'}, prefixEnd(p), pebble.Sync); err != nil {
			return err
		}
	}
	return nil
}

// ClearRobots drops the cached robots.txt files so they are re-fetched.
func (d *DB) ClearRobots() error {
	return d.p.DeleteRange([]byte{'r', '/'}, prefixEnd('r'), pebble.Sync)
}

// Compact rewrites the store to reclaim space left by deletions. It can take a
// while on a large database, so callers should run it off the request path.
func (d *DB) Compact() error {
	return d.p.Compact(context.Background(), []byte{0x00}, []byte{0xff, 0xff, 0xff, 0xff}, true)
}

// PendingApprox reports the queue depth.
func (d *DB) PendingApprox() int64 {
	v := d.pending.Load()
	if v < 0 {
		return 0
	}
	return v
}

// CountPending walks the queue to correct counter drift, giving up after max
// entries so it can never stall the engine.
func (d *DB) CountPending(max int) (int64, bool, error) {
	iter, err := d.p.NewIter(prefixBounds('q'))
	if err != nil {
		return 0, false, err
	}
	defer iter.Close()
	var n int64
	for iter.First(); iter.Valid(); iter.Next() {
		n++
		if int(n) >= max {
			return n, false, iter.Error()
		}
	}
	if err := iter.Error(); err != nil {
		return 0, false, err
	}
	d.pending.Store(n)
	return n, true, nil
}

// ------------------------------------------------------------------- spool --

// SpoolRead returns up to n documents with seq greater than after.
func (d *DB) SpoolRead(after uint64, n int) ([]json.RawMessage, uint64, error) {
	lo := seqKey('d', after+1)
	hi := prefixEnd('d')
	iter, err := d.p.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, after, err
	}
	defer iter.Close()

	out := make([]json.RawMessage, 0, n)
	last := after
	for iter.First(); iter.Valid() && len(out) < n; iter.Next() {
		out = append(out, append(json.RawMessage{}, iter.Value()...))
		last = binary.BigEndian.Uint64(iter.Key()[2:])
	}
	return out, last, iter.Error()
}

// SpoolAck drops every spooled document up to and including seq. The indexer
// only calls this after its own commit, so a crash on either side replays
// rather than loses documents.
func (d *DB) SpoolAck(seq uint64) error {
	if seq == 0 {
		return nil
	}
	if seq > d.spoolTail.Load() {
		seq = d.spoolTail.Load()
	}
	if err := d.p.DeleteRange(seqKey('d', 0), seqKey('d', seq+1), pebble.NoSync); err != nil {
		return err
	}
	for {
		cur := d.spoolHead.Load()
		if seq <= cur || d.spoolHead.CompareAndSwap(cur, seq) {
			break
		}
	}
	return nil
}

// SpoolDepth is how many documents are waiting to be indexed.
func (d *DB) SpoolDepth() uint64 {
	t, h := d.spoolTail.Load(), d.spoolHead.Load()
	if t < h {
		return 0
	}
	return t - h
}

// SpoolCursor is the last sequence the indexer acknowledged.
func (d *DB) SpoolCursor() uint64 { return d.spoolHead.Load() }

// ------------------------------------------------------------------ robots --

// GetRobots reads the cached robots.txt for a host.
func (d *DB) GetRobots(host string) (Robots, bool) {
	v, closer, err := d.p.Get(key('r', host))
	if err != nil {
		return Robots{}, false
	}
	defer closer.Close()
	var r Robots
	if err := json.Unmarshal(v, &r); err != nil {
		return Robots{}, false
	}
	return r, true
}

// PutRobots caches a robots.txt fetch.
func (d *DB) PutRobots(host string, r Robots) error {
	v, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return d.p.Set(key('r', host), v, pebble.NoSync)
}

// DiskUsage reports the on-disk size of the crawl store. It walks the
// directory rather than asking Pebble to estimate, because the estimate only
// covers sstables and reads as zero until the first compaction.
func (d *DB) DiskUsage() uint64 {
	var total uint64
	err := filepath.Walk(d.dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable entries just do not count
		}
		if !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	if err != nil {
		return total
	}
	return total
}

// ------------------------------------------------------------------- keys ---

func key(prefix byte, s string) []byte {
	k := make([]byte, 2+len(s))
	k[0], k[1] = prefix, '/'
	copy(k[2:], s)
	return k
}

func seqKey(prefix byte, seq uint64) []byte {
	k := make([]byte, 2+8)
	k[0], k[1] = prefix, '/'
	binary.BigEndian.PutUint64(k[2:], seq)
	return k
}

func timeKey(ts int64, url string) []byte {
	k := make([]byte, 2+8+len(url))
	k[0], k[1] = 't', '/'
	if ts < 0 {
		ts = 0
	}
	binary.BigEndian.PutUint64(k[2:], uint64(ts))
	copy(k[10:], url)
	return k
}

func prefixBounds(p byte) *pebble.IterOptions {
	return &pebble.IterOptions{LowerBound: []byte{p, '/'}, UpperBound: prefixEnd(p)}
}

func prefixEnd(p byte) []byte { return []byte{p, '/' + 1} }

func counterKey(name string) []byte { return key('m', name) }

func (d *DB) counter(name string) uint64 {
	v, closer, err := d.p.Get(counterKey(name))
	if err != nil {
		return 0
	}
	defer closer.Close()
	if len(v) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

func setCounter(b *pebble.Batch, name string, v uint64) {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, v)
	_ = b.Set(counterKey(name), buf, nil)
}

// hostOf is a dependency-free host extractor for keys we already normalized.
func hostOf(u string) string {
	i := 0
	if j := indexOf(u, "://"); j >= 0 {
		i = j + 3
	}
	rest := u[i:]
	end := len(rest)
	for x := 0; x < len(rest); x++ {
		c := rest[x]
		if c == '/' || c == '?' || c == '#' {
			end = x
			break
		}
	}
	h := rest[:end]
	if j := indexOf(h, ":"); j >= 0 {
		h = h[:j]
	}
	return h
}

func indexOf(s, sub string) int {
	n := len(sub)
	for i := 0; i+n <= len(s); i++ {
		if s[i:i+n] == sub {
			return i
		}
	}
	return -1
}
