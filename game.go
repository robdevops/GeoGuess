package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robdevops/GeoGuess/geo"
	"github.com/robdevops/signalbot/addon"
)

// The texts players see.
const (
	guard     = "No hints during a round 😉"
	noChat    = "GeoGuess works only in a chat."
	noRound   = "There's no round on. Start one with /geo."
	noPhoto   = "I couldn't find a good spot in time; I'm still looking, so try /geo again in a minute."
	badLength = "A round can be 30s to 30m, such as /geo 90s or /geo 5m."
	usage     = "Try /geo, /geo 90s, /geo start, /geo wait, /geo end, /geo skip, /geo scores, or a region such as /geo europe or /geo AU,NZ."
	longGame  = "That game would run too long: ask for shorter rounds, such as /geo 5m."
	noSpot    = "I couldn't find a spot, so the game is off." // a game found no photo by fetchMax
)

// The lobby's buttons and their typed equivalents, /geo start, /geo end
// and /geo wait (on Signal): the starter's only; ✅, anyone's, is a
// reaction's join.
const (
	notStarter = "Only the player who started this game can do that."
	started    = "That game has already started."
	gameOver   = "That game is over."
	offText    = "The game is off." // the chat's word that the starter called the game off before round 1
	// moreWait is what one press of ⏱ +30s, or /geo wait, adds to the lobby's wait.
	moreWait = 30 * time.Second
	// pressTool is the tool the lobby's buttons call.
	pressTool = "geo_press"
	// alreadyIn is ✅'s pop-up for a player who has joined.
	alreadyIn = "You're already in."
	// alreadyMost is a toggle's pop-up when its setting can't go higher.
	alreadyMost = "Already the most."
	// buttonGone is the pop-up of the answer timer toggle, gone, pressed
	// under an older lobby.
	buttonGone = "That button is gone."
	// noCountdown is /geo wait in a lobby with buttons, which no timer starts.
	noCountdown = "There's no countdown: the starter presses ▶️ when everyone's in."
)

// readyIn is ✅'s pop-up for a join: how many have joined.
func readyIn(n int) string {
	return fmt.Sprintf("You're in: %d joined.", n)
}

// pin is the reaction to a recorded guess; on Telegram, which allows only
// some emoji as reactions (not 📍), pinTelegram.
const (
	pin         = "📍"
	pinTelegram = "👌"
)

// alarm is the bot's reaction to a round's photo with warnLeft to go (on
// Telegram, which allows only some emoji as reactions, not ⏰,
// alarmTelegram); a round shorter than warnMin has none, nothing useful
// to warn.
const (
	alarm         = "⏰"
	alarmTelegram = "😱"
	warnLeft      = 5 * time.Second
	warnMin       = 10 * time.Second
)

// replyTime is what /geo leaves before the bot's deadline to start the
// round and send the photo, so it never starts one the bot won't show.
const replyTime = 3 * time.Second

// startGrace is how long /geo waits for a game's photos when the stock
// lacks some before it answers without them. fetchMax is how long a game
// gathers its photos, from /geo: more than one whole search (findTimeout).
const (
	startGrace = 3 * time.Second
	fetchMax   = 5 * time.Minute
	// introPause is how long after /geo's answer round 1's photo follows,
	// when its photos are ready, so the answer goes out first.
	introPause = 2 * time.Second
)

// fetching is /geo's answer in a DM, or a group without a lobby, while the
// game's photos are gathered.
func fetching(of int) string {
	if of == 1 {
		return "🔎 Fetching a spot…"
	}
	return fmt.Sprintf("🔎 Fetching spots for %d rounds…", of)
}

// stillFetching is the post when a lobby ends before the game has a
// photo for each round.
func stillFetching(have, of int) string {
	return fmt.Sprintf("🔎 Still fetching: %d of %d %s ready…", have, of, plural(of, "spot", "spots"))
}

// shortGame is the post before round 1 of a game that found only n
// photos by fetchMax, and so plays n rounds.
func shortGame(n int) string {
	return fmt.Sprintf("Only found %d %s, so this game is %d %s.", n, plural(n, "spot", "spots"), n, plural(n, "round", "rounds"))
}

// tools are GeoGuess's tools, both for the bot alone: neither is offered
// to the model.
func (s *server) tools() []addon.Tool {
	return []addon.Tool{
		{
			Name:        "geo",
			Description: "The /geo command: start, end or skip this chat's round, start its lobby now or make it wait, or show its scores.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"args":{"type":"string"}}}`),
			Visibility:  []string{"bot"},
			Call: func(ctx context.Context, c *addon.Context, raw json.RawMessage) (addon.Result, error) {
				var a struct {
					Args string `json:"args"`
				}
				if err := addon.DecodeArgs(raw, &a); err != nil {
					return addon.Result{}, err
				}
				return s.command(ctx, c, a.Args), nil
			},
		},
		{
			Name:        "geo_guess",
			Description: "Records the location in one chat message as a guess in this chat's round.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
			Visibility:  []string{"bot"},
			Call: func(_ context.Context, c *addon.Context, raw json.RawMessage) (addon.Result, error) {
				var a struct {
					Text     string `json:"text"`
					Reaction string `json:"reaction"` // a reaction to the game's messages: a join, in the lobby
					Removed  bool   `json:"removed"`
				}
				if err := addon.DecodeArgs(raw, &a); err != nil {
					return addon.Result{}, err
				}
				if a.Reaction != "" {
					return s.reacted(c, a.Removed), nil
				}
				return s.guess(c, a.Text), nil
			},
		},
		{
			Name:        pressTool,
			Description: "A press of a button under a game's lobby: ready, the rounds toggle, start or cancel.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"press":{"type":"string"}},"required":["press"]}`),
			Visibility:  []string{"bot"},
			Call: func(_ context.Context, c *addon.Context, raw json.RawMessage) (addon.Result, error) {
				var p addon.Press
				if err := addon.DecodeArgs(raw, &p); err != nil {
					return addon.Result{}, err
				}
				if !fromChat(c) {
					return addon.Text(noChat), nil
				}
				action, id, _ := strings.Cut(p.Press, ":")
				return addon.Text(s.lobbyAct(c, action, id, true)), nil
			},
		},
	}
}

