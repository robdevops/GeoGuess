package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/addon"
)

// lobbyClient is a fake clock and a client of a server playing games of
// one round, initialized by a bot that takes reactions, and shows buttons
// if buttons is set; its outbox is read, not subscribed to.
func lobbyClient(t *testing.T, dir string, buttons bool) (*fakeClock, *client) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	c := connect(t, testServer(t, dir, clk, &fakeFinder{}, &syncBuffer{}))
	c.result("initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"experimental": map[string]any{vendor + "addon": map[string]any{"version": 1, "watchReactions": true, "buttons": buttons}}},
	}, new(initializeResult))
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return clk, c
}

// press is user pressing the button with data under the lobby in chat:
// the pop-up.
func (c *client) press(clk clock, data, chat, user string) string {
	c.t.Helper()
	cc := from(clk, chat, user)
	cc.Ref = "r_lobby"
	return words(c.tool("geo_press", map[string]string{"press": data}, cc))
}

// lobby is alice's /geo in chat, once its photos are in: the game's ID.
func (c *client) lobby(clk clock, chat string) string {
	c.t.Helper()
	r := c.geo("1m", from(clk, chat, alice))
	if r.Meta == nil || r.Meta.Watch == nil || !r.Meta.Watch.Reactions {
		c.t.Fatalf("lobby %+v", r)
	}
	waitFor(c.t, c.s, "the game's photos", func() bool { return c.s.reserved(chat) == need(1) })
	return r.Meta.Watch.ID
}

// photos is how many round photos the outbox has for chat.
func (c *client) photos(chat string) int {
	c.t.Helper()
	n := 0
	for _, o := range c.outbox("0").Ops {
		if o.Op == "post" && o.Chat == chat && len(o.Images) == 1 {
			n++
		}
	}
	return n
}

// inLobby reports whether chat's game is still in its lobby.
func (s *server) inLobby(chat string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rounds[chat]
	return r != nil && r.Lobby
}

// rowsJSON is rows as JSON, to compare.
func rowsJSON(rows [][]addon.Button) string {
	b, _ := json.Marshal(rows)
	return string(b)
}

// lobbyRow is the one row of buttons under a lobby with them: Ready with
// ready joined, the rounds toggle, Start if start, and Cancel.
func lobbyRow(id string, ready int, rounds string, start bool) [][]addon.Button {
	row := []addon.Button{
		{Label: fmt.Sprintf("✅ %d", ready), Data: "ready:" + id},
		{Label: "Rounds: " + rounds, Data: "t1:" + id},
	}
	if start {
		row = append(row, addon.Button{Label: "▶️", Data: "start:" + id})
	}
	return [][]addon.Button{append(row, addon.Button{Label: "✖️", Data: "cancel:" + id})}
}

// buttonOps is the buttons ops in the outbox, from cursor.
func (c *client) buttonOps(cursor string) []outboxOp {
	c.t.Helper()
	var out []outboxOp
	for _, o := range c.outbox(cursor).Ops {
		if o.Op == "buttons" {
			out = append(out, o)
		}
	}
	return out
}

