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

	"github.com/robdevops/chat-plugin-geoguess/geo"
	"github.com/robdevops/signalbot/addon"
)

// The texts players see.
const (
	guard     = "No hints during a round 😉"
	noChat    = "GeoGuess works only in a chat."
	noRound   = "There's no round on. Start one with /geo."
	noPhoto   = "I couldn't find a good spot in time; I'm still looking, so try /geo again in a minute."
	badLength = "A round can be 30s to 30m, such as /geo 90s or /geo 5m."
	usage     = "Try /geo, /geo 90s, /geo end, /geo skip, /geo scores, or a region such as /geo europe or /geo AU,NZ."
	longGame  = "That game would run too long: ask for shorter rounds, such as /geo 5m."
)

// pin is the reaction to a recorded guess.
const pin = "📍"

// replyTime is what /geo leaves before the bot's deadline to start the
// round and send the photo, so it never starts one the bot won't show.
const replyTime = 3 * time.Second

// tools are GeoGuess's tools, both for the bot alone: neither is offered
// to the model.
func (s *server) tools() []addon.Tool {
	return []addon.Tool{
		{
			Name:        "geo",
			Description: "The /geo command: start, end or skip this chat's round, or show its scores.",
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
					Text string `json:"text"`
				}
				if err := addon.DecodeArgs(raw, &a); err != nil {
					return addon.Result{}, err
				}
				return s.guess(c, a.Text), nil
			},
		},
	}
}

// fromChat reports whether c names a chat and a sender.
func fromChat(c *addon.Context) bool {
	return c != nil && validID(c.Chat, "c_") && validID(c.Sender, "u_")
}

