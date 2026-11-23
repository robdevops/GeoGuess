package geo

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NominatimURL is OpenStreetMap's public Nominatim, whose policy allows at
// most one request a second, with a User-Agent that names the program.
const NominatimURL = "https://nominatim.openstreetmap.org"

// Place is where a photo was taken, as the reveal names it.
type Place struct {
	Name        string // "Ballarat, Victoria, Australia"
	Country     string
	CountryCode string // ISO 3166-1 alpha-2, upper case
}

// Nominatim names places by reverse geocoding, at most once a second.
type Nominatim struct {
	url    string
	client *http.Client
	pace   *pacer
}

// NewNominatim is a client for the Nominatim at base (NominatimURL, or a
// self-hosted one).
func NewNominatim(base string, client *http.Client) *Nominatim {
	return &Nominatim{url: strings.TrimRight(base, "/"), client: client, pace: &pacer{every: time.Second}}
}

// Reverse names the town, region and country at p. A place with no country,
// such as the open sea, is RejectNoPlace.
func (n *Nominatim) Reverse(ctx context.Context, p Point) (Place, error) {
	if err := n.pace.wait(ctx); err != nil {
		return Place{}, err
	}
	q := url.Values{
		"format": {"jsonv2"}, "lat": {coord(p.Lat)}, "lon": {coord(p.Lon)},
		"zoom": {"10"}, "addressdetails": {"1"}, "accept-language": {"en"},
	}
	var a struct {
		Error   string            `json:"error"`
		Address map[string]string `json:"address"`
	}
	r := request{client: n.client, source: "Nominatim", url: n.url + "/reverse?" + q.Encode(), limit: 1 << 20}
	if err := r.getJSON(ctx, &a); err != nil {
		return Place{}, err
	}
	country := a.Address["country"]
	if a.Error != "" || country == "" {
		return Place{}, RejectNoPlace
	}
	parts := []string{}
	add := func(s string) {
		if s != "" && (len(parts) == 0 || parts[len(parts)-1] != s) {
			parts = append(parts, s)
		}
	}
	add(first(a.Address, "city", "town", "village", "municipality", "hamlet", "suburb", "county"))
	add(first(a.Address, "state", "region", "province", "state_district"))
	add(country)
	return Place{
		Name:        strings.Join(parts, ", "),
		Country:     country,
		CountryCode: strings.ToUpper(a.Address["country_code"]),
	}, nil
}

// first is the first of keys that address has.
func first(address map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := address[k]; v != "" {
			return v
		}
	}
	return ""
}

// offroadTypes are the OpenStreetMap features a photo off roads is taken
// on or in, as Nominatim's zoom-17 reverse lookup names them: by category,
// the types that count, or nil for every type. Anything else, such as a
// road, a building, a shop or an address, is on a road.
var offroadTypes = map[string]map[string]bool{
	"highway":       set("path", "footway", "cycleway", "track", "bridleway", "steps"),
	"natural":       nil,
	"waterway":      set("river", "stream", "waterfall", "rapids"),
	"mountain_pass": nil,
	"leisure":       set("park", "nature_reserve", "garden", "common", "recreation_ground", "golf_course", "beach_resort"),
	"boundary":      set("national_park", "protected_area"),
	"landuse":       set("forest", "meadow", "grass", "farmland", "vineyard", "orchard", "allotments", "recreation_ground"),
	"tourism":       set("viewpoint", "picnic_site", "camp_site", "alpine_hut", "wilderness_hut"),
	"place":         set("locality", "islet", "island"),
}

// set is a set of strings.
func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

// isOffroad reports whether a feature of category and type is off roads,
// by offroadTypes.
func isOffroad(category, typ string) bool {
	types, ok := offroadTypes[category]
	return ok && (types == nil || types[typ])
}

// Offroad reports whether p is off roads: on a path or track, or in a
// park, nature, a beach or the countryside, by the feature Nominatim finds
// there at zoom 17. It shares Reverse's pace. A place Nominatim can't
// name is not off roads; only a failed request is an error, ErrSlowDown if
// Nominatim rate-limits or blocks it.
func (n *Nominatim) Offroad(ctx context.Context, p Point) (bool, error) {
	if err := n.pace.wait(ctx); err != nil {
		return false, err
	}
	q := url.Values{
		"format": {"jsonv2"}, "lat": {coord(p.Lat)}, "lon": {coord(p.Lon)},
		"zoom": {"17"}, "addressdetails": {"0"},
	}
	var a struct {
		Error    string `json:"error"`
		Category string `json:"category"`
		Type     string `json:"type"`
	}
	r := request{client: n.client, source: "Nominatim", url: n.url + "/reverse?" + q.Encode(), limit: 1 << 20}
	if err := r.getJSON(ctx, &a); err != nil {
		if se := (StatusError{}); errors.As(err, &se) && (se.Code == http.StatusTooManyRequests || se.Code == http.StatusForbidden) {
			return false, ErrSlowDown // limited or blocked: stop searching for now
		}
		return false, err
	}
	return a.Error == "" && isOffroad(a.Category, a.Type), nil
}
