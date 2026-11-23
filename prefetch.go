package main

import (
	"cmp"
	"context"
	"errors"
	"log"
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

// findTimeout is the longest one search may take.
const findTimeout = 3 * time.Minute

// retry is when a region key whose search failed is searched again, how
// many times in a row it has failed, and whether a source asked to slow
// down.
type retry struct {
	at       time.Time
	failures int
	slow     bool
}

// prefetch finds the photos games need, so no round ever waits for a
// search: first for games still gathering theirs, then to keep the stock
// filled for each key it is kept for, taking the keys in turn so one slow
// key can't starve the rest. It is the only caller of Find. A key whose
// search fails waits its own backoff while the others go on; a source
// asking to slow down holds up every search.
func (s *server) prefetch(ctx context.Context) {
	for {
		key, ok, next := s.nextSearch(time.Now())
		if !ok {
			if !s.idle(ctx, next) {
				return
			}
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, findTimeout)
		s.mu.Lock()
		s.searching, s.stopSearch, s.stopped = key, cancel, false
		s.mu.Unlock()
		p, err := s.photos.Find(fctx, key)
		cancel()
		s.mu.Lock()
		stopped := s.stopped
		s.searching, s.stopSearch, s.stopped = "", nil, false
		s.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			s.add(p, key)
			continue
		}
		if stopped { // for a game gathering photos: no failure
			continue
		}
		s.mu.Lock()
		r := s.retries[key]
		wait := backoff(r.failures, err)
		s.retries[key] = retry{time.Now().Add(wait), min(r.failures+1, 10), errors.Is(err, geo.ErrSlowDown)}
		s.mu.Unlock()
		s.log.Printf("no photo found (%v); trying that region again in %s", err, wait)
		if !errors.Is(err, geo.ErrSlowDown) {
			continue
		}
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

// idle waits for work: a wake, or next, when a key may be searched again
// (zero for none). It is false once ctx is done.
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

// gathering reports whether r's game still gathers photos at now: it
// lacks some of its need, and fetchMax since /geo hasn't passed.
func gathering(r *round, now time.Time) bool {
	return !r.GiveUp.IsZero() && now.Before(r.GiveUp) && r.Number+len(r.Photos) < need(r.of())
}

// gatherers is the games gathering photos, the longest-waiting first.
// s.mu is held.
func (s *server) gatherers() []*round {
	now := s.clock.Now()
	var out []*round
	for _, r := range s.rounds {
		if gathering(r, now) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b *round) int { return cmp.Or(a.GiveUp.Compare(b.GiveUp), strings.Compare(a.Chat, b.Chat)) })
	return out
}

// hurry lets a game's region key be searched at once, though an earlier
// search there failed, unless a source asked to slow down. s.mu is held.
func (s *server) hurry(key string) {
	if r, ok := s.retries[key]; ok && !r.slow {
		r.at = time.Time{}
		s.retries[key] = r
	}
}

// interruptLocked stops the search under way if it only refills the
// stock and isn't for key, a game's that just began gathering photos, so
// that game's search comes first. s.mu is held.
func (s *server) interruptLocked(key string) {
	if s.stopSearch == nil || strings.EqualFold(s.searching, key) ||
		slices.ContainsFunc(s.gatherers(), func(r *round) bool { return strings.EqualFold(r.Region, s.searching) }) {
		return
	}
	s.stopped = true
	s.stopSearch()
}

// nextSearch is the region key to search next: a gathering game's, else,
// while fewer than maxHeld photos are kept, the next key in turn the stock
// lacks photos for, each only once its retry is due at now. With none,
// next is the earliest retry still to come, or zero.
func (s *server) nextSearch(now time.Time) (key string, ok bool, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	due := func(key string) bool {
		r, failed := s.retries[key]
		if !failed || !now.Before(r.at) {
			return true
		}
		if next.IsZero() || r.at.Before(next) {
			next = r.at
		}
		return false
	}
	// Full: a found photo would only push out one that is wanted.
	full := s.held() >= maxHeld && !slices.ContainsFunc(s.stock, func(k kept) bool { return s.surplus(k.For) })
	if !full || len(s.stock) > 0 { // a game's photo then pushes out a stocked one
		for _, r := range s.gatherers() {
			if due(r.Region) {
				return r.Region, true, time.Time{}
			}
		}
	}
	if full {
		return "", false, next
	}
	keys := s.stockKeys(s.clock.Now())
	for i := range keys {
		k := keys[(s.turn+i)%len(keys)]
		if s.stocked(k.key) >= k.want || !due(k.key) {
			continue
		}
		s.turn = (s.turn + i + 1) % len(keys)
		return k.key, true, time.Time{}
	}
	return "", false, next
}

// add keeps a photo found for key, and marks it used so no later search
// finds it again: it goes to a game gathering photos in its region, the
// one it was found for first, which starts once it has a photo for each
// round; else to the stock. Its JPEG is saved first.
func (s *server) add(p geo.Photo, key string) {
	k := kept{Candidate: p.Candidate, Place: p.Place, For: key, Found: s.clock.Now(), JPEG: p.JPEG}
	s.writePhoto(k)
	s.mu.Lock()
	s.markUsed(p.Key())
	delete(s.retries, key)
	var to *round
	for _, r := range s.gatherers() {
		if !s.in(k, r.Region) {
			continue
		}
		if to == nil || strings.EqualFold(r.Region, key) && !strings.EqualFold(to.Region, key) {
			to = r
		}
	}
	if to != nil {
		to.Photos = append(to.Photos, k)
		s.startReadyLocked(to)
	} else {
		s.stock = append(s.stock, k)
	}
	s.trimLocked()
	s.changedLocked()
	n := s.held()
	s.mu.Unlock()
	s.save()
	if to != nil {
		s.log.Printf("a %s photo is ready for game %s; %d kept in all", p.Source, to.ID, n)
	} else {
		s.log.Printf("a %s photo is ready; %d kept in all", p.Source, n)
	}
}

// in reports whether p is in region ("" is GEO_REGIONS, and empty too the
// world). s.mu is held.
func (s *server) in(p kept, region string) bool {
	if region == "" {
		region = s.cfg.Regions
	}
	if region == "" {
		return true
	}
	seeds, err := s.seedsLocked(region)
	return err == nil && seeds.Has(p.Place.CountryCode)
}

// regionSeeds is the seeds for region, a /geo argument, or an error if it
// names none.
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

// wakeUp tells the prefetcher to look for work again, without waiting.
func (s *server) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// search is the real finder: Mapillary and Panoramax, checked by the geo
// package, with places named by Nominatim.
type search struct {
	sources []geo.Source
	rules   geo.Rules
	places  *geo.Nominatim
	rand    *rand.Rand
	used    func(key string) bool
	log     *log.Logger
	seeds   map[string]*geo.Seeds
	regions string // GEO_REGIONS
}

// seedsFor is the seeds of regions, or of GEO_REGIONS with none.
func (f *search) seedsFor(regions string) (*geo.Seeds, error) {
	if regions == "" {
		regions = f.regions
	}
	if seeds := f.seeds[regions]; seeds != nil {
		return seeds, nil
	}
	seeds, err := geo.LoadSeeds(regions)
	if err != nil {
		return nil, err
	}
	f.seeds[regions] = seeds
	return seeds, nil
}

// Find looks for one photo. It logs only counts of rejections, by reason,
// and errors, which hold no URL.
func (f *search) Find(ctx context.Context, regions string) (geo.Photo, error) {
	seeds, err := f.seedsFor(regions)
	if err != nil {
		return geo.Photo{}, err
	}
	rejected := map[string]int{}
	finder := geo.Finder{
		Sources: f.sources, Seeds: seeds, Rules: f.rules, Places: f.places, Rand: f.rand, Now: time.Now,
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