// The lobby's buttons are under its answer in a group, with a bot that
// shows them, in one row: Ready with none joined, the toggles and Cancel,
// no Start yet; not without the capability, and never in a DM. The intro
// says to press, never to react.
func TestLobbyButtons(t *testing.T) {
	clk, c := lobbyClient(t, t.TempDir(), true)
	r := c.geo("1m", from(clk, chat, alice))
	id := r.Meta.Watch.ID
	want := lobbyRow(id, 0, "1", false)
	if got := rowsJSON(r.Meta.Buttons); got != rowsJSON(want) || r.Meta.OnPress != "geo_press" || len(r.Meta.Buttons) != 1 {
		t.Errorf("buttons %s, onPress %q", got, r.Meta.OnPress)
	}
	if got := rowsJSON(r.Meta.Buttons); strings.Contains(got, "timer") || strings.Contains(got, "t2:") {
		t.Errorf("an answer timer button: %s", got)
	}
	for _, b := range lobbyRow(id, 99, "99", true)[0] {
		if len(b.Data) > 64 || len([]rune(b.Label)) > 32 {
			t.Errorf("%+v: too long", b)
		}
	}
	w := words(r)
	if !strings.Contains(w, "Press ✅ to join**, whoever started it too") || !strings.Contains(w, "the starter presses ▶️ when everyone's in (✖️ calls it off)") ||
		!strings.Contains(w, "change the rounds with the buttons") || strings.Contains(w, "timer") || strings.Contains(strings.ToLower(w), "react") || strings.Contains(w, "/geo wait") {
		t.Errorf("the intro with buttons: %q", w)
	}
	if got := words(c.geo("", from(clk, chat, bob))); got != "A game is waiting for players: press ✅ under its message to join." {
		t.Errorf("busy: %q", got)
	}
	if !strings.Contains(r.Meta.Watch.Until, "15:00:") && !strings.Contains(r.Meta.Watch.Until, "14:59:") { // lobbyMax and maxGame from 14:00
		t.Errorf("watch until %s", r.Meta.Watch.Until)
	}
	if d := c.geo("1m", dmFrom(clk, "c_dm", alice)); d.Meta == nil || d.Meta.Buttons != nil || d.Meta.OnPress != "" {
		t.Errorf("a DM has buttons: %+v", d.Meta)
	}

	clk, c = lobbyClient(t, t.TempDir(), false)
	r = c.geo("1m", from(clk, chat, alice))
	if r.Meta == nil || !r.Meta.Watch.Reactions || r.Meta.Buttons != nil || r.Meta.OnPress != "" {
		t.Errorf("buttons without the capability: %+v", r.Meta)
	}
	if !strings.Contains(words(r), "React to this message with any emoji to join**, whoever started it too") || strings.Contains(words(r), "✅") ||
		!strings.Contains(words(r), "/geo start") || !strings.Contains(words(r), "/geo wait") {
		t.Errorf("the intro without buttons: %q", words(r))
	}
	if got := words(c.geo("", from(clk, chat, bob))); got != "A game is waiting for players: react to its message to join, or /geo start." {
		t.Errorf("busy without buttons: %q", got)
	}
}

// joined is who has joined chat's lobby.
func (s *server) joined(chat string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rounds[chat]; r != nil {
		return slices.Clone(r.Joined)
	}
	return nil
}

// ✅: anyone's join, the starter's too, counted as a reaction's;
// twice is already in; with a reaction's join, also already in.
func TestPressReady(t *testing.T) {
	clk, c := lobbyClient(t, t.TempDir(), true)
	id := c.lobby(clk, chat)
	if got := c.press(clk, "ready:"+id, chat, bob); got != "You're in: 1 joined." {
		t.Errorf("bob's ready: %q", got)
	}
	if got := c.press(clk, "ready:"+id, chat, bob); got != "You're already in." {
		t.Errorf("bob again: %q", got)
	}
	join(c, clk, chat, "u_carol") // a reaction still counts
	if got := c.press(clk, "ready:"+id, chat, "u_carol"); got != "You're already in." {
		t.Errorf("carol, who reacted: %q", got)
	}
	if got := c.press(clk, "ready:"+id, chat, alice); got != "You're in: 3 joined." {
		t.Errorf("alice, the starter: %q", got)
	}
	if j := c.s.joined(chat); !slices.Equal(j, []string{bob, "u_carol", alice}) || !c.s.inLobby(chat) {
		t.Errorf("joined %v", j)
	}
	if got := c.press(clk, "ready:rold", chat, bob); got != "That game is over." {
		t.Errorf("an older game's ready: %q", got)
	}
}

