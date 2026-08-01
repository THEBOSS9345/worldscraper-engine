// Package extract turns a raw HTML response into the fields the index and the
// dashboard need: title, description, language, body text and outbound links.
//
// It streams with a tokenizer rather than building a DOM, because at crawl
// rates the allocation cost of a full parse tree dominates everything else.
package extract

import (
	"bytes"
	"io"
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"worldscraper/engine/internal/urlx"
)

// Doc is the extracted content of one page.
type Doc struct {
	Title       string
	Description string
	Lang        string
	Canonical   string
	Text        string
	Links       []string
	NoIndex     bool
	NoFollow    bool
}

// Limits bounds the work done per document.
type Limits struct {
	MaxLinks int
	MaxText  int
}

// skipContent are elements whose text is never part of the readable document.
//
// <head> is deliberately absent: the document title lives inside it, and every
// other text-bearing element in a head (script, style) is skipped by name.
var skipContent = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true,
	"canvas": true, "template": true, "iframe": true,
}

// Parse extracts a document. base is the URL the body was fetched from and is
// used to resolve relative links.
func Parse(body []byte, contentType string, base *url.URL, lim Limits) *Doc {
	if lim.MaxLinks <= 0 {
		lim.MaxLinks = 150
	}
	if lim.MaxText <= 0 {
		lim.MaxText = 24 << 10
	}

	// Decode to UTF-8 using the Content-Type header, then any meta charset.
	var r io.Reader = bytes.NewReader(body)
	if dec, err := charset.NewReader(r, contentType); err == nil {
		r = dec
	} else {
		r = bytes.NewReader(body)
	}

	doc := &Doc{}
	seenLink := make(map[string]struct{}, lim.MaxLinks)

	var (
		text     strings.Builder
		skip     int
		inTitle  bool
		z        = html.NewTokenizer(r)
	)
	text.Grow(4096)

	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			goto done

		case html.StartTagToken, html.SelfClosingTagToken:
			nameB, hasAttr := z.TagName()
			name := string(nameB)

			if skipContent[name] {
				// A self-closing form encloses no content to skip.
				if tt == html.StartTagToken {
					skip++
				}
				continue
			}

			switch name {
			case "title":
				// Ignore <title> nested in skipped subtrees, e.g. inside <svg>.
				if doc.Title == "" && skip == 0 {
					inTitle = true
				}
			case "html":
				if hasAttr {
					if v, ok := attr(z, "lang"); ok && doc.Lang == "" {
						doc.Lang = normalizeLang(v)
					}
				}
			case "meta":
				if hasAttr {
					handleMeta(z, doc)
				}
			case "link":
				if hasAttr {
					handleLink(z, doc, base)
				}
			case "a":
				if hasAttr && len(doc.Links) < lim.MaxLinks {
					if href, ok := attr(z, "href"); ok {
						if abs, ok := urlx.Resolve(base, href); ok {
							if _, dup := seenLink[abs]; !dup {
								seenLink[abs] = struct{}{}
								doc.Links = append(doc.Links, abs)
							}
						}
					}
				}
			case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
				if skip == 0 && text.Len() < lim.MaxText {
					text.WriteByte(' ')
				}
			}

		case html.EndTagToken:
			nameB, _ := z.TagName()
			name := string(nameB)
			if skipContent[name] {
				if skip > 0 {
					skip--
				}
				continue
			}
			if name == "title" {
				inTitle = false
			}

		case html.TextToken:
			if skip > 0 {
				continue
			}
			raw := z.Text()
			if inTitle {
				if len(doc.Title) < 512 {
					doc.Title += string(raw)
				}
				continue
			}
			if text.Len() < lim.MaxText {
				text.Write(raw)
			}
		}
	}

done:
	doc.Title = squash(doc.Title, 300)
	doc.Description = squash(doc.Description, 600)
	doc.Text = squash(text.String(), lim.MaxText)
	if doc.Lang == "" {
		doc.Lang = guessScript(doc.Text)
	}
	if doc.NoFollow {
		doc.Links = nil
	}
	return doc
}

