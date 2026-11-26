package main

import (
	"encoding/json"
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

// The lobby's buttons are under its answer in a group, with a bot that
// shows them; not without the capability, and never in a DM.
func TestLobbyButtons(t *testing.T) {
	clk, c := lobbyClient(t, t.TempDir(), true)
	r := c.geo("1m", from(clk, chat, alice))
	id := r.Meta.Watch.ID
	want := [][]addon.Button{
		{{Label: "✋ Ready", Data: "ready:" + id}},
		{{Label: "▶️ Start", Data: "start:" + id}, {Label: "✖️ Cancel", Data: "cancel:" + id}, {Label: "⏱ +30s", Data: "wait:" + id}},
	}
	got, _ := json.Marshal(r.Meta.Buttons)
	w, _ := json.Marshal(want)
	if string(got) != string(w) || r.Meta.OnPress != "geo_press" {
		t.Errorf("buttons %s, onPress %q", got, r.Meta.OnPress)
	}
	for _, row := range want {
		for _, b := range row {
			if len(b.Data) > 64 {
				t.Errorf("%s: %d bytes", b.Data, len(b.Data))
			}
		}
	}
	if !strings.Contains(words(r), "/geo start") || !strings.Contains(words(r), "/geo wait") {
		t.Errorf("the intro doesn't give the typed commands: %q", words(r))
	}
	if !strings.Contains(words(r), "Press ✋ Ready to join**, whoever started it too") || strings.Contains(words(r), "React to this message") {
		t.Errorf("the intro with buttons: %q", words(r))
	}
	if d := c.geo("1m", dmFrom(clk, "c_dm", alice)); d.Meta == nil || d.Meta.Buttons != nil || d.Meta.OnPress != "" {
		t.Errorf("a DM has buttons: %+v", d.Meta)
	}

	clk, c = lobbyClient(t, t.TempDir(), false)
	r = c.geo("1m", from(clk, chat, alice))
	if r.Meta == nil || !r.Meta.Watch.Reactions || r.Meta.Buttons != nil || r.Meta.OnPress != "" {
		t.Errorf("buttons without the capability: %+v", r.Meta)
	}
	if !strings.Contains(words(r), "React to this message with any emoji to join**, whoever started it too") || strings.Contains(words(r), "Ready") {
		t.Errorf("the intro without buttons: %q", words(r))
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

// ✋ Ready: anyone's join, the starter's too, counted as a reaction's;
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

// Two players by ✋ Ready, the lobby's timer starts the game with both,
// lobbyWait after the last; then Ready says it has started, and after
// the game that it is over. The joins are saved, as a reaction's are.
func TestReadyStarts(t *testing.T) {
	dir := t.TempDir()
	clk, c := lobbyClient(t, dir, true)
	id := c.lobby(clk, chat)
	c.press(clk, "ready:"+id, chat, alice)
	clk.Advance(10 * time.Second)
	c.press(clk, "ready:"+id, chat, bob)
	saved, err := os.ReadFile(filepath.Join(dir, "geo.json"))
	if err != nil || !strings.Contains(string(saved), bob) {
		t.Errorf("bob's join not saved: %v", err)
	}
	clk.Advance(lobbyWait - time.Second) // re-armed by bob's join
	if !c.s.inLobby(chat) {
		t.Fatal("started before lobbyWait after the last join")
	}
	clk.Advance(time.Second)
	opened(t, c.s, chat, 1)
	c.s.mu.Lock()
	players := slices.Clone(c.s.rounds[chat].Players)
	c.s.mu.Unlock()
	if !slices.Equal(players, []string{alice, bob}) {
		t.Errorf("players %v", players)
	}
	if got := c.press(clk, "ready:"+id, chat, "u_carol"); got != "That game has already started." {
		t.Errorf("ready after the start: %q", got)
	}
	c.geo("end", from(clk, chat, alice))
	if got := c.press(clk, "ready:"+id, chat, "u_carol"); got != "That game is over." {
		t.Errorf("ready after the game: %q", got)
	}
}

// A ✋ Ready racing the lobby's timer, or ▶️ Start, starts the game once,
// and the player is either in it or told it has started.
func TestReadyRaces(t *testing.T) {
	for i := range 20 {
		clk, c := lobbyClient(t, t.TempDir(), true)
		id := c.lobby(clk, chat)
		join(c, clk, chat, alice)
		join(c, clk, chat, bob)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				clk.Advance(lobbyWait)
			} else { // the start pressed: the tool's work, as the client is one caller at a time
				cc := &addon.Context{Chat: chat, Sender: alice, Group: true}
				c.s.lobbyAct(cc, "start", id, true)
			}
		}()
		got := c.press(clk, "ready:"+id, chat, "u_carol")
		wg.Wait()
		if c.s.inLobby(chat) { // carol joined once the clock moved on: lobbyWait from her join
			clk.Advance(lobbyWait)
		}
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

// ▶️ Start: the starter's only; their press is their join; with 2 joined
// the game starts at once, round 1's photo following; with fewer the lobby
// goes on. After the start, and under an older game's lobby, a press does
// nothing.
func TestPressStart(t *testing.T) {
	clk, c := lobbyClient(t, t.TempDir(), true)
	id := c.lobby(clk, chat)
	if got := c.press(clk, "start:"+id, chat, bob); got != "Only the player who started this game can do that." {
		t.Errorf("bob's press: %q", got)
	}
	if got := c.press(clk, "start:"+id, chat, alice); got != "Waiting for at least 2 players: press ✋ Ready to join." {
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
	for _, data := range []string{"start:" + id, "cancel:" + id, "wait:" + id} {
		if got := c.press(clk, data, chat, alice); got != "That game has already started." {
			t.Errorf("%s after the start: %q", data, got)
		}
	}
	if got := c.press(clk, "start:"+id, chat, bob); got != "That game has already started." {
		t.Errorf("bob after the start: %q", got)
	}
	clk.Advance(lobbyWait) // the lobby's timer, stopped, starts nothing more
	if n := c.photos(chat); n != 1 {
		t.Errorf("%d photos after lobbyWait", n)
	}
	if got := c.press(clk, "start:rold", chat, alice); got != "That game is over." {
		t.Errorf("an older game's press: %q", got)
	}
	c.geo("end", from(clk, chat, alice))
	if got := c.press(clk, "start:"+id, chat, alice); got != "That game is over." {
		t.Errorf("after the game: %q", got)
	}
}

// /geo start is ▶️ Start typed, by the same rules.
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

// ✖️ Cancel: the starter's only; it calls the lobby off as /geo end does,
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

// ⏱ +30s: the starter's only; each press moves the start 30s later, an
// armed one or one to come, but never past the lobby's call-off; /geo wait
// is the same, and the extra wait is saved.
func TestPressWait(t *testing.T) {
	dir := t.TempDir()
	clk, c := lobbyClient(t, dir, true)
	id := c.lobby(clk, chat)
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

// A press and the lobby's timer at the same moment start the game once.
func TestPressRacesTimer(t *testing.T) {
	for range 20 {
		clk, c := lobbyClient(t, t.TempDir(), true)
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
	clk, c := lobbyClient(t, t.TempDir(), true)
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
