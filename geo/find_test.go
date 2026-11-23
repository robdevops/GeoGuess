package geo

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"testing"
	"time"
)

// fakeSource offers the same candidates in every box, or with once only in
// the first, with photos by ID.
type fakeSource struct {
	cands   []Candidate
	once    bool
	photos  map[string][]byte
	err     error
	boxes   []BBox
	fetched []string
}

func (f *fakeSource) Name() string { return "Fake" }

func (f *fakeSource) Candidates(ctx context.Context, b BBox) ([]Candidate, error) {
	f.boxes = append(f.boxes, b)
	if f.once && len(f.boxes) > 1 {
		return nil, f.err
	}
	return f.cands, f.err
}

func (f *fakeSource) Fetch(ctx context.Context, c Candidate) ([]byte, error) {
	f.fetched = append(f.fetched, c.ID)
	if p, ok := f.photos[c.ID]; ok {
		return p, nil
	}
	return nil, StatusError{Source: "Fake", Code: 404}
}

func newTestFinder(t *testing.T, country string, sources ...Source) (*Finder, *[]Reject, *[]error) {
	api := newFakeAPI(t)
	api.handle = func(w http.ResponseWriter, r *http.Request) {
		if country == "" {
			w.Write([]byte(`{"error":"Unable to geocode"}`))
			return
		}
		w.Write([]byte(`{"address":{"town":"Ballarat","state":"Victoria","country":"` + country + `","country_code":"au"}}`))
	}
	places := NewNominatim(api.URL, api.Client())
	places.pace.every = 0
	seeds, err := LoadSeeds("AU")
	if err != nil {
		t.Fatal(err)
	}
	var rejects []Reject
	var fails []error
	f := &Finder{
		Sources: sources, Seeds: seeds, Places: places,
		Rules: Rules{MinQuality: 0.5, MaxAge: 10 * 365 * 24 * time.Hour},
		Rand:  rand.New(rand.NewPCG(1, 2)),
		Now:   func() time.Time { return time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC) },
		Used:  func(key string) bool { return key == "Fake:used" },
		Rejected: func(source string, why Reject) {
			if source != "Fake" {
				t.Errorf("rejected by %q", source)
			}
			rejects = append(rejects, why)
		},
		Failed: func(err error) { fails = append(fails, err) },
	}
	return f, &rejects, &fails
}