// fromChat reports whether c names a chat and a sender.
func fromChat(c *addon.Context) bool {
	return c != nil && validID(c.Chat, "c_") && validID(c.Sender, "u_")
}

// command is /geo with its arguments: end, skip or scores, or a game's
// round length and regions, in any order.
func (s *server) command(ctx context.Context, c *addon.Context, args string) addon.Result {
	if !fromChat(c) {
		return addon.Result{Content: addon.Text(noChat).Content, IsError: true}
	}
	fields := strings.Fields(args)
	if len(fields) == 1 {
		switch strings.ToLower(fields[0]) {
		case "end", "stop":
			return s.finish(c, false)
		case "skip":
			return s.finish(c, true)
		case "scores":
			return addon.Text(s.scoreboard(c.Chat))
		case "start", "wait":
			return addon.Text(s.lobbyAct(c, strings.ToLower(fields[0]), "", false))
		}
	}
	length := s.cfg.Round
	var regions []string
	for _, f := range fields {
		if f[0] >= '0' && f[0] <= '9' {
			d, err := roundLength(strings.ToLower(f))
			if err != nil {
				return addon.Text(badLength)
			}
			length = d
			continue
		}
		regions = append(regions, f)
	}
	region := strings.Join(regions, ",")
	if region != "" {
		if _, err := s.regionSeeds(region); err != nil {
			return addon.Text("I don't know that region. " + usage)
		}
	}
	s.mu.Lock()
	of := s.perGame
	s.mu.Unlock()
	if gameTime(of, length, s.cfg.gap) > maxGame {
		return addon.Text(longGame)
	}
	return s.start(ctx, c, length, region)
}

// busy is the answer to /geo while a game is on.
func busy(r *round, now time.Time) addon.Result {
	if r.Lobby && r.Buttons {
		return addon.Text("A game is waiting for players: press ✅ under its message to join.")
	}
	if r.Lobby {
		return addon.Text("A game is waiting for players: react to its message to join, or /geo start.")
	}
	if r.Waiting {
		return addon.Text(fmt.Sprintf("A game is on: round %d's photo is on its way.", r.Number+1))
	}
	left := max(r.Deadline.Sub(now), 0).Round(time.Second)
	m, sec := int(left/time.Minute), int(left%time.Minute/time.Second)
	var label string
	switch {
	case m == 0:
		label = fmt.Sprintf("%ds", sec)
	case sec == 0:
		label = fmt.Sprintf("%dm", m)
	default:
		label = fmt.Sprintf("%dm%ds", m, sec)
	}
	return addon.Text("A round is on: " + label + " left.")
}

// start begins a game in c's chat, reserving a photo from the stock for
// each of its rounds and a spare for a tie-breaker. In a group with a
// lobby, it answers at once with the lobby's intro. Else, with a photo for
// each round in hand, or within startGrace, it answers with the game's
// intro and the watch for guesses, and round 1's photo follows as a post
// introPause later; else it answers that it is fetching the photos, which
// the game gathers as the prefetcher finds them, for up to fetchMax, and
// round 1 follows once there is one for each round.
func (s *server) start(ctx context.Context, c *addon.Context, length time.Duration, region string) addon.Result {
	s.mu.Lock()
	now := s.clock.Now()
	if r := s.rounds[c.Chat]; r != nil {
		s.mu.Unlock()
		return busy(r, now)
	}
	of := s.perGame
	r := &round{
		ID: "r" + randomID(6), Chat: c.Chat, Starter: c.Sender,
		Guesses: map[string]guess{},
		Of:      of, Region: region, Length: length, Totals: map[string]int{},
		Waiting: true, starting: true, Network: c.Network,
	}
	if r.Network == "" {
		r.Network = s.srv.Network()
	}
	s.rounds[c.Chat] = r
	s.reserveLocked(r)
	if len(r.Photos) < need(of) { // the prefetcher gathers the rest for the game first
		r.GiveUp = now.Add(fetchMax)
		s.hurry(region)
		s.interruptLocked(region)
		s.wakeUp()
	}
	s.noteKey(region, now)
	s.mu.Unlock()
	lobby := c.Group && s.srv.WatchReactions()
	if !lobby { // a lobby gathers its photos while players join
		wait := s.cfg.grace
		if c.Deadline != 0 { // leave the bot time to send the answer
			wait = min(wait, time.Until(time.Unix(c.Deadline, 0))-replyTime)
		}
		s.gather(ctx, r, wait)
	}
	s.mu.Lock()
	r.starting = false
	if s.rounds[c.Chat] != r || ctx.Err() != nil { // ended meanwhile, or the bot gave up
		if s.rounds[c.Chat] == r {
			delete(s.rounds, c.Chat)
			s.restockLocked(r)
		}
		s.mu.Unlock()
		return addon.Text(noPhoto)
	}
	now = s.clock.Now()
	r.Until = now.Add(gameTime(of, length, s.cfg.gap))
	if !lobby && len(r.Photos) < of { // round 1 waits for its photos
		r.Until = r.Until.Add(fetchMax)
	} else if !lobby { // round 1 follows introPause after the answer
		r.Until = r.Until.Add(introPause)
	}
	if lobby { // players join by reacting to the intro first; photos are gathered meanwhile, until GiveUp, within lobbyMax
		r.Lobby, r.Waiting, r.Joined, r.CallOff = true, false, nil, now.Add(lobbyMax)
		r.Until = r.Until.Add(lobbyMax)
		if s.srv.Buttons() { // started by ▶️ alone; the watch has room for the toggles' longest game
			r.Buttons, r.BaseOf = true, of
			r.Until = r.CallOff.Add(maxGame)
		}
		s.armLobby(r)
		intro, meta, have := lobbyIntro(r), watchMeta(r, true), len(r.Photos)
		if r.Buttons {
			meta.Buttons, meta.OnPress = lobbyRows(r), pressTool
		}
		s.mu.Unlock()
		s.save()
		s.log.Printf("game %s waiting for players, %d of %d photos ready", r.ID, have, need(of))
		return addon.Result{Content: addon.Text(intro).Content, Meta: meta}
	}
	if !c.Group { // a DM: its one player, so no round waits for the timer
		r.Players = []string{c.Sender}
	}
	text := intro(r)
	if len(r.Photos) < of { // round 1 follows once the photos are in
		r.Closed = now // the gap lets the answer go out before round 1
		s.armGiveUp(r)
		text = fetching(of)
	} else { // round 1 follows as a post, a moment after the answer
		r.Closed = now.Add(introPause - s.cfg.gap)
		s.startReadyLocked(r)
	}
	meta, have := watchMeta(r, false), len(r.Photos)
	s.mu.Unlock()
	s.save()
	s.log.Printf("game %s starting, %d of %d photos ready", r.ID, have, need(of))
	return addon.Result{Content: addon.Text(text).Content, Meta: meta}
}

