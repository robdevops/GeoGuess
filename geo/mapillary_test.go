package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mapillaryPool is every image in the Mapillary fixtures, by ID, as the
// details call answers them.
func mapillaryPool(t *testing.T, base string) map[string]json.RawMessage {
	t.Helper()
	pool := map[string]json.RawMessage{}
	for _, name := range []string{"mapillary_bbox.json", "mapillary_radius.json"} {
		var a struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(fixture(t, name, base), &a); err != nil {
			t.Fatal(err)
		}
		for _, raw := range a.Data {
			var im struct {
				ID string `json:"id"`
			}
			json.Unmarshal(raw, &im)
			pool[im.ID] = raw
		}
	}
	return pool
}

// fixtureImages is a Mapillary fixture's images, parsed as a search used
// to parse them, by ID.
func fixtureImages(t *testing.T, name, base string) map[string]mapillaryImage {
	t.Helper()
	var a mapillaryAnswer
	if err := json.Unmarshal(fixture(t, name, base), &a); err != nil {
		t.Fatal(err)
	}
	out := map[string]mapillaryImage{}
	for _, im := range a.Data {
		out[im.ID] = im
	}
	return out
}

// detailsAnswer is the details call's answer for r's IDs from pool, less
// any in skip.
func detailsAnswer(r *http.Request, pool map[string]json.RawMessage, skip ...string) []byte {
	out := map[string]json.RawMessage{}
	for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if raw, ok := pool[id]; ok && !slices.Contains(skip, id) {
			out[id] = raw
		}
	}
	data, _ := json.Marshal(out)
	return data
}