// Each join, by ✅ or a reaction, and each leave re-queue the
// lobby's buttons for its watch, still one row: ▶️ appears with 2
// joined, and goes with fewer. No timer starts the game: ▶️ does,
// with those who joined. Then Ready says it has started, and after the
// game that it is over. The joins are saved, as a reaction's are.
func TestReadyRows(t *testing.T) {
	dir := t.TempDir()
	clk, c := lobbyClient(t, dir, true)
	id := c.lobby(clk, chat)
	if got := c.press(clk, "ready:"+id, chat, alice); got != "You're in: 1 joined." {
		t.Errorf("alice: %q", got)
	}
	ops := c.buttonOps("0")
	if len(ops) != 1 || ops[0].Chat != chat || ops[0].Watch != id || rowsJSON(ops[0].Buttons) != rowsJSON(lobbyRow(id, 1, "1", false)) {
		t.Fatalf("after alice: %+v", ops)
	}
	clk.Advance(10 * time.Second)
	c.press(clk, "ready:"+id, chat, bob)
	ops = c.buttonOps("0")
	if len(ops) != 2 || ops[1].Watch != id || rowsJSON(ops[1].Buttons) != rowsJSON(lobbyRow(id, 2, "1", true)) || len(ops[1].Buttons) != 1 {
		t.Fatalf("after bob: %+v", ops)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "geo.json"))
	if err != nil || !strings.Contains(string(saved), bob) {
		t.Errorf("bob's join not saved: %v", err)
	}
	c.result("tools/call", map[string]any{"name": "geo_guess", "arguments": map[string]any{"reaction": "👍", "removed": true},
		"_meta": map[string]any{vendor + "context": from(clk, chat, bob)}}, new(toolResult))
	join(c, clk, chat, "u_carol")
	ops = c.buttonOps("0")
	if len(ops) != 4 || rowsJSON(ops[2].Buttons) != rowsJSON(lobbyRow(id, 1, "1", false)) || rowsJSON(ops[3].Buttons) != rowsJSON(lobbyRow(id, 2, "1", true)) {
		t.Fatalf("bob's reaction taken back, carol's: %+v", ops)
	}
	clk.Advance(lobbyWait * 3)
	if !c.s.inLobby(chat) || c.photos(chat) != 0 {
		t.Fatal("the lobby's timer started the game")
	}
	if got := c.press(clk, "start:"+id, chat, alice); got != "Starting!" {
		t.Errorf("start: %q", got)
	}
	opened(t, c.s, chat, 1)
	c.s.mu.Lock()
	players := slices.Clone(c.s.rounds[chat].Players)
	c.s.mu.Unlock()
	if !slices.Equal(players, []string{alice, "u_carol"}) {
		t.Errorf("players %v", players)
	}
	if got := c.press(clk, "ready:"+id, chat, "u_dave"); got != "That game has already started." {
		t.Errorf("ready after the start: %q", got)
	}
	c.geo("end", from(clk, chat, alice))
	if got := c.press(clk, "ready:"+id, chat, "u_dave"); got != "That game is over." {
		t.Errorf("ready after the game: %q", got)
	}
}

// A ✅ racing ▶️ starts the game once, and the player is
// either in it or told it has started.
func TestReadyRaces(t *testing.T) {
	for range 20 {
		clk, c := lobbyClient(t, t.TempDir(), true)
		id := c.lobby(clk, chat)
		join(c, clk, chat, alice)
		join(c, clk, chat, bob)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The start pressed: the tool's work, as the client is one caller at a time.
			cc := &addon.Context{Chat: chat, Sender: alice, Group: true}
			c.s.lobbyAct(cc, "start", id, true)
		}()
		got := c.press(clk, "ready:"+id, chat, "u_carol")
		wg.Wait()
		opened(t, c.s, chat, 1)
		c.s.mu.Lock()
		players := slices.Clone(c.s.rounds[chat].Players)
		c.s.mu.Unlock()
		switch {
		case got == "You're in: 3 joined." && len(players) == 3:
		case got == "That game has already started." && len(players) == 2:
		default:
			t.Errorf("ready %q, players %v", got, players)
		}
		if n := c.photos(chat); n != 1 {
			t.Fatalf("%d round 1 photos", n)
		}
		c.close()
	}
}

