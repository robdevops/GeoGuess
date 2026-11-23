package main

import (
	"context"
	"errors"
	"log"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/robdevops/GeoGuess/geo"
)

// finder finds a photo worth a round in regions ("" is GEO_REGIONS): the
// real search, or a test's.
type finder interface {
	Find(ctx context.Context, regions string) (geo.Photo, error)
}

// The prefetcher's limits: one search's time, and the photos kept beyond
// GEO_BUFFER for rounds in other regions.
const (
	findTimeout = 3 * time.Minute
	extraPhotos = 4
)

// retry is when a GEO_KEEP region whose search failed is searched again,
// and how many times in a row it has failed.
type retry struct {
	at       time.Time
	failures int
}

// prefetch keeps GEO_BUFFER checked photos ready, so /geo answers at once,
// and looks for one in each region a /geo asked for. It is the only caller
// of Find. A GEO_KEEP region whose search fails waits its own backoff,
// while the rest go on, so one hard region can't starve the buffer.
func (s *server) prefetch(ctx context.Context) {
	failures := 0
	for {
		region, ok, next := s.nextSearch(time.Now())
		if !ok {
			if !s.idle(ctx, next) {
				return
			}
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, findTimeout)
		p, err := s.photos.Find(fctx, region)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			failures = 0
			s.add(p, region)
			continue
		}
		s.mu.Lock()
		keep := slices.Contains(s.cfg.Keep, region)
		// A GEO_KEEP region is searched again after its backoff; another is
		// asked again by the next /geo, or kept while a game waits for it.
		if keep || !s.awaited(region) {
			delete(s.wants, region)
		}
		var wait time.Duration
		if keep {
			r := s.retries[region]
			wait = backoff(r.failures, err)
			s.retries[region] = retry{time.Now().Add(wait), min(r.failures+1, 10)}
		}
		s.mu.Unlock()
		// Overpass being busy slows down only a kept region's off-road
		// searches; other sources being busy slows down every search.
		if keep && (!errors.Is(err, geo.ErrSlowDown) || errors.Is(err, geo.ErrOverpassBusy)) {
			s.log.Printf("no photo found for a kept region (%v); trying it again in %s", err, wait)
			continue
		}
		wait = backoff(failures, err)
		failures = min(failures+1, 10)
		s.log.Printf("no photo found (%v); trying again in %s", err, wait)
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// backoff is how long to wait after failures failed searches in a row and
// then err: from 5s, doubling to 5 minutes, or a minute when asked to slow
// down.
func backoff(failures int, err error) time.Duration {
	if errors.Is(err, geo.ErrSlowDown) {
		return time.Minute
	}
	return min(5*time.Second<<failures, 5*time.Minute)
}

// idle waits for work: a wake, or next, when a GEO_KEEP region may be
// searched again (zero for none). It is false once ctx is done.
func (s *server) idle(ctx context.Context, next time.Time) bool {
	var due <-chan time.Time
	if !next.IsZero() {
		t := time.NewTimer(time.Until(next))
		defer t.Stop()
		due = t.C
	}
	select {
	case <-s.wake:
	case <-due:
	case <-ctx.Done():
		return false
	}
	return true
}

// awaited reports whether a game yet to open round 1, or between rounds,
// waits for a photo in region, so a failed search there is tried again.
// s.mu is held.
func (s *server) awaited(region string) bool {
	for _, r := range s.rounds {
		if (r.Number == 0 || r.Waiting) && r.Region == region {
			return true
		}
	}
	return false
}

// nextSearch is the region to look in next: the first a /geo asked for
// that the buffer lacks, else the first GEO_KEEP region it lacks whose
// retry is due at now, else GEO_REGIONS while the buffer isn't full. With
// none, next is the earliest retry still to come, or zero.
func (s *server) nextSearch(now time.Time) (region string, ok bool, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, region := range slices.Sorted(maps.Keys(s.wants)) {
		if s.match(region) >= 0 {
			delete(s.wants, region)
			continue
		}
		return region, true, time.Time{}
	}
	for _, region := range s.cfg.Keep {
		if s.match(region) >= 0 {
			delete(s.retries, region)
			continue
		}
		r, failed := s.retries[region]
		if !failed || !now.Before(r.at) {
			return region, true, time.Time{}
		}
		if next.IsZero() || r.at.Before(next) {
			next = r.at
		}
	}
	return "", len(s.buffer) < s.cfg.Buffer, next
}

// add puts a photo found for region in the buffer, and marks it used so no
// later search finds it again. Past GEO_BUFFER and extraPhotos it drops the
// oldest, but never a GEO_KEEP region's last.
func (s *server) add(p geo.Photo, region string) {
	s.mu.Lock()
	s.buffer = append(s.buffer, p)
	for over := len(s.buffer) - s.cfg.Buffer - extraPhotos; over > 0; over-- {
		i := slices.IndexFunc(s.buffer, s.droppable)
		if i < 0 {
			break
		}
		s.buffer = slices.Delete(s.buffer, i, i+1)
	}
	s.markUsed(p.Key())
	delete(s.wants, region)
	delete(s.retries, region)
	close(s.changed)
	s.changed = make(chan struct{})
	n := len(s.buffer)
	s.mu.Unlock()
	s.save()
	s.log.Printf("a %s photo is ready; %d in all", p.Source, n)
}

// droppable reports whether p, in the buffer, can leave it without leaving
// a GEO_KEEP region with no photo. s.mu is held.
func (s *server) droppable(p geo.Photo) bool {
	for _, region := range s.cfg.Keep {
		if !s.in(p, region) {
			continue
		}
		n := 0
		for _, q := range s.buffer {
			if s.in(q, region) {
				n++
			}
		}
		if n < 2 {
			return false
		}
	}
	return true
}

// take is a photo from the buffer for region, waiting up to wait for the
// prefetcher to find one. Taking one wakes the prefetcher, which refills a
// GEO_KEEP region it used up.
func (s *server) take(ctx context.Context, region string, wait time.Duration) (geo.Photo, bool) {
	t := time.NewTimer(max(wait, 0))
	defer t.Stop()
	for {
		s.mu.Lock()
		if i := s.pick(region); i >= 0 {
			p := s.buffer[i]
			s.buffer = slices.Delete(s.buffer, i, i+1)
			s.mu.Unlock()
			s.wakeUp()
			return p, true
		}
		if region != "" {
			s.wants[region] = true
		}
		changed := s.changed
		s.mu.Unlock()
		s.wakeUp()
		select {
		case <-changed:
		case <-t.C:
			return geo.Photo{}, false
		case <-ctx.Done():
			return geo.Photo{}, false
		}
	}
}

// pick is the index of the photo take gives for region, or -1: the oldest
// in it, but for a region not in GEO_KEEP, one a GEO_KEEP region can spare
// if there is one, so a plain /geo leaves the kept photo for its region.
// s.mu is held.
func (s *server) pick(region string) int {
	if !slices.ContainsFunc(s.cfg.Keep, func(k string) bool { return strings.EqualFold(k, region) }) {
		if i := slices.IndexFunc(s.buffer, func(p geo.Photo) bool { return s.in(p, region) && s.droppable(p) }); i >= 0 {
			return i
		}
	}
	return s.match(region)
}

// match is the index of the oldest photo in the buffer that is in region
// ("" is GEO_REGIONS), or -1. s.mu is held.
func (s *server) match(region string) int {
	return slices.IndexFunc(s.buffer, func(p geo.Photo) bool { return s.in(p, region) })
}

// offroad is the token in a region that asks for photos off roads, as in
// /geo offroad: "offroad", or "offroad,europe" with regions.
const offroad = "offroad"

// splitOffroad is region without its offroad token, and whether it had
// one. The rest is what geo.LoadSeeds reads.
func splitOffroad(region string) (bool, string) {
	var off bool
	var rest []string
	for _, r := range strings.Split(region, ",") {
		switch r = strings.TrimSpace(r); {
		case strings.EqualFold(r, offroad):
			off = true
		case r != "":
			rest = append(rest, r)
		}
	}
	return off, strings.Join(rest, ",")
}

// withOffroad is rest with the offroad token first if off: the region a
// /geo offroad keys the buffer by.
func withOffroad(off bool, rest string) string {
	switch {
	case !off:
		return rest
	case rest == "":
		return offroad
	}
	return offroad + "," + rest
}

// in reports whether p is in region ("" is GEO_REGIONS, and empty too the
// world), and off roads if region asks for that. s.mu is held.
func (s *server) in(p geo.Photo, region string) bool {
	off, region := splitOffroad(region)
	if off && !p.Offroad {
		return false
	}
	if region == "" {
		region = s.cfg.Regions
	}
	if region == "" {
		return true
	}
	seeds, err := s.seedsLocked(region)
	return err == nil && seeds.Has(p.Place.CountryCode)
}

// regionSeeds is the seeds for region, a /geo argument without the
// offroad token, or an error if it names none.
func (s *server) regionSeeds(region string) (*geo.Seeds, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seedsLocked(region)
}

// seedsLocked is regionSeeds with s.mu held.
func (s *server) seedsLocked(region string) (*geo.Seeds, error) {
	key := strings.ToUpper(region)
	if seeds := s.seeds[key]; seeds != nil {
		return seeds, nil
	}
	seeds, err := geo.LoadSeeds(region)
	if err == nil && len(s.seeds) < 64 {
		s.seeds[key] = seeds
	}
	return seeds, err
}

// wakeUp tells the prefetcher to look at the buffer again, without waiting.
func (s *server) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// search is the real finder: Mapillary and Panoramax, checked by the geo
// package, with places named by Nominatim and off-road spots told by
// Overpass.
type search struct {
	sources  []geo.Source
	rules    geo.Rules
	places   *geo.Nominatim
	overpass *geo.Overpass
	rand     *rand.Rand
	used     func(key string) bool
	log      *log.Logger
	seeds    map[string]*geo.Seeds
	regions  string // GEO_REGIONS
}

// seedsFor is whether regions asks for photos off roads, and the seeds of
// the rest of it, or of GEO_REGIONS with none.
func (f *search) seedsFor(regions string) (bool, *geo.Seeds, error) {
	off, regions := splitOffroad(regions)
	if regions == "" {
		regions = f.regions
	}
	if seeds := f.seeds[regions]; seeds != nil {
		return off, seeds, nil
	}
	seeds, err := geo.LoadSeeds(regions)
	if err != nil {
		return off, nil, err
	}
	f.seeds[regions] = seeds
	return off, seeds, nil
}

// Find looks for one photo, off roads if regions has the offroad token.
// It logs only counts of rejections, by reason, and errors, which hold no
// URL.
func (f *search) Find(ctx context.Context, regions string) (geo.Photo, error) {
	off, seeds, err := f.seedsFor(regions)
	if err != nil {
		return geo.Photo{}, err
	}
	rejected := map[string]int{}
	finder := geo.Finder{
		Sources: f.sources, Seeds: seeds, Rules: f.rules, Places: f.places, Rand: f.rand, Now: time.Now,
		Offroad: off, Overpass: f.overpass,
		Used:     f.used,
		Rejected: func(source string, why geo.Reject) { rejected[source+": "+string(why)]++ },
		Failed:   func(err error) { f.log.Printf("search failed: %v", err) },
	}
	p, err := finder.Find(ctx)
	if len(rejected) > 0 {
		f.log.Printf("photos rejected: %v", rejected)
	}
	return p, err
}
