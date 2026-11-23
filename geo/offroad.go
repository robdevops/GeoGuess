package geo

import "math"

// The off-road check's distances, in metres: a spot is on a road if a
// drivable road is within roadClear of it, and on a path if a path or
// track is within pathNear.
const (
	roadClear = 40
	pathNear  = 20
)

// The OpenStreetMap tags the off-road check looks for, by key: the
// drivable roads, the ways off them, and the areas of parks, nature and
// countryside.
var (
	roadTypes = set("motorway", "trunk", "primary", "secondary", "tertiary", "unclassified",
		"residential", "living_street", "service", "road", "busway",
		"motorway_link", "trunk_link", "primary_link", "secondary_link", "tertiary_link")
	pathTypes = set("path", "footway", "track", "bridleway", "steps", "cycleway")
	areaTypes = map[string]map[string]bool{
		"leisure":  set("park", "nature_reserve", "garden", "common", "recreation_ground"),
		"natural":  set("wood", "scrub", "heath", "grassland", "beach", "sand", "bare_rock", "scree", "wetland"),
		"landuse":  set("forest", "meadow", "grass", "farmland", "vineyard", "orchard"),
		"boundary": set("national_park", "protected_area"),
	}
	// attached are the footway and cycleway values of a path that is part
	// of a road: a sidewalk, a crossing or a lane.
	attached = set("sidewalk", "crossing", "lane", "shared_lane", "share_busway")
)

// set is a set of strings.
func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

// osmElement is a way as Overpass gives it with "out geom": its tags and
// its nodes' positions; or a count, from "out count", whose tags hold the
// number of elements counted as "total".
type osmElement struct {
	Type     string            `json:"type"`
	Tags     map[string]string `json:"tags"`
	Geometry []*osmPoint       `json:"geometry"`
}

// osmPoint is a node's position; Overpass gives null for one it lacks.
type osmPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// line is a way's positions, skipping any missing.
func line(geom []*osmPoint) []Point {
	out := make([]Point, 0, len(geom))
	for _, g := range geom {
		if g != nil {
			out = append(out, Point{Lat: g.Lat, Lon: g.Lon})
		}
	}
	return out
}

// features are the roads and paths around some spots.
type features struct {
	roads, paths [][]Point
}

// sortElements files Overpass's ways as roads and paths by their tags;
// anything else is dropped.
func sortElements(els []osmElement) features {
	var f features
	for _, e := range els {
		if e.Type != "way" {
			continue
		}
		hw := e.Tags["highway"]
		switch {
		case roadTypes[hw]:
			f.roads = append(f.roads, line(e.Geometry))
		case pathTypes[hw] && !attached[e.Tags["footway"]] && !attached[e.Tags["cycleway"]]:
			f.paths = append(f.paths, line(e.Geometry))
		}
	}
	return f
}

// offroad is nil if p is off roads by f, else why not: RejectRoad if a
// drivable road is within roadClear, else RejectNotOffroad unless a path
// is within pathNear or p is in a park, nature or countryside area
// (inArea, which Overpass works out).
func (f features) offroad(p Point, inArea bool) error {
	near := func(lines [][]Point, metres float64) bool {
		for _, l := range lines {
			if distance(p, l) <= metres {
				return true
			}
		}
		return false
	}
	switch {
	case near(f.roads, roadClear):
		return RejectRoad
	case inArea || near(f.paths, pathNear):
		return nil
	}
	return RejectNotOffroad
}

// metresPerDegree is the length of a degree of latitude, on the sphere of
// earthRadius.
const metresPerDegree = earthRadius * 1000 * math.Pi / 180

// project is q in metres east and north of p, by an equirectangular
// projection at p's latitude: close enough within a few km.
func project(p, q Point) (x, y float64) {
	dLon := math.Mod(q.Lon-p.Lon+540, 360) - 180
	return dLon * metresPerDegree * math.Cos(radians(p.Lat)), (q.Lat - p.Lat) * metresPerDegree
}

// distance is how far p is from the polyline l, in metres; +Inf for an
// empty one.
func distance(p Point, l []Point) float64 {
	if len(l) == 1 {
		x, y := project(p, l[0])
		return math.Hypot(x, y)
	}
	best := math.Inf(1)
	for i := 0; i+1 < len(l); i++ {
		ax, ay := project(p, l[i])
		bx, by := project(p, l[i+1])
		best = min(best, segmentDistance(ax, ay, bx, by))
	}
	return best
}

// segmentDistance is how far the origin is from the segment a–b.
func segmentDistance(ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := 0.0
	if l2 := dx*dx + dy*dy; l2 > 0 {
		t = math.Max(0, math.Min(1, -(ax*dx+ay*dy)/l2))
	}
	return math.Hypot(ax+t*dx, ay+t*dy)
}
