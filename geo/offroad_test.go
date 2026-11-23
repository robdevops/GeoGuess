package geo

import (
	"math"
	"testing"
)

// spot is where the off-road tests' photos are taken.
var spot = Point{-37.56, 143.85}

// at is the point east and north metres from spot.
func at(east, north float64) *osmPoint {
	return &osmPoint{
		Lat: spot.Lat + north/metresPerDegree,
		Lon: spot.Lon + east/(metresPerDegree*math.Cos(radians(spot.Lat))),
	}
}

// way is a way tagged k=v through pts, each [east, north] of spot.
func way(k, v string, pts ...[2]float64) osmElement {
	e := osmElement{Type: "way", Tags: map[string]string{k: v}}
	for _, p := range pts {
		e.Geometry = append(e.Geometry, at(p[0], p[1]))
	}
	return e
}

// eastWest is a line along north metres north of spot, 1 km long.
func eastWest(north float64) [][2]float64 {
	return [][2]float64{{-500, north}, {500, north}}
}

func TestOffroadClassify(t *testing.T) {
	sidewalk := way("highway", "footway", eastWest(-5)...)
	sidewalk.Tags["footway"] = "sidewalk"
	for _, tc := range []struct {
		name   string
		els    []osmElement
		inArea bool
		want   error
	}{
		{"on a road", []osmElement{way("highway", "residential", eastWest(10)...)}, false, RejectRoad},
		{"on a link", []osmElement{way("highway", "primary_link", eastWest(-35)...), way("highway", "path", eastWest(0)...)}, false, RejectRoad},
		{"footpath in a park 60 m from a road", []osmElement{way("highway", "tertiary", eastWest(60)...), way("highway", "footway", eastWest(5)...)}, true, nil},
		{"in a park 30 m from a road", []osmElement{way("highway", "secondary", eastWest(30)...)}, true, RejectRoad},
		{"in a park", nil, true, nil},
		{"on a track 45 m from a road", []osmElement{way("highway", "track", eastWest(3)...), way("highway", "service", eastWest(45)...)}, false, nil},
		{"near a path", []osmElement{way("highway", "path", eastWest(30)...)}, false, RejectNotOffroad},
		{"on a sidewalk", []osmElement{sidewalk, way("highway", "residential", eastWest(60)...)}, false, RejectNotOffroad},
		{"on a cycleway", []osmElement{way("highway", "cycleway", eastWest(2)...)}, false, nil},
		{"a building", []osmElement{way("building", "yes", eastWest(0)...)}, false, RejectNotOffroad},
		{"a count", []osmElement{{Type: "count", Tags: map[string]string{"highway": "residential"}}}, false, RejectNotOffroad},
		{"nothing nearby", nil, false, RejectNotOffroad},
	} {
		if got := sortElements(tc.els).offroad(spot, tc.inArea); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLineDistance(t *testing.T) {
	line := func(pts ...[2]float64) []Point {
		var out []Point
		for _, p := range pts {
			q := at(p[0], p[1])
			out = append(out, Point{q.Lat, q.Lon})
		}
		return out
	}
	for _, tc := range []struct {
		name string
		l    []Point
		want float64
	}{
		{"beside a segment", line([2]float64{-100, 50}, [2]float64{100, 50}), 50},
		{"past its end", line([2]float64{30, 0}, [2]float64{100, 0}), 30},
		{"a bend", line([2]float64{-100, 80}, [2]float64{0, 20}, [2]float64{100, 80}), 20},
		{"a node", line([2]float64{0, 12}), 12},
	} {
		if got := distance(spot, tc.l); math.Abs(got-tc.want) > 0.01 {
			t.Errorf("%s: %.3f m, want %.0f", tc.name, got, tc.want)
		}
	}
	if !math.IsInf(distance(spot, nil), 1) {
		t.Error("an empty line is near")
	}
	// Across the antimeridian, 0.001° of longitude at the equator is
	// about 111 m.
	if got := distance(Point{0, 179.9995}, []Point{{-1, -179.9995}, {1, -179.9995}}); math.Abs(got-111.2) > 0.5 {
		t.Errorf("across the antimeridian: %.1f m", got)
	}
}