// ▶️: the starter's only; their press is their join; with 2 joined
// the game starts at once, round 1's photo following; with fewer the lobby
// goes on. After the start, and under an older game's lobby, a press does
// nothing.
func TestPressStart(t *testing.T) {
	clk, c := lobbyClient(t, t.TempDir(), true)
	id := c.lobby(clk, chat)
	if got := c.press(clk, "start:"+id, chat, bob); got != "Only the player who started this game can do that." {
		t.Errorf("bob's press: %q", got)
	}
	if got := c.press(clk, "start:"+id, chat, alice); got != "Waiting for at least 2 players: press ✅ to join." {
		t.Errorf("alice alone: %q", got)
	}
	c.s.mu.Lock()
	joined := c.s.rounds[chat].Joined
	c.s.mu.Unlock()
	if len(joined) != 1 || joined[0] != alice || !c.s.inLobby(chat) || c.photos(chat) != 0 {
		t.Fatalf("after alice's press: joined %v", joined)
	}
	join(c, clk, chat, bob)
	if got := c.press(clk, "start:"+id, chat, alice); got != "Starting!" {
		t.Errorf("alice with bob: %q", got)
	}
	opened(t, c.s, chat, 1) // no clock moved: at once
	if n := c.photos(chat); n != 1 {
		t.Errorf("%d photos", n)
	}
	c.s.mu.Lock()
	if p := c.s.rounds[chat].Players; len(p) != 2 {
		t.Errorf("players %v", p)
	}
	c.s.mu.Unlock()
	for _, data := range []string{"start:" + id, "cancel:" + id, "wait:" + id, "t1:" + id, "t2:" + id} {
		if got := c.press(clk, data, chat, alice); got != "That game has already started." {
			t.Errorf("%s after the start: %q", data, got)
		}
	}
	if got := c.press(clk, "start:"+id, chat, bob); got != "That game has already started." {
		t.Errorf("bob after the start: %q", got)
	}
	clk.Advance(lobbyMax) // the lobby's timer, stopped, does nothing more
	if n := c.photos(chat); n != 1 {
		t.Errorf("%d photos after lobbyWait", n)
	}
	for _, data := range []string{"start:rold", "wait:rold", "t1:rold"} {
		if got := c.press(clk, data, chat, alice); got != "That game is over." {
			t.Errorf("an older game's %s: %q", data, got)
		}
	}
	c.geo("end", from(clk, chat, alice))
	if got := c.press(clk, "start:"+id, chat, alice); got != "That game is over." {
		t.Errorf("after the game: %q", got)
	}
}

// /geo start is ▶️ typed, by the same rules.
func TestTypedStart(t *testing.T) {
	clk, c := lobbyClient(t, t.TempDir(), false)
	if got := words(c.geo("start", from(clk, chat, alice))); got != noRound {
		t.Errorf("no game: %q", got)
	}
	c.lobby(clk, chat)
	if got := words(c.geo("start", from(clk, chat, bob))); got != "Only the player who started this game can do that." {
		t.Errorf("bob: %q", got)
	}
	if got := words(c.geo("start", from(clk, chat, alice))); got != "Waiting for at least 2 players: react to join." {
		t.Errorf("alice alone: %q", got)
	}
	join(c, clk, chat, bob)
	if got := words(c.geo("START", from(clk, chat, alice))); got != "Starting!" {
		t.Errorf("alice with bob: %q", got)
	}
	opened(t, c.s, chat, 1)
	if got := words(c.geo("start", from(clk, chat, alice))); got != "That game has already started." {
		t.Errorf("after the start: %q", got)
	}
	if got := words(c.geo("wait", from(clk, chat, alice))); got != "That game has already started." {
		t.Errorf("wait after the start: %q", got)
	}
}

// The photos not in when the starter starts: the game says it is still
// fetching, as when the lobby's timer runs out, and round 1 follows.
func TestPressStartFetching(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := fed{&fakeFinder{}, make(chan struct{})}
	c := fetchClient(t, t.TempDir(), clk, photos, 3, true)
	c.geo("1m", from(clk, chat, alice))
	photos.feed <- struct{}{}
	waitFor(t, c.s, "a photo", func() bool { return c.s.reserved(chat) == 1 })
	join(c, clk, chat, bob)
	if got := words(c.geo("start", from(clk, chat, alice))); got != "Starting!" {
		t.Fatalf("start: %q", got)
	}
	page := c.outbox("0")
	if len(page.Ops) != 1 || page.Ops[0].Text != "🔎 Still fetching: 1 of 3 spots ready…" {
		t.Fatalf("at the start %+v", page)
	}
	photos.feed <- struct{}{}
	photos.feed <- struct{}{}
	opened(t, c.s, chat, 1)
}