func TestFind(t *testing.T) {
	t.Parallel()
	noon := time.Date(2024, 12, 21, 2, 19, 0, 0, time.UTC) // in Victoria
	at := Point{-37.56, 143.85}
	c := func(id string) Candidate {
		return Candidate{ID: id, Source: "Fake", Sequence: id, At: at, Raw: at, Captured: noon, Quality: 0.9}
	}
	r := rand.New(rand.NewPCG(3, 3))
	good := jpegOf(t, scene(1024, 768, r))
	foggy := jpegOf(t, testPhotos(1024, 768)[3].m)
	night := c("night")
	night.Captured = noon.Add(12 * time.Hour)
	src := &fakeSource{
		cands:  []Candidate{c("used"), night, c("foggy"), c("good")},
		photos: map[string][]byte{"foggy": foggy, "good": good},
	}
	f, rejects, _ := newTestFinder(t, "Australia", src)
	// The candidates kept are tried in a random order: search until the
	// foggy one is tried before the good one.
	for range 10 {
		*rejects = nil
		src.fetched = nil
		p, err := f.Find(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if p.ID != "good" || p.Place.Name != "Ballarat, Victoria, Australia" || len(p.JPEG) == 0 {
			t.Fatalf("found %+v", p.Candidate)
		}
		if !slices.Contains(*rejects, RejectUsed) || !slices.Contains(*rejects, RejectDark) {
			t.Errorf("rejected for %v", *rejects)
		}
		if slices.Contains(src.fetched, "foggy") {
			if !slices.Contains(*rejects, RejectContrast) {
				t.Errorf("fog rejected for %v", *rejects)
			}
			return
		}
	}
	t.Error("the foggy photo was never tried")
}

func TestFindBoxes(t *testing.T) {
	src := &fakeSource{}
	f, _, _ := newTestFinder(t, "Australia", src)
	if _, err := f.Find(context.Background()); err != ErrNotFound {
		t.Errorf("empty: %v", err)
	}
	if len(src.boxes) != seedsPerFind*boxesPerSeed {
		t.Errorf("%d boxes searched", len(src.boxes))
	}
	for _, b := range src.boxes {
		c := b.Centre()
		if c.Lat > -10 || c.Lat < -44 || c.Lon < 113 || c.Lon > 154 {
			t.Errorf("searched outside Australia: %v", c)
		}
	}
}

func TestFindErrors(t *testing.T) {
	noon := time.Date(2024, 12, 21, 2, 19, 0, 0, time.UTC)
	at := Point{-37.56, 143.85}
	good := Candidate{ID: "good", Source: "Fake", At: at, Raw: at, Captured: noon, Quality: -1}
	photo := jpegOf(t, scene(1024, 768, rand.New(rand.NewPCG(4, 4))))

	// A failing source falls through to the next.
	broken := &fakeSource{err: StatusError{Source: "Fake", Code: 500}}
	working := &fakeSource{cands: []Candidate{good}, photos: map[string][]byte{"good": photo}}
	f, _, fails := newTestFinder(t, "Australia", broken, working)
	if p, err := f.Find(context.Background()); err != nil || p.ID != "good" {
		t.Errorf("fallback: %+v, %v", p.Candidate, err)
	}
	if len(*fails) != 1 || (*fails)[0] != (StatusError{Source: "Fake", Code: 500}) {
		t.Errorf("failures %v", *fails)
	}

	// A source that asks to slow down stops the search.
	slow := &fakeSource{err: ErrSlowDown}
	f, _, _ = newTestFinder(t, "Australia", slow, working)
	if _, err := f.Find(context.Background()); !errors.Is(err, ErrSlowDown) || len(slow.boxes) != 1 || len(working.boxes) != 1 {
		t.Errorf("slow down: %v after %d boxes", err, len(slow.boxes))
	}

	// A photo with no country is dropped.
	f, rejects, _ := newTestFinder(t, "", &fakeSource{cands: []Candidate{good}, photos: map[string][]byte{"good": photo}, once: true})
	if _, err := f.Find(context.Background()); err != ErrNotFound || !slices.Contains(*rejects, RejectNoPlace) {
		t.Errorf("no country: %v, %v", err, *rejects)
	}

	// A download that fails is reported, and the search goes on.
	f, _, fails = newTestFinder(t, "Australia", &fakeSource{cands: []Candidate{good}})
	if _, err := f.Find(context.Background()); err != ErrNotFound || len(*fails) != seedsPerFind*boxesPerSeed {
		t.Errorf("failed downloads: %v, %d reported", err, len(*fails))
	}

	// A cancelled search stops.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f, _, _ = newTestFinder(t, "Australia", &fakeSource{err: context.Canceled})
	if _, err := f.Find(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// An off-road search looks each candidate up before its download: one on
// a road is rejected and never downloaded, and one on a path is the photo.
func TestFindOffroad(t *testing.T) {
	noon := time.Date(2024, 12, 21, 2, 19, 0, 0, time.UTC)
	road, path := Point{-37.56, 143.85}, Point{-37.57, 143.86}
	c := func(id string, at Point) Candidate {
		return Candidate{ID: id, Source: "Fake", Sequence: id, At: at, Raw: at, Captured: noon, Quality: 0.9}
	}
	photo := jpegOf(t, scene(1024, 768, rand.New(rand.NewPCG(5, 5))))
	src := &fakeSource{
		cands:  []Candidate{c("road", road), c("path", path)},
		photos: map[string][]byte{"road": photo, "path": photo},
	}
	f, rejects, _ := newTestFinder(t, "Australia", src)
	api := newFakeAPI(t)
	api.handle = func(w http.ResponseWriter, r *http.Request) {
		switch q := r.URL.Query(); {
		case q.Get("zoom") == "10":
			w.Write([]byte(`{"address":{"town":"Ballarat","state":"Victoria","country":"Australia","country_code":"au"}}`))
		case q.Get("lat") == coord(road.Lat):
			w.Write([]byte(`{"category":"highway","type":"residential"}`))
		default:
			w.Write([]byte(`{"category":"highway","type":"footway"}`))
		}
	}
	f.Places = NewNominatim(api.URL, api.Client())
	f.Places.pace.every = 0
	f.Offroad = true
	for range 5 {
		*rejects, src.fetched = nil, nil
		p, err := f.Find(context.Background())
		if err != nil || p.ID != "path" || !p.Offroad || p.Place.CountryCode != "AU" {
			t.Fatalf("found %+v, offroad %v: %v", p.Candidate, p.Offroad, err)
		}
		if slices.Contains(src.fetched, "road") {
			t.Fatal("the photo on a road was downloaded")
		}
		if len(*rejects) > 0 && !slices.Equal(*rejects, []Reject{RejectRoad}) {
			t.Errorf("rejected for %v", *rejects)
		}
	}

	// Only roads: nothing is downloaded.
	src.cands, src.fetched = []Candidate{c("road", road)}, nil
	if _, err := f.Find(context.Background()); err != ErrNotFound || len(src.fetched) != 0 {
		t.Errorf("roads only: %v, fetched %v", err, src.fetched)
	}

	// Nominatim rate-limiting the check stops the search at once.
	src.cands, src.fetched = []Candidate{c("path", path), c("path2", path)}, nil
	handle := api.handle
	api.handle = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }
	before := len(api.requests())
	if _, err := f.Find(context.Background()); err != ErrSlowDown || len(api.requests())-before != 1 || len(src.fetched) != 0 {
		t.Errorf("rate-limited: %v after %d requests, fetched %v", err, len(api.requests())-before, src.fetched)
	}
	api.handle = handle

	// Without Offroad a photo is not marked off roads.
	f.Offroad = false
	src.cands = []Candidate{c("path", path)}
	if p, err := f.Find(context.Background()); err != nil || p.Offroad {
		t.Errorf("plain: %v, %v", p.Offroad, err)
	}
}