func handleMeta(z *html.Tokenizer, doc *Doc) {
	var name, property, content, httpEquiv string
	for {
		k, v, more := z.TagAttr()
		switch strings.ToLower(string(k)) {
		case "name":
			name = strings.ToLower(string(v))
		case "property":
			property = strings.ToLower(string(v))
		case "content":
			content = string(v)
		case "http-equiv":
			httpEquiv = strings.ToLower(string(v))
		}
		if !more {
			break
		}
	}
	if content == "" {
		return
	}
	switch {
	case name == "description" && doc.Description == "":
		doc.Description = content
	case property == "og:description" && doc.Description == "":
		doc.Description = content
	case property == "og:title" && doc.Title == "":
		doc.Title = content
	case name == "robots" || name == "googlebot":
		lower := strings.ToLower(content)
		if strings.Contains(lower, "noindex") {
			doc.NoIndex = true
		}
		if strings.Contains(lower, "nofollow") {
			doc.NoFollow = true
		}
	case httpEquiv == "content-language" && doc.Lang == "":
		doc.Lang = normalizeLang(content)
	}
}

func handleLink(z *html.Tokenizer, doc *Doc, base *url.URL) {
	var rel, href string
	for {
		k, v, more := z.TagAttr()
		switch strings.ToLower(string(k)) {
		case "rel":
			rel = strings.ToLower(string(v))
		case "href":
			href = string(v)
		}
		if !more {
			break
		}
	}
	if rel == "canonical" && href != "" && doc.Canonical == "" {
		if abs, ok := urlx.Resolve(base, href); ok {
			doc.Canonical = abs
		}
	}
}

// attr scans the current tag's attributes for one key.
func attr(z *html.Tokenizer, want string) (string, bool) {
	for {
		k, v, more := z.TagAttr()
		if strings.EqualFold(string(k), want) {
			return string(v), true
		}
		if !more {
			return "", false
		}
	}
}

// squash collapses all runs of whitespace to single spaces and truncates on a
// rune boundary.
func squash(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	space := true // leading whitespace is dropped
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		if r < 0x20 {
			continue
		}
		space = false
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// normalizeLang reduces "en-GB, en" to "en".
func normalizeLang(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	if i := strings.IndexAny(v, ",;"); i >= 0 {
		v = v[:i]
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimSpace(v)
	if len(v) < 2 || len(v) > 3 {
		return ""
	}
	for _, r := range v {
		if r < 'a' || r > 'z' {
			return ""
		}
	}
	return v
}

// guessScript is a last-resort language hint based on the dominant script.
// It is only used when the page declares no language at all.
func guessScript(s string) string {
	if s == "" {
		return ""
	}
	var cyr, cjk, arab, deva, hang, thai, heb, greek, total int
	for i, r := range s {
		if i > 2000 {
			break
		}
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsDigit(r) {
			continue
		}
		total++
		switch {
		case r >= 0x0400 && r <= 0x04FF:
			cyr++
		case r >= 0x4E00 && r <= 0x9FFF:
			cjk++
		case r >= 0x0600 && r <= 0x06FF:
			arab++
		case r >= 0x0900 && r <= 0x097F:
			deva++
		case r >= 0xAC00 && r <= 0xD7AF:
			hang++
		case r >= 0x0E00 && r <= 0x0E7F:
			thai++
		case r >= 0x0590 && r <= 0x05FF:
			heb++
		case r >= 0x0370 && r <= 0x03FF:
			greek++
		}
	}
	if total < 20 {
		return ""
	}
	type cand struct {
		code string
		n    int
	}
	best := cand{"", 0}
	for _, c := range []cand{{"ru", cyr}, {"zh", cjk}, {"ar", arab}, {"hi", deva},
		{"ko", hang}, {"th", thai}, {"he", heb}, {"el", greek}} {
		if c.n > best.n {
			best = c
		}
	}
	if best.n*4 > total {
		return best.code
	}
	return "en"
}
