package geo

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// Photo is a photo that passed every check, with its place named: ready for
// a round.
type Photo struct {
	Candidate
	Place   Place
	JPEG    []byte
	Offroad bool // it passed the off-road check
}

// Finder looks for a photo worth a round: it picks a seed region, searches
// random boxes in it, and checks what it finds, cheapest checks first.
type Finder struct {
	Sources []Source // tried in order for each box
	Seeds   *Seeds
	Rules   Rules
	Places  *Nominatim
	Rand    *rand.Rand
	Now     func() time.Time
	// Offroad finds only photos off roads: a box's candidates are looked
	// up together with Overpass before any download.
	Offroad  bool
	Overpass *Overpass // needed for Offroad

	// Used reports whether a candidate's Key was used recently; nil is none.
	Used func(key string) bool
	// Rejected, if set, is told why each candidate was turned down.
	Rejected func(source string, why Reject)
	// Failed, if set, is told of each failed search or download, whose
	// error holds no URL.
	Failed func(err error)
}

// The search's limits: boxes tried in a seed, seeds tried in one Find,
// and photos downloaded from one box. In an off-road search a box's
// candidates (up to overpassBatch) are looked up in one Overpass query,
// and ctx (the caller's timeout) ends a Find that runs long.
const (
	boxesPerSeed  = 4
	seedsPerFind  = 8
	fetchesPerBox = 3
)

// ErrNotFound is a Find that tried its seeds and found nothing.
var ErrNotFound = errors.New("no photo found")

// Find is a photo that passed every check. It gives up with ErrNotFound
// after seedsPerFind seeds, at once with ErrSlowDown if a source asks it to,
// or when ctx ends.
func (f *Finder) Find(ctx context.Context) (Photo, error) {
	for range seedsPerFind {
		seed := f.Seeds.Pick(f.Rand)
		for range boxesPerSeed {
			box := Around(seed.Point(f.Rand))
			for _, src := range f.Sources {
				p, err := f.search(ctx, src, box)
				if err == nil || errors.Is(err, ErrSlowDown) || ctx.Err() != nil {
					return p, err
				}
			}
		}
	}
	return Photo{}, ErrNotFound
}

// search looks in one box of one source.
func (f *Finder) search(ctx context.Context, src Source, box BBox) (Photo, error) {
	cands, err := src.Candidates(ctx, box)
	if err != nil {
		if !errors.Is(err, ErrSlowDown) && ctx.Err() == nil && f.Failed != nil {
			f.Failed(err)
		}
		return Photo{}, err
	}
	reject := func(why Reject) {
		if f.Rejected != nil {
			f.Rejected(src.Name(), why)
		}
	}
	kept := f.Rules.Filter(cands, f.Now(), f.Used, reject)
	f.Rand.Shuffle(len(kept), func(i, j int) { kept[i], kept[j] = kept[j], kept[i] })
	if f.Offroad {
		if kept, err = f.offroad(ctx, kept, reject); err != nil {
			return Photo{}, err
		}
	}
	for _, c := range kept[:min(len(kept), fetchesPerBox)] {
		data, err := src.Fetch(ctx, c)
		if err != nil {
			if ctx.Err() != nil {
				return Photo{}, ctx.Err()
			}
			if f.Failed != nil {
				f.Failed(err)
			}
			continue
		}
		jpeg, err := Vet(data, c.Pano, f.Rand)
		if err != nil {
			reject(err.(Reject))
			continue
		}
		place, err := f.Places.Reverse(ctx, c.At)
		if err != nil {
			if why, ok := err.(Reject); ok {
				reject(why)
			} else if ctx.Err() != nil {
				return Photo{}, ctx.Err()
			} else if f.Failed != nil {
				f.Failed(err)
			}
			continue
		}
		return Photo{Candidate: c, Place: place, JPEG: jpeg, Offroad: f.Offroad}, nil
	}
	return Photo{}, ErrNotFound
}

// offroad is the first overpassBatch of kept that Overpass finds off
// roads, rejecting the rest of them. A failed lookup fails the box:
// ErrNotFound once reported, or ErrSlowDown or ctx's error as it is.
func (f *Finder) offroad(ctx context.Context, kept []Candidate, reject func(Reject)) ([]Candidate, error) {
	kept = kept[:min(len(kept), overpassBatch)]
	pts := make([]Point, len(kept))
	for i, c := range kept {
		pts[i] = c.At
	}
	whys, err := f.Overpass.Offroad(ctx, pts)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, ErrSlowDown):
		return nil, err
	case err != nil:
		if f.Failed != nil {
			f.Failed(err)
		}
		return nil, ErrNotFound
	}
	var off []Candidate
	for i, c := range kept {
		if whys[i] != nil {
			reject(whys[i].(Reject))
			continue
		}
		off = append(off, c)
	}
	return off, nil
}