// gather waits up to wait, or until ctx ends, for r's game to have a photo
// for each round, or to end.
func (s *server) gather(ctx context.Context, r *round, wait time.Duration) {
	t := time.NewTimer(max(wait, 0))
	defer t.Stop()
	for {
		s.mu.Lock()
		done := len(r.Photos) >= r.of() || s.rounds[r.Chat] != r
		changed := s.changed
		s.mu.Unlock()
		if done {
			return
		}
		select {
		case <-changed:
		case <-t.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// popLocked takes the next of r's reserved photos for its round about to
// open, deleting its file. s.mu is held.
func (s *server) popLocked(r *round) kept {
	k := r.Photos[0]
	r.Photos = slices.Delete(r.Photos, 0, 1)
	s.removePhoto(k)
	return k
}

// watchMeta is the watch for r's game, which /geo's answer carries: for
// reactions too in a lobby.
func watchMeta(r *round, reactions bool) *addon.ResultMeta {
	return &addon.ResultMeta{Watch: &addon.Watch{
		ID: r.ID, Pattern: geo.Pattern, Until: r.Until.Add(watchSlack).Format(time.RFC3339),
		Tool: "geo_guess", Guard: guard, Over: "That game is over. Start another with /geo.", Reactions: reactions,
	}}
}

// gameTime is the longest a game of of rounds of length may run once
// round 1 is due: a tie-breaker round after them, if one is possible, and
// the gap before each round's photo.
func gameTime(of int, length, gap time.Duration) time.Duration {
	if of == 1 {
		return length + settle + gap
	}
	return time.Duration(of+1) * (length + settle + gap)
}

// roundLine heads a round's photo and results in a game of more than one:
// "Round 2 of 3 · ", or "Tie-breaker · ".
func roundLine(r *round) string {
	switch {
	case r.of() == 1:
		return ""
	case r.Number > r.of():
		return "**Tie-breaker** · "
	}
	return fmt.Sprintf("**Round %d of %d** · ", r.Number, r.of())
}

// caption is the photo's caption: by when to guess and, for round 1
// (first), how, on the chat network the bot said it is on.
func caption(deadline time.Time, length time.Duration, network string, first bool) string {
	when := "🌍 **Where is this?** Send a location before " + deadline.Local().Format("15:04") + " (" + lengthLabel(length) + ")."
	if !first {
		return when
	}
	steps := "Signal: **+** → Location; Telegram: 📎 → Location. Pinch to zoom out, **drag the pin**, confirm (on Signal), then send."
	switch network {
	case "signal":
		steps = "Tap **+** → Location, pinch to zoom out, **drag the pin**, confirm, then send."
	case "telegram":
		steps = "Tap 📎 → Location, pinch to zoom out, **drag the pin**, then send."
	}
	return when + "\n" +
		steps + " Or type coordinates, e.g. `-37.81, 144.96`.\n" +
		"⚠️ Location starts at *your* position. Move the pin before you send."
}

// lengthLabel is a round's length as the caption says it: 3 min, 90 sec.
func lengthLabel(d time.Duration) string {
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + " min"
	}
	return strconv.Itoa(int(d.Round(time.Second)/time.Second)) + " sec"
}

// network is the chat network of r's game: the one it started on, or
// for a game saved before that was kept, the bot's.
func (s *server) network(r *round) string {
	if r.Network != "" {
		return r.Network
	}
	return s.srv.Network()
}

// emoji is the reaction for r's game: telegram on Telegram, which allows
// only some emoji as reactions, else any.
func (s *server) emoji(r *round, other, telegram string) string {
	if s.network(r) == "telegram" {
		return telegram
	}
	return other
}

// arm sets r's timer to reveal its open round once its time is up,
// replacing any timer it had, and its ⏰. s.mu is held.
func (s *server) arm(r *round) {
	chat, id, number := r.Chat, r.ID, r.Number
	s.setTimer(chat, r.Deadline.Add(settle).Sub(s.clock.Now()), func() { s.timeUp(chat, id, number) })
	s.armWarn(r)
}

// armWarn sets r's ⏰, replacing any it had: a reaction to its open
// round's photo with warnLeft to go. There is none for a photo without
// an op ID (a post the outbox refused), a round shorter than warnMin, or one with warnLeft or less to go, as one
// that ended early has. s.mu is held.
func (s *server) armWarn(r *round) {
	s.stopWarn(r.Chat)
	d := r.Deadline.Add(-warnLeft).Sub(s.clock.Now())
	if r.Post == "" || r.Waiting || r.Deadline.Sub(r.Started) < warnMin || d <= 0 {
		return
	}
	chat, id, number, post, deadline := r.Chat, r.ID, r.Number, r.Post, r.Deadline
	s.warns[chat] = s.clock.AfterFunc(d, func() {
		s.mu.Lock()
		r := s.rounds[chat]
		if r == nil || r.ID != id || r.Number != number || r.Waiting || r.Post != post || !r.Deadline.Equal(deadline) { // over, or ended early
			s.mu.Unlock()
			return
		}
		delete(s.warns, chat)
		s.queue(addon.Op{Op: "react", Chat: chat, Post: post, Emoji: s.emoji(r, alarm, alarmTelegram)})
		s.mu.Unlock()
		s.save()
	})
}

// stopWarn stops the ⏰ of chat's game, if it has one. s.mu is held.
func (s *server) stopWarn(chat string) {
	if t := s.warns[chat]; t != nil {
		t.Stop()
		delete(s.warns, chat)
	}
}

// setTimer sets chat's game's one timer to call f after d, replacing any
// it had. s.mu is held.
func (s *server) setTimer(chat string, d time.Duration, f func()) {
	if t := s.timers[chat]; t != nil {
		t.Stop()
	}
	s.timers[chat] = s.clock.AfterFunc(max(d, 0), f)
}

// timeUp reveals round number of the game id in chat, if it is still open.
func (s *server) timeUp(chat, id string, number int) {
	s.mu.Lock()
	r := s.rounds[chat]
	if r == nil || r.ID != id || r.Number != number || r.Waiting {
		s.mu.Unlock()
		return
	}
	s.timeUpLocked(r)
	s.mu.Unlock()
	s.save()
}

// timeUpLocked ends r's open round, queues its results, and moves the game
// on. s.mu is held.
func (s *server) timeUpLocked(r *round) {
	s.advanceLocked(r, s.closeRound(r, true))
}

// advanceLocked queues the results of r's round just closed, then opens
// the next round after the gap; after the last round, the game's totals
// too, and the end of its watch. Players tied on wins after the last round
// play a tie-breaker with the game's spare photo; with no spare, points
// decide, as the totals rank them. s.mu is held.
func (s *server) advanceLocked(r *round, results string) {
	tie := tied(r)
	if r.Number == r.of() && len(tie) > 1 && len(r.Photos) > 0 {
		names := make([]string, len(tie))
		for i, u := range tie {
			names[i] = "{" + u + "}"
		}
		results += "\n\n🤝 " + strings.Join(names, ", ") + fmt.Sprintf(" are tied on %d %s: a tie-breaker round decides it, and only they can win it.", r.Wins[tie[0]], plural(r.Wins[tie[0]], "win", "wins"))
		r.Tie = tie
	}
	if len(r.Photos) > 0 && (r.Number < r.of() || len(r.Tie) > 0 && r.Number == r.of()) {
		s.queue(addon.Op{Op: "post", Chat: r.Chat, Text: results})
		r.Waiting, r.Closed = true, s.clock.Now()
		s.nextLocked(r)
		return
	}
	s.queue(addon.Op{Op: "post", Chat: r.Chat, Text: results + totals(r)})
	s.endGame(r)
}

// endGame forgets r's game, returns its unused photos to the stock, and
// queues the end of its watch. s.mu is held.
func (s *server) endGame(r *round) {
	delete(s.rounds, r.Chat)
	if t := s.timers[r.Chat]; t != nil {
		t.Stop()
		delete(s.timers, r.Chat)
	}
	s.stopWarn(r.Chat)
	s.restockLocked(r)
	s.queue(addon.Op{Op: "unwatch", Chat: r.Chat, Watch: r.ID})
}

// nextLocked opens r's next round with its next reserved photo once the
// gap after the last results has passed; with no photo left, which only
// a restart leaves (a photo file lost, or a game saved before games
// reserved photos), it ends the game. s.mu is held.
func (s *server) nextLocked(r *round) {
	if len(r.Photos) == 0 {
		s.queue(addon.Op{Op: "post", Chat: r.Chat, Text: "This game's photos were lost in a restart, so it ends here." + totals(r)})
		s.endGame(r)
		return
	}
	chat, id, number := r.Chat, r.ID, r.Number+1
	wait := r.Closed.Add(s.cfg.gap).Sub(s.clock.Now())
	if wait <= 0 {
		if t := s.timers[chat]; t != nil {
			t.Stop()
			delete(s.timers, chat)
		}
		s.openLocked(r, number)
		return
	}
	s.setTimer(chat, wait, func() { // a pause to read the results
		s.mu.Lock()
		r := s.rounds[chat]
		if r == nil || r.ID != id || r.Number != number-1 || !r.Waiting {
			s.mu.Unlock()
			return
		}
		delete(s.timers, chat)
		s.openLocked(r, number)
		s.mu.Unlock()
		s.save()
	})
}

// readyLocked moves on r's game, waiting for round 1 with its lobby over
// (or none): round 1 opens if there is a photo for each round; at
// GiveUp, the game plays the rounds it has photos for, or is called off
// with none; else it waits for the photos until GiveUp, saying so first
// if progress is set. s.mu is held.
func (s *server) readyLocked(r *round, progress bool) {
	switch {
	case len(r.Photos) >= r.of():
		s.nextLocked(r)
	case !s.clock.Now().Before(r.GiveUp):
		s.giveUpLocked(r)
	default:
		if progress {
			s.queue(addon.Op{Op: "post", Chat: r.Chat, Text: stillFetching(len(r.Photos), r.of())})
		}
		s.armGiveUp(r)
	}
}

// armGiveUp sets r's timer for GiveUp, when its game stops waiting for
// round 1's photos. s.mu is held.
func (s *server) armGiveUp(r *round) {
	chat, id := r.Chat, r.ID
	s.setTimer(chat, r.GiveUp.Sub(s.clock.Now()), func() {
		s.mu.Lock()
		r := s.rounds[chat]
		if r == nil || r.ID != id || r.Lobby || !r.Waiting || r.Number != 0 {
			s.mu.Unlock()
			return
		}
		delete(s.timers, chat)
		s.readyLocked(r, false)
		s.mu.Unlock()
		s.save()
	})
}

// giveUpLocked settles r's game, which gathered too few photos by
// GiveUp: with some, it plays that many rounds, saying so; with none, it
// is called off. s.mu is held.
func (s *server) giveUpLocked(r *round) {
	n := len(r.Photos)
	s.log.Printf("game %s stopped gathering photos: %d found", r.ID, n)
	if n == 0 {
		s.queue(addon.Op{Op: "post", Chat: r.Chat, Text: noSpot})
		s.endGame(r)
		return
	}
	r.Of = n
	s.queue(addon.Op{Op: "post", Chat: r.Chat, Text: shortGame(n)})
	r.Closed = s.clock.Now() // the gap lets the post be read before round 1
	s.nextLocked(r)
}

// openLocked opens round number of r's game with its next reserved photo,
// and queues the photo; with no time left on the game's watch, it ends the
// game. s.mu is held.
func (s *server) openLocked(r *round, number int) {
	chat := r.Chat
	now := s.clock.Now()
	length := min(r.Length, r.Until.Sub(now)-settle)
	if length < minRound {
		s.queue(addon.Op{Op: "post", Chat: chat, Text: fmt.Sprintf("No time is left for round %d, so the game ends here.", number) + totals(r)})
		s.endGame(r)
		return
	}
	photo := s.popLocked(r)
	r.Number, r.Waiting = number, false
	r.Started, r.Deadline = now, now.Add(length)
	r.Answer, r.Place, r.Guesses = photo.At, photo.Place.Name, map[string]guess{}
	r.Credit = credit{Creator: photo.Creator, Source: photo.Source, Licence: photo.Licence, Link: photo.Link}
	r.Post = s.queueID(addon.Op{Op: "post", Chat: chat, Text: roundLine(r) + caption(r.Deadline, length, s.network(r), r.Number == 1),
		Images: []addon.Image{{Data: base64.StdEncoding.EncodeToString(photo.JPEG), MimeType: "image/jpeg", Name: "geoguess.jpg"}}})
	s.arm(r)
	s.log.Printf("round %d of game %s started: %s, a %s photo", number, r.ID, length, photo.Source)
}

// tied is the players tied for the most round wins, if more than one and
// at least one win each, sorted.
func tied(r *round) []string {
	most := 0
	for _, w := range r.Wins {
		most = max(most, w)
	}
	var out []string
	for u, w := range r.Wins {
		if w == most && most > 0 {
			out = append(out, u)
		}
	}
	slices.Sort(out)
	if len(out) < 2 {
		return nil
	}
	return out
}

// totals is the end of a game of more than one round: the winner, by
// rounds won, then the players ranked by wins and then points. After a
// tie-breaker that none of the tied won, points decide among them.
// Players are {u_…} placeholders.
func totals(r *round) string {
	if r.of() == 1 {
		return ""
	}
	type total struct {
		user         string
		wins, points int
	}
	var list []total
	for u, p := range r.Totals {
		list = append(list, total{u, r.Wins[u], p})
	}
	slices.SortFunc(list, func(a, b total) int {
		return cmp.Or(cmp.Compare(b.wins, a.wins), cmp.Compare(b.points, a.points), strings.Compare(a.user, b.user))
	})
	var b strings.Builder
	b.WriteString("\n\n🏁 **Game over**")
	switch {
	case len(list) == 0 || list[0].wins == 0:
		b.WriteString("\nNobody scored.")
		return b.String()
	case len(list) > 1 && list[1].wins == list[0].wins && list[1].points == list[0].points:
		b.WriteString(": a draw.")
	default:
		b.WriteString(": 🏆 {" + list[0].user + "} wins!")
	}
	rank := 0
	for i, t := range list {
		if i == 0 || t.wins != list[i-1].wins || t.points != list[i-1].points {
			rank = i + 1
		}
		b.WriteString(fmt.Sprintf("\n%s {%s} %d %s · %s", medal(rank), t.user, t.wins, plural(t.wins, "win", "wins"), geo.Thousands(t.points)))
	}
	return b.String()
}

// plural is one or many, by n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// finish is /geo end (skip false) or /geo skip, which only the player who
// started the round may use. It answers with the results, and queues the
// end of the round's watch.
func (s *server) finish(c *addon.Context, skip bool) addon.Result {
	s.mu.Lock()
	r := s.rounds[c.Chat]
	switch {
	case r == nil:
		s.mu.Unlock()
		return addon.Text(noRound)
	case r.Starter != c.Sender && skip:
		s.mu.Unlock()
		return addon.Text("Only the player who started this round can skip it.")
	case r.Starter != c.Sender:
		s.mu.Unlock()
		return addon.Text("Only the player who started this round can end it.")
	case r.starting: // the bot has no watch for it yet
		s.mu.Unlock()
		return addon.Text("A game is starting: try again in a moment.")
	case r.Lobby, r.Waiting && r.Number == 0 && !skip: // before round 1
		s.callOffLocked(r, "")
		s.mu.Unlock()
		s.save()
		return addon.Text(offText)
	}
	var text string
	switch {
	case skip && r.Waiting:
		s.mu.Unlock()
		return addon.Text(fmt.Sprintf("Round %d's photo is on its way.", r.Number+1))
	case skip && r.Number < r.of() && len(r.Photos) > 0: // the next round follows, as at the round's end
		text = s.closeRound(r, false)
		r.Waiting, r.Closed = true, s.clock.Now()
		s.nextLocked(r)
	case skip:
		text = s.closeRound(r, false) + totals(r)
		s.endGame(r)
	case r.Waiting: // /geo end between rounds
		text = strings.TrimPrefix(totals(r), "\n\n")
		s.endGame(r)
	default:
		text = s.closeRound(r, true) + totals(r)
		s.endGame(r)
	}
	s.mu.Unlock()
	s.save()
	return addon.Text(text)
}

// closeRound closes r's open round, adding its points to the chat's scores
// and the game's totals if scored, and returns its results. Round 1's
// guessers become the game's players. s.mu is held.
func (s *server) closeRound(r *round, scored bool) string {
	if t := s.timers[r.Chat]; t != nil {
		t.Stop()
		delete(s.timers, r.Chat)
	}
	s.stopWarn(r.Chat)
	if r.Number <= 1 && len(r.Players) == 0 { // no lobby: round 1's guessers are the game's players
		r.Players = slices.Sorted(maps.Keys(r.Guesses))
	}
	guesses := make([]geo.Guess, 0, len(r.Guesses))
	for user, g := range r.Guesses {
		guesses = append(guesses, geo.Guess{User: user, Km: g.Km, At: g.At})
	}
	placings := geo.Rank(guesses)
	how := "revealed"
	if scored {
		now := s.clock.Now()
		s.pruneScores(now)
		if r.Totals == nil {
			r.Totals = map[string]int{}
		}
		for _, p := range placings {
			s.scores[r.Chat] = append(s.scores[r.Chat], score{User: p.User, Points: p.Points, At: now})
			r.Totals[p.User] += p.Points
		}
		if r.Wins == nil {
			r.Wins = map[string]int{}
		}
		for _, p := range placings { // the round's winner: in a tie-breaker, the nearest of the tied
			if len(r.Tie) == 0 || slices.Contains(r.Tie, p.User) {
				r.Wins[p.User]++
				break
			}
		}
	} else {
		how = "skipped"
	}
	s.log.Printf("round %s %s: %d guesses", r.ID, how, len(placings))
	return roundLine(r) + results(r, placings, scored)
}

// results is a round's reveal: the place, then each player's distance and
// points, never where they guessed, then the photo's credit. Players are
// {u_…} placeholders, which the bot turns into names.
func results(r *round, placings []geo.Placing, scored bool) string {
	place := "📍 [**" + plain(r.Place) + "**](" + mapLink(r.Answer) + ")"
	var b strings.Builder
	switch {
	case !scored:
		b.WriteString("Round skipped. It was " + place)
	case len(placings) == 0:
		b.WriteString("Nobody guessed. It was " + place)
	default:
		b.WriteString(place)
		for _, p := range placings {
			b.WriteString("\n" + medal(p.Rank) + " {" + p.User + "} " + geo.Distance(p.Km) + " · " + geo.Thousands(p.Points))
		}
	}
	b.WriteString("\n" + r.Credit.String())
	return b.String()
}

// String is the credit line: "Photo: jsmith, Mapillary, CC BY-SA 4.0",
// linked to the photo's page.
func (c credit) String() string {
	var parts []string
	for _, p := range []string{plain(c.Creator), c.Source, c.Licence} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	line := strings.Join(parts, ", ")
	if strings.HasPrefix(c.Link, "https://") && !strings.ContainsAny(c.Link, " ()") {
		line = "[" + line + "](" + c.Link + ")"
	}
	return "Photo: " + line
}

// plain is s without the characters Markdown would read, so a photo's
// author can't style or link the results.
func plain(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if strings.ContainsRune("[]()*_`~\\<>|#", r) {
			return -1
		}
		return r
	}, s)), " ")
}

