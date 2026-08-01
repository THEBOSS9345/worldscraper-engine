// Package geoip resolves a server's IP address to a real location.
//
// The database is optional and never bundled: MaxMind's GeoLite2 files carry
// their own licence, so the user supplies one. When no database is present the
// crawler runs exactly as before and the dashboard falls back to showing hosts
// at an approximate, non-geographic position.
package geoip

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"sync"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Filenames accepted in the data directory, in priority order. City databases
// come first because they carry coordinates as well as country names.
var candidates = []string{
	"geoip.mmdb",
	"GeoLite2-City.mmdb",
	"GeoIP2-City.mmdb",
	"dbip-city-lite.mmdb",
	"GeoLite2-Country.mmdb",
	"GeoIP2-Country.mmdb",
	"dbip-country-lite.mmdb",
}

// Location is what the dashboard needs to place a host on the globe.
type Location struct {
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Country string  `json:"country"` // ISO code, e.g. "DE"
	City    string  `json:"city,omitempty"`
	Exact   bool    `json:"exact"` // true when coordinates came from the database
}

// mmdbRecord is the subset of the MaxMind schema we read.
type mmdbRecord struct {
	Location struct {
		Latitude  float64 `maxminddb:"latitude"`
		Longitude float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
}

// DB is a thread-safe lookup handle. A nil *DB is valid and always misses,
// which keeps the call sites free of conditionals.
type DB struct {
	reader *maxminddb.Reader
	path   string

	mu    sync.RWMutex
	cache map[string]Location // keyed by host, not IP: hosts repeat constantly
}

// Open looks for a database in dir. A missing file is not an error — it
// returns (nil, nil) and the caller carries on without geolocation.
func Open(dir string) (*DB, error) {
	for _, name := range candidates {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		reader, err := maxminddb.Open(path)
		if err != nil {
			return nil, err
		}
		return &DB{
			reader: reader,
			path:   path,
			cache:  make(map[string]Location, 4096),
		}, nil
	}
	return nil, nil
}

// Close releases the database.
func (d *DB) Close() error {
	if d == nil || d.reader == nil {
		return nil
	}
	return d.reader.Close()
}

// Path is the file in use, for display in the UI.
func (d *DB) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// Available reports whether lookups can succeed.
func (d *DB) Available() bool { return d != nil && d.reader != nil }

// Lookup resolves an IP to a location, caching by host so that repeated hits on
// the same site skip the database entirely.
func (d *DB) Lookup(host, ip string) (Location, bool) {
	if !d.Available() || ip == "" {
		return Location{}, false
	}

	if host != "" {
		d.mu.RLock()
		loc, ok := d.cache[host]
		d.mu.RUnlock()
		if ok {
			return loc, loc.Country != "" || loc.Exact
		}
	}

	loc, err := d.lookupIP(ip)
	if err != nil {
		return Location{}, false
	}

	if host != "" {
		d.mu.Lock()
		// Bound the cache; a broad crawl sees millions of hosts.
		if len(d.cache) > 200_000 {
			d.cache = make(map[string]Location, 4096)
		}
		d.cache[host] = loc
		d.mu.Unlock()
	}
	return loc, loc.Country != "" || loc.Exact
}

func (d *DB) lookupIP(ip string) (Location, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return Location{}, err
	}
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() {
		return Location{}, errors.New("non-routable address")
	}

	var rec mmdbRecord
	if err := d.reader.Lookup(addr).Decode(&rec); err != nil {
		return Location{}, err
	}

	loc := Location{
		Lat:     rec.Location.Latitude,
		Lon:     rec.Location.Longitude,
		Country: rec.Country.ISOCode,
	}
	if loc.Country == "" {
		loc.Country = rec.RegisteredCountry.ISOCode
	}
	if n, ok := rec.City.Names["en"]; ok {
		loc.City = n
	}

	// A country-only database has no coordinates; fall back to the country
	// centroid so the globe still places the host truthfully, if coarsely.
	if loc.Lat == 0 && loc.Lon == 0 {
		if c, ok := countryCentroid[loc.Country]; ok {
			loc.Lat, loc.Lon = c[0], c[1]
		}
	} else {
		loc.Exact = true
	}

	return loc, nil
}
