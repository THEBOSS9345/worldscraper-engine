// Package classify assigns a coarse topical category to a crawled document.
//
// Classification is heuristic (host name, TLD, path and title keywords). It is
// used purely for dashboard breakdowns and for the UI's optional content
// filter — it is not a moderation system.
package classify

import "strings"

// Categories is the canonical list, in dashboard display order.
var Categories = []string{
	"news", "docs", "academic", "code", "wiki", "social", "video",
	"shopping", "forum", "gov", "blog", "adult", "other",
}

type rule struct {
	cat      string
	hostBits []string
	pathBits []string
	textBits []string
}

// Ordered by precedence: the first rule that matches wins.
var rules = []rule{
	{
		cat:      "adult",
		hostBits: []string{"porn", "xvideo", "xnxx", "xhamster", "redtube", "youporn", "brazzers", "onlyfans", "chaturbate", "camsoda", "stripchat", "nsfw", "hentai", "rule34", "e-hentai", "nhentai", "fapel", "spankbang", "eporner", "tnaflix", "motherless", "adultfriend", "escort", "livejasmin", "bongacams", "erome", "sex.com"},
		pathBits: []string{"/porn/", "/xxx/", "/nsfw/", "/hentai/"},
		textBits: []string{"free porn", "xxx videos", "sex videos", "adult videos", "live sex cams"},
	},
	{
		cat:      "gov",
		hostBits: []string{".gov", ".gov.", ".mil", "europa.eu", "parliament.", "senate.", "whitehouse."},
	},
	{
		cat:      "academic",
		hostBits: []string{".edu", ".ac.", "arxiv.", "pubmed", "ncbi.nlm", "sciencedirect", "springer", "jstor", "researchgate", "semanticscholar", "biorxiv", "ssrn", "nature.com", "science.org", "ieee.org", "acm.org", "scholar."},
	},
	{
		cat:      "code",
		hostBits: []string{"github", "gitlab", "bitbucket", "sourceforge", "npmjs", "pypi", "crates.io", "pkg.go.dev", "rubygems", "packagist", "nuget", "hex.pm", "codeberg", "gitea", "godbolt", "replit", "codepen", "jsfiddle"},
	},
	{
		cat:      "docs",
		hostBits: []string{"docs.", "developer.", "devdocs", "readthedocs", "mdn", "developer.mozilla", "learn.microsoft", "docs.rs", "man7.org", "w3.org", "rfc-editor", "ietf.org", "swagger", "apidocs", "documentation."},
		pathBits: []string{"/docs/", "/documentation/", "/api/reference", "/manual/", "/guide/", "/reference/", "/handbook/"},
	},
	{
		cat:      "wiki",
		hostBits: []string{"wikipedia", "wikimedia", "wiktionary", "wikidata", "wikihow", "fandom.com", "wikia", "britannica", "wikivoyage", "wikiquote", "wikisource"},
		pathBits: []string{"/wiki/"},
	},
	{
		cat:      "news",
		hostBits: []string{"news", "bbc.", "cnn.", "nytimes", "reuters", "guardian", "washingtonpost", "aljazeera", "bloomberg", "forbes", "wsj.com", "apnews", "npr.org", "ft.com", "economist", "cnbc", "abcnews", "nbcnews", "cbsnews", "foxnews", "dw.com", "france24", "thehindu", "timesofindia", "indianexpress", "scmp.com", "japantimes", "techcrunch", "theverge", "arstechnica", "engadget", "wired.com", "zdnet", "vice.com", "politico", "axios", "hindustantimes", "ndtv"},
		pathBits: []string{"/news/", "/article/", "/story/", "/politics/"},
	},
	{
		cat:      "video",
		hostBits: []string{"youtube", "youtu.be", "vimeo", "twitch", "dailymotion", "netflix", "hulu", "disneyplus", "primevideo", "rumble.com", "bitchute", "odysee", "tiktok"},
		pathBits: []string{"/watch", "/video/"},
	},
	{
		cat:      "social",
		hostBits: []string{"facebook", "instagram", "twitter", "x.com", "linkedin", "pinterest", "snapchat", "threads.net", "mastodon", "bsky.app", "bluesky", "tumblr", "vk.com", "weibo", "telegram", "whatsapp", "discord"},
	},
	{
		cat:      "forum",
		hostBits: []string{"reddit", "stackoverflow", "stackexchange", "superuser", "serverfault", "askubuntu", "quora", "hackernews", "news.ycombinator", "4chan", "discourse", "forum", "phpbb", "vbulletin", "xda-developers"},
		pathBits: []string{"/forum/", "/thread/", "/topic/", "/questions/", "/r/"},
	},
	{
		cat:      "shopping",
		hostBits: []string{"amazon.", "ebay", "aliexpress", "alibaba", "etsy", "walmart", "target.com", "bestbuy", "flipkart", "shopify", "shein", "temu", "wayfair", "ikea", "costco", "newegg", "rakuten", "mercadolibre", "myntra", "ajio"},
		pathBits: []string{"/product/", "/shop/", "/cart", "/checkout", "/dp/", "/store/"},
	},
	{
		cat:      "blog",
		hostBits: []string{"blog", "medium.com", "substack", "wordpress", "blogspot", "ghost.io", "dev.to", "hashnode", "svbtle", "write.as"},
		pathBits: []string{"/blog/", "/posts/", "/post/"},
	},
}

// Of returns the best-guess category for a document.
func Of(host, path, title, description string) string {
	h := strings.ToLower(host)
	p := strings.ToLower(path)
	text := strings.ToLower(title + " " + description)

	for _, r := range rules {
		for _, bit := range r.hostBits {
			if strings.Contains(h, bit) {
				return r.cat
			}
		}
		for _, bit := range r.pathBits {
			if strings.Contains(p, bit) {
				return r.cat
			}
		}
		for _, bit := range r.textBits {
			if strings.Contains(text, bit) {
				return r.cat
			}
		}
	}
	return "other"
}

// HostLooksAdult reports whether a hostname alone is enough to flag adult
// content. Used to skip fetching entirely when the user disables adult crawling.
func HostLooksAdult(host string) bool {
	h := strings.ToLower(host)
	for _, bit := range rules[0].hostBits {
		if strings.Contains(h, bit) {
			return true
		}
	}
	return strings.HasSuffix(h, ".xxx") || strings.HasSuffix(h, ".adult") || strings.HasSuffix(h, ".porn") || strings.HasSuffix(h, ".sex")
}