// mapLink is a map of the answer on OpenStreetMap.
func mapLink(p geo.Point) string {
	lat, lon := strconv.FormatFloat(p.Lat, 'f', 5, 64), strconv.FormatFloat(p.Lon, 'f', 5, 64)
	return "https://www.openstreetmap.org/?mlat=" + lat + "&mlon=" + lon + "#map=12/" + lat + "/" + lon
}

// medal marks a rank: a medal for the first three, else the number.
func medal(rank int) string {
	if rank <= 3 {
		return []string{"🥇", "🥈", "🥉"}[rank-1]
	}
	return strconv.Itoa(rank) + "."
}

// guess records the location in a chat message as its sender's guess in
// the chat's round: only how far it is from the answer, never where it is.
// A recorded guess answers with a pin for the bot to react with.
func (s *server) guess(c *addon.Context, msg string) addon.Result {
	if !fromChat(c) {
		return addon.Text(noChat)
	}
	p, ok := geo.Parse(msg)
	if !ok {
		return addon.Text("No location found.")
	}
	at := s.clock.Now()
	if t := cmp.Or(c.Edited, c.Sent); t > 0 {
		at = time.Unix(t, 0)
	}
	s.mu.Lock()
	r := s.rounds[c.Chat]
	switch {
	case r == nil:
		s.mu.Unlock()
		return addon.Text(noRound)
	case r.Lobby, r.Waiting && r.Number == 0:
		s.mu.Unlock()
		return addon.Text("The game hasn't started yet.")
	case c.Sent > 0 && c.Sent < r.Started.Unix():
		s.mu.Unlock()
		return addon.Text("That message is from before this round.")
	case at.After(r.Deadline):
		s.mu.Unlock()
		return addon.Text("Too late for this round.")
	}
	r.Guesses[c.Sender] = guess{Km: geo.Km(p, r.Answer), At: at}
	react := s.emoji(r, pin, pinTelegram)
	if s.allGuessed(r) {
		// Everyone playing has guessed: end the round now, revealing it after
		// settle, as at its deadline, for guesses still on their way.
		r.Deadline = s.clock.Now()
		s.arm(r)
		s.log.Printf("round %d of game %s ended early", r.Number, r.ID)
	}
	s.mu.Unlock()
	s.save()
	return addon.Result{Content: addon.Text("Recorded.").Content, Meta: &addon.ResultMeta{React: react}}
}

