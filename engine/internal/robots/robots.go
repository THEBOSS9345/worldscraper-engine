// Package robots implements robots.txt parsing and matching.
//
// It follows the usual crawler conventions: the most specific matching
// User-agent group wins, patterns support * and $, and on an equal-length
// match Allow beats Disallow.
package robots

import (
	"strconv"
	"strings"
	"time"
)

// Rules is the parsed directive set that applies to one user agent.
type Rules struct {
	allow      []string
	disallow   []string
	CrawlDelay time.Duration
	Sitemaps   []string
	// AllowAll is set when the host published no applicable restrictions.
	AllowAll bool
}

// AllowAll returns permissive rules, used when robots.txt is missing or 4xx.
func AllowAllRules() *Rules { return &Rules{AllowAll: true} }

// DenyAll returns rules that block everything, used for a 5xx robots.txt where
// the polite behaviour is to back off.
func DenyAll() *Rules { return &Rules{disallow: []string{"/"}} }

type group struct {
	agents     []string
	allow      []string
	disallow   []string
	crawlDelay time.Duration
	specificity int
}

// Parse reads a robots.txt body and returns the rules applying to ua.
// ua should be the bare product token, e.g. "worldscraperbot".
func Parse(body, ua string) *Rules {
	ua = strings.ToLower(strings.TrimSpace(ua))

	var (
		groups   []group
		cur      *group
		sitemaps []string
		// A blank line or a non-agent directive ends the run of User-agent
		// lines that share one rule block.
		lastWasAgent bool
	)

	for _, rawLine := range strings.Split(body, "\n") {
		line := rawLine
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			lastWasAgent = false
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		field := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])

		switch field {
		case "user-agent":
			if cur == nil || !lastWasAgent {
				groups = append(groups, group{})
				cur = &groups[len(groups)-1]
			}
			cur.agents = append(cur.agents, strings.ToLower(value))
			lastWasAgent = true
		case "disallow":
			lastWasAgent = false
			if cur != nil {
				cur.disallow = append(cur.disallow, value)
			}
		case "allow":
			lastWasAgent = false
			if cur != nil {
				cur.allow = append(cur.allow, value)
			}
		case "crawl-delay":
			lastWasAgent = false
			if cur != nil {
				if f, err := strconv.ParseFloat(value, 64); err == nil && f >= 0 && f < 3600 {
					cur.crawlDelay = time.Duration(f * float64(time.Second))
				}
			}
		case "sitemap":
			lastWasAgent = false
			if value != "" {
				sitemaps = append(sitemaps, value)
			}
		default:
			lastWasAgent = false
		}
	}

	// Pick the group whose agent token is the longest match for ua; fall back
	// to the wildcard group.
	best := -1
	bestLen := -1
	for i := range groups {
		for _, a := range groups[i].agents {
			switch {
			case a == "*":
				if bestLen < 0 {
					best, bestLen = i, 0
				}
			case a != "" && strings.Contains(ua, a):
				if len(a) > bestLen {
					best, bestLen = i, len(a)
				}
			}
		}
	}

	r := &Rules{Sitemaps: sitemaps}
	if best < 0 {
		r.AllowAll = true
		return r
	}
	g := groups[best]
	r.allow = g.allow
	r.disallow = g.disallow
	r.CrawlDelay = g.crawlDelay
	if len(r.allow) == 0 && len(r.disallow) == 0 {
		r.AllowAll = true
	}
	return r
}

// Allowed reports whether path (including any query string) may be fetched.
func (r *Rules) Allowed(path string) bool {
	if r == nil || r.AllowAll {
		return true
	}
	if path == "" {
		path = "/"
	}
	bestAllow := matchLen(r.allow, path)
	bestDisallow := matchLen(r.disallow, path)

	if bestDisallow < 0 {
		return true
	}
	// Equal-length match: Allow wins, per the de-facto standard.
	return bestAllow >= bestDisallow
}

// matchLen returns the length of the longest pattern in pats matching path,
// or -1 when none match.
func matchLen(pats []string, path string) int {
	best := -1
	for _, p := range pats {
		if p == "" {
			// "Disallow:" with an empty value means allow everything; it is
			// never a match for blocking purposes.
			continue
		}
		if match(p, path) && len(p) > best {
			best = len(p)
		}
	}
	return best
}

// match implements robots.txt glob semantics: '*' matches any run of
// characters, a trailing '$' anchors the end, and patterns are prefix matches
// otherwise.
func match(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	if !strings.Contains(pattern, "*") {
		if anchored {
			return path == pattern
		}
		return strings.HasPrefix(path, pattern)
	}

	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(path, part) {
				return false
			}
			pos = len(part)
			continue
		}
		idx := strings.Index(path[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	if anchored {
		last := parts[len(parts)-1]
		if last == "" {
			return true // pattern ended with '*$'
		}
		return strings.HasSuffix(path, last)
	}
	return true
}