// ✖️: the starter's only; it calls the lobby off as /geo end does,
// saying so in the chat; /geo stop and /geo end still do.
func TestPressCancel(t *testing.T) {
	clk, c := lobbyClient(t, t.TempDir(), true)
	id := c.lobby(clk, chat)
	if got := c.press(clk, "cancel:"+id, chat, bob); got != "Only the player who started this game can do that." {
		t.Errorf("bob: %q", got)
	}
	if !c.s.inLobby(chat) {
		t.Fatal("bob's press ended the lobby")
	}
	if got := c.press(clk, "cancel:"+id, chat, alice); got != "Cancelled." {
		t.Errorf("alice: %q", got)
	}
	ops := c.outbox("0").Ops
	if len(ops) != 2 || ops[0].Op != "unwatch" || ops[0].Watch != id || ops[1].Op != "post" || ops[1].Text != "The game is off." {
		t.Errorf("ops %+v", ops)
	}
	c.s.mu.Lock()
	gone := c.s.rounds[chat] == nil
	c.s.mu.Unlock()
	if !gone {
		t.Error("the game is still on")
	}
	if got := c.press(clk, "cancel:"+id, chat, alice); got != "That game is over." {
		t.Errorf("again: %q", got)
	}
	c.lobby(clk, chat)
	if got := words(c.geo("stop", from(clk, chat, bob))); got != "Only the player who started this round can end it." {
		t.Errorf("bob's /geo stop: %q", got)
	}
	if got := words(c.geo("stop", from(clk, chat, alice))); got != "The game is off." {
		t.Errorf("alice's /geo stop: %q", got)
	}
}

// /geo wait, or ⏱ +30s under a lobby from before the buttons' one row:
// the starter's only; each moves the start 30s later, an armed one or one
// to come, but never past the lobby's call-off, and the extra wait is
// saved. With buttons there is no countdown to move.
func TestPressWait(t *testing.T) {
	clk, c := lobbyClient(t, t.TempDir(), true)
	id := c.lobby(clk, chat)
	if got := words(c.geo("wait", from(clk, chat, alice))); got != "There's no countdown: the starter presses ▶️ when everyone's in." {
		t.Errorf("/geo wait with buttons: %q", got)
	}
	if got := c.press(clk, "wait:"+id, chat, alice); got != "There's no countdown: the starter presses ▶️ when everyone's in." {
		t.Errorf("a +30s press with buttons: %q", got)
	}
	c.close()

	dir := t.TempDir()
	clk, c = lobbyClient(t, dir, false)
	id = c.lobby(clk, chat)
	if got := c.press(clk, "wait:"+id, chat, bob); got != "Only the player who started this game can do that." {
		t.Errorf("bob: %q", got)
	}
	if got := words(c.geo("wait", from(clk, chat, bob))); got != "Only the player who started this game can do that." {
		t.Errorf("bob's /geo wait: %q", got)
	}
	// Before 2 have joined: the start to come is 30s later.
	if got := c.press(clk, "wait:"+id, chat, alice); got != "Starts 30s later." {
		t.Errorf("alice: %q", got)
	}
	join(c, clk, chat, alice)
	join(c, clk, chat, bob)
	// Armed: lobbyWait and the 30s already asked, and 30s more typed.
	if got := words(c.geo("wait", from(clk, chat, alice))); got != "Starts 30s later." {
		t.Errorf("/geo wait: %q", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "geo.json"))
	if err != nil || !strings.Contains(string(data), `"extra": 60000000000`) {
		t.Errorf("the extra wait isn't saved: %v %.300s", err, data)
	}
	clk.Advance(lobbyWait + 59*time.Second)
	if !c.s.inLobby(chat) || c.photos(chat) != 0 {
		t.Fatal("started before its extra wait")
	}
	clk.Advance(time.Second)
	opened(t, c.s, chat, 1)
	c.geo("end", from(clk, chat, alice))

	// Never past the call-off.
	id = c.lobby(clk, chat)
	clk.Advance(lobbyMax - lobbyWait - 50*time.Second)
	if got := c.press(clk, "wait:"+id, chat, alice); got != "Starts 30s later." {
		t.Errorf("50s before the limit: %q", got)
	}
	if got := c.press(clk, "wait:"+id, chat, alice); got != "Starts 20s later: can't wait any longer." {
		t.Errorf("20s before the limit: %q", got)
	}
	if got := c.press(clk, "wait:"+id, chat, alice); got != "Can't wait any longer." {
		t.Errorf("at the limit: %q", got)
	}
	join(c, clk, chat, alice)
	join(c, clk, chat, bob)
	clk.Advance(lobbyWait + 49*time.Second)
	if !c.s.inLobby(chat) {
		t.Fatal("started before the call-off")
	}
	clk.Advance(time.Second) // at the call-off, with 2 joined: it starts
	opened(t, c.s, chat, 1)
}