// lobbyIntro is /geo's answer in a group: the game, and how to join: by
// ✅ where the lobby has buttons, and the starter starts it with ▶️;
// else by a reaction, and it starts by itself.
func lobbyIntro(r *round) string {
	if r.Buttons { // a reaction still counts, unsaid
		return intro(r) + "\n" +
			"👋 **Press ✅ to join**, whoever started it too; the starter presses ▶️ when everyone's in (✖️ calls it off), and can change the rounds with the buttons first. " +
			"A round ends as soon as everyone who joined has guessed."
	}
	return intro(r) + "\n" +
		"👋 **React to this message with any emoji to join**, whoever started it too: the game starts 30 seconds after the last player joins, once at least 2 have; a round ends as soon as everyone who joined has guessed. " +
		"The starter can start it now with /geo start, or wait 30s longer with /geo wait."
}

// lobbyRows is the buttons under a lobby with them, in one row: ✅
// with how many have joined, the rounds toggle, ▶️ once enough have joined, and ✖️.
func lobbyRows(r *round) [][]addon.Button {
	row := []addon.Button{
		{Label: fmt.Sprintf("✅ %d", len(r.Joined)), Data: "ready:" + r.ID},
		{Label: fmt.Sprintf("Rounds: %d", r.of()), Data: "t1:" + r.ID},
	}
	if len(r.Joined) >= minPlayers {
		row = append(row, addon.Button{Label: "▶️", Data: "start:" + r.ID})
	}
	return [][]addon.Button{append(row, addon.Button{Label: "✖️", Data: "cancel:" + r.ID})}
}

