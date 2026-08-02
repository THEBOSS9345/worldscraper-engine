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
	"shopping", "forum", "gov", "blog", "gaming", "finance", "health",
	"sports", "food", "travel", "weather", "jobs", "music", "crypto",
	"automotive", "adult", "other",
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
		hostBits: []string{"facebook", "instagram", "twitter", "linkedin", "pinterest", "snapchat", "threads.net", "mastodon", "bsky.app", "bluesky", "tumblr", "vk.com", "weibo", "telegram", "whatsapp", "discord"},
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
	{
		cat:      "gaming",
		hostBits: []string{"steam", "steampowered", "roblox", "epicgames", "playstation", "nintendo", "xbox", "ign.com", "pcgamer", "gamerant", "gamefaqs", "gog.com", "itch.io", "battle.net", "ea.com", "ubisoft", "capcom", "squareenix", "sega.com", "genshin", "fortnite", "minecraft", "wowhead", "dota", "ggtracker"},
		pathBits: []string{"/games/", "/game/", "/gaming/", "/walkthrough/", "/cheats/"},
		textBits: []string{"video game", "gameplay", "gaming", "esports", "playstation", "xbox", "game review"},
	},
	{
		cat:      "finance",
		hostBits: []string{"investopedia", "morningstar", "fidelity", "vanguard", "robinhood", "etrade", "charlesschwab", "tradingview", "investing.com", "marketwatch", "seekingalpha", "bankrate", "nerdwallet", "creditkarma", "mint.com", "stripe.com", "paypal", "wise.com", "revolut", "zillow", "redfin", "realtor.com", "mortgage"},
		pathBits: []string{"/stocks/", "/investing/", "/finance/", "/markets/", "/funds/", "/loans/"},
		textBits: []string{"stock market", "interest rate", "personal finance", "investing", "mortgage rates", "retirement", "savings account", "credit score"},
	},
	{
		cat:      "health",
		hostBits: []string{"mayoclinic", "webmd", "healthline", "medlineplus", "who.int", "nhs.uk", "hopkinsmedicine", "clevelandclinic", "medicalnewstoday", "verywellhealth", "everydayhealth", "health.com", "medscape", "neurosciencenews", "psypost"},
		pathBits: []string{"/health/", "/conditions/", "/symptoms/", "/medication/", "/wellness/"},
		textBits: []string{"healthy", "medical", "symptoms", "treatment", "health care", "wellness", "nutrition", "mental health"},
	},
	{
		cat:      "sports",
		hostBits: []string{"espn", "foxsports", "nbcsports", "skysports", "bleacherreport", "sportingnews", "theathletic", "espncricinfo", "fifa.com", "uefa.com", "nba.com", "nfl.com", "mlb.com", "nhl.com", "ncaa", "premierleague", "laliga", "formula1", "f1.com", "motorsport", "cricket", "goal.com", "dazn", "sports.yahoo"},
		pathBits: []string{"/sports/", "/football/", "/soccer/", "/matches/", "/fixtures/", "/scores/"},
		textBits: []string{"sports", "football", "basketball", "soccer", "champions league", "world cup", "olympic", "match report", "scoreboard"},
	},
	{
		cat:      "food",
		hostBits: []string{"foodnetwork", "allrecipes", "seriouseats", "bonappetit", "epicurious", "delish", "tasty.co", "kitchenstories", "jamesbeard", "food52", "saveur", "tasteofhome", "foodandwine", "simplyrecipes"},
		pathBits: []string{"/recipes/", "/recipe/", "/cooking/", "/kitchen/"},
		textBits: []string{"recipe", "cooking", "baking", "ingredients", "cuisine", "meal prep"},
	},
	{
		cat:      "travel",
		hostBits: []string{"tripadvisor", "booking.com", "expedia", "kayak", "airbnb", "skyscanner", "lonelyplanet", "rome2rio", "hopper", "travelocity", "priceline", "orbitz", "hostelworld", "getyourguide", "viator", "trip.com"},
		pathBits: []string{"/travel/", "/vacation/", "/itinerary/", "/flights/", "/hotels/"},
		textBits: []string{"travel guide", "vacation", "flight deals", "hotel deals", "tourist", "itinerary", "trip planning"},
	},
	{
		cat:      "weather",
		hostBits: []string{"weather.com", "accuweather", "wunderground", "metoffice", "openweathermap", "windy.com", "foreca", "darksky"},
		pathBits: []string{"/weather/", "/forecast/", "/radar/"},
		textBits: []string{"weather forecast", "temperature", "humidity", "rain forecast", "storm", "hurricane", "climate"},
	},
	{
		cat:      "jobs",
		hostBits: []string{"indeed.com", "glassdoor", "monster.com", "careerbuilder", "ziprecruiter", "dice.com", "lever.co", "greenhouse.io", "workday", "jobvite", "jobs.com"},
		pathBits: []string{"/jobs/", "/careers/", "/career/", "/vacancies/", "/openings/"},
		textBits: []string{"job posting", "job openings", "hiring", "career opportunities", "resume", "salary"},
	},
	{
		cat:      "music",
		hostBits: []string{"spotify", "soundcloud", "pandora", "deezer", "bandcamp", "genius.com", "allmusic", "last.fm", "musicbrainz", "rollingstone.com", "billboard.com", "nme.com", "pitchfork"},
		pathBits: []string{"/music/", "/artist/", "/album/", "/tracks/", "/lyrics/"},
		textBits: []string{"album", "song", "lyrics", "concert", "playlist", "music"},
	},
	{
		cat:      "crypto",
		hostBits: []string{"coinbase", "binance", "coinmarketcap", "coingecko", "ethereum", "bitcoin", "blockchain.com", "cryptocurrency", "ledger.com", "trezor", "uniswap", "metamask", "opensea", "bitcoin.org", "crypto"},
		pathBits: []string{"/crypto/", "/bitcoin/", "/ethereum/", "/nft/", "/wallets/", "/tokens/"},
		textBits: []string{"cryptocurrency", "bitcoin", "ethereum", "blockchain", "nft", "defi", "web3", "token"},
	},
	{
		cat:      "automotive",
		hostBits: []string{"caranddriver", "autoblog", "motortrend", "topspeed", "jalopnik", "edmunds", "kbb.com", "cargurus", "autotrader", "tesla.com", "honda.com", "toyota.com", "bmw", "mercedes", "ford.com", "chevrolet", "nissan", "hyundai", "kia.com", "porsche", "ferrari", "lamborghini", "carfax"},
		pathBits: []string{"/cars/", "/vehicles/", "/auto/", "/dealerships/"},
		textBits: []string{"car review", "car buying", "fuel economy", "horsepower", "electric vehicle", "test drive"},
	},
}

// Of returns the best-guess category for a document.
func Of(host, path, title, description string) string {
	h := strings.ToLower(host)
	p := strings.ToLower(path)
	text := strings.ToLower(title + " " + description)

	// x.com needs an exact-domain check: the bare substring "x.com" would also
	// match every host that merely ends in "x.com" (roblox.com, nix.com, …).
	if h == "x.com" || strings.HasSuffix(h, ".x.com") {
		return "social"
	}

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