// syntheticImage is a full image in sequence seq, at noon in Victoria.
func syntheticImage(id, seq string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%q,"computed_geometry":{"type":"Point","coordinates":[143.86,-37.56]},`+
		`"captured_at":1734747540000,"camera_type":"perspective","quality_score":0.8,"width":2048,"height":1536,`+
		`"sequence":%q,"creator":{"username":"alice"},"thumb_2048_url":"x"}`, id, seq))
}

func TestMapillaryCandidates(t *testing.T) {
	api := newFakeAPI(t)
	pool := mapillaryPool(t, api.URL)
	api.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/images" {
			w.Write(fixture(t, "mapillary_bbox.json", api.URL))
			return
		}
		w.Write(detailsAnswer(r, pool))
	}
	m := NewMapillary(api.URL, "MLY|test", api.Client())
	cands, err := m.Candidates(context.Background(), testBox)
	if err != nil {
		t.Fatal(err)
	}
	// 8 images in 2 sequences: one from each.
	if len(cands) != 2 || cands[0].Sequence == cands[1].Sequence {
		t.Fatalf("candidates %+v", cands)
	}
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	for _, req := range reqs {
		if got := req.Header.Get("Authorization"); got != "OAuth MLY|test" {
			t.Errorf("Authorization %q", got)
		}
		if req.Header.Get("User-Agent") != userAgent {
			t.Errorf("User-Agent %q", req.Header.Get("User-Agent"))
		}
	}
	q := reqs[0].URL.Query()
	if reqs[0].URL.Path != "/images" || q.Get("bbox") != "143.850000,-37.570000,143.870000,-37.550000" || q.Get("limit") != "1000" || q.Get("fields") != "id,sequence,computed_geometry" {
		t.Errorf("survey %s %v", reqs[0].URL.Path, q)
	}
	q = reqs[1].URL.Query()
	ids := strings.Split(q.Get("ids"), ",")
	slices.Sort(ids)
	want := []string{cands[0].ID, cands[1].ID}
	slices.Sort(want)
	if reqs[1].URL.Path != "/" || !slices.Equal(ids, want) || q.Get("fields") != mapillaryFields {
		t.Errorf("details %s %v", reqs[1].URL.Path, q)
	}
	// Each candidate is its image as a search used to parse it.
	old := fixtureImages(t, "mapillary_bbox.json", api.URL)
	for _, c := range cands {
		if w, ok := old[c.ID].candidate(); !ok || c != w {
			t.Errorf("candidate\n%+v\nwant\n%+v", c, w)
		}
	}
}

func TestMapillaryDetails(t *testing.T) {
	api := newFakeAPI(t)
	pool := mapillaryPool(t, api.URL)
	api.handle = func(w http.ResponseWriter, r *http.Request) { w.Write(detailsAnswer(r, pool)) }
	m := NewMapillary(api.URL, "MLY|test", api.Client())
	details, err := m.details(context.Background(), []string{"156004253082834", "1197032491288598", "999"})
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 2 {
		t.Fatalf("%d images", len(details))
	}
	flat, _ := details["156004253082834"].candidate()
	pano, _ := details["1197032491288598"].candidate()
	want := Candidate{
		ID: "156004253082834", Source: "Mapillary", Creator: "alice", Licence: "CC BY-SA 4.0",
		Link:     "https://www.mapillary.com/app/?pKey=156004253082834",
		Captured: time.UnixMilli(1566687341409).UTC(), Fisheye: true, Quality: 0.612,
		Width: 3840, Height: 2160, Sequence: "xb2e6e9trvde7vkhc65y3i", At: flat.At, Raw: flat.Raw,
		image: api.URL + "/cdn/156004253082834.jpg",
	}
	if flat != want {
		t.Errorf("candidate\n%+v\nwant\n%+v", flat, want)
	}
	if flat.At == flat.Raw || Km(flat.At, flat.Raw) > 0.05 || flat.At.Lat > -37.5 {
		t.Errorf("positions %v (computed) and %v (raw)", flat.At, flat.Raw)
	}
	if !pano.Pano || pano.Fisheye || pano.Creator != "bob" || pano.Quality != 0.775 {
		t.Errorf("panorama %+v", pano)
	}
	old := fixtureImages(t, "mapillary_bbox.json", api.URL)
	for id, im := range details {
		got, _ := im.candidate()
		if w, _ := old[id].candidate(); got != w {
			t.Errorf("%s parsed\n%+v\nwant\n%+v", id, got, w)
		}
	}
}

func TestMapillaryOnePerSequence(t *testing.T) {
	api := newFakeAPI(t)
	pool := map[string]json.RawMessage{}
	var survey []map[string]string
	for s := range 3 {
		for i := range 100 {
			id, seq := fmt.Sprint(1000*(s+1)+i), fmt.Sprint("seq", s)
			pool[id] = syntheticImage(id, seq)
			survey = append(survey, map[string]string{"id": id, "sequence": seq})
		}
	}
	body, _ := json.Marshal(map[string]any{"data": survey})
	api.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/images" {
			w.Write(body)
			return
		}
		w.Write(detailsAnswer(r, pool))
	}
	m := NewMapillary(api.URL, "MLY|test", api.Client())
	firsts := 0
	for range 10 {
		cands, err := m.Candidates(context.Background(), testBox)
		if err != nil {
			t.Fatal(err)
		}
		seqs := map[string]bool{}
		for _, c := range cands {
			seqs[c.Sequence] = true
			if c.ID == "1000" || c.ID == "2000" || c.ID == "3000" {
				firsts++
			}
			if c.Captured.IsZero() || c.Quality != 0.8 || c.At != (Point{-37.56, 143.86}) {
				t.Errorf("candidate %+v", c)
			}
		}
		if len(cands) != 3 || len(seqs) != 3 {
			t.Fatalf("%d candidates from %d sequences", len(cands), len(seqs))
		}
	}
	if firsts == 30 {
		t.Error("always picked each sequence's first image")
	}

	// At most mapillaryPicks sequences, in one details call.
	survey = nil
	for i := range 60 {
		id := fmt.Sprint(i + 1)
		pool[id] = syntheticImage(id, "s"+id)
		survey = append(survey, map[string]string{"id": id, "sequence": "s" + id})
	}
	body, _ = json.Marshal(map[string]any{"data": survey})
	before := len(api.requests())
	cands, err := m.Candidates(context.Background(), testBox)
	if err != nil || len(cands) != mapillaryPicks {
		t.Fatalf("%d candidates, %v", len(cands), err)
	}
	reqs := api.requests()[before:]
	if len(reqs) != 2 || len(strings.Split(reqs[1].URL.Query().Get("ids"), ",")) != mapillaryPicks {
		t.Errorf("%d requests", len(reqs))
	}
	// The picks are shuffled: not always the survey's first 40, in order.
	inOrder := 0
	for range 5 {
		cands, err := m.Candidates(context.Background(), testBox)
		if err != nil || len(cands) != mapillaryPicks {
			t.Fatalf("%d candidates, %v", len(cands), err)
		}
		ordered := true
		for i, c := range cands {
			ordered = ordered && c.ID == fmt.Sprint(i+1)
		}
		if ordered {
			inOrder++
		}
	}
	if inOrder == 5 {
		t.Error("picks not shuffled")
	}
	// Picks over a batch are asked for in batches: 15, 15 and 10.
	m.batch = 15
	before = len(api.requests())
	cands, err = m.Candidates(context.Background(), testBox)
	if err != nil || len(cands) != mapillaryPicks {
		t.Fatalf("%d candidates, %v", len(cands), err)
	}
	reqs = api.requests()[before:]
	seen := map[string]bool{}
	var sizes []int
	for _, r := range reqs[1:] {
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		sizes = append(sizes, len(ids))
		for _, id := range ids {
			seen[id] = true
		}
	}
	if !slices.Equal(sizes, []int{15, 15, 10}) || len(seen) != mapillaryPicks {
		t.Errorf("details batches %v, %d ids", sizes, len(seen))
	}
}

func TestPickPerSequence(t *testing.T) {
	images := []mapillaryImage{{ID: "a1", Sequence: "a"}, {ID: "a2", Sequence: "a"}, {ID: "x"}, {ID: "y"}, {Sequence: "b"}, {ID: "b1", Sequence: "b"}}
	got := pickPerSequence(images, 10)
	slices.Sort(got)
	if len(got) != 4 || !slices.Contains(got, "x") || !slices.Contains(got, "y") || !slices.Contains(got, "b1") || (!slices.Contains(got, "a1") && !slices.Contains(got, "a2")) {
		t.Errorf("picked %v", got)
	}
	if got := pickPerSequence(images, 2); len(got) != 2 {
		t.Errorf("picked %v", got)
	}
	if got := pickPerSequence(nil, 2); len(got) != 0 {
		t.Errorf("picked %v", got)
	}
}

func TestMapillaryRetries(t *testing.T) {
	big := Around(Point{Lat: -37.81, Lon: 144.96}) // 0.02° each way
	for _, tc := range []struct {
		name    string
		box     BBox
		answers []string // status and body, by request: a fixture, "details", or "details-1" leaving out 1197032491288598 and 1011962813864670
		want    int      // candidates
		calls   int
		halves  []float64 // each survey's half-size, if checked
		err     error
	}{
		{"empty is asked again", testBox, []string{`200 {"data":[]}`, "200 mapillary_bbox.json", "200 details"}, 2, 3, nil, nil},
		{"empty twice", testBox, []string{`200 {"data":[]}`, `200 {"data":[]}`}, 0, 2, nil, nil},
		{"too dense falls back to a quarter", big, []string{"500 mapillary_error.json", "200 mapillary_radius.json", "200 details"}, 1, 3, []float64{0.02, 0.01}, nil},
		{"too dense twice", big, []string{"500 mapillary_error.json", "502 x", "200 mapillary_bbox.json", "200 details"}, 2, 4, []float64{0.02, 0.01, 0.005}, nil},
		{"too dense down to the smallest", big, []string{"500 mapillary_error.json"}, 0, 4, []float64{0.02, 0.01, 0.005, 0.0025},
			StatusError{Source: "Mapillary", Code: 500}},
		{"too dense from a smaller box", testBox, []string{"500 mapillary_error.json"}, 0, 3, []float64{0.01, 0.005, 0.0025},
			StatusError{Source: "Mapillary", Code: 500}},
		{"timed out", big, []string{"200 slow", "200 mapillary_bbox.json", "200 details"}, 2, 3, []float64{0.02, 0.01}, nil},
		{"timed out down to the smallest", big, []string{"200 slow"}, 0, 4, nil, errSurveyTimeout},
		{"missing details are skipped", testBox, []string{"200 mapillary_bbox.json", "200 details-1"}, 1, 2, nil, nil},
		{"details fail", testBox, []string{"200 mapillary_bbox.json", "500 x"}, 0, 2, nil, StatusError{Source: "Mapillary", Code: 500}},
		{"rate limited", big, []string{`429 {"error":{"code":4,"message":"limit"}}`}, 0, 1, nil, ErrSlowDown},
		{"rate limited without a code", big, []string{`429 x`}, 0, 1, nil, ErrSlowDown},
		{"forbidden", big, []string{`403 x`}, 0, 1, nil, ErrSlowDown},
		{"rate limited as a 400", big, []string{`400 {"error":{"code":4,"message":"limit"}}`}, 0, 1, nil, ErrSlowDown},
		{"details rate limited", testBox, []string{"200 mapillary_bbox.json", "429 x"}, 0, 2, nil, ErrSlowDown},
		{"refused", big, []string{`401 {"error":{"code":190}}`}, 0, 1, nil, StatusError{Source: "Mapillary", Code: 401}},
		{"bad request", big, []string{`400 {"error":{"code":100}}`}, 0, 1, nil, StatusError{Source: "Mapillary", Code: 400}},
		{"unreadable", testBox, []string{`200 <html>`}, 0, 1, nil, errors.New("Mapillary: unreadable answer")},
		{"unreadable details", testBox, []string{"200 mapillary_bbox.json", `200 {"error":{"code":1}}`}, 0, 2, nil, errors.New("Mapillary: unreadable answer")},
	} {
		api := newFakeAPI(t)
		pool := mapillaryPool(t, api.URL)
		var n atomic.Int32
		api.handle = func(w http.ResponseWriter, r *http.Request) {
			i := int(n.Add(1)) - 1
			status, body, _ := strings.Cut(tc.answers[min(i, len(tc.answers)-1)], " ")
			switch {
			case body == "slow":
				select {
				case <-r.Context().Done():
				case <-time.After(time.Second):
				}
				return
			case body == "details":
				body = string(detailsAnswer(r, pool))
			case body == "details-1":
				body = string(detailsAnswer(r, pool, "1197032491288598", "1011962813864670"))
			case strings.HasSuffix(body, ".json"):
				body = string(fixture(t, body, api.URL))
			}
			code := map[string]int{"200": 200, "400": 400, "401": 401, "403": 403, "429": 429, "500": 500, "502": 502}[status]
			w.WriteHeader(code)
			w.Write([]byte(body))
		}
		m := NewMapillary(api.URL, "MLY|test", api.Client())
		m.survey = 50 * time.Millisecond
		cands, err := m.Candidates(context.Background(), tc.box)
		if (err == nil) != (tc.err == nil) || (err != nil && err.Error() != tc.err.Error()) {
			t.Errorf("%s: error %v, want %v", tc.name, err, tc.err)
		}
		reqs := api.requests()
		if len(cands) != tc.want || len(reqs) != tc.calls {
			t.Errorf("%s: %d candidates in %d calls, want %d in %d", tc.name, len(cands), len(reqs), tc.want, tc.calls)
			continue
		}
		var surveys []BBox
		for _, r := range reqs {
			if r.URL.Path == "/images" {
				var b BBox
				fmt.Sscanf(r.URL.Query().Get("bbox"), "%f,%f,%f,%f", &b.MinLon, &b.MinLat, &b.MaxLon, &b.MaxLat)
				surveys = append(surveys, b)
			}
		}
		for i, half := range tc.halves {
			if i >= len(surveys) {
				t.Errorf("%s: %d surveys", tc.name, len(surveys))
				break
			}
			b := surveys[i]
			if math.Abs((b.MaxLat-b.MinLat)/2-half) > 1e-6 || math.Abs((b.MaxLon-b.MinLon)/2-half) > 1e-6 {
				t.Errorf("%s: survey %d is %v, want half %v", tc.name, i, b, half)
			}
			if i > 0 {
				p := surveys[i-1]
				if b.MinLat < p.MinLat-1e-6 || b.MaxLat > p.MaxLat+1e-6 || b.MinLon < p.MinLon-1e-6 || b.MaxLon > p.MaxLon+1e-6 {
					t.Errorf("%s: survey %d %v is outside %v", tc.name, i, b, p)
				}
			}
		}
	}
}

// A search whose own deadline passes during a survey ends with its
// deadline, without surveying a quarter.
func TestMapillaryDeadline(t *testing.T) {
	api := newFakeAPI(t)
	api.handle = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}
	m := NewMapillary(api.URL, "MLY|test", api.Client())
	m.survey = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := m.Candidates(ctx, Around(Point{Lat: -37.81, Lon: 144.96}))
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errSurveyTimeout) {
		t.Errorf("error %v", err)
	}
	if n := len(api.requests()); n != 1 {
		t.Errorf("%d requests", n)
	}
}

func TestBBoxQuarter(t *testing.T) {
	b := BBox{MinLon: 10, MinLat: 20, MaxLon: 12, MaxLat: 22}
	want := []BBox{{10, 20, 11, 21}, {11, 20, 12, 21}, {10, 21, 11, 22}, {11, 21, 12, 22}}
	for i, w := range want {
		if got := b.quarter(i); got != w {
			t.Errorf("quarter %d: %v, want %v", i, got, w)
		}
	}
}

// TestMapillaryLive surveys three boxes with the real API, only when
// GEO_LIVE_MAPILLARY=1; GEO_MAPILLARY_TOKEN is the token, or a placeholder
// where a proxy adds the real one. It logs counts and times only.
func TestMapillaryLive(t *testing.T) {
	if os.Getenv("GEO_LIVE_MAPILLARY") != "1" {
		t.Skip("set GEO_LIVE_MAPILLARY=1 to search the real Mapillary")
	}
	calls := &countingTransport{next: http.DefaultTransport}
	m := NewMapillary(MapillaryURL, os.Getenv("GEO_MAPILLARY_TOKEN"), &http.Client{Transport: calls})
	at := func(lat, lon, half float64) BBox {
		return BBox{MinLon: lon - half, MinLat: lat - half, MaxLon: lon + half, MaxLat: lat + half}
	}
	for _, tc := range []struct {
		name string
		box  BBox
	}{
		{"central Melbourne ±0.005°", at(-37.81, 144.96, 0.005)},
		{"Ballarat ±0.02°", Around(Point{Lat: -37.56, Lon: 143.85})},
		{"central Melbourne ±0.02°, too dense for one survey", Around(Point{Lat: -37.81, Lon: 144.96})},
	} {
		calls.surveys.Store(0)
		calls.details.Store(0)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		start := time.Now()
		cands, err := m.Candidates(ctx, tc.box)
		took := time.Since(start)
		cancel()
		if err != nil {
			t.Errorf("%s: %v after %v", tc.name, err, took.Round(time.Millisecond))
			continue
		}
		seqs := map[string]bool{}
		dated, placed := 0, 0
		for _, c := range cands {
			seqs[c.Sequence] = true
			if !c.Captured.IsZero() {
				dated++
			}
			if c.At.valid() && c.At.Lat != 0 {
				placed++
			}
		}
		t.Logf("%s: %d candidates from %d sequences, %d dated, %d placed, in %v (%d survey calls, %d details calls)",
			tc.name, len(cands), len(seqs), dated, placed, took.Round(time.Millisecond), calls.surveys.Load(), calls.details.Load())
		if len(cands) < 5 || len(seqs) < 5 || dated < 5 || placed < 5 {
			t.Errorf("%s: too few candidates", tc.name)
		}
	}
}

// countingTransport counts the survey and details calls it passes on.
type countingTransport struct {
	next             http.RoundTripper
	surveys, details atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/images" {
		c.surveys.Add(1)
	} else {
		c.details.Add(1)
	}
	return c.next.RoundTrip(r)
}