// buttonsLocked queues the buttons for r's lobby as they now are, if it
// has them, in place of those under its intro. s.mu is held.
func (s *server) buttonsLocked(r *round) {
	if r.Buttons {
		s.queue(addon.Op{Op: "buttons", Chat: r.Chat, Watch: r.ID, Buttons: lobbyRows(r)})
	}
}

// mostRounds is the most rounds a game of rounds of length may have
// within maxGame.
func mostRounds(length, gap time.Duration) int {
	n := 1
	for gameTime(n+1, length, gap) <= maxGame {
		n++
	}
	return n
}

// toggleRoundsLocked switches r's lobby's rounds between what it opened
// with and double that, at most mostRounds: its pop-up, and rewrites the
// lobby's intro, which says them, with its buttons. The game's photos
// follow: the spares go back to the stock, or more are reserved and, if
// the stock lacks them, gathered, until fetchMax from now but not past
// the lobby's call-off. s.mu is held.
func (s *server) toggleRoundsLocked(r *round) string {
	base := max(r.BaseOf, 1)
	next := base
	if r.of() == base {
		next = min(2*base, mostRounds(r.Length, s.cfg.gap))
		if next <= base {
			return alreadyMost
		}
	}
	r.Of = next
	if n := need(next); len(r.Photos) > n {
		extra := slices.Clone(r.Photos[n:])
		r.Photos = slices.Clone(r.Photos[:n])
		s.unreserveLocked(extra)
	} else if len(r.Photos) < n {
		s.reserveLocked(r)
		now := s.clock.Now()
		if len(r.Photos) < n {
			until := now.Add(fetchMax)
			if r.CallOff.Before(until) {
				until = r.CallOff
			}
			if until.After(r.GiveUp) {
				r.GiveUp = until
			}
			s.hurry(r.Region)
			s.interruptLocked(r.Region)
			s.wakeUp()
		}
	}
	// The intro says the rounds: rewrite it with the buttons.
	s.queue(addon.Op{Op: "buttons", Chat: r.Chat, Watch: r.ID, Buttons: lobbyRows(r), Text: lobbyIntro(r)})
	return fmt.Sprintf("Rounds: %d", next)
}

