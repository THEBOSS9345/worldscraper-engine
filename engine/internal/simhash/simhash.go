// Package simhash computes a 64-bit content fingerprint that stays stable
// under small edits, so near-duplicate pages can be recognised. Two pages that
// are copies (or light rewrites) of each other land within a few bits' Hamming
// distance, while unrelated pages differ in ~32 bits.
//
// The scheme is Charikar's SimHash with per-token FNV-1a hashing over
// lowercase words and word bigrams. It is intentionally dependency-free: this
// runs inside the crawl hot path on every page body.
package simhash

import (
	"hash/fnv"
	"unicode"
)

// Fingerprint returns the 64-bit SimHash of text. Empty or whitespace-only
// text yields 0, which callers should treat as "no fingerprint".
func Fingerprint(text string) uint64 {
	var acc [64]int64
	var features int

	for _, tok := range tokens(text) {
		features++
		h := fnv64(tok)
		// Bigrams keep word order in the picture; a shuffled sentence still
		// shares most unigrams but none of its bigrams.
		for i := 0; i < 64; i++ {
			if h&(1<<i) != 0 {
				acc[i]++
			} else {
				acc[i]--
			}
		}
	}

	if features == 0 {
		return 0
	}
	var fp uint64
	for i := 0; i < 64; i++ {
		if acc[i] > 0 {
			fp |= 1 << i
		}
	}
	return fp
}

// Distance returns the number of differing bits between two fingerprints.
func Distance(a, b uint64) int {
	d := a ^ b
	n := 0
	for d != 0 {
		d &= d - 1
		n++
	}
	return n
}

// Near reports whether a and b look like copies of the same page. The band
// structure is baked into the caller's LSH lookup; this is the exact check.
//
// Unrelated 64-bit fingerprints sit ~32 bits apart; syndicated copies of the
// same article land within 3-6 even after the aggregator adds its own header
// and footer. 8 leaves a wide margin on both sides.
func Near(a, b uint64) bool {
	if a == 0 || b == 0 {
		return false
	}
	return Distance(a, b) <= 8
}

// NearThreshold is the Hamming distance at or under which two pages are
// treated as near-duplicates. Kept in one place so the indexer matches.
const NearThreshold = 8

// bands returns the 8 band values of an 8-byte fingerprint, 8 bits each.
// Two fingerprints within a few bits of each other always share at least one
// band with high probability, which is what the LSH index exploits.
func bands(fp uint64) [8]byte {
	var out [8]byte
	for i := 0; i < 8; i++ {
		out[i] = byte(fp >> (i * 8))
	}
	return out
}

// Bands exposes the band split for storage/querying.
func Bands(fp uint64) [8]byte { return bands(fp) }

func tokens(text string) []string {
	words := make([]string, 0, 64)
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		words = append(words, string(cur))
		cur = cur[:0]
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()

	// Unigrams plus word bigrams: the bigrams keep word order in the picture so
	// a shuffled sentence still shares most unigrams but none of its bigrams.
	out := make([]string, 0, len(words)*2)
	out = append(out, words...)
	for i := 1; i < len(words); i++ {
		out = append(out, words[i-1]+words[i])
	}
	return out
}

func fnv64(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}
