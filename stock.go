package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/robdevops/GeoGuess/geo"
	"github.com/robdevops/GeoGuess/internal/fsx"
)

// The stock's limits: the most photos kept on disk, in the stock and
// reserved by games together, how old a stocked photo may be at start, the
// region keys kept stocked because games used them, and for how long.
const (
	maxHeld     = 32
	maxStockAge = 7 * 24 * time.Hour
	maxRecent   = 6
	recentAge   = 7 * 24 * time.Hour
)

// photoDir is the directory, in GEO_STATE_DIR, that holds kept photos'
// JPEGs.
const photoDir = "photos"

// kept is a photo kept for a round, in the stock or reserved by a game.
// Its metadata is saved in geo.json, its JPEG in photoDir, never in both.
type kept struct {
	geo.Candidate
	Place   geo.Place `json:"place"`
	Offroad bool      `json:"offroad,omitempty"`
	For     string    `json:"for,omitempty"` // the region key it was searched for
	Found   time.Time `json:"found"`
	JPEG    []byte    `json:"-"`
}

// recentKey is a region key a game used, and when it last did: kept
// stocked for recentAge after.
type recentKey struct {
	Key string    `json:"key"`
	At  time.Time `json:"at"`
}

// need is the photos a game of of rounds reserves: one each, and a spare
// for a tie-breaker if it has more than one.
func need(of int) int {
	if of > 1 {
		return of + 1
	}
	return 1
}

// fileName is the name of a photo's JPEG in photoDir: its key, with only
// letters, digits, - and _, and a hash of the key so two keys never share
// one.
func fileName(key string) string {
	clean := strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, key)
	if len(clean) > 64 {
		clean = clean[:64]
	}
	h := fnv.New32a()
	h.Write([]byte(key))
	return fmt.Sprintf("%s-%08x.jpg", clean, h.Sum32())
}

// photoPath is where k's JPEG is kept.
func (s *server) photoPath(k kept) string {
	return filepath.Join(s.cfg.StateDir, photoDir, fileName(k.Key()))
}

// writePhoto saves k's JPEG, atomically and for GeoGuess's user alone,
// logging a failure: the photo is still used, but a restart drops it.
func (s *server) writePhoto(k kept) {
	if err := fsx.WriteAtomic(s.photoPath(k), k.JPEG); err != nil {
		s.log.Printf("saving a photo failed: %v", bare(err))
	}
}

// removePhoto deletes k's JPEG, once it is shown or leaves the stock.
func (s *server) removePhoto(k kept) {
	if err := os.Remove(s.photoPath(k)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Printf("removing a photo failed: %v", bare(err))
	}
}

// bare is err without the path a file error carries, for the log.
func bare(err error) error {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err
	}
	return err
}

// loadPhotos is list with each photo's JPEG read back, dropping those
// whose file is missing and, with maxAge, those found longer ago than it
// (their file too).
func (s *server) loadPhotos(list []kept, now time.Time, maxAge time.Duration) []kept {
	var out []kept
	for _, k := range list {
		if maxAge > 0 && now.Sub(k.Found) > maxAge {
			s.removePhoto(k)
			continue
		}
		data, err := os.ReadFile(s.photoPath(k))
		if err != nil {
			continue
		}
		k.JPEG = data
		out = append(out, k)
	}
	return out
}

