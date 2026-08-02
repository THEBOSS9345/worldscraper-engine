// Package fetch is the HTTP client the crawler uses, tuned for many thousands
// of concurrent requests against unrelated hosts.
package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

// stripPort reduces "1.2.3.4:443" or "[::1]:443" to the bare address.
func stripPort(addr string) string {
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// Options configures the client.
type Options struct {
	UserAgent      string
	Timeout        time.Duration
	MaxBytes       int64
	MaxRedirects   int
	InsecureTLS    bool
	MaxConnsPerHost int
}

// Client is a shared, concurrency-safe HTTP fetcher.
type Client struct {
	hc   *http.Client
	opts Options
}

// Response is the outcome of one fetch.
type Response struct {
	URL         string // final URL after redirects
	Status      int
	ContentType string
	Body        []byte
	Bytes       int64
	Latency     time.Duration
	Truncated   bool
	// RemoteIP is the address actually connected to. It comes free from the
	// connection we already opened, so geolocation costs no extra lookup.
	RemoteIP string
}

// ErrTooLarge is returned when a body exceeds MaxBytes before any useful
// content was read.
var ErrTooLarge = errors.New("response body over size limit")

// New builds a client. A single Client should be shared by all workers so that
// connection pooling and DNS caching actually help.
func New(o Options) *Client {
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 3 << 20
	}
	if o.MaxRedirects <= 0 {
		o.MaxRedirects = 5
	}
	if o.MaxConnsPerHost <= 0 {
		o.MaxConnsPerHost = 4
	}

	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   8 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2048,
		MaxIdleConnsPerHost:   o.MaxConnsPerHost,
		MaxConnsPerHost:       o.MaxConnsPerHost * 2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: o.Timeout,
		DisableCompression:    false, // let net/http negotiate gzip for us
		TLSClientConfig: &tls.Config{
			// Crawling only reads public documents and never sends credentials,
			// so certificate problems are treated as a content-quality issue
			// rather than a security boundary. Toggleable in config.
			InsecureSkipVerify: o.InsecureTLS,
			// Offer only modern protocol versions; advertising TLS 1.0/1.1 is a
			// fingerprint no real browser has had for years.
			MinVersion: tls.VersionTLS12,
			// Mirror Chrome's TLS 1.2 cipher suite preference order.
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			},
			CurvePreferences: []tls.CurveID{
				tls.X25519, tls.CurveP256, tls.CurveP384,
			},
		},
	}

	c := &Client{opts: o}
	c.hc = &http.Client{
		Transport: tr,
		// A browser returns a Set-Cookie and sends it back; so do we.
		Jar:     newCookieJar(),
		Timeout: o.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= o.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", o.MaxRedirects)
			}
			return nil
		},
	}
	return c
}

// newCookieJar returns a shared, domain-scoped cookie jar so each host keeps
// whatever session state it set, like a browser tab would.
func newCookieJar() http.CookieJar {
	jar, _ := cookiejar.New(nil)
	return jar
}

// Get fetches a URL. htmlOnly makes it abandon non-HTML responses after the
// headers, without downloading the body.
func (c *Client) Get(ctx context.Context, url string, htmlOnly bool) (*Response, error) {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	// Record which address we actually reached. GotConn fires on the transport's
	// goroutine, so the value is guarded.
	var (
		peerMu sync.Mutex
		peer   string
	)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn == nil {
				return
			}
			addr := info.Conn.RemoteAddr().String()
			peerMu.Lock()
			peer = addr
			peerMu.Unlock()
		},
	}))
	req.Header.Set("User-Agent", c.opts.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// The sec-ch-ua / sec-fetch-* / upgrade-insecure-requests trio is sent by
	// every modern browser navigation. Its absence is a strong bot signal.
	req.Header.Set("Sec-Ch-Ua", `"Chromium";v="126", "Google Chrome";v="126", "Not-A.Brand";v="99"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	// Deliberately no Referer: these are top-level navigations (Sec-Fetch-Site
	// none), which browsers send bare. Accept-Encoding is left to net/http:
	// it adds "gzip" itself and decompresses transparently, whereas a manual
	// header would hand back raw compressed bytes.

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	peerMu.Lock()
	remote := peer
	peerMu.Unlock()

	ct := resp.Header.Get("Content-Type")
	out := &Response{
		URL:         resp.Request.URL.String(),
		Status:      resp.StatusCode,
		ContentType: ct,
		Latency:     time.Since(start),
		RemoteIP:    stripPort(remote),
	}

	if htmlOnly && !isHTML(ct) {
		// Drain a little so the connection can be reused, then stop.
		io.CopyN(io.Discard, resp.Body, 4096)
		return out, nil
	}
	if resp.StatusCode >= 400 {
		io.CopyN(io.Discard, resp.Body, 4096)
		return out, nil
	}

	limited := io.LimitReader(resp.Body, c.opts.MaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil && len(body) == 0 {
		return nil, err
	}
	if int64(len(body)) > c.opts.MaxBytes {
		body = body[:c.opts.MaxBytes]
		out.Truncated = true
	}
	out.Body = body
	out.Bytes = int64(len(body))
	out.Latency = time.Since(start)
	return out, nil
}

// isHTML reports whether a Content-Type is worth parsing for links and text.
func isHTML(ct string) bool {
	ct = strings.ToLower(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	switch ct {
	case "text/html", "application/xhtml+xml", "application/xml", "text/xml", "":
		return true
	}
	return false
}

// IsHTML is the exported form used by the crawler.
func IsHTML(ct string) bool { return isHTML(ct) }
