package geo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// overpassAnswer is Overpass's JSON answer holding the ways els, then a
// count of areas for each spot, n of them.
func overpassAnswer(t testing.TB, areas []int, els ...osmElement) []byte {
	t.Helper()
	for _, n := range areas {
		els = append(els, osmElement{Type: "count", Tags: map[string]string{"areas": strconv.Itoa(n), "total": strconv.Itoa(n)}})
	}
	b, err := json.Marshal(map[string]any{"version": 0.6, "elements": els})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOverpassOffroad(t *testing.T) {
	api := newFakeAPI(t)
	// A road by the first spot, which is in an area too (the road wins);
	// the second is in two areas, and the third in none.
	answer := overpassAnswer(t, []int{1, 2, 0}, way("highway", "residential", eastWest(10)...))
	status := http.StatusOK
	api.handle = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write(answer)
	}
	o := NewOverpass(api.URL+"/api/interpreter", api.Client())
	o.pace.every = 0
	ctx := context.Background()
	inPark := at(0, 500)
	pts := []Point{spot, {inPark.Lat, inPark.Lon}, {spot.Lat + 0.01, spot.Lon}}
	got, err := o.Offroad(ctx, pts)
	if err != nil || len(got) != 3 || got[0] != RejectRoad || got[1] != nil || got[2] != RejectNotOffroad {
		t.Fatalf("%v, %v", got, err)
	}
	req := api.requests()[0]
	if req.Method != http.MethodPost || req.URL.Path != "/api/interpreter" || req.Header.Get("User-Agent") != userAgent {
		t.Errorf("request %s %s, User-Agent %q", req.Method, req.URL.Path, req.Header.Get("User-Agent"))
	}
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	q := req.PostForm.Get("data")
	for _, want := range []string{"[out:json]", "[timeout:15]", ");\nout geom;\nis_in(-37.560000,143.850000)->.a;\n(area.a[boundary~\"^(national_park|protected_area)$\"];area.a[landuse~",
		"way(around:45,-37.560000,143.850000)[highway~", "way(around:25,-37.550000,143.850000)[highway~\"^(bridleway|cycleway|footway|path|steps|track)$\"]",
		"motorway_link", "busway", "area.a[leisure~\"^(common|garden|nature_reserve|park|recreation_ground)$\"];area.a[natural~"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q:\n%s", want, q)
		}
	}
	if strings.Count(q, "is_in(") != 3 || strings.Count(q, ");\nout count;\n") != 3 || !strings.HasSuffix(q, "out count;\n") {
		t.Errorf("query:\n%s", q)
	}

	// No spots, no query.
	if got, err := o.Offroad(ctx, nil); got != nil || err != nil || len(api.requests()) != 1 {
		t.Errorf("no spots: %v, %v", got, err)
	}
	// A query cut short is an error: a road may be missing.
	answer = []byte(`{"elements":[],"remark":"runtime error: Query timed out in \"query\" at line 3 after 16 seconds."}`)
	if _, err := o.Offroad(ctx, pts); err == nil || errors.Is(err, ErrSlowDown) || strings.Contains(err.Error(), "http") {
		t.Errorf("timed out: %v", err)
	}
	answer = []byte(`not json`)
	if _, err := o.Offroad(ctx, pts); err == nil {
		t.Error("unreadable answer read")
	}
	// An answer without a count for every spot is unreadable too.
	answer = overpassAnswer(t, []int{0, 0})
	if _, err := o.Offroad(ctx, pts); err == nil {
		t.Error("missing count read")
	}
	for code, want := range map[int]error{
		http.StatusTooManyRequests: ErrOverpassBusy, http.StatusForbidden: ErrOverpassBusy, http.StatusGatewayTimeout: ErrOverpassBusy,
		http.StatusBadRequest: StatusError{Source: "Overpass", Code: 400},
	} {
		status = code
		if _, err := o.Offroad(ctx, pts); err != want || (want == ErrOverpassBusy && !errors.Is(err, ErrSlowDown)) {
			t.Errorf("%d: %v", code, err)
		}
	}
}