// cleanPhotos deletes the files in photoDir that no kept photo names, as
// a restart leaves when a game being started had claimed them. s.mu is
// held.
func (s *server) cleanPhotos() {
	dir := filepath.Join(s.cfg.StateDir, photoDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := map[string]bool{}
	for _, k := range s.stock {
		names[fileName(k.Key())] = true
	}
	for _, r := range s.rounds {
		for _, k := range r.Photos {
			names[fileName(k.Key())] = true
		}
	}
	for _, e := range entries {
		if !names[e.Name()] && !e.IsDir() {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// held is how many photos are kept, in the stock and reserved by games.
// s.mu is held.
func (s *server) held() int {
	n := len(s.stock)
	for _, r := range s.rounds {
		n += len(r.Photos)
	}
	return n
}

// stockKey is a region key the stock is kept filled for, and how many
// photos it wants.
type stockKey struct {
	key  string
	want int
}

// stockKeys is what the stock is kept filled for: a full game for each
// GEO_KEEP region and each key a game used lately, and GEO_BUFFER photos
// for GEO_REGIONS. s.mu is held.
func (s *server) stockKeys(now time.Time) []stockKey {
	var keys []stockKey
	add := func(key string, want int) {
		if !slices.ContainsFunc(keys, func(k stockKey) bool { return strings.EqualFold(k.key, key) }) {
			keys = append(keys, stockKey{key, want})
		}
	}
	full := need(s.perGame)
	for _, k := range s.cfg.Keep {
		add(k, full)
	}
	for _, k := range s.recent {
		if now.Sub(k.At) < recentAge {
			add(k.Key, full)
		}
	}
	add("", s.cfg.Buffer)
	return keys
}

// noteKey remembers that a game used region key, so the stock is kept
// filled for it: the maxRecent most recent, newest first. GEO_REGIONS and
// GEO_KEEP's keys are kept anyway. s.mu is held.
func (s *server) noteKey(key string, now time.Time) {
	if key == "" || slices.ContainsFunc(s.cfg.Keep, func(k string) bool { return strings.EqualFold(k, key) }) {
		return
	}
	s.recent = slices.DeleteFunc(s.recent, func(k recentKey) bool { return strings.EqualFold(k.Key, key) || now.Sub(k.At) >= recentAge })
	s.recent = slices.Insert(s.recent, 0, recentKey{key, now})
	s.recent = s.recent[:min(len(s.recent), maxRecent)]
}

// stocked is how many photos in the stock were found for key. s.mu is
// held.
func (s *server) stocked(key string) int {
	n := 0
	for _, k := range s.stock {
		if strings.EqualFold(k.For, key) {
			n++
		}
	}
	return n
}

// want is how many photos the stock is kept filled with for key. s.mu is
// held.
func (s *server) want(key string) int {
	for _, k := range s.stockKeys(s.clock.Now()) {
		if strings.EqualFold(k.key, key) {
			return k.want
		}
	}
	return 0
}

// surplus reports whether the stock holds more photos found for key than
// it is kept filled with, so one can go. s.mu is held.
func (s *server) surplus(key string) bool {
	return s.stocked(key) > s.want(key)
}

// otherMode reports whether k was found off roads for a key, and a game in
// region doesn't ask for that: off-road photos are slow to find, so such a
// game takes one only if the stock can spare it.
func otherMode(k kept, region string) bool {
	keyOff, _ := splitOffroad(k.For)
	off, _ := splitOffroad(region)
	return keyOff && !off
}

// pick is the index in the stock of the photo a game in region takes
// next, or -1: the oldest found for region, else the oldest whose key can
// spare it, else the oldest in region, never an off-road photo the stock
// can't spare for a game that doesn't ask for one. s.mu is held.
func (s *server) pick(region string) int {
	best, rank := -1, 3
	for i, k := range s.stock {
		if !s.in(k, region) {
			continue
		}
		r := 2
		switch {
		case strings.EqualFold(k.For, region):
			r = 0
		case s.surplus(k.For):
			r = 1
		case otherMode(k, region):
			continue
		}
		if r < rank {
			best, rank = i, r
		}
	}
	return best
}

// reserveLocked moves photos for r's game from the stock into its own,
// up to the game's need, and wakes the prefetcher to refill the stock.
// s.mu is held.
func (s *server) reserveLocked(r *round) {
	took := false
	for r.Number+len(r.Photos) < need(r.of()) {
		i := s.pick(r.Region)
		if i < 0 {
			break
		}
		r.Photos = append(r.Photos, s.stock[i])
		s.stock = slices.Delete(s.stock, i, i+1)
		took = true
	}
	if took {
		s.wakeUp()
	}
}

// restockLocked returns r's unused photos to the stock, in the order they
// were found, then offers them to the games still gathering photos. s.mu
// is held.
func (s *server) restockLocked(r *round) {
	if len(r.Photos) == 0 {
		return
	}
	s.stock = append(s.stock, r.Photos...)
	r.Photos = nil
	slices.SortStableFunc(s.stock, func(a, b kept) int { return a.Found.Compare(b.Found) })
	for _, g := range s.gatherers() {
		s.reserveLocked(g)
		s.startReadyLocked(g)
	}
	s.trimLocked()
	s.changedLocked()
	s.wakeUp()
}

// startReadyLocked opens round 1 of r's game, waiting for its photos with
// no lobby, once it has one for each round. s.mu is held.
func (s *server) startReadyLocked(r *round) {
	if r.Waiting && r.Number == 0 && !r.Lobby && !r.starting && len(r.Photos) >= r.of() {
		s.nextLocked(r)
	}
}

// trimLocked drops stocked photos, and their files, while more than
// maxHeld are kept: first those whose key has more than it wants, then the
// oldest. s.mu is held.
func (s *server) trimLocked() {
	for s.held() > maxHeld && len(s.stock) > 0 {
		i := slices.IndexFunc(s.stock, func(k kept) bool { return s.surplus(k.For) })
		if i < 0 {
			i = 0
		}
		s.removePhoto(s.stock[i])
		s.stock = slices.Delete(s.stock, i, i+1)
	}
}

// changedLocked tells a /geo waiting for photos that the stock or a game
// gained one. s.mu is held.
func (s *server) changedLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}