// A press and the lobby's timer, without buttons, at the same moment
// start the game once.
func TestPressRacesTimer(t *testing.T) {
	for range 20 {
		clk, c := lobbyClient(t, t.TempDir(), false)
		id := c.lobby(clk, chat)
		join(c, clk, chat, alice)
		join(c, clk, chat, bob)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			clk.Advance(lobbyWait)
		}()
		got := c.press(clk, "start:"+id, chat, alice)
		wg.Wait()
		if got != "Starting!" && got != "That game has already started." {
			t.Errorf("press: %q", got)
		}
		opened(t, c.s, chat, 1)
		if n := c.photos(chat); n != 1 {
			t.Fatalf("%d round 1 photos", n)
		}
		c.close()
	}
}

// A second /geo in the lobby says the game is waiting for players, and
// how to join or start it; once it has started, how long its round has.
func TestBusyLobby(t *testing.T) {
	clk, c := lobbyClient(t, t.TempDir(), false)
	c.lobby(clk, chat)
	if got := words(c.geo("", from(clk, chat, bob))); got != "A game is waiting for players: react to its message to join, or /geo start." {
		t.Errorf("in the lobby: %q", got)
	}
	join(c, clk, chat, alice)
	join(c, clk, chat, bob)
	clk.Advance(lobbyWait)
	opened(t, c.s, chat, 1)
	if got := words(c.geo("", from(clk, chat, bob))); got != "A round is on: 1m0s left." && got != "A round is on: 1m left." {
		t.Errorf("in round 1: %q", got)
	}
}

// The rounds toggle: the starter's only; it flips between what the lobby
// opened with and double that, re-queuing the buttons for its watch; the
// game that starts plays the toggled rounds, of the length /geo gave,
// with a photo for each round and the spare. A press of the answer timer
// toggle, gone, from an older lobby, says so and changes nothing.
func TestToggles(t *testing.T) {
	dir := t.TempDir()
	clk, c := lobbyClient(t, dir, true)
	c.games(2)
	r := c.geo("2m", from(clk, chat, alice))
	id := r.Meta.Watch.ID
	waitFor(t, c.s, "the game's photos", func() bool { return c.s.reserved(chat) == need(2) })
	if got := c.press(clk, "t1:"+id, chat, bob); got != "Only the player who started this game can do that." {
		t.Errorf("bob's t1: %q", got)
	}
	if ops := c.buttonOps("0"); len(ops) != 0 {
		t.Fatalf("bob's presses changed the buttons: %+v", ops)
	}
	if got := c.press(clk, "t1:"+id, chat, alice); got != "Rounds: 4" {
		t.Errorf("rounds: %q", got)
	}
	for _, user := range []string{alice, bob} {
		if got := c.press(clk, "t2:"+id, chat, user); got != buttonGone {
			t.Errorf("%s's timer: %q", user, got)
		}
	}
	ops := c.buttonOps("0")
	if len(ops) != 1 || ops[0].Watch != id || rowsJSON(ops[0].Buttons) != rowsJSON(lobbyRow(id, 0, "4", false)) {
		t.Fatalf("after the toggles: %+v", ops)
	}
	if !strings.HasPrefix(ops[0].Text, "🌍 **GeoGuess** · 4 rounds of 2 min") || !strings.Contains(ops[0].Text, "Press ✅ to join") {
		t.Errorf("the intro after the toggle: %q", ops[0].Text)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "geo.json"))
	if err != nil || !strings.Contains(string(saved), `"of": 4`) {
		t.Errorf("the toggle isn't saved: %v", err)
	}
	// Back and forth: the spares go back to the stock, and come again.
	if got := c.press(clk, "t1:"+id, chat, alice); got != "Rounds: 2" {
		t.Errorf("rounds back: %q", got)
	}
	if ops := c.buttonOps("0"); len(ops) != 2 || !strings.HasPrefix(ops[1].Text, "🌍 **GeoGuess** · 2 rounds of 2 min") {
		t.Errorf("the intro back: %+v", ops)
	}
	c.s.mu.Lock()
	n := c.s.reserved(chat)
	c.s.mu.Unlock()
	if n != need(2) {
		t.Errorf("%d photos reserved for 2 rounds", n)
	}
	if got := c.press(clk, "t1:"+id, chat, alice); got != "Rounds: 4" {
		t.Errorf("rounds again: %q", got)
	}
	waitFor(t, c.s, "4 rounds' photos", func() bool { return c.s.reserved(chat) == need(4) })
	join(c, clk, chat, bob)
	if got := c.press(clk, "start:"+id, chat, alice); got != "Starting!" {
		t.Fatalf("start: %q", got)
	}
	opened(t, c.s, chat, 1)
	c.s.mu.Lock()
	g := c.s.rounds[chat]
	of, length := g.of(), g.Deadline.Sub(g.Started)
	c.s.mu.Unlock()
	if of != 4 || length != 2*time.Minute {
		t.Errorf("the game: %d rounds of %s", of, length)
	}
	var round1 string
	for _, o := range c.outbox("0").Ops {
		if o.Op == "post" && len(o.Images) == 1 {
			round1 = o.Text
		}
	}
	if !strings.HasPrefix(round1, "**Round 1 of 4** · ") {
		t.Errorf("round 1: %q", round1)
	}
}

