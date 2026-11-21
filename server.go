package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/chat-plugin-geoguess/geo"
	"github.com/robdevops/chat-plugin-geoguess/internal/state"
	"github.com/robdevops/signalbot/addon"
)

// server is GeoGuess: the chats' rounds, their scores and the photos kept
// ready. srv speaks to the bot, and holds the outbox it reads.
type server struct {
	cfg    config
	clock  clock
	photos finder
	log    *log.Logger
	doc    *state.Doc
	srv    *addon.Server
	wake   chan struct{} // the prefetcher has work
	// perGame is the rounds in a game: gameRounds, or 1 in the tests of a
	// single round.
	perGame int

	mu      sync.Mutex
	rounds  map[string]*round  // by chat
	timers  map[string]stopper // each round's, by chat
	used    []string           // the last maxUsed photos' keys, oldest first
	scores  map[string][]score // by chat, the last scoreDays days
	buffer  []geo.Photo        // photos ready for a round, oldest first
	wants   map[string]bool    // regions a /geo asked for that the buffer lacks
	retries map[string]retry   // GEO_KEEP regions whose last search failed
	changed chan struct{}      // closed when the buffer gains a photo
	seeds   map[string]*geo.Seeds

	saveMu sync.Mutex // one save at a time, so the last one written is the newest
}

// round is one chat's game, at its open round. Its guesses are distances
// only: where anyone guessed is never kept.
type round struct {
	ID       string           `json:"id"` // the game's, also the watch's
	Chat     string           `json:"chat"`
	Starter  string           `json:"starter"` // who may end or skip it
	Started  time.Time        `json:"started"`
	Deadline time.Time        `json:"deadline"`
	Answer   geo.Point        `json:"answer"`
	Place    string           `json:"place"`
	Credit   credit           `json:"credit"`
	Guesses  map[string]guess `json:"guesses"` // by player

	Number  int            `json:"number,omitempty"`  // the open round, from 1
	Of      int            `json:"of,omitempty"`      // the game's rounds; 0, a round saved before games, is 1
	Region  string         `json:"region,omitempty"`  // the game's
	Length  time.Duration  `json:"length,omitempty"`  // each round's
	Until   time.Time      `json:"until,omitempty"`   // when the game's watch ends, less its slack
	Players []string       `json:"players,omitempty"` // who guessed in round 1: a later round ends once they all have
	Totals  map[string]int `json:"totals,omitempty"`  // the game's points so far, by player
	Waiting bool           `json:"waiting,omitempty"` // between rounds, for the next photo
}

// of is the rounds in r's game.
func (r *round) of() int { return max(r.Of, 1) }

// guess is how far one player's last guess was from the answer, and when
// it was sent.
type guess struct {
	Km float64   `json:"km"`
	At time.Time `json:"at"`
}

// credit names a photo's author, source and licence, with a link to it.
type credit struct {
	Creator string `json:"creator"`
	Source  string `json:"source"`
	Licence string `json:"licence"`
	Link    string `json:"link"`
}

// score is a player's points from one round.
type score struct {
	User   string    `json:"user"`
	Points int       `json:"points"`
	At     time.Time `json:"at"`
}

// saved is geo.json.
type saved struct {
	Rounds []*round           `json:"rounds"`
	Used   []string           `json:"used"`
	Scores map[string][]score `json:"scores"`
	Outbox []addon.Held       `json:"outbox"`
}

// The state's limits: photos remembered as used, days of scores, and the
// slack on a round's end.
const (
	maxUsed   = 1000
	scoreDays = 7
	// settle is how long after its deadline a round is revealed, so a guess
	// sent in time that waited in the bot's queue still counts.
	settle = 3 * time.Second
	// watchSlack is how long after the deadline the bot keeps the watch, so a
	// late guess still stays out of the chat's history.
	watchSlack = 5 * time.Second
	// gameRounds is the rounds in a game, and maxGame the longest a game
	// may run, with its waits for photos: within the bot's default
	// watch_max of 1h.
	gameRounds = 3
	maxGame    = 55 * time.Minute
)

