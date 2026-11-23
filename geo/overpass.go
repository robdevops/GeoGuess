package geo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// OverpassURL is OpenStreetMap's main public Overpass API, which asks for
// light use: GeoGuess sends it at most one query every two seconds.
const OverpassURL = "https://overpass-api.de/api/interpreter"

// The Overpass query's limits: the spots looked up in one query, the
// server's time and memory for it, and the most of its answer read.
const (
	overpassBatch   = 25
	overpassTimeout = 15 // seconds
	overpassMaxSize = 64 << 20
	overpassLimit   = 4 << 20
)

// Overpass asks OpenStreetMap's Overpass API what is around photos' spots,
// to tell which were taken off roads, at most once every two seconds.
type Overpass struct {
	url    string
	client *http.Client
	pace   *pacer
}

// NewOverpass is a client for the Overpass interpreter at u (OverpassURL,
// or another instance's).
func NewOverpass(u string, client *http.Client) *Overpass {
	return &Overpass{url: u, client: client, pace: &pacer{every: 2 * time.Second}}
}

// Offroad looks up every spot in pts in one query and returns, for each,
// nil if it is off roads, RejectRoad if a drivable road is within 40 m,
// or RejectNotOffroad if it is neither on a path nor in a park, nature or
// countryside. Only the spots' coordinates are sent. A failed query is
// an error, ErrOverpassBusy (an ErrSlowDown) if Overpass is busy or
// limits or blocks it.
func (o *Overpass) Offroad(ctx context.Context, pts []Point) ([]error, error) {
	if len(pts) == 0 {
		return nil, nil
	}
	if err := o.pace.wait(ctx); err != nil {
		return nil, err
	}
	var a struct {
		Remark   string       `json:"remark"`
		Elements []osmElement `json:"elements"`
	}
	r := request{client: o.client, source: "Overpass", url: o.url, form: url.Values{"data": {overpassQuery(pts)}}, limit: overpassLimit}
	if err := r.getJSON(ctx, &a); err != nil {
		if se := (StatusError{}); errors.As(err, &se) {
			switch se.Code {
			case http.StatusTooManyRequests, http.StatusForbidden, http.StatusGatewayTimeout:
				return nil, ErrOverpassBusy // busy, limited or blocked: stop searching for now
			}
		}
		return nil, err
	}
	if strings.Contains(a.Remark, "error") {
		// A query cut short by the server's timeout or memory limit: its
		// answer is partial, so a road may be missing.
		return nil, fmt.Errorf("Overpass: query failed")
	}
	var ways []osmElement
	var inArea []bool
	for _, e := range a.Elements {
		if e.Type == "count" {
			inArea = append(inArea, e.Tags["total"] != "0")
		} else {
			ways = append(ways, e)
		}
	}
	if len(inArea) != len(pts) {
		return nil, fmt.Errorf("Overpass: unreadable answer")
	}
	f := sortElements(ways)
	out := make([]error, len(pts))
	for i, p := range pts {
		out[i] = f.offroad(p, inArea[i])
	}
	return out, nil
}

// overpassQuery is the query for the roads and paths near each of pts,
// with their geometry, and then, for each of pts in turn, a count of the
// park, nature and countryside areas it is in. Overpass's around is a
// little wider than the checks, which measure the distances themselves;
// the areas are counted, not drawn, as a national park's outline alone
// can run to megabytes.
func overpassQuery(pts []Point) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[out:json][timeout:%d][maxsize:%d];\n(\n", overpassTimeout, overpassMaxSize)
	roads := strings.Join(keys(roadTypes), "|")
	paths := strings.Join(keys(pathTypes), "|")
	for _, p := range pts {
		at := coord(p.Lat) + "," + coord(p.Lon)
		fmt.Fprintf(&b, "way(around:%d,%s)[highway~\"^(%s)$\"];\n", roadClear+5, at, roads)
		fmt.Fprintf(&b, "way(around:%d,%s)[highway~\"^(%s)$\"];\n", pathNear+5, at, paths)
	}
	b.WriteString(");\nout geom;\n")
	var areas strings.Builder
	for _, k := range keys(areaTypes) {
		fmt.Fprintf(&areas, "area.a[%s~\"^(%s)$\"];", k, strings.Join(keys(areaTypes[k]), "|"))
	}
	for _, p := range pts {
		fmt.Fprintf(&b, "is_in(%s,%s)->.a;\n(%s);\nout count;\n", coord(p.Lat), coord(p.Lon), areas.String())
	}
	return b.String()
}

// keys is m's keys in order, for a query that is the same each time.
func keys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
