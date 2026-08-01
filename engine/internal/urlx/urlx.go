// Package urlx normalizes and classifies URLs so the frontier can dedupe them.
package urlx

import (
	"net/url"
	"strings"
)

// trackingParams are query keys that never change the document, only analytics.
var trackingParams = map[string]bool{
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "utm_id": true, "utm_name": true,
	"gclid": true, "fbclid": true, "msclkid": true, "dclid": true,
	"mc_cid": true, "mc_eid": true, "igshid": true, "ref_src": true,
	"yclid": true, "_ga": true, "_gl": true, "spm": true, "scm": true,
}

// skipExtensions are file types that carry no crawlable HTML.
var skipExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".bmp": true, ".ico": true, ".svg": true, ".tif": true, ".tiff": true,
	".mp3": true, ".mp4": true, ".m4a": true, ".m4v": true, ".avi": true,
	".mov": true, ".mkv": true, ".webm": true, ".flv": true, ".wmv": true,
	".ogg": true, ".oga": true, ".ogv": true, ".wav": true, ".flac": true,
	".zip": true, ".gz": true, ".bz2": true, ".xz": true, ".7z": true,
	".rar": true, ".tar": true, ".iso": true, ".dmg": true, ".exe": true,
	".msi": true, ".apk": true, ".deb": true, ".rpm": true, ".pkg": true,
	".css": true, ".js": true, ".mjs": true, ".map": true, ".woff": true,
	".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true,
	".pptx": true, ".psd": true, ".ai": true, ".bin": true, ".dll": true,
}

// Normalize canonicalizes a URL for storage and dedupe. It returns ok=false for
// anything the crawler should never enqueue.
func Normalize(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2000 {
		return "", false
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}

	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}

	// Fragments never identify a distinct document to a crawler.
	u.Fragment = ""
	u.RawFragment = ""
	u.User = nil

	host := strings.ToLower(u.Hostname())
	if host == "" || !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
		return "", false
	}
	if isLocalHost(host) {
		return "", false
	}

	// Drop the port when it is the scheme default.
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = host + ":" + port
	} else {
		u.Host = host
	}

	if strings.Contains(u.Path, "//") {
		u.Path = collapseSlashes(u.Path)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	if skipExtensions[extOf(u.Path)] {
		return "", false
	}

	// Strip analytics-only query parameters, keep the rest sorted for stability.
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			if trackingParams[strings.ToLower(k)] {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode() // Encode sorts keys
	}

	return u.String(), true
}

// Resolve turns a possibly-relative href into a normalized absolute URL.
func Resolve(base *url.URL, href string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return "", false
	}
	lower := strings.ToLower(href)
	for _, bad := range []string{"javascript:", "mailto:", "tel:", "data:", "about:", "file:", "ftp:", "sms:", "magnet:"} {
		if strings.HasPrefix(lower, bad) {
			return "", false
		}
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	return Normalize(base.ResolveReference(ref).String())
}

// Host returns the lowercase hostname of a normalized URL.
func Host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// RegistrableSuffix returns a coarse "site" grouping (last two labels) used for
// display grouping. It is a heuristic, not a public-suffix-list lookup.
func RegistrableSuffix(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	// Handle the common two-part public suffixes without shipping the full list.
	twoPart := map[string]bool{
		"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true,
		"com.au": true, "net.au": true, "org.au": true, "co.jp": true,
		"co.nz": true, "co.in": true, "co.za": true, "com.br": true,
		"com.cn": true, "com.mx": true, "com.tr": true, "com.sg": true,
	}
	last2 := strings.Join(parts[len(parts)-2:], ".")
	if twoPart[last2] && len(parts) >= 3 {
		return strings.Join(parts[len(parts)-3:], ".")
	}
	return last2
}

// TLD returns the final label of a hostname.
func TLD(host string) string {
	if i := strings.LastIndex(host, "."); i >= 0 && i+1 < len(host) {
		return host[i+1:]
	}
	return ""
}

func extOf(path string) string {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return ""
	}
	if j := strings.LastIndex(path, "/"); j > i {
		return ""
	}
	ext := strings.ToLower(path[i:])
	if len(ext) > 6 {
		return ""
	}
	return ext
}

func collapseSlashes(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	prevSlash := false
	for _, r := range p {
		if r == '/' {
			if prevSlash {
				continue
			}
			prevSlash = true
		} else {
			prevSlash = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1":
		return true
	}
	return strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".localhost") ||
		strings.HasPrefix(host, "192.168.") ||
		strings.HasPrefix(host, "10.") ||
		strings.HasPrefix(host, "169.254.")
}