// The rounds toggle's second setting is capped by maxGame; at the most,
// it changes nothing.
func TestTogglesCapped(t *testing.T) {
	for _, tc := range []struct {
		rounds int
		args   string
		t1     string
	}{
		{1, "30m", alreadyMost}, // a round of 30m: no room for 2
		{1, "25m", alreadyMost},
		{2, "10m", "Rounds: 3"}, // 4 rounds of 10m would run past maxGame
	} {
		clk, c := lobbyClient(t, t.TempDir(), true)
		c.games(tc.rounds)
		id := c.geo(tc.args, from(clk, chat, alice)).Meta.Watch.ID
		if got := c.press(clk, "t1:"+id, chat, alice); got != tc.t1 {
			t.Errorf("%d of %s: rounds %q", tc.rounds, tc.args, got)
		}
		c.s.mu.Lock()
		r := c.s.rounds[chat]
		fits := gameTime(r.of(), r.Length, c.s.cfg.gap) <= maxGame
		c.s.mu.Unlock()
		if !fits {
			t.Errorf("%d of %s: too long a game", tc.rounds, tc.args)
		}
		if tc.t1 == alreadyMost && len(c.buttonOps("0")) != 0 {
			t.Errorf("%d of %s: ops %+v", tc.rounds, tc.args, c.buttonOps("0"))
		}
		c.close()
	}
}

// With buttons the lobby is called off after lobbyMax, whoever joined;
// with 2 joined and no ▶️, it says nobody started it.
func TestButtonsCallOff(t *testing.T) {
	for _, tc := range []struct {
		joins []string
		why   string
	}{
		{[]string{alice}, "Not enough players joined, so the game is off."},
		{[]string{alice, bob}, "Nobody pressed ▶️ in time, so the game is off."},
	} {
		clk, c := lobbyClient(t, t.TempDir(), true)
		id := c.lobby(clk, chat)
		for _, u := range tc.joins {
			c.press(clk, "ready:"+id, chat, u)
		}
		clk.Advance(lobbyMax - time.Second)
		if !c.s.inLobby(chat) || c.photos(chat) != 0 {
			t.Fatalf("%v: not in the lobby before lobbyMax", tc.joins)
		}
		clk.Advance(time.Second)
		waitFor(t, c.s, "the call-off", func() bool { return c.s.rounds[chat] == nil })
		var posts []string
		for _, o := range c.outbox("0").Ops {
			if o.Op == "post" {
				posts = append(posts, o.Text)
			}
		}
		if !slices.Equal(posts, []string{tc.why}) {
			t.Errorf("%v: posts %q", tc.joins, posts)
		}
		c.close()
	}
}

// Typed /geo start with too few, on a network with buttons, says to press
// ✅, and the starter's join re-queues the buttons.
func TestTypedStartButtons(t *testing.T) {
	clk, c := lobbyClient(t, t.TempDir(), true)
	id := c.lobby(clk, chat)
	if got := words(c.geo("start", from(clk, chat, alice))); got != "Waiting for at least 2 players: press ✅ to join." {
		t.Errorf("alone: %q", got)
	}
	ops := c.buttonOps("0")
	if len(ops) != 1 || rowsJSON(ops[0].Buttons) != rowsJSON(lobbyRow(id, 1, "1", false)) {
		t.Errorf("ops %+v", ops)
	}
}