// newServer loads the state saved in cfg.StateDir, re-arms its rounds'
// timers and reveals at once those whose time passed while it was down.
func newServer(cfg config, clk clock, photos finder, logger *log.Logger) (*server, error) {
	s := &server{
		cfg: cfg, clock: clk, photos: photos, log: logger,
		doc:     state.Open(cfg.StateDir, "geo"),
		wake:    make(chan struct{}, 1),
		perGame: gameRounds,
		rounds:  map[string]*round{},
		timers:  map[string]stopper{},
		scores:  map[string][]score{},
		wants:   map[string]bool{},
		retries: map[string]retry{},
		changed: make(chan struct{}),
		seeds:   map[string]*geo.Seeds{},
	}
	s.srv = addon.New(addon.Config{Name: "geoguess", Title: "GeoGuess", Version: "1", Tools: s.tools(), Now: clk.Now, Changed: s.save})
	var st saved
	if _, err := s.doc.Load(&st); err != nil {
		return nil, err
	}
	s.used = st.Used
	if st.Scores != nil {
		s.scores = st.Scores
	}
	s.srv.Restore(st.Outbox)
	s.mu.Lock()
	revealed := 0
	for _, r := range st.Rounds {
		if r == nil || s.rounds[r.Chat] != nil {
			continue
		}
		if r.Guesses == nil {
			r.Guesses = map[string]guess{}
		}
		s.rounds[r.Chat] = r
		switch {
		case r.Waiting: // for its next photo
			go s.nextRound(r.Chat, r.ID, r.Number+1)
		case !s.clock.Now().Before(r.Deadline.Add(settle)):
			s.timeUpLocked(r)
			revealed++
		default:
			s.arm(r)
		}
	}
	s.mu.Unlock()
	s.log.Printf("started: %d rounds open, %d revealed late, %d ops held", len(s.rounds), revealed, len(s.srv.Outbox()))
	if revealed > 0 {
		s.save()
	}
	return s, nil
}

// save writes geo.json, logging a failure; the state stays in memory, and
// the next change tries again.
func (s *server) save() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	st := saved{Used: s.used, Scores: s.scores, Outbox: s.srv.Outbox(), Rounds: make([]*round, 0, len(s.rounds))}
	for _, r := range s.rounds {
		st.Rounds = append(st.Rounds, r)
	}
	slices.SortFunc(st.Rounds, func(a, b *round) int { return strings.Compare(a.Chat, b.Chat) })
	data, err := json.Marshal(st)
	s.mu.Unlock()
	if err == nil {
		err = s.doc.Save(json.RawMessage(data))
	}
	if err != nil {
		s.log.Printf("saving failed: %v", err)
	}
}

// markUsed remembers a photo's key, forgetting the oldest past maxUsed.
// s.mu is held.
func (s *server) markUsed(key string) {
	s.used = append(s.used, key)
	if over := len(s.used) - maxUsed; over > 0 {
		s.used = slices.Delete(s.used, 0, over)
	}
}

// isUsed reports whether a photo's key is among those used recently.
func (s *server) isUsed(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.used, key)
}

// randomID is n random bytes in hex.
func randomID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// validID reports whether id is one of signalbot's pseudonyms with prefix
// (c_ or u_), so it can go into a {u_…} placeholder safely.
func validID(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok || rest == "" || len(rest) > 100 {
		return false
	}
	for _, c := range rest {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// queue adds ops to the outbox. GeoGuess's ops are small and have no
// images, so the outbox never refuses them; if it did, the log says so.
func (s *server) queue(ops ...addon.Op) {
	if err := s.srv.Queue(ops...); err != nil {
		s.log.Printf("an op was refused: %v", err)
	}
}