// command is /geo with its arguments.
func (s *server) command(ctx context.Context, c *addon.Context, args string) addon.Result {
	if !fromChat(c) {
		return addon.Result{Content: addon.Text(noChat).Content, IsError: true}
	}
	fields := strings.Fields(args)
	if len(fields) == 1 {
		switch strings.ToLower(fields[0]) {
		case "end":
			return s.finish(c, false)
		case "skip":
			return s.finish(c, true)
		case "scores":
			return addon.Text(s.scoreboard(c.Chat))
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
	if of := s.perGame; gameTime(of, length, s.cfg.Wait+s.cfg.gap) > maxGame {
		return addon.Text(longGame)
	}
	return s.start(ctx, c, length, region)
}

// busy is the answer to /geo while a game is on.
func busy(r *round, now time.Time) addon.Result {
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

// start begins a round in c's chat with a photo from the buffer, and
// answers with the photo, its caption and the watch for guesses.
func (s *server) start(ctx context.Context, c *addon.Context, length time.Duration, region string) addon.Result {
	s.mu.Lock()
	r := s.rounds[c.Chat]
	s.mu.Unlock()
	if r != nil {
		return busy(r, s.clock.Now())
	}
	wait := s.cfg.Wait
	if c.Deadline != 0 { // leave the bot time to send the photo
		wait = min(wait, time.Until(time.Unix(c.Deadline, 0))-replyTime)
	}
	photo, ok := s.take(ctx, region, wait)
	if !ok {
		return addon.Text(noPhoto)
	}
	s.mu.Lock()
	now := s.clock.Now()
	r = s.rounds[c.Chat]
	if r != nil || ctx.Err() != nil { // another /geo began one meanwhile, or the bot gave up
		s.buffer = append([]geo.Photo{photo}, s.buffer...)
		close(s.changed) // for a /geo waiting in another chat
		s.changed = make(chan struct{})
		s.mu.Unlock()
		if r == nil {
			return addon.Text(noPhoto)
		}
		return busy(r, now)
	}
	of := s.perGame
	r = &round{
		ID: "r" + randomID(6), Chat: c.Chat, Starter: c.Sender,
		Started: now, Deadline: now.Add(length),
		Answer: photo.At, Place: photo.Place.Name,
		Credit:  credit{Creator: photo.Creator, Source: photo.Source, Licence: photo.Licence, Link: photo.Link},
		Guesses: map[string]guess{},
		Number:  1, Of: of, Region: region, Length: length, Totals: map[string]int{},
		Until: now.Add(gameTime(of, length, s.cfg.Wait+s.cfg.gap)),
	}
	s.rounds[c.Chat] = r
	s.arm(r)
	s.mu.Unlock()
	s.save()
	s.log.Printf("round %s started: %s, a %s photo", r.ID, length, photo.Source)
	return addon.Result{
		Content: []addon.Content{
			{Type: "image", Data: base64.StdEncoding.EncodeToString(photo.JPEG), MimeType: "image/jpeg"},
			{Type: "text", Text: roundLine(r) + caption(r.Deadline, length, s.srv.Network())},
		},
		Meta: &addon.ResultMeta{Watch: &addon.Watch{
			ID: r.ID, Pattern: geo.Pattern, Until: r.Until.Add(watchSlack).Format(time.RFC3339),
			Tool: "geo_guess", Guard: guard,
		}},
	}
}

// gameTime is the longest a game of of rounds of length may run: a
// tie-breaker round after them, if one is needed, and the waits for each
// later round's photo.
func gameTime(of int, length, wait time.Duration) time.Duration {
	if of == 1 {
		return length + settle
	}
	return time.Duration(of+1)*(length+settle) + time.Duration(of)*wait
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

// caption is the photo's caption: how to guess, on the chat network the
// bot said it is on, and by when.
func caption(deadline time.Time, length time.Duration, network string) string {
	steps := "Signal: **+** → Location; Telegram: 📎 → Location. Pinch to zoom out, **drag the pin**, confirm (on Signal), then send."
	switch network {
	case "signal":
		steps = "Tap **+** → Location, pinch to zoom out, **drag the pin**, confirm, then send."
	case "telegram":
		steps = "Tap 📎 → Location, pinch to zoom out, **drag the pin**, then send."
	}
	return "🌍 **Where is this?** Send a location before " + deadline.Local().Format("15:04") + " (" + lengthLabel(length) + ").\n" +
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

// arm sets r's timer to reveal its open round once its time is up,
// replacing any timer it had. s.mu is held.
func (s *server) arm(r *round) {
	chat, id, number := r.Chat, r.ID, r.Number
	if t := s.timers[chat]; t != nil {
		t.Stop()
	}
	s.timers[chat] = s.clock.AfterFunc(r.Deadline.Add(settle).Sub(s.clock.Now()), func() { s.timeUp(chat, id, number) })
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

// advanceLocked queues the results of r's round just closed, then asks for
// the next round's photo; after the last round, the game's totals too, and
// the end of its watch. s.mu is held.
func (s *server) advanceLocked(r *round, results string) {
	tie := tied(r)
	if r.Number == r.of() && len(tie) > 1 {
		names := make([]string, len(tie))
		for i, u := range tie {
			names[i] = "{" + u + "}"
		}
		results += "\n\n🤝 " + strings.Join(names, ", ") + fmt.Sprintf(" are tied on %d %s: a tie-breaker round decides it, and only they can win it.", r.Wins[tie[0]], plural(r.Wins[tie[0]], "win", "wins"))
		r.Tie = tie
	}
	if r.Number < r.of() || len(r.Tie) > 0 && r.Number == r.of() {
		s.queue(addon.Op{Op: "post", Chat: r.Chat, Text: results})
		r.Waiting, r.Closed = true, s.clock.Now()
		go s.nextRound(r.Chat, r.ID, r.Number+1)
		return
	}
	s.queue(addon.Op{Op: "post", Chat: r.Chat, Text: results + totals(r)})
	s.endGame(r)
}

// endGame forgets r's game and queues the end of its watch. s.mu is held.
func (s *server) endGame(r *round) {
	delete(s.rounds, r.Chat)
	if t := s.timers[r.Chat]; t != nil {
		t.Stop()
		delete(s.timers, r.Chat)
	}
	s.queue(addon.Op{Op: "unwatch", Chat: r.Chat, Watch: r.ID})
}

// nextRound opens round number of the game id in chat with a photo from
// the buffer, waiting up to GEO_WAIT for one, and queues the photo; with
// none, or no time left on the game's watch, it ends the game.
func (s *server) nextRound(chat, id string, number int) {
	s.mu.Lock()
	r := s.rounds[chat]
	if r == nil || r.ID != id || r.Number != number-1 || !r.Waiting {
		s.mu.Unlock()
		return
	}
	region := r.Region
	s.mu.Unlock()
	photo, ok := s.take(context.Background(), region, s.cfg.Wait)
	s.mu.Lock()
	r = s.rounds[chat]
	if r == nil || r.ID != id || r.Number != number-1 || !r.Waiting { // ended meanwhile
		if ok {
			s.buffer = append([]geo.Photo{photo}, s.buffer...)
			close(s.changed)
			s.changed = make(chan struct{})
		}
		s.mu.Unlock()
		return
	}
	if !ok {
		s.queue(addon.Op{Op: "post", Chat: chat, Text: fmt.Sprintf("I couldn't find a spot for round %d, so the game ends here.", number) + totals(r)})
		s.endGame(r)
		s.mu.Unlock()
		s.save()
		return
	}
	if wait := r.Closed.Add(s.cfg.gap).Sub(s.clock.Now()); wait > 0 { // a pause to read the results
		s.clock.AfterFunc(wait, func() {
			s.mu.Lock()
			r := s.rounds[chat]
			if r == nil || r.ID != id || r.Number != number-1 || !r.Waiting {
				s.mu.Unlock()
				return
			}
			s.openLocked(r, number, photo)
			s.mu.Unlock()
			s.save()
		})
		s.mu.Unlock()
		return
	}
	s.openLocked(r, number, photo)
	s.mu.Unlock()
	s.save()
}

// openLocked opens round number of r's game with photo, and queues the
// photo; with no time left on the game's watch, it ends the game. s.mu is
// held.
func (s *server) openLocked(r *round, number int, photo geo.Photo) {
	chat := r.Chat
	now := s.clock.Now()
	length := min(r.Length, r.Until.Sub(now)-settle)
	switch {
	case length < minRound:
		s.buffer = append([]geo.Photo{photo}, s.buffer...)
		close(s.changed)
		s.changed = make(chan struct{})
		s.queue(addon.Op{Op: "post", Chat: chat, Text: fmt.Sprintf("No time is left for round %d, so the game ends here.", number) + totals(r)})
		s.endGame(r)
	default:
		r.Number, r.Waiting = number, false
		r.Started, r.Deadline = now, now.Add(length)
		r.Answer, r.Place, r.Guesses = photo.At, photo.Place.Name, map[string]guess{}
		r.Credit = credit{Creator: photo.Creator, Source: photo.Source, Licence: photo.Licence, Link: photo.Link}
		s.arm(r)
		s.queue(addon.Op{Op: "post", Chat: chat, Text: roundLine(r) + caption(r.Deadline, length, s.srv.Network()),
			Images: []addon.Image{{Data: base64.StdEncoding.EncodeToString(photo.JPEG), MimeType: "image/jpeg", Name: "geoguess.jpg"}}})
		s.log.Printf("round %d of game %s started: %s, a %s photo", number, r.ID, length, photo.Source)
	}
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
	}
	var text string
	switch {
	case skip && r.Waiting:
		s.mu.Unlock()
		return addon.Text(fmt.Sprintf("Round %d's photo is on its way.", r.Number+1))
	case skip && r.Number < r.of(): // the next round follows, as at the round's end
		text = s.closeRound(r, false)
		r.Waiting, r.Closed = true, s.clock.Now()
		go s.nextRound(r.Chat, r.ID, r.Number+1)
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
	if r.Number <= 1 {
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
	case c.Sent > 0 && c.Sent < r.Started.Unix():
		s.mu.Unlock()
		return addon.Text("That message is from before this round.")
	case at.After(r.Deadline):
		s.mu.Unlock()
		return addon.Text("Too late for this round.")
	}
	r.Guesses[c.Sender] = guess{Km: geo.Km(p, r.Answer), At: at}
	if s.allGuessed(r) {
		// Everyone playing has guessed: end the round now, revealing it after
		// settle, as at its deadline, for guesses still on their way.
		r.Deadline = s.clock.Now()
		s.arm(r)
		s.log.Printf("round %d of game %s ended early", r.Number, r.ID)
	}
	s.mu.Unlock()
	s.save()
	return addon.Result{Content: addon.Text("Recorded.").Content, Meta: &addon.ResultMeta{React: pin}}
}

// allGuessed reports whether a round after the first has a guess from
// each player of round 1, and is still before its deadline. s.mu is held.
func (s *server) allGuessed(r *round) bool {
	players := r.Players
	if len(r.Tie) > 0 {
		players = r.Tie
	}
	if r.Number <= 1 || len(players) == 0 || !s.clock.Now().Before(r.Deadline) {
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