// intro is the game's first line: its rounds and their length.
func intro(r *round) string {
	return fmt.Sprintf("🌍 **GeoGuess** · %d %s of %s: guess where each photo was taken.", r.of(), plural(r.of(), "round", "rounds"), lengthLabel(r.Length))
}

// reacted is a reaction to the game's messages: in the lobby, a player
// joining, or leaving if removed; after it, nothing.
func (s *server) reacted(c *addon.Context, removed bool) addon.Result {
	if !fromChat(c) {
		return addon.Text(noChat)
	}
	s.mu.Lock()
	r := s.rounds[c.Chat]
	if r == nil || !r.Lobby {
		s.mu.Unlock()
		return addon.Text("Noted.")
	}
	in := slices.Contains(r.Joined, c.Sender)
	switch {
	case !removed && !in:
		s.joinLocked(r, c.Sender)
	case removed && in:
		r.Joined = slices.DeleteFunc(r.Joined, func(u string) bool { return u == c.Sender })
	default:
		s.mu.Unlock()
		return addon.Text("Noted.")
	}
	s.armLobby(r)
	s.buttonsLocked(r)
	s.mu.Unlock()
	s.save()
	return addon.Text("Noted.")
}

// joinLocked adds who to r's lobby, if not yet in it, reporting whether
// they joined: a reaction, ✅ or the starter's ▶️. The
// caller re-arms the lobby (armLobby) or begins the game. s.mu is held.
func (s *server) joinLocked(r *round, who string) bool {
	if slices.Contains(r.Joined, who) {
		return false
	}
	r.Joined = append(r.Joined, who)
	r.LastJoin = s.clock.Now()
	return true
}

// armLobby sets r's lobby timer: with enough players, to start lobbyWait
// after the last join; else, or with buttons, where only ▶️ starts
// it, to call the game off at CallOff. s.mu is held.
func (s *server) armLobby(r *round) {
	at := r.CallOff
	if len(r.Joined) >= minPlayers && !r.Buttons {
		at = startAt(r)
	}
	chat, id := r.Chat, r.ID
	s.setTimer(chat, at.Sub(s.clock.Now()), func() {
		s.mu.Lock()
		r := s.rounds[chat]
		if r == nil || r.ID != id || !r.Lobby {
			s.mu.Unlock()
			return
		}
		now := s.clock.Now()
		switch {
		case len(r.Joined) >= minPlayers && !r.Buttons && !now.Before(startAt(r)):
			s.beginLocked(r)
		case len(r.Joined) < minPlayers && !now.Before(r.CallOff):
			s.callOffLocked(r, "Not enough players joined, so the game is off.")
		case r.Buttons && !now.Before(r.CallOff):
			s.callOffLocked(r, "Nobody pressed ▶️ in time, so the game is off.")
		default:
			s.armLobby(r)
		}
		s.mu.Unlock()
		s.save()
	})
}

// startAt is when r's lobby, with enough players, starts: lobbyWait after
// the last join, with the starter's extra wait, which never takes it past
// CallOff.
func startAt(r *round) time.Time {
	base := r.LastJoin.Add(lobbyWait)
	at := base.Add(r.Extra)
	if r.Extra > 0 && at.After(r.CallOff) {
		at = r.CallOff
		if base.After(at) {
			at = base
		}
	}
	return at
}

// beginLocked ends r's lobby and starts its game with those who joined:
// round 1's photo comes now, with no pause, if the game has its photos,
// else once it has them. The lobby's timer, the ▶️ button and /geo
// start all come here, under s.mu, so a game starts once. s.mu is held.
func (s *server) beginLocked(r *round) {
	r.Lobby, r.Players = false, slices.Clone(r.Joined)
	r.Waiting, r.Closed = true, s.clock.Now().Add(-s.cfg.gap)
	if t := s.timers[r.Chat]; t != nil {
		t.Stop()
		delete(s.timers, r.Chat)
	}
	s.log.Printf("game %s began: %d players", r.ID, len(r.Players))
	s.readyLocked(r, true)
}

// lobbyAct is the starter's start, cancel, wait or rounds toggle (t1)
// for the lobby of the game in c's chat, or anyone's ready (a join), or
// t2, the answer timer toggle under an older lobby, now gone: a press of one of its buttons (pressed, for the
// game id), or /geo start or /geo wait. It answers in words: the press's
// pop-up, or the command's answer. Its work is in memory and in the
// outbox, so it answers at once, within a press's 5 seconds.
func (s *server) lobbyAct(c *addon.Context, action, id string, pressed bool) string {
	s.mu.Lock()
	r := s.rounds[c.Chat]
	switch {
	case pressed && (r == nil || r.ID != id):
		s.mu.Unlock()
		return gameOver
	case r == nil:
		s.mu.Unlock()
		return noRound
	case r.starting:
		s.mu.Unlock()
		return "A game is starting: try again in a moment."
	case !r.Lobby:
		s.mu.Unlock()
		return started
	case action == "t2": // the answer timer toggle, under an older lobby, is gone: anyone's press changes nothing
		s.mu.Unlock()
		return buttonGone
	case action == "ready": // anyone's, as a reaction's join
		if !s.joinLocked(r, c.Sender) {
			s.mu.Unlock()
			return alreadyIn
		}
		s.armLobby(r)
		s.buttonsLocked(r)
		text := readyIn(len(r.Joined))
		s.mu.Unlock()
		s.save()
		return text
	case r.Starter != c.Sender:
		s.mu.Unlock()
		return notStarter
	}
	var text string
	switch action {
	case "start":
		joined := s.joinLocked(r, c.Sender) // the starter's press is their join
		if len(r.Joined) >= minPlayers {
			s.beginLocked(r)
			text = "Starting!"
		} else {
			s.armLobby(r)
			if joined {
				s.buttonsLocked(r)
			}
			how := "react to join"
			if pressed || r.Buttons {
				how = "press ✅ to join"
			}
			text = fmt.Sprintf("Waiting for at least %d players: %s.", minPlayers, how)
		}
	case "cancel": // as /geo end in the lobby, whose answer the chat sees
		s.callOffLocked(r, "")
		s.queue(addon.Op{Op: "post", Chat: r.Chat, Text: offText})
		text = "Cancelled."
	case "wait":
		if r.Buttons {
			s.mu.Unlock()
			return noCountdown
		}
		text = s.waitLocked(r)
	case "t1":
		if !r.Buttons {
			s.mu.Unlock()
			return ""
		}
		text = s.toggleRoundsLocked(r)
	default:
		s.mu.Unlock()
		return ""
	}
	s.mu.Unlock()
	s.save()
	return text
}

// waitLocked adds moreWait to r's lobby's wait, for its start armed or to
// come, but never past CallOff. s.mu is held.
func (s *server) waitLocked(r *round) string {
	at := s.clock.Now().Add(lobbyWait + r.Extra) // a start yet to come: as if a join now
	if len(r.Joined) >= minPlayers {
		at = r.LastJoin.Add(lobbyWait + r.Extra)
	}
	add := min(moreWait, r.CallOff.Sub(at)).Truncate(time.Second)
	if add <= 0 {
		return "Can't wait any longer."
	}
	r.Extra += add
	s.armLobby(r)
	if add < moreWait {
		return fmt.Sprintf("Starts %ds later: can't wait any longer.", int(add/time.Second))
	}
	return "Starts 30s later."
}

// callOffLocked ends r's game before round 1, saying why if why isn't "",
// and ends its watch. s.mu is held.
func (s *server) callOffLocked(r *round, why string) {
	if why != "" {
		s.queue(addon.Op{Op: "post", Chat: r.Chat, Text: why})
	}
	s.log.Printf("game %s called off before round 1: %d joined", r.ID, len(r.Joined))
	s.endGame(r)
}

// allGuessed reports whether a round after the first has a guess from
// each player of round 1, and is still before its deadline. s.mu is held.
func (s *server) allGuessed(r *round) bool {
	players := r.Players
	if len(r.Tie) > 0 {
		players = r.Tie
	}
	if len(players) == 0 || !s.clock.Now().Before(r.Deadline) { // none yet, in round 1 without a lobby
		return false
	}
	for _, p := range players {
		if _, ok := r.Guesses[p]; !ok {
			return false
		}
	}
	return true
}

// scoreboard is /geo scores: the chat's totals over the last scoreDays.
func (s *server) scoreboard(chat string) string {
	s.mu.Lock()
	s.pruneScores(s.clock.Now())
	type total struct {
		user           string
		points, rounds int
	}
	byUser := map[string]*total{}
	var totals []*total
	for _, sc := range s.scores[chat] {
		t := byUser[sc.User]
		if t == nil {
			t = &total{user: sc.User}
			byUser[sc.User] = t
			totals = append(totals, t)
		}
		t.points += sc.Points
		t.rounds++
	}
	s.mu.Unlock()
	if len(totals) == 0 {
		return "No scores in the last 7 days. Start a round with /geo."
	}
	// Scores are kept in the order they were won, so a tie goes to who
	// scored first.
	slices.SortStableFunc(totals, func(a, b *total) int { return cmp.Compare(b.points, a.points) })
	var b strings.Builder
	b.WriteString("🏆 **Scores, last 7 days**")
	for i, t := range totals[:min(len(totals), 10)] {
		rounds := "1 round"
		if t.rounds != 1 {
			rounds = strconv.Itoa(t.rounds) + " rounds"
		}
		fmt.Fprintf(&b, "\n%s {%s} %s · %s", medal(i+1), t.user, geo.Thousands(t.points), rounds)
	}
	return b.String()
}

// pruneScores forgets scores older than scoreDays. s.mu is held.
func (s *server) pruneScores(now time.Time) {
	since := now.Add(-scoreDays * 24 * time.Hour)
	for chat, list := range s.scores {
		s.scores[chat] = slices.DeleteFunc(list, func(sc score) bool { return sc.At.Before(since) })
		if len(s.scores[chat]) == 0 {
			delete(s.scores, chat)
		}
	}
}
