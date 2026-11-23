package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/GeoGuess/geo"
)

// fakeClock is a clock whose time moves only when a test says.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	was := !t.stopped
	t.stopped = true
	return was
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) stopper {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the time on by d, firing the timers then due in order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	c.timers = slices.DeleteFunc(c.timers, func(t *fakeTimer) bool {
		if !t.at.After(c.now) {
			due = append(due, t)
			return true
		}
		return false
	})
	c.mu.Unlock()
	slices.SortStableFunc(due, func(a, b *fakeTimer) int { return a.at.Compare(b.at) })
	for _, t := range due {
		if !t.stopped {
			t.f()
		}
	}
}

// The photos the fake finder offers: one in Ballarat, and one in Paris for
// a round in Europe.
var (
	ballarat = geo.Point{Lat: -37.5622, Lon: 143.8503}
	paris    = geo.Point{Lat: 48.8566, Lon: 2.3522}
)

// syncBuffer is a log a test can read while the server may write it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeFinder finds a photo at once, or never if off: in Ballarat, or in
// Paris for Europe, or with elsewhere for anywhere but AU. It keeps the
// regions it was asked for.
type fakeFinder struct {
	mu        sync.Mutex
	n         int
	off       bool
	elsewhere bool
	asked     []string
}

func (f *fakeFinder) Find(ctx context.Context, regions string) (geo.Photo, error) {
	f.mu.Lock()
	f.n++
	n, off, elsewhere := f.n, f.off, f.elsewhere
	f.asked = append(f.asked, regions)
	f.mu.Unlock()
	if off {
		<-ctx.Done()
		return geo.Photo{}, ctx.Err()
	}
	p := geo.Photo{
		Candidate: geo.Candidate{
			ID: strconv.Itoa(n), Source: "Mapillary", Creator: "alice", Licence: "CC BY-SA 4.0",
			Link: "https://www.mapillary.com/app/?pKey=" + strconv.Itoa(n), At: ballarat,
		},
		Place: geo.Place{Name: "Ballarat, Victoria, Australia", Country: "Australia", CountryCode: "AU"},
		JPEG:  []byte("jpeg " + strconv.Itoa(n)),
	}
	if strings.Contains(strings.ToLower(regions), "europe") || elsewhere && regions != "AU" {
		p.At, p.Place = paris, geo.Place{Name: "Paris, Île-de-France, France", Country: "France", CountryCode: "FR"}
	}
	return p, nil
}

// client drives a server over pipes, as signalbot would, and checks that
// every line the server writes is a valid JSON-RPC message.
type client struct {
	t      *testing.T
	s      *server
	in     *io.PipeWriter
	notes  chan string
	served chan error
	done   sync.WaitGroup // the reader and the prefetcher
	stop   context.CancelFunc

	mu      sync.Mutex
	id      int
	waiting map[string]chan response
	lines   int
}

// rpcIn is a message as the client reads it.
type rpcIn struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// testServer is a server keeping its state in dir, on clk, with photos from
// the fake finder, and its log in logs; extra settings come from env.
func testServer(t *testing.T, dir string, clk clock, photos finder, logs io.Writer, env ...string) *server {
	t.Helper()
	vars := map[string]string{"GEO_STATE_DIR": dir, "GEO_BUFFER": "2"}
	for i := 0; i+1 < len(env); i += 2 {
		vars[env[i]] = env[i+1]
	}
	cfg, err := loadConfig(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	if vars["gap"] == "" {
		cfg.gap = 0 // the next photo at once; TestGameGap has the pause
	}
	s, err := newServer(cfg, clk, photos, log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s.perGame = 1 // the tests of a single round; TestGame plays a whole game
	return s
}

// connect serves s over pipes, with its prefetcher running.
func connect(t *testing.T, s *server) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{t: t, s: s, in: inW, notes: make(chan string, 16), served: make(chan error, 1), stop: cancel, waiting: map[string]chan response{}}
	c.done.Add(2)
	go func() {
		defer c.done.Done()
		s.prefetch(ctx)
	}()
	go func() {
		err := s.srv.Run(ctx, inR, outW)
		outW.Close()
		c.served <- err
	}()
	go func() {
		defer c.done.Done()
		c.read(outR)
	}()
	t.Cleanup(c.close)
	return c
}

// read checks and dispatches each line the server writes.
func (c *client) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		c.mu.Lock()
		c.lines++
		c.mu.Unlock()
		var m rpcIn
		dec := json.NewDecoder(bytes.NewReader(line))
		if err := dec.Decode(&m); err != nil || dec.More() || m.JSONRPC != "2.0" {
			c.t.Errorf("stdout has a line that isn't one JSON-RPC message: %.200s", line)
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) == 0:
			c.notes <- m.Method
		case len(m.ID) > 0 && m.Method == "" && (m.Error == nil) != (len(m.Result) == 0):
			c.mu.Lock()
			ch := c.waiting[string(m.ID)]
			delete(c.waiting, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- response{ID: m.ID, Result: m.Result, Error: m.Error}
			}
		default:
			c.t.Errorf("stdout has a message that is neither a response nor a notification: %.200s", line)
		}
	}
}

// close ends the session as signalbot does, by closing stdin, and waits for
// the server to finish.
func (c *client) close() {
	if c.in == nil {
		return
	}
	c.in.Close()
	c.in = nil
	select {
	case err := <-c.served:
		if err != nil {
			c.t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		c.t.Error("the server didn't stop")
	}
	c.stop()
	c.done.Wait()
}

// send writes one raw line to the server.
func (c *client) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

// request sends a request and waits for its response.
func (c *client) request(method string, params any) response {
	c.t.Helper()
	c.mu.Lock()
	c.id++
	id := strconv.Itoa(c.id)
	ch := make(chan response, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(string(data))
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		c.t.Fatalf("no answer to %s", method)
	}
	return response{}
}

// result is a request's result, which must not be an error.
func (c *client) result(method string, params any, v any) {
	c.t.Helper()
	r := c.request(method, params)
	if r.Error != nil {
		c.t.Fatalf("%s: error %d %s", method, r.Error.Code, r.Error.Message)
	}
	if err := json.Unmarshal(r.Result.(json.RawMessage), v); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
}

// from is the context of a message sent now by user in chat.
func from(clk clock, chat, user string) callContext {
	return callContext{Chat: chat, Sender: user, Ref: "r_1", Addressed: true, Group: true, Sent: clk.Now().Unix()}
}

// tool calls a tool as the bot does, with the message's context in _meta.
func (c *client) tool(name string, args map[string]string, ctx callContext) toolResult {
	c.t.Helper()
	var r toolResult
	c.result("tools/call", map[string]any{
		"name": name, "arguments": args,
		"_meta": map[string]any{vendor + "context": ctx},
	}, &r)
	return r
}

func (c *client) geo(args string, ctx callContext) toolResult {
	c.t.Helper()
	return c.tool("geo", map[string]string{"args": args}, ctx)
}

func (c *client) guess(text string, ctx callContext) toolResult {
	c.t.Helper()
	return c.tool("geo_guess", map[string]string{"text": text}, ctx)
}

// read is the outbox page after cursor.
func (c *client) outbox(cursor string) outboxPage {
	c.t.Helper()
	var r readResult
	c.result("resources/read", uriParams{outboxURI + "/" + cursor}, &r)
	if len(r.Contents) != 1 || r.Contents[0].MimeType != "application/json" {
		c.t.Fatalf("read %+v", r)
	}
	var p outboxPage
	if err := json.Unmarshal([]byte(r.Contents[0].Text), &p); err != nil {
		c.t.Fatal(err)
	}
	return p
}

// note waits for a notification.
func (c *client) note() string {
	c.t.Helper()
	select {
	case m := <-c.notes:
		return m
	case <-time.After(5 * time.Second):
		c.t.Fatal("no notification")
	}
	return ""
}

// words is a tool result's text.
func words(r toolResult) string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func reacted(r toolResult) bool { return r.Meta != nil && r.Meta.React == pin }

// start is a fake clock, a server and a client, with the session
// initialized and subscribed to the outbox.
func start(t *testing.T) (*fakeClock, *client, *syncBuffer) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	c := connect(t, testServer(t, t.TempDir(), clk, &fakeFinder{}, logs))
	var init initializeResult
	c.result("initialize", map[string]any{
		"protocolVersion": protocolVersion, "clientInfo": map[string]string{"name": "signalbot", "version": "1"},
		"capabilities": map[string]any{"experimental": map[string]any{vendor + "addon": map[string]int{"version": 1}}},
	}, &init)
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	var empty struct{}
	c.result("resources/subscribe", uriParams{outboxURI}, &empty)
	return clk, c, logs
}

const (
	chat  = "c_one"
	alice = "u_alice"
	bob   = "u_bob"
)

// The caption gives the steps for the network the bot said it is on, and
// both when it said none.
func TestCaption(t *testing.T) {
	at := time.Date(2026, 11, 20, 14, 3, 0, 0, time.Local)
	for _, tc := range []struct{ network, want, not string }{
		{"signal", "Tap **+** → Location, pinch to zoom out, **drag the pin**, confirm, then send.", "📎"},
		{"telegram", "Tap 📎 → Location, pinch to zoom out, **drag the pin**, then send.", "confirm"},
		{"", "Signal: **+** → Location; Telegram: 📎 → Location. Pinch to zoom out, **drag the pin**, confirm (on Signal), then send.", "Tap"},
	} {
		got := caption(at, 3*time.Minute, tc.network)
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.not) || !strings.Contains(got, "14:03 (3 min)") ||
			!strings.Contains(got, "-37.81, 144.96") || !strings.Contains(got, "Move the pin before you send") {
			t.Errorf("%q: %q", tc.network, got)
		}
	}
}

// A round's caption uses the network the bot offered in the handshake.
func TestCaptionNetwork(t *testing.T) {
	clk, c, _ := start(t)
	var init initializeResult
	c.result("initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"experimental": map[string]any{vendor + "addon": map[string]any{"version": 1, "network": "signal"}}},
	}, &init)
	r := c.geo("90s", from(clk, chat, alice))
	if got := words(r); !strings.Contains(got, "Tap **+** → Location") || strings.Contains(got, "📎") {
		t.Errorf("caption %q", got)
	}
}

func TestInitializeAndTools(t *testing.T) {
	_, c, _ := start(t)
	var init initializeResult
	c.result("initialize", map[string]any{"protocolVersion": "2099-01-01"}, &init)
	if init.ProtocolVersion != protocolVersion || init.Capabilities.Experimental[vendor+"addon"].Version != 1 ||
		!init.Capabilities.Resources.Subscribe || init.ServerInfo.Name != "geoguess" {
		t.Errorf("initialize %+v", init)
	}
	var list struct {
		Tools []tool `json:"tools"`
	}
	c.result("tools/list", struct{}{}, &list)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
		if !slices.Equal(tl.Meta.Visibility, []string{"bot"}) || tl.InputSchema.Type != "object" {
			t.Errorf("tool %+v", tl)
		}
	}
	if !slices.Equal(names, []string{"geo", "geo_guess"}) {
		t.Errorf("tools %v", names)
	}
	var pong struct{}
	c.result("ping", nil, &pong)
	if r := c.request("tools/call", map[string]any{"name": "nope"}); r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Errorf("unknown tool %+v", r)
	}
	if r := c.request("resources/read", uriParams{"file:///etc/passwd"}); r.Error == nil || r.Error.Code != codeNotFound {
		t.Errorf("another resource %+v", r)
	}
	// Called without a chat, as another MCP client might.
	var r toolResult
	c.result("tools/call", map[string]any{"name": "geo", "arguments": map[string]string{"args": ""}}, &r)
	if !r.IsError || words(r) != noChat {
		t.Errorf("no context %+v", r)
	}
}

// A whole round: the photo with its watch, guesses, the timer, and the
// results in the outbox.
func TestRound(t *testing.T) {
	clk, c, logs := start(t)
	began := clk.Now()
	r := c.geo("90s", from(clk, chat, alice))
	if len(r.Content) != 2 || r.Content[0].Type != "image" || r.Content[0].MimeType != "image/jpeg" {
		t.Fatalf("content %+v", r.Content)
	}
	if data, _ := base64.StdEncoding.DecodeString(r.Content[0].Data); !bytes.HasPrefix(data, []byte("jpeg ")) {
		t.Errorf("image %q", data)
	}
	want := caption(began.Add(90*time.Second), 90*time.Second, "")
	if words(r) != want || !strings.Contains(want, "(90 sec)") {
		t.Errorf("caption %q", words(r))
	}
	if r.Meta == nil || r.Meta.Watch == nil {
		t.Fatalf("no watch: %+v", r.Meta)
	}
	w := *r.Meta.Watch
	if w.ID == "" || w.Pattern != geo.Pattern || w.Tool != "geo_guess" || w.Guard != guard ||
		w.Until != began.Add(90*time.Second+settle+watchSlack).Format(time.RFC3339) {
		t.Errorf("watch %+v", w)
	}

	if got := words(c.geo("", from(clk, chat, bob))); got != "A round is on: 1m30s left." {
		t.Errorf("a second /geo: %q", got)
	}
	clk.Advance(10 * time.Second)
	if r := c.guess("https://maps.google.com/maps?q=-37.6%2C143.9", from(clk, chat, alice)); !reacted(r) {
		t.Errorf("Alice's guess: %+v", r)
	}
	if r := c.guess("I reckon -33.87, 151.21", from(clk, chat, bob)); !reacted(r) {
		t.Errorf("Bob's guess: %+v", r)
	}
	if r := c.guess("see you at 5", from(clk, chat, bob)); reacted(r) {
		t.Errorf("no location, but %+v", r)
	}
	if r := c.guess("-33.87, 151.21", from(clk, "c_two", bob)); reacted(r) || words(r) != noRound {
		t.Errorf("a guess in another chat: %+v", r)
	}
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Errorf("ops before time is up: %+v", p)
	}

	clk.Advance(80*time.Second + settle)
	if m := c.note(); m != "notifications/resources/updated" {
		t.Errorf("notification %q", m)
	}
	page := c.outbox("0")
	aliceKm, bobKm := geo.Km(geo.Point{Lat: -37.6, Lon: 143.9}, ballarat), geo.Km(geo.Point{Lat: -33.87, Lon: 151.21}, ballarat)
	results := "📍 [**Ballarat, Victoria, Australia**](https://www.openstreetmap.org/?mlat=-37.56220&mlon=143.85030#map=12/-37.56220/143.85030)\n" +
		"🥇 {u_alice} " + geo.Distance(aliceKm) + " · " + geo.Thousands(geo.Points(aliceKm)) + "\n" +
		"🥈 {u_bob} " + geo.Distance(bobKm) + " · " + geo.Thousands(geo.Points(bobKm)) + "\n" +
		"Photo: [alice, Mapillary, CC BY-SA 4.0](https://www.mapillary.com/app/?pKey=1)"
	if len(page.Ops) != 2 || page.More {
		t.Fatalf("page %+v", page)
	}
	if o := page.Ops[0]; o.Op != "post" || o.Chat != chat || o.Text != results {
		t.Errorf("post %+v\nwant %q", o, results)
	}
	if o := page.Ops[1]; o.Op != "unwatch" || o.Chat != chat || o.Watch != w.ID || o.ID != page.Cursor {
		t.Errorf("unwatch %+v, cursor %s", o, page.Cursor)
	}
	if strings.Contains(page.Ops[0].Text, "151.2") || strings.Contains(page.Ops[0].Text, "-33.8") {
		t.Error("the results say where Bob guessed")
	}
	if again := c.outbox(page.Cursor); len(again.Ops) != 0 || again.Cursor != page.Cursor {
		t.Errorf("read past: %+v", again)
	}
	if r := c.guess("-37.56, 143.85", from(clk, chat, bob)); reacted(r) || words(r) != noRound {
		t.Errorf("a guess after the round: %+v", r)
	}
	scores := c.geo("scores", from(clk, chat, bob))
	wantScores := "🏆 **Scores, last 7 days**\n🥇 {u_alice} " + geo.Thousands(geo.Points(aliceKm)) + " · 1 round\n🥈 {u_bob} " + geo.Thousands(geo.Points(bobKm)) + " · 1 round"
	if words(scores) != wantScores {
		t.Errorf("scores %q", words(scores))
	}
	clk.Advance(8 * 24 * time.Hour)
	if got := words(c.geo("scores", from(clk, chat, bob))); !strings.HasPrefix(got, "No scores") {
		t.Errorf("scores a week later: %q", got)
	}

	c.close()
	for _, secret := range []string{"143.9", "151.2", "-33.8", "u_alice", "u_bob", "c_one", "maps.google", "reckon", "Ballarat"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs.String(), "revealed: 2 guesses") {
		t.Errorf("log:\n%s", logs)
	}
}

// An edited guess replaces the first; a late one, or an edit of a message
// from before the round, doesn't count.
func TestEditAndLateGuess(t *testing.T) {
	clk, c, _ := start(t)
	c.geo("1m", from(clk, chat, alice))
	clk.Advance(10 * time.Second)
	first := from(clk, chat, alice)
	if !reacted(c.guess("-33.87, 151.21", first)) {
		t.Fatal("Alice's first guess")
	}
	clk.Advance(10 * time.Second)
	edit := first
	edit.Edited = clk.Now().Unix()
	if !reacted(c.guess("geo:-37.57,143.86", edit)) {
		t.Fatal("Alice's edit")
	}
	old := from(clk, chat, bob)
	old.Sent, old.Edited = clk.Now().Add(-time.Hour).Unix(), clk.Now().Unix()
	if r := c.guess("-37.56, 143.85", old); reacted(r) {
		t.Errorf("an edit of an older message counted: %+v", r)
	}
	clk.Advance(41 * time.Second) // a second past the deadline, before the reveal
	if r := c.guess("-37.56, 143.85", from(clk, chat, bob)); reacted(r) || words(r) != "Too late for this round." {
		t.Errorf("a late guess: %+v", r)
	}
	clk.Advance(settle)
	c.note()
	ops := c.outbox("0").Ops
	km := geo.Km(geo.Point{Lat: -37.57, Lon: 143.86}, ballarat)
	if len(ops) != 2 || !strings.Contains(ops[0].Text, "\n🥇 {u_alice} "+geo.Distance(km)+" ·") || strings.Contains(ops[0].Text, "u_bob") || strings.Contains(ops[0].Text, "🥈") {
		t.Errorf("results %+v", ops)
	}
}

// Only the player who started a round may end or skip it; a skip scores
// nothing, and an end reveals at once.
func TestEndAndSkip(t *testing.T) {
	clk, c, _ := start(t)
	if got := words(c.geo("end", from(clk, chat, alice))); got != noRound {
		t.Errorf("end with no round: %q", got)
	}
	c.geo("", from(clk, chat, alice))
	if got := words(c.geo("end", from(clk, chat, bob))); got != "Only the player who started this round can end it." {
		t.Errorf("Bob ends: %q", got)
	}
	if got := words(c.geo("SKIP", from(clk, chat, bob))); got != "Only the player who started this round can skip it." {
		t.Errorf("Bob skips: %q", got)
	}
	c.guess("-37.6, 143.9", from(clk, chat, bob))
	skipped := words(c.geo("skip", from(clk, chat, alice)))
	if !strings.HasPrefix(skipped, "Round skipped. It was 📍 [**Ballarat, Victoria, Australia**](") || strings.Contains(skipped, "u_bob") {
		t.Errorf("skip: %q", skipped)
	}
	c.note()
	page := c.outbox("0")
	if len(page.Ops) != 1 || page.Ops[0].Op != "unwatch" {
		t.Errorf("after a skip: %+v", page)
	}
	if got := words(c.geo("scores", from(clk, chat, alice))); !strings.HasPrefix(got, "No scores") {
		t.Errorf("a skip scored: %q", got)
	}

	c.geo("5m", from(clk, chat, alice))
	clk.Advance(time.Minute)
	c.guess("-37.6, 143.9", from(clk, chat, bob))
	ended := words(c.geo("end", from(clk, chat, alice)))
	if !strings.Contains(ended, "\n🥇 {u_bob} ") {
		t.Errorf("end: %q", ended)
	}
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 1 || page.Ops[0].Op != "unwatch" {
		t.Errorf("after an end: %+v", page)
	}
	clk.Advance(5 * time.Minute) // the round's timer, stopped
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Errorf("the timer of an ended round fired: %+v", p)
	}
	if got := words(c.geo("scores", from(clk, chat, alice))); !strings.Contains(got, "🥇 {u_bob} ") {
		t.Errorf("scores %q", got)
	}
}

// A cursor the server doesn't know, such as one from before a restart,
// gets everything it still holds; one it gave lets go of what was read.
func TestUnknownCursor(t *testing.T) {
	clk, c, _ := start(t)
	c.geo("30s", from(clk, chat, alice))
	clk.Advance(30*time.Second + settle)
	c.note()
	nonce, _, _ := strings.Cut(c.outbox("0").Cursor, ".") // this run's
	for _, cursor := range []string{"0", "k7f2.18", "garbage", nonce + ".99", nonce + ".-1"} {
		if p := c.outbox(cursor); len(p.Ops) != 2 {
			t.Errorf("cursor %q: %+v", cursor, p)
		}
	}
	p := c.outbox(nonce + ".1") // read the post, not the unwatch
	if len(p.Ops) != 1 || p.Ops[0].Op != "unwatch" {
		t.Errorf("after the post: %+v", p)
	}
	p = c.outbox(p.Cursor)
	if len(p.Ops) != 0 {
		t.Errorf("after all: %+v", p)
	}
	if again := c.outbox("unknown"); len(again.Ops) != 0 || again.Cursor != p.Cursor {
		t.Errorf("nothing held: %+v", again)
	}
}

// A round survives a restart: its guesses are kept, its timer re-armed, and
// one whose time passed while GeoGuess was down is revealed at once.
func TestRestart(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	var logs syncBuffer
	c := connect(t, testServer(t, dir, clk, &fakeFinder{}, &logs))
	c.geo("1m", from(clk, chat, alice))
	c.geo("1m", from(clk, "c_two", bob))
	clk.Advance(10 * time.Second)
	c.guess("-37.6, 143.9", from(clk, chat, alice))
	c.guess("-37.6, 143.9", from(clk, "c_two", alice))
	c.close()

	// Back before c_one's deadline, with a new clock: the old timers are gone.
	clk = &fakeClock{now: clk.Now().Add(20 * time.Second)}
	c = connect(t, testServer(t, dir, clk, &fakeFinder{}, &logs))
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
	if got := words(c.geo("", from(clk, chat, bob))); got != "A round is on: 30s left." {
		t.Errorf("after a restart: %q", got)
	}
	c.guess("-33.87, 151.21", from(clk, chat, bob))
	c.close()

	// Back after both deadlines: both rounds are revealed at once.
	clk = &fakeClock{now: clk.Now().Add(time.Hour - 2*time.Minute)}
	s := testServer(t, dir, clk, &fakeFinder{}, &logs)
	c = connect(t, s)
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
	c.note()
	page := c.outbox("old.4")
	if len(page.Ops) != 4 {
		t.Fatalf("ops %+v", page)
	}
	ids := map[string]bool{}
	for _, o := range page.Ops {
		ids[o.ID] = true
		if o.Op == "post" && o.Chat == chat && (!strings.Contains(o.Text, "🥇 {u_alice}") || !strings.Contains(o.Text, "🥈 {u_bob}")) {
			t.Errorf("results %q", o.Text)
		}
	}
	if len(ids) != 4 {
		t.Errorf("op IDs %v", ids)
	}
	c.close()

	// Unread ops outlive another restart, with the same IDs, so the bot can
	// tell those it already applied.
	c = connect(t, testServer(t, dir, clk, &fakeFinder{}, &logs))
	again := c.outbox("0")
	if len(again.Ops) != 4 {
		t.Fatalf("after another restart: %+v", again)
	}
	for _, o := range again.Ops {
		if !ids[o.ID] {
			t.Errorf("op %s changed its ID", o.ID)
		}
	}
	if got := words(c.geo("", from(clk, chat, bob))); !strings.HasPrefix(got, "🌍") {
		t.Errorf("a new round: %q", got)
	}
	// Ops unread for an hour are let go; only the new round's are left.
	clk.Advance(keepOps + time.Second)
	if p := c.outbox("0"); len(p.Ops) != 2 || ids[p.Ops[0].ID] || ids[p.Ops[1].ID] {
		t.Errorf("an hour on: %+v", p)
	}
}

// Stdout carries only JSON-RPC, whatever arrives on stdin.
func TestStdoutIsProtocol(t *testing.T) {
	_, c, _ := start(t)
	c.send("not json")
	c.send("")
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	c.send(`{"jsonrpc":"2.0","id":99,"result":{}}`)
	c.send(`{"id":"x"` + strings.Repeat(" ", maxLine) + `}`)
	if r := c.request("nope/nope", nil); r.Error == nil || r.Error.Code != codeNoMethod {
		t.Errorf("unknown method %+v", r)
	}
	if r := c.request("tools/call", "not an object"); r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Errorf("bad params %+v", r)
	}
	c.result("ping", nil, new(struct{}))
	c.close()
	// initialize, subscribe, the parse error, nope/nope, tools/call and ping.
	if c.lines != 6 {
		t.Errorf("%d lines on stdout", c.lines)
	}
}

func TestNoPhotoAndRegions(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := &fakeFinder{off: true}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard)
	s.cfg.grace = 50 * time.Millisecond
	c := connect(t, s)
	began := time.Now()
	if got := words(c.geo("", from(clk, chat, alice))); got != "🔎 Fetching a spot…" {
		t.Errorf("no photo: %q", got)
	}
	if waited := time.Since(began); waited < s.cfg.grace {
		t.Errorf("fetching after %s, before the grace", waited)
	}
	if got := words(c.geo("end", from(clk, chat, alice))); got != "The game is off." {
		t.Errorf("end: %q", got)
	}

	clk, c, _ = start(t)
	r := c.geo("Europe 2m", from(clk, chat, alice))
	if !strings.Contains(words(r), "(2 min)") {
		t.Errorf("caption %q", words(r))
	}
	if got := words(c.geo("end", from(clk, chat, alice))); !strings.Contains(got, "Paris") {
		t.Errorf("a round in Europe: %q", got)
	}
	for _, args := range []string{"atlantis", "10s", "31m", "2x"} {
		if r := c.geo(args, from(clk, chat, alice)); r.Meta != nil {
			t.Errorf("/geo %s started a round", args)
		}
	}
	if got := words(c.geo("", from(clk, chat, alice))); !strings.HasPrefix(got, "🌍") {
		t.Errorf("a round anywhere: %q", got)
	}
	if got := words(c.geo("end", from(clk, chat, alice))); !strings.Contains(got, "Ballarat") {
		t.Errorf("a world round: %q", got)
	}
}

func TestConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := loadConfig(env(map[string]string{"GEO_STATE_DIR": "/x"}))
	if err != nil || c.Round != 3*time.Minute || c.MemLimit != 200<<20 || c.Buffer != 4 || c.MinQuality != 0.5 || c.Keep != nil || c.grace != startGrace || c.gap != gapTime ||
		c.Nominatim != geo.NominatimURL || c.Panoramax != geo.PanoramaxURL || c.Mapillary != geo.MapillaryURL || c.MaxAge < 10*365*24*time.Hour {
		t.Errorf("defaults %+v, %v", c, err)
	}
	c, err = loadConfig(env(map[string]string{"GEO_STATE_DIR": "/x", "GEO_ROUND": "90s", "GEO_MEMLIMIT": "1GiB", "GEO_REGIONS": "europe", "GEO_BUFFER": "5",
		"GEO_KEEP": " AU, europe,"}))
	if err != nil || c.Round != 90*time.Second || c.MemLimit != 1<<30 || c.Regions != "europe" || c.Buffer != 5 ||
		!slices.Equal(c.Keep, []string{"AU", "europe"}) {
		t.Errorf("set %+v, %v", c, err)
	}
	_, err = loadConfig(env(map[string]string{"GEO_ROUND": "1h", "GEO_MEMLIMIT": "lots", "GEO_REGIONS": "atlantis", "GEO_BUFFER": "0", "GEO_KEEP": "AU,atlantis",
		"GEO_MIN_QUALITY": "2", "GEO_MAX_AGE_YEARS": "-1", "GEO_NOMINATIM_URL": "http://x",
		"GEO_PANORAMAX_URL": "ftp://x", "GEO_MAPILLARY_URL": "graph.example"}))
	for _, name := range []string{"GEO_STATE_DIR", "GEO_ROUND", "GEO_MEMLIMIT", "GEO_REGIONS", "GEO_BUFFER", "GEO_KEEP", "GEO_MIN_QUALITY", "GEO_MAX_AGE_YEARS",
		"GEO_NOMINATIM_URL", "GEO_PANORAMAX_URL", "GEO_MAPILLARY_URL"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("no error for %s: %v", name, err)
		}
	}
	for s, want := range map[string]int64{"200MiB": 200 << 20, "1GiB": 1 << 30, "67108864": 64 << 20, "65536KiB": 64 << 20, "0.5GiB": 512 << 20} {
		if got, err := parseSize(s); got != want || err != nil {
			t.Errorf("parseSize(%q) = %d, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "1MiB", "-5MiB", "200MB", "MiB"} {
		if _, err := parseSize(s); err == nil {
			t.Errorf("parseSize(%q) passed", s)
		}
	}
	if err := checkDir(t.TempDir()); err != nil {
		t.Error(err)
	}
	if err := checkDir(t.TempDir() + "/missing/geo"); err != nil {
		t.Errorf("a missing directory isn't created: %v", err)
	}
	if err := checkDir("/proc/geo"); err == nil || !strings.Contains(err.Error(), "GEO_STATE_DIR") {
		t.Errorf("a missing directory: %v", err)
	}
}

// waitFor fails the test unless ok holds within a few seconds, checked
// with s.mu held.
func waitFor(t *testing.T, s *server, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		done := ok()
		s.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s", what)
		}
	}
}

// count is how many photos in the stock are in region. s.mu is held.
func (s *server) count(region string) int {
	n := 0
	for _, k := range s.stock {
		if s.in(k, region) {
			n++
		}
	}
	return n
}

// match is the index of the oldest photo in the stock that is in region,
// or -1. s.mu is held.
func (s *server) match(region string) int {
	return slices.IndexFunc(s.stock, func(k kept) bool { return s.in(k, region) })
}

// With GEO_KEEP=AU the prefetcher keeps an Australian photo ready with no
// /geo asking, a /geo AU takes it at once, and it is replaced.
func TestKeep(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := &fakeFinder{elsewhere: true}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard, "GEO_KEEP", "AU")
	for range s.cfg.Buffer {
		p, _ := photos.Find(context.Background(), "")
		s.add(p, "")
	}
	c := connect(t, s)
	var ready string
	waitFor(t, s, "an AU photo", func() bool {
		if i := s.match("AU"); i >= 0 {
			ready = s.stock[i].Link
		}
		return ready != ""
	})
	photos.mu.Lock()
	if asked := photos.asked[s.cfg.Buffer:]; !slices.Equal(asked, []string{"AU"}) {
		t.Errorf("searched %q", asked)
	}
	photos.mu.Unlock()

	if got := words(c.geo("stop", from(clk, chat, alice))); got != noRound {
		t.Errorf("end with no round: %q", got)
	}
	r := c.geo("AU 2m", from(clk, chat, alice))
	if r.Meta == nil {
		t.Fatalf("/geo AU: %q", words(r))
	}
	s.mu.Lock()
	if got := s.rounds[chat].Credit.Link; got != ready {
		t.Errorf("/geo AU used %s, not the photo kept ready", got)
	}
	s.mu.Unlock()
	if got := words(c.geo("end", from(clk, chat, alice))); !strings.Contains(got, "Ballarat") {
		t.Errorf("a round in AU: %q", got)
	}
	waitFor(t, s, "a new AU photo", func() bool { return s.count("AU") == 1 })
	photos.mu.Lock()
	if asked := photos.asked[s.cfg.Buffer:]; !slices.Equal(asked, []string{"AU", "AU"}) {
		t.Errorf("searched %q", asked)
	}
	photos.mu.Unlock()
}

// The photos kept on disk are capped at maxHeld, one file each: past it
// the stock drops photos its keys can spare first, so a GEO_KEEP region's
// photo stays; files leave with their photos.
func TestDiskCap(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := &fakeFinder{elsewhere: true}
	dir := t.TempDir()
	s := testServer(t, dir, clk, photos, io.Discard, "GEO_KEEP", "AU")
	find := func(region string) geo.Photo {
		p, err := photos.Find(context.Background(), region)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	s.add(find("AU"), "AU")
	for range maxHeld + 8 {
		s.add(find(""), "")
	}
	s.mu.Lock()
	n, au, first := len(s.stock), s.count("AU"), s.stock[0].ID
	s.mu.Unlock()
	if n != maxHeld || au != 1 || first != "1" {
		t.Errorf("%d photos, %d in AU, the oldest %s", n, au, first)
	}
	if files := photoFiles(t, dir); len(files) != maxHeld {
		t.Errorf("%d files", len(files))
	}
}

// photoFiles is the names of the photo files in dir's photoDir, failing
// the test unless each is for GeoGuess's user alone.
func photoFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, photoDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.Mode().Perm() != 0o600 || !strings.HasSuffix(e.Name(), ".jpg") {
			t.Errorf("photo file %s: %v, %v", e.Name(), info.Mode(), err)
		}
		names = append(names, e.Name())
	}
	return names
}

// failIn is the fake finder, failing every search in one region, with err
// if set.
type failIn struct {
	*fakeFinder
	region string
	err    error
}

func (f failIn) Find(ctx context.Context, regions string) (geo.Photo, error) {
	if regions == f.region {
		f.mu.Lock()
		f.asked = append(f.asked, regions)
		f.mu.Unlock()
		if f.err != nil {
			return geo.Photo{}, f.err
		}
		return geo.Photo{}, errors.New("nothing good there")
	}
	return f.fakeFinder.Find(ctx, regions)
}

// A GEO_KEEP region whose searches fail waits its own backoff, and the
// buffer still fills meanwhile.
func TestKeepFailing(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := failIn{&fakeFinder{elsewhere: true}, "AU", nil}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard, "GEO_KEEP", "AU")
	connect(t, s)
	waitFor(t, s, "a full stock", func() bool { return len(s.stock) == s.cfg.Buffer })
	s.mu.Lock()
	r, au := s.retries["AU"], s.count("AU")
	s.mu.Unlock()
	if r.failures != 1 || au != 0 {
		t.Errorf("retry %+v, %d in AU", r, au)
	}
	photos.mu.Lock()
	if asked := photos.asked; !slices.Equal(asked, []string{"AU", "", ""}) {
		t.Errorf("searched %q", asked)
	}
	photos.mu.Unlock()
}

// A source asking to slow down holds up every search, not just its
// region's, for the wait.
func TestKeepSlowDown(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := failIn{&fakeFinder{elsewhere: true}, "AU", geo.ErrSlowDown}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard, "GEO_KEEP", "AU")
	connect(t, s)
	waitFor(t, s, "a slow-down", func() bool { return s.retries["AU"].slow })
	time.Sleep(300 * time.Millisecond) // for any other search
	s.mu.Lock()
	r, stocked := s.retries["AU"], len(s.stock)
	s.mu.Unlock()
	if r.failures != 1 || stocked != 0 {
		t.Errorf("retry %+v, %d stocked", r, stocked)
	}
	photos.mu.Lock()
	if asked := photos.asked; !slices.Equal(asked, []string{"AU"}) {
		t.Errorf("searched %q", asked)
	}
	photos.mu.Unlock()
}

// A plain /geo takes a photo GEO_KEEP can spare, leaving the kept one.
func TestKeepPlainTake(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := &fakeFinder{elsewhere: true}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard, "GEO_KEEP", "AU")
	for _, region := range []string{"AU", ""} {
		p, _ := photos.Find(context.Background(), region)
		s.add(p, region)
	}
	c := connect(t, s)
	if r := c.geo("", from(clk, chat, alice)); r.Meta == nil {
		t.Fatalf("/geo: %q", words(r))
	}
	s.mu.Lock()
	place, au := s.rounds[chat].Place, s.count("AU")
	s.mu.Unlock()
	if !strings.Contains(place, "Paris") || au != 1 {
		t.Errorf("/geo used a photo in %s, leaving %d in AU", place, au)
	}
}

// A /geo the bot gives up on starts no round: one cancelled while it waits
// leaves the photo found later ready, and one whose call is over when its
// photo is taken puts it back. Near the bot's deadline it doesn't wait.
func TestGeoAbandoned(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := &fakeFinder{off: true}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard)
	c := connect(t, s)
	got := make(chan response, 1)
	c.mu.Lock()
	c.waiting["99"] = got
	c.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "tools/call", "params": map[string]any{
		"name": "geo", "arguments": map[string]string{"args": ""}, "_meta": map[string]any{vendor + "context": from(clk, chat, alice)},
	}})
	c.send(string(data))
	time.Sleep(100 * time.Millisecond) // for it to wait
	began := time.Now()
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":99}}`)
	select {
	case r := <-got:
		var res toolResult
		json.Unmarshal(r.Result.(json.RawMessage), &res)
		if words(res) != noPhoto || time.Since(began) > time.Second {
			t.Errorf("cancelled: %q after %s", words(res), time.Since(began))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the cancelled /geo went on waiting")
	}
	p := geo.Photo{Candidate: geo.Candidate{ID: "1", Source: "Mapillary", At: ballarat},
		Place: geo.Place{Name: "Ballarat, Victoria, Australia", Country: "Australia", CountryCode: "AU"}}
	s.add(p, "")
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	n, round := len(s.stock), s.rounds[chat]
	s.mu.Unlock()
	if n != 1 || round != nil {
		t.Errorf("%d photos ready, round %+v", n, round)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cc := from(clk, chat, alice)
	if got := words(s.start(ctx, &cc, s.cfg.Round, "")); got != noPhoto {
		t.Errorf("start, the call over: %q", got)
	}
	s.mu.Lock()
	n, round = len(s.stock), s.rounds[chat]
	s.mu.Unlock()
	if n != 1 || round != nil {
		t.Errorf("after the call was over: %d photos ready, round %+v", n, round)
	}

	cc = from(clk, "c_two", bob)
	cc.Deadline = time.Now().Add(replyTime).Unix()
	began = time.Now()
	if got := words(c.geo("Europe", cc)); got != "🔎 Fetching a spot…" || time.Since(began) > time.Second {
		t.Errorf("near the deadline: %q after %s", got, time.Since(began))
	}
	if got := words(c.geo("end", cc)); got != "The game is off." {
		t.Errorf("end: %q", got)
	}
}

func TestLabels(t *testing.T) {
	for d, want := range map[time.Duration]string{3 * time.Minute: "3 min", 90 * time.Second: "90 sec", 30 * time.Minute: "30 min"} {
		if got := lengthLabel(d); got != want {
			t.Errorf("lengthLabel(%s) = %q", d, got)
		}
	}
	r := &round{Deadline: time.Unix(1000, 0)}
	for left, want := range map[time.Duration]string{30 * time.Second: "30s", 2 * time.Minute: "2m", 102 * time.Second: "1m42s", -time.Second: "0s"} {
		if got := words(busy(r, r.Deadline.Add(-left))); got != fmt.Sprintf("A round is on: %s left.", want) {
			t.Errorf("busy %s: %q", left, got)
		}
	}
	if got := (credit{Creator: "[bob](https://evil.example)*", Source: "Panoramax", Licence: "etalab-2.0", Link: "https://x.example/p"}).String(); got != "Photo: [bobhttps://evil.example, Panoramax, etalab-2.0](https://x.example/p)" {
		t.Errorf("credit %q", got)
	}
	if got := (credit{Source: "Mapillary"}).String(); got != "Photo: Mapillary" {
		t.Errorf("credit %q", got)
	}
	if !validID("u_ABC234", "u_") || validID("u_", "u_") || validID("u_a}b", "u_") || validID("c_x", "u_") {
		t.Error("validID")
	}
}

// A game of 3 rounds: round 1 runs its time and its guessers are the
// players; a later round ends once they have all guessed, a newcomer
// isn't waited for, each round's photo is a post with its image, and the
// last results carry the game's totals and end the watch.
func TestGame(t *testing.T) {
	clk, c, _ := start(t)
	c.games(3)
	began := clk.Now()
	r := c.geo("1m", from(clk, chat, alice))
	if !strings.HasPrefix(words(r), "**Round 1 of 3** · 🌍") {
		t.Errorf("round 1 %q", words(r))
	}
	if w := r.Meta.Watch; w.Until != began.Add(4*(time.Minute+settle)+watchSlack).Format(time.RFC3339) {
		t.Errorf("watch until %s", w.Until)
	}
	there, further := "-37.6, 143.9", "-37.6, 143.95" // Alice is always nearest
	c.guess(there, from(clk, chat, alice))
	c.guess(further, from(clk, chat, bob))
	clk.Advance(30 * time.Second)
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("round 1 ended early: %+v", p)
	}
	clk.Advance(30*time.Second + settle)
	waitFor(t, c.s, "round 2", func() bool { r := c.s.rounds[chat]; return r != nil && r.Number == 2 && !r.Waiting })
	page := c.outbox("0")
	if len(page.Ops) != 2 || !strings.HasPrefix(page.Ops[0].Text, "**Round 1 of 3** · 📍") ||
		!strings.HasPrefix(page.Ops[1].Text, "**Round 2 of 3** · 🌍") || len(page.Ops[1].Images) != 1 || page.Ops[1].Images[0].MimeType != "image/jpeg" {
		t.Fatalf("after round 1 %+v", page)
	}
	if p := c.s.rounds[chat].Players; !slices.Equal(p, []string{alice, bob}) {
		t.Errorf("players %v", p)
	}

	// Round 2: Alice and a newcomer, then Bob, who ends it.
	clk.Advance(5 * time.Second)
	c.guess(there, from(clk, chat, alice))
	c.guess(further, from(clk, chat, "u_carol"))
	clk.Advance(settle)
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Fatalf("round 2 ended before Bob guessed: %+v", p)
	}
	c.guess(further, from(clk, chat, bob))
	clk.Advance(settle)
	waitFor(t, c.s, "round 3", func() bool { r := c.s.rounds[chat]; return r != nil && r.Number == 3 && !r.Waiting })
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 2 || !strings.HasPrefix(page.Ops[0].Text, "**Round 2 of 3** · 📍") || !strings.HasPrefix(page.Ops[1].Text, "**Round 3 of 3**") {
		t.Fatalf("after round 2 %+v", page)
	}

	// Round 3: only Alice guesses, so it runs its time.
	c.guess(there, from(clk, chat, alice))
	clk.Advance(30 * time.Second)
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Fatalf("round 3 ended early: %+v", p)
	}
	clk.Advance(30*time.Second + settle)
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 2 || !strings.Contains(page.Ops[0].Text, "\n\n🏁 **Game over**: 🏆 {u_alice} wins!\n🥇 {u_alice} 3 wins · ") || page.Ops[1].Op != "unwatch" {
		t.Fatalf("the end %+v", page)
	}
	if c.s.rounds[chat] != nil {
		t.Error("the game is still on")
	}
}

// /geo skip moves to the next round unscored; /geo end ends the game with
// its totals.
func TestGameSkipAndEnd(t *testing.T) {
	clk, c, _ := start(t)
	c.games(3)
	c.geo("1m", from(clk, chat, alice))
	c.guess("-37.6, 143.9", from(clk, chat, bob))
	if got := words(c.geo("skip", from(clk, chat, alice))); !strings.HasPrefix(got, "**Round 1 of 3** · Round skipped.") {
		t.Errorf("skip %q", got)
	}
	waitFor(t, c.s, "round 2", func() bool { r := c.s.rounds[chat]; return r != nil && r.Number == 2 && !r.Waiting })
	c.guess("-37.6, 143.9", from(clk, chat, bob))
	got := words(c.geo("end", from(clk, chat, alice)))
	if !strings.HasPrefix(got, "**Round 2 of 3** · 📍") || !strings.Contains(got, "\n\n🏁 **Game over**: 🏆 {u_bob} wins!\n🥇 {u_bob} 1 win · ") {
		t.Errorf("end %q", got)
	}
	if c.s.rounds[chat] != nil {
		t.Error("the game is still on")
	}
	if got := words(c.geo("90m", from(clk, chat, alice))); got != badLength {
		t.Errorf("too long a round: %q", got)
	}
	if got := words(c.geo("20m", from(clk, chat, alice))); got != longGame {
		t.Errorf("too long a game: %q", got)
	}
}

// Tied on round wins after round 3, the tied players play a tie-breaker,
// which ends once they have both guessed, and which only they can win.
func TestGameTieBreaker(t *testing.T) {
	clk, c, _ := start(t)
	c.games(3)
	near, far := "-37.56, 143.85", "-37.6, 143.9"
	next := func(n int) {
		t.Helper()
		waitFor(t, c.s, fmt.Sprint("round ", n), func() bool { r := c.s.rounds[chat]; return r != nil && r.Number == n && !r.Waiting })
	}
	c.geo("1m", from(clk, chat, alice))
	waitFor(t, c.s, "the spare", func() bool { return len(c.s.rounds[chat].Photos) == 3 })
	c.guess(near, from(clk, chat, alice)) // Alice wins round 1
	c.guess(far, from(clk, chat, bob))
	clk.Advance(time.Minute + settle)
	next(2)
	c.guess(far, from(clk, chat, alice))
	c.guess(near, from(clk, chat, bob)) // Bob wins round 2, which both ending ends
	clk.Advance(settle)
	next(3)
	clk.Advance(time.Minute + settle) // nobody guesses round 3
	next(4)
	page := c.outbox("0")
	if last := page.Ops[len(page.Ops)-2].Text; !strings.Contains(last, "\n\n🤝 {u_alice}, {u_bob} are tied on 1 win: a tie-breaker round decides it") {
		t.Errorf("round 3's results %q", last)
	}
	if photo := page.Ops[len(page.Ops)-1]; !strings.HasPrefix(photo.Text, "**Tie-breaker** · 🌍") || len(photo.Images) != 1 {
		t.Errorf("the tie-breaker %+v", photo)
	}
	c.guess(near, from(clk, chat, "u_carol")) // nearest, but not tied
	c.guess(far, from(clk, chat, alice))
	c.guess(far+"1", from(clk, chat, bob))
	clk.Advance(settle)
	c.note()
	waitFor(t, c.s, "the end", func() bool { return c.s.rounds[chat] == nil })
	page = c.outbox(page.Cursor)
	if len(page.Ops) < 2 || !strings.Contains(page.Ops[len(page.Ops)-2].Text, "**Tie-breaker** · 📍") ||
		!strings.Contains(page.Ops[len(page.Ops)-2].Text, "🏁 **Game over**: 🏆 {u_alice} wins!\n🥇 {u_alice} 2 wins · ") {
		t.Errorf("the end %+v", page)
	}
}

// With the pause, a round's results go out at once and the next photo 5s
// later, with the round's full time; a guess in between counts for
// nothing.
func TestGameGap(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	var logs syncBuffer
	c := connect(t, testServer(t, t.TempDir(), clk, &fakeFinder{}, &logs, "gap", "on"))
	c.result("initialize", map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}}, new(initializeResult))
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
	c.games(3)
	began := clk.Now()
	r := c.geo("1m", from(clk, chat, alice))
	if w := r.Meta.Watch; w.Until != began.Add(4*(time.Minute+settle+gapTime)+watchSlack).Format(time.RFC3339) {
		t.Errorf("watch until %s", w.Until)
	}
	clk.Advance(time.Minute + settle)
	c.note()
	waitFor(t, c.s, "the photo taken", func() bool { return len(c.s.timers) == 0 || c.s.rounds[chat].Waiting })
	time.Sleep(50 * time.Millisecond) // for the photo to be taken and held for the pause
	page := c.outbox("0")
	if len(page.Ops) != 1 || !strings.HasPrefix(page.Ops[0].Text, "**Round 1 of 3** · ") {
		t.Fatalf("the results alone %+v", page)
	}
	if g := c.guess("-37.6, 143.9", from(clk, chat, bob)); reacted(g) {
		t.Errorf("a guess in the pause: %+v", g)
	}
	clk.Advance(gapTime)
	waitFor(t, c.s, "round 2", func() bool { r := c.s.rounds[chat]; return r != nil && r.Number == 2 && !r.Waiting })
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 1 || !strings.HasPrefix(page.Ops[0].Text, "**Round 2 of 3** · 🌍") {
		t.Fatalf("round 2 %+v", page)
	}
	c.s.mu.Lock()
	if left := c.s.rounds[chat].Deadline.Sub(clk.Now()); left != time.Minute {
		t.Errorf("round 2 has %s", left)
	}
	c.s.mu.Unlock()
}

// In a group, with a bot that passes reactions, /geo answers with a lobby;
// round 1's photo comes 30s after the last join once there are 2, as a
// post with its image; the joined are the players; fewer than 2 after 5
// minutes calls it off; a DM starts at once with the photo.
func TestGeoLobby(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	c := connect(t, testServer(t, t.TempDir(), clk, &fakeFinder{}, logs))
	c.result("initialize", map[string]any{
		"protocolVersion": protocolVersion, "clientInfo": map[string]string{"name": "signalbot", "version": "1"},
		"capabilities": map[string]any{"experimental": map[string]any{vendor + "addon": map[string]any{"version": 1, "watchReactions": true}}},
	}, new(initializeResult))
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
	c.games(3)
	react := func(removed bool, ctx callContext) {
		t.Helper()
		c.result("tools/call", map[string]any{"name": "geo_guess", "arguments": map[string]any{"reaction": "👍", "removed": removed},
			"_meta": map[string]any{vendor + "context": ctx}}, new(toolResult))
	}
	r := c.geo("1m", from(clk, chat, alice))
	if !strings.Contains(words(r), "React to this message with any emoji to join") || !strings.Contains(words(r), "whoever started it too") || len(r.Content) != 1 || !r.Meta.Watch.Reactions || !strings.HasSuffix(r.Meta.Watch.Over, "is over. Start another with /geo.") || r.Meta.Watch.Pattern != geo.Pattern {
		t.Fatalf("lobby %+v", r)
	}
	if g := c.guess("-37.6, 143.9", from(clk, chat, bob)); words(g) != "The game hasn't started yet." {
		t.Errorf("a guess in the lobby: %q", words(g))
	}
	react(false, from(clk, chat, alice)) // the starter joins too, but alone doesn't start it
	clk.Advance(lobbyWait)
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("started with one player: %+v", p)
	}
	react(false, from(clk, chat, bob))
	waitFor(t, c.s, "the game's photos", func() bool { return len(c.s.rounds[chat].Photos) == need(3) })
	clk.Advance(29 * time.Second)
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("a photo before the lobby ended: %+v", p)
	}
	clk.Advance(time.Second)
	waitFor(t, c.s, "round 1", func() bool { r := c.s.rounds[chat]; return r != nil && r.Number == 1 && !r.Waiting })
	page := c.outbox("0")
	if len(page.Ops) != 1 || !strings.HasPrefix(page.Ops[0].Text, "**Round 1 of 3** · 🌍") || len(page.Ops[0].Images) != 1 {
		t.Fatalf("round 1 %+v", page)
	}
	c.s.mu.Lock()
	if p := c.s.rounds[chat].Players; !slices.Equal(p, []string{alice, bob}) {
		t.Errorf("players %v", p)
	}
	c.s.mu.Unlock()
	// Round 1 ends once both have guessed.
	c.guess("-37.6, 143.9", from(clk, chat, alice))
	c.guess("-37.6, 143.95", from(clk, chat, bob))
	clk.Advance(settle)
	waitFor(t, c.s, "round 2", func() bool { r := c.s.rounds[chat]; return r != nil && r.Number == 2 })

	c.geo("1m", from(clk, "c_two", alice))
	clk.Advance(lobbyMax)
	waitFor(t, c.s, "the call-off", func() bool { return c.s.rounds["c_two"] == nil })
	dm := from(clk, "c_dm", alice)
	dm.Group = false
	if r := c.geo("1m", dm); len(r.Content) != 2 || r.Content[0].Type != "image" {
		t.Errorf("a DM has a lobby: %+v", words(r))
	}
}

// In a DM the starter is the one player: a round ends as soon as they
// guess, from round 1.
func TestDMEndsEarly(t *testing.T) {
	clk, c, _ := start(t)
	c.games(3)
	dm := from(clk, "c_dm", alice)
	dm.Group = false
	c.geo("1m", dm)
	c.guess("-37.6, 143.9", dm)
	clk.Advance(settle)
	waitFor(t, c.s, "round 2", func() bool { r := c.s.rounds["c_dm"]; return r != nil && r.Number == 2 })
}

// The real finder searches the regions it is asked for, or GEO_REGIONS
// with none.
func TestSearchSeeds(t *testing.T) {
	f := &search{seeds: map[string]*geo.Seeds{}, regions: "NZ"}
	for _, tc := range []struct{ regions, cc string }{{"AU", "AU"}, {"", "NZ"}} {
		seeds, err := f.seedsFor(tc.regions)
		if err != nil || !seeds.Has(tc.cc) || seeds.Has("FR") {
			t.Errorf("%q: %v", tc.regions, err)
		}
	}
	if seeds, _ := f.seedsFor("AU"); seeds.Has("NZ") {
		t.Error("AU has seeds in NZ")
	}
}

// gated is the fake finder, finding nothing until open is closed.
type gated struct {
	*fakeFinder
	open chan struct{}
}

func (f gated) Find(ctx context.Context, regions string) (geo.Photo, error) {
	select {
	case <-f.open:
	case <-ctx.Done():
		return geo.Photo{}, ctx.Err()
	}
	return f.fakeFinder.Find(ctx, regions)
}

// fed is the fake finder, finding a photo only when the test feeds it one.
type fed struct {
	*fakeFinder
	feed chan struct{}
}

func (f fed) Find(ctx context.Context, regions string) (geo.Photo, error) {
	select {
	case <-f.feed:
	case <-ctx.Done():
		return geo.Photo{}, ctx.Err()
	}
	return f.fakeFinder.Find(ctx, regions)
}

// strict is the fake finder, failing the test if it is asked for a photo
// once armed.
type strict struct {
	*fakeFinder
	t     *testing.T
	armed *bool
}

func (f strict) Find(ctx context.Context, regions string) (geo.Photo, error) {
	f.mu.Lock()
	armed := *f.armed
	f.mu.Unlock()
	if armed {
		f.t.Errorf("searched %q during a game", regions)
	}
	return f.fakeFinder.Find(ctx, regions)
}

// fetchClient is a client of a server keeping its state in dir, on clk,
// with photos, games of of rounds and a short grace, initialized and
// subscribed; with lobby, the bot takes reactions.
func fetchClient(t *testing.T, dir string, clk *fakeClock, photos finder, of int, lobby bool) *client {
	t.Helper()
	s := testServer(t, dir, clk, photos, io.Discard)
	s.cfg.grace = 50 * time.Millisecond
	s.perGame = of
	c := connect(t, s)
	c.result("initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"experimental": map[string]any{vendor + "addon": map[string]any{"version": 1, "watchReactions": lobby}}},
	}, new(initializeResult))
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
	return c
}

// dmFrom is the context of a DM sent now by user in chat.
func dmFrom(clk clock, chat, user string) callContext {
	cc := from(clk, chat, user)
	cc.Group = false
	return cc
}

// join is user reacting to the lobby's intro in chat.
func join(c *client, clk clock, chat, user string) {
	c.t.Helper()
	c.result("tools/call", map[string]any{"name": "geo_guess", "arguments": map[string]any{"reaction": "👍"},
		"_meta": map[string]any{vendor + "context": from(clk, chat, user)}}, new(toolResult))
}

// opened waits until the game in chat has round number open.
func opened(t *testing.T, s *server, chat string, number int) {
	t.Helper()
	waitFor(t, s, fmt.Sprint("round ", number), func() bool { r := s.rounds[chat]; return r != nil && r.Number == number && !r.Waiting })
}

// reserved is how many photos the game in chat has reserved, or -1 with
// none. s.mu is held.
func (s *server) reserved(chat string) int {
	if r := s.rounds[chat]; r != nil {
		return len(r.Photos)
	}
	return -1
}

// stockUp puts n photos found for key in s's stock, as the prefetcher
// would.
func stockUp(s *server, photos *fakeFinder, key string, n int) {
	for range n {
		p, _ := photos.Find(context.Background(), key)
		s.add(p, key)
	}
}

// A game with a full stock reserves a photo for each round and a spare at
// /geo, and never searches during play: round after round, and the
// tie-breaker, take the photos it reserved.
func TestReserveFull(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	armed := false
	photos := strict{&fakeFinder{}, t, &armed}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard)
	s.perGame = 3
	stockUp(s, photos.fakeFinder, "", need(3)+s.cfg.Buffer) // so the stock stays full
	photos.mu.Lock()
	armed = true
	photos.mu.Unlock()
	c := connect(t, s)
	r := c.geo("1m", dmFrom(clk, "c_dm", alice))
	if !strings.HasPrefix(words(r), "**Round 1 of 3** · 🌍") || len(r.Content) != 2 {
		t.Fatalf("/geo %q", words(r))
	}
	s.mu.Lock()
	left, stock := s.reserved("c_dm"), len(s.stock)
	s.mu.Unlock()
	if left != 3 || stock != s.cfg.Buffer {
		t.Errorf("%d photos reserved after round 1's, %d in the stock", left, stock)
	}
	near, far := "-37.56, 143.85", "-37.6, 143.9"
	c.guess(near, dmFrom(clk, "c_dm", alice)) // Alice wins round 1
	clk.Advance(settle)
	opened(t, s, "c_dm", 2)
	c.guess(near, dmFrom(clk, "c_dm", bob)) // Bob wins round 2
	c.guess(far, dmFrom(clk, "c_dm", alice))
	clk.Advance(settle)
	opened(t, s, "c_dm", 3)
	clk.Advance(time.Minute + settle) // nobody guesses round 3
	opened(t, s, "c_dm", 4)
	c.guess(near, dmFrom(clk, "c_dm", alice))
	c.guess(far, dmFrom(clk, "c_dm", bob)) // the tie-breaker waits for both
	clk.Advance(settle)
	waitFor(t, s, "the end", func() bool { return s.rounds["c_dm"] == nil })
	time.Sleep(50 * time.Millisecond) // for a search, if one were wrongly made
	photos.mu.Lock()
	defer photos.mu.Unlock()
	if len(photos.asked) != need(3)+s.cfg.Buffer {
		t.Errorf("searched %q", photos.asked)
	}
}

// In a DM with too few photos ready, /geo answers that it is fetching
// them, with the game's watch; round 1 follows once there is a photo for
// each round, and the spare, found during play, serves a tie-breaker.
func TestFetchDM(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := fed{&fakeFinder{}, make(chan struct{})}
	c := fetchClient(t, t.TempDir(), clk, photos, 3, false)
	began := clk.Now()
	r := c.geo("1m", dmFrom(clk, "c_dm", alice))
	if words(r) != "🔎 Fetching spots for 3 rounds…" || len(r.Content) != 1 || r.Meta == nil || r.Meta.Watch == nil || r.Meta.Watch.Reactions ||
		r.Meta.Watch.Until != began.Add(gameTime(3, time.Minute, 0)+fetchMax+watchSlack).Format(time.RFC3339) {
		t.Fatalf("/geo %+v", r)
	}
	if g := c.guess("-37.6, 143.9", dmFrom(clk, "c_dm", alice)); words(g) != "The game hasn't started yet." || reacted(g) {
		t.Errorf("a guess before round 1: %q", words(g))
	}
	if got := words(c.geo("", dmFrom(clk, "c_dm", alice))); got != "A game is on: round 1's photo is on its way." {
		t.Errorf("/geo while fetching: %q", got)
	}
	for n := 1; n <= 2; n++ {
		photos.feed <- struct{}{}
		waitFor(t, c.s, fmt.Sprint(n, " photos"), func() bool { return c.s.reserved("c_dm") == n })
	}
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("ops before the photos: %+v", p)
	}
	photos.feed <- struct{}{}
	opened(t, c.s, "c_dm", 1)
	page := c.outbox("0")
	if len(page.Ops) != 1 || !strings.HasPrefix(page.Ops[0].Text, "**Round 1 of 3** · 🌍") || len(page.Ops[0].Images) != 1 {
		t.Fatalf("round 1 %+v", page)
	}
	photos.feed <- struct{}{} // the spare, during round 1
	waitFor(t, c.s, "the spare", func() bool { return c.s.reserved("c_dm") == 3 })

	near, far := "-37.56, 143.85", "-37.6, 143.9"
	c.guess(near, dmFrom(clk, "c_dm", alice))
	clk.Advance(settle)
	opened(t, c.s, "c_dm", 2)
	c.guess(near, dmFrom(clk, "c_dm", bob))
	c.guess(far, dmFrom(clk, "c_dm", alice))
	clk.Advance(settle)
	opened(t, c.s, "c_dm", 3)
	clk.Advance(time.Minute + settle)
	opened(t, c.s, "c_dm", 4)
	page = c.outbox(page.Cursor)
	if n := len(page.Ops); n < 2 || !strings.Contains(page.Ops[n-2].Text, "are tied on 1 win") || !strings.HasPrefix(page.Ops[n-1].Text, "**Tie-breaker** · 🌍") {
		t.Fatalf("the tie-breaker %+v", page)
	}
}

// Without a spare, players tied after the last round aren't given a
// tie-breaker: points decide.
func TestTieWithoutSpare(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := fed{&fakeFinder{}, make(chan struct{})}
	c := fetchClient(t, t.TempDir(), clk, photos, 2, false)
	c.geo("1m", dmFrom(clk, "c_dm", alice))
	for range 2 {
		photos.feed <- struct{}{}
	}
	opened(t, c.s, "c_dm", 1)
	c.guess("-37.56, 143.85", dmFrom(clk, "c_dm", alice))
	clk.Advance(settle)
	opened(t, c.s, "c_dm", 2)
	c.guess("-37.56, 143.85", dmFrom(clk, "c_dm", bob))
	c.guess("-37.6, 143.9", dmFrom(clk, "c_dm", alice))
	clk.Advance(settle)
	waitFor(t, c.s, "the end", func() bool { return c.s.rounds["c_dm"] == nil })
	page := c.outbox("0")
	if last := page.Ops[len(page.Ops)-2].Text; strings.Contains(last, "tie-breaker") || !strings.Contains(last, "🏁 **Game over**") {
		t.Errorf("the end %q", last)
	}
}

// A lobby gathers the game's photos while players join: with them all by
// the countdown's end, round 1 follows at once; else one post says how
// many are ready, and round 1 follows once there is one for each round.
func TestFetchLobby(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := gated{&fakeFinder{}, make(chan struct{})}
	c := fetchClient(t, t.TempDir(), clk, photos, 3, true)
	began := clk.Now()
	r := c.geo("europe 1m", from(clk, chat, alice))
	if !strings.Contains(words(r), "React to this message with any emoji to join") || r.Meta == nil || !r.Meta.Watch.Reactions ||
		r.Meta.Watch.Until != began.Add(gameTime(3, time.Minute, 0)+lobbyMax+fetchMax+watchSlack).Format(time.RFC3339) {
		t.Fatalf("lobby %+v", r)
	}
	close(photos.open)
	waitFor(t, c.s, "the photos, during the lobby", func() bool { return c.s.reserved(chat) == need(3) })
	join(c, clk, chat, alice)
	join(c, clk, chat, bob)
	clk.Advance(lobbyWait)
	opened(t, c.s, chat, 1)
	page := c.outbox("0")
	if len(page.Ops) != 1 || !strings.HasPrefix(page.Ops[0].Text, "**Round 1 of 3** · 🌍") || len(page.Ops[0].Images) != 1 {
		t.Fatalf("round 1 %+v", page)
	}
	c.s.mu.Lock()
	if place := c.s.rounds[chat].Place; !strings.Contains(place, "Paris") {
		t.Errorf("round 1 in %s", place)
	}
	c.s.mu.Unlock()

	// One photo when the countdown ends.
	clk = &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	fedPhotos := fed{&fakeFinder{}, make(chan struct{})}
	c = fetchClient(t, t.TempDir(), clk, fedPhotos, 3, true)
	if r := c.geo("1m", from(clk, chat, alice)); r.Meta == nil || !r.Meta.Watch.Reactions {
		t.Fatalf("lobby %+v", r)
	}
	fedPhotos.feed <- struct{}{}
	waitFor(t, c.s, "a photo", func() bool { return c.s.reserved(chat) == 1 })
	join(c, clk, chat, alice)
	join(c, clk, chat, bob)
	clk.Advance(lobbyWait)
	page = c.outbox("0")
	if len(page.Ops) != 1 || page.Ops[0].Text != "🔎 Still fetching: 1 of 3 spots ready…" || len(page.Ops[0].Images) != 0 {
		t.Fatalf("at the lobby's end %+v", page)
	}
	if g := c.guess("-37.6, 143.9", from(clk, chat, bob)); words(g) != "The game hasn't started yet." {
		t.Errorf("a guess before round 1: %q", words(g))
	}
	fedPhotos.feed <- struct{}{}
	fedPhotos.feed <- struct{}{}
	opened(t, c.s, chat, 1)
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 1 || !strings.HasPrefix(page.Ops[0].Text, "**Round 1 of 3** · 🌍") || len(page.Ops[0].Images) != 1 {
		t.Fatalf("round 1 %+v", page)
	}
	c.s.mu.Lock()
	if p := c.s.rounds[chat].Players; !slices.Equal(p, []string{alice, bob}) {
		t.Errorf("players %v", p)
	}
	c.s.mu.Unlock()
}

// fetchMax after /geo, a game with too few photos plays as many rounds as
// it has, saying so; with none, it is called off.
func TestFetchGiveUp(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := fed{&fakeFinder{}, make(chan struct{})}
	c := fetchClient(t, t.TempDir(), clk, photos, 3, false)
	r := c.geo("1m", dmFrom(clk, "c_dm", alice))
	for range 2 {
		photos.feed <- struct{}{}
	}
	waitFor(t, c.s, "2 photos", func() bool { return c.s.reserved("c_dm") == 2 })
	clk.Advance(fetchMax - time.Second)
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("gave up early: %+v", p)
	}
	clk.Advance(time.Second)
	page := c.outbox("0")
	if len(page.Ops) != 2 || page.Ops[0].Text != "Only found 2 spots, so this game is 2 rounds." || !strings.HasPrefix(page.Ops[1].Text, "**Round 1 of 2** · 🌍") {
		t.Fatalf("the give-up %+v", page)
	}
	c.guess("-37.6, 143.9", dmFrom(clk, "c_dm", alice))
	clk.Advance(settle)
	opened(t, c.s, "c_dm", 2)
	c.guess("-37.6, 143.9", dmFrom(clk, "c_dm", alice))
	clk.Advance(settle)
	waitFor(t, c.s, "the end", func() bool { return c.s.rounds["c_dm"] == nil })
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 4 || !strings.HasPrefix(page.Ops[2].Text, "**Round 2 of 2** · 📍") || !strings.Contains(page.Ops[2].Text, "🏁 **Game over**") ||
		page.Ops[3].Op != "unwatch" || page.Ops[3].Watch != r.Meta.Watch.ID {
		t.Fatalf("the end %+v", page)
	}

	c = fetchClient(t, t.TempDir(), clk, &fakeFinder{off: true}, 3, false)
	r = c.geo("1m", dmFrom(clk, "c_dm", alice))
	if words(r) != "🔎 Fetching spots for 3 rounds…" || r.Meta == nil {
		t.Fatalf("/geo %+v", r)
	}
	clk.Advance(fetchMax)
	page = c.outbox("0")
	if len(page.Ops) != 2 || page.Ops[0].Text != "I couldn't find a spot, so the game is off." ||
		page.Ops[1].Op != "unwatch" || page.Ops[1].Watch != r.Meta.Watch.ID {
		t.Fatalf("the call-off %+v", page)
	}
}

// In a group without a lobby, the starter's /geo end calls off a game
// still fetching its photos; /geo skip only says they are on their way.
func TestFetchEnd(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	c := fetchClient(t, t.TempDir(), clk, &fakeFinder{off: true}, 1, false)
	r := c.geo("1m", from(clk, chat, alice))
	if words(r) != "🔎 Fetching a spot…" || r.Meta == nil || r.Meta.Watch.Reactions {
		t.Fatalf("/geo %+v", r)
	}
	for _, tc := range []struct{ args, user, want string }{
		{"skip", alice, "Round 1's photo is on its way."},
		{"end", bob, "Only the player who started this round can end it."},
		{"end", alice, "The game is off."},
	} {
		if got := words(c.geo(tc.args, from(clk, chat, tc.user))); got != tc.want {
			t.Errorf("/geo %s by %s: %q", tc.args, tc.user, got)
		}
	}
	if p := c.outbox("0"); len(p.Ops) != 1 || p.Ops[0].Op != "unwatch" || p.Ops[0].Watch != r.Meta.Watch.ID {
		t.Errorf("ops %+v", p)
	}
	clk.Advance(fetchMax)
	if p := c.outbox("0"); len(p.Ops) != 1 {
		t.Errorf("ops after the end %+v", p)
	}
}

// /geo end returns a game's unused photos, the spare too, to the stock;
// the stock and a game's reserved photos survive a restart, files and
// all, but for those whose file is gone.
func TestStockKept(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	photos := &fakeFinder{}
	s := testServer(t, dir, clk, photos, io.Discard)
	s.perGame = 3
	stockUp(s, photos, "", need(3)+s.cfg.Buffer)
	c := connect(t, s)
	if r := c.geo("1m", dmFrom(clk, "c_dm", alice)); len(r.Content) != 2 {
		t.Fatalf("/geo %q", words(r))
	}
	if files := photoFiles(t, dir); len(files) != 3+s.cfg.Buffer {
		t.Errorf("%d files, with round 1's shown", len(files))
	}
	c.close()
	s.mu.Lock()
	gone := s.photoPath(s.stock[0])
	s.mu.Unlock()
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	stray := filepath.Join(dir, photoDir, "stray-00000000.jpg")
	if err := os.WriteFile(stray, []byte("jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}

	s = testServer(t, dir, clk, &fakeFinder{off: true}, io.Discard)
	s.perGame = 3
	for _, name := range []string{gone, stray} {
		if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a file no photo names is kept: %v", err)
		}
	}
	s.mu.Lock()
	reserved, stock := s.reserved("c_dm"), len(s.stock)
	jpegs := 0
	for _, k := range append(slices.Clone(s.stock), s.rounds["c_dm"].Photos...) {
		if bytes.HasPrefix(k.JPEG, []byte("jpeg ")) {
			jpegs++
		}
	}
	s.mu.Unlock()
	if reserved != 3 || stock != s.cfg.Buffer-1 || jpegs != 3+s.cfg.Buffer-1 {
		t.Errorf("after a restart: %d reserved, %d in the stock, %d JPEGs", reserved, stock, jpegs)
	}
	c = connect(t, s)
	if got := words(c.geo("end", dmFrom(clk, "c_dm", alice))); !strings.Contains(got, "Ballarat") {
		t.Errorf("/geo end: %q", got)
	}
	s.mu.Lock()
	stock = len(s.stock)
	sorted := slices.IsSortedFunc(s.stock, func(a, b kept) int { return a.Found.Compare(b.Found) })
	s.mu.Unlock()
	if stock != 3+s.cfg.Buffer-1 || !sorted {
		t.Errorf("%d in the stock after /geo end, oldest first %v", stock, sorted)
	}
	c.close()
	var st saved
	if _, err := s.doc.Load(&st); err != nil || len(st.Stock) != 3+s.cfg.Buffer-1 || len(st.Rounds) != 0 {
		t.Errorf("saved %d in the stock, %d games, %v", len(st.Stock), len(st.Rounds), err)
	}
	if files := photoFiles(t, dir); len(files) != len(st.Stock) {
		t.Errorf("%d files", len(files))
	}
}

// A stocked photo too old is dropped at start, its file too, and a state
// saved before the stock loads.
func TestStockOld(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "geo.json"), []byte(`{"rounds":[],"used":["Mapillary:9"],"scores":{},"outbox":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	photos := &fakeFinder{}
	s := testServer(t, dir, clk, photos, io.Discard)
	if len(s.used) != 1 || len(s.stock) != 0 {
		t.Fatalf("an older state: %d used, %d in the stock", len(s.used), len(s.stock))
	}
	stockUp(s, photos, "", 2)
	clk.Advance(maxStockAge + time.Minute)
	stockUp(s, photos, "", 1)
	s = testServer(t, dir, clk, photos, io.Discard)
	if len(s.stock) != 1 || s.stock[0].ID != "3" {
		t.Errorf("after a week: %+v", s.stock)
	}
	if files := photoFiles(t, dir); len(files) != 1 {
		t.Errorf("%d files", len(files))
	}
}

// A game in Europe keeps "europe" stocked with a full game, remembered
// across a restart, after which the prefetcher fills it unasked.
func TestAutoKeep(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	photos := &fakeFinder{}
	s := testServer(t, dir, clk, &fakeFinder{off: true}, io.Discard) // no refill before the restart
	s.perGame = 3
	stockUp(s, photos, "europe", need(3))
	c := connect(t, s)
	if r := c.geo("europe 1m", dmFrom(clk, "c_dm", alice)); len(r.Content) != 2 {
		t.Fatalf("/geo europe %q", words(r))
	}
	s.mu.Lock()
	recent := slices.Clone(s.recent)
	s.mu.Unlock()
	if len(recent) != 1 || recent[0].Key != "europe" {
		t.Errorf("recent %+v", recent)
	}
	c.close()

	photos = &fakeFinder{}
	s = testServer(t, dir, clk, photos, io.Discard)
	s.perGame = 3
	connect(t, s)
	waitFor(t, s, "a full game in Europe", func() bool { return s.stocked("europe") == need(3) && s.count("europe") == need(3) })
	photos.mu.Lock()
	defer photos.mu.Unlock()
	if n := len(slices.DeleteFunc(slices.Clone(photos.asked), func(r string) bool { return r != "europe" })); n != need(3) {
		t.Errorf("%d searches in Europe", n)
	}
}

// The prefetcher searches for a gathering game before the stock, and
// refills the stock's keys in turn, so a slow key can't starve another;
// the auto-kept keys are bounded.
func TestSearchOrder(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	s := testServer(t, t.TempDir(), clk, &fakeFinder{}, io.Discard, "GEO_KEEP", "oceania")
	now := time.Now()
	s.mu.Lock()
	for _, key := range []string{"AU", "NZ", "europe", "asia", "africa", "FR", "JP", "OCEANIA"} {
		s.noteKey(key, clk.Now())
	}
	if len(s.recent) != maxRecent || s.recent[0].Key != "JP" {
		t.Errorf("recent %+v", s.recent)
	}
	s.rounds[chat] = &round{Chat: chat, Region: "africa", Of: 1, GiveUp: clk.Now().Add(fetchMax)}
	s.mu.Unlock()
	var order []string
	for range 4 {
		key, ok, _ := s.nextSearch(now)
		if !ok {
			t.Fatal("nothing to search")
		}
		order = append(order, key)
		if key == "africa" {
			s.mu.Lock()
			s.rounds[chat].Photos = []kept{{}}
			s.mu.Unlock()
		}
	}
	if !slices.Equal(order, []string{"africa", "oceania", "JP", "FR"}) {
		t.Errorf("searched %q", order)
	}
	s.mu.Lock()
	s.retries["africa"] = retry{at: now.Add(time.Minute)} // a failing key waits its turn's backoff
	s.mu.Unlock()
	for range 3 {
		if key, _, _ := s.nextSearch(now); key == "africa" {
			t.Error("searched a key in its backoff")
		}
	}
}

// A game saved while gathering its photos goes on gathering after a
// restart (a lobby's too), giving up fetchMax after /geo, or starts once
// they are found.
func TestFetchRestart(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	c := fetchClient(t, dir, clk, &fakeFinder{off: true}, 1, false)
	c.geo("1m", dmFrom(clk, "c_dm", alice))
	c.close()

	clk = &fakeClock{now: clk.Now().Add(2 * time.Minute)}
	c = fetchClient(t, dir, clk, &fakeFinder{off: true}, 1, false)
	if got := words(c.geo("", dmFrom(clk, "c_dm", alice))); got != "A game is on: round 1's photo is on its way." {
		t.Errorf("after a restart: %q", got)
	}
	clk.Advance(fetchMax - 2*time.Minute - time.Second)
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("gave up early: %+v", p)
	}
	clk.Advance(time.Second)
	page := c.outbox("0")
	if len(page.Ops) != 2 || page.Ops[0].Text != "I couldn't find a spot, so the game is off." || page.Ops[1].Op != "unwatch" {
		t.Fatalf("the give-up %+v", page)
	}
	c.close()

	clk = &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	dir = t.TempDir()
	c = fetchClient(t, dir, clk, &fakeFinder{off: true}, 1, false)
	c.geo("1m", dmFrom(clk, "c_dm", alice))
	c.close()
	c = fetchClient(t, dir, clk, &fakeFinder{}, 1, false)
	opened(t, c.s, "c_dm", 1)
	if page := c.outbox("0"); len(page.Ops) != 1 || !strings.HasPrefix(page.Ops[0].Text, "🌍 **Where is this?**") || len(page.Ops[0].Images) != 1 {
		t.Fatalf("round 1 after a restart %+v", page)
	}
	c.close()

	dir = t.TempDir()
	c = fetchClient(t, dir, clk, &fakeFinder{off: true}, 1, true)
	if r := c.geo("europe 1m", from(clk, chat, alice)); r.Meta == nil || !r.Meta.Watch.Reactions {
		t.Fatalf("/geo europe: %q", words(r))
	}
	c.close()
	s := testServer(t, dir, clk, &fakeFinder{off: true}, io.Discard)
	s.mu.Lock()
	lobby := s.rounds[chat] != nil && s.rounds[chat].Lobby
	s.mu.Unlock()
	if key, ok, _ := s.nextSearch(time.Now()); !lobby || !ok || key != "europe" {
		t.Errorf("after a restart: lobby %v, searching %q", lobby, key)
	}
}

// A game gathering photos where searches fail leaves them to the key's
// backoff, rather than searching again at once.
func TestFetchBackoff(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := failIn{&fakeFinder{elsewhere: true}, "AU", geo.ErrNotFound}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard, "GEO_KEEP", "AU")
	s.cfg.grace = 50 * time.Millisecond
	c := connect(t, s)
	if got := words(c.geo("AU 1m", dmFrom(clk, "c_dm", alice))); got != "🔎 Fetching a spot…" {
		t.Fatalf("/geo AU: %q", got)
	}
	time.Sleep(300 * time.Millisecond)
	photos.mu.Lock()
	tries := len(slices.DeleteFunc(slices.Clone(photos.asked), func(r string) bool { return r != "AU" }))
	photos.mu.Unlock()
	if tries > 5 {
		t.Errorf("searched in AU %d times without a pause", tries)
	}
	if got := words(c.geo("end", dmFrom(clk, "c_dm", alice))); got != "The game is off." {
		t.Errorf("/geo end: %q", got)
	}
}

// Round 1 found after /geo answered waits the gap, so the answer goes out
// before the photo.
func TestFetchGap(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := gated{&fakeFinder{}, make(chan struct{})}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard, "gap", "on")
	s.cfg.grace = 50 * time.Millisecond
	c := connect(t, s)
	if got := words(c.geo("1m", dmFrom(clk, "c_dm", alice))); got != "🔎 Fetching a spot…" {
		t.Fatalf("/geo: %q", got)
	}
	close(photos.open)
	waitFor(t, s, "the photo", func() bool { return s.reserved("c_dm") == 1 })
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Fatalf("round 1 without the gap: %+v", p)
	}
	clk.Advance(gapTime)
	opened(t, s, "c_dm", 1)
	if p := c.outbox("0"); len(p.Ops) != 1 || len(p.Ops[0].Images) != 1 {
		t.Fatalf("round 1 %+v", p)
	}
}

// The texts while a game's photos are fetched.
func TestFetchTexts(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{fetching(3), "🔎 Fetching spots for 3 rounds…"},
		{fetching(1), "🔎 Fetching a spot…"},
		{stillFetching(2, 3), "🔎 Still fetching: 2 of 3 spots ready…"},
		{shortGame(1), "Only found 1 spot, so this game is 1 round."},
		{fileName("Panoramax:ab/../c d"), "Panoramax_ab____c_d-" + fmt.Sprintf("%08x", fnvOf("Panoramax:ab/../c d")) + ".jpg"},
	} {
		if tc.got != tc.want {
			t.Errorf("%q, want %q", tc.got, tc.want)
		}
	}
}

// fnvOf is the FNV-1a hash fileName adds to a key.
func fnvOf(key string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(key))
	return h.Sum32()
}

// games makes the server's games of rounds from now on.
func (c *client) games(rounds int) {
	c.s.mu.Lock()
	c.s.perGame = rounds
	c.s.mu.Unlock()
}

// Photos of a kind no longer kept don't stop the stock refilling at the
// cap: a refill pushes one out.
func TestCapSurplus(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := &fakeFinder{}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard)
	s.perGame = 3
	s.mu.Lock()
	s.noteKey("AU", clk.Now())
	s.mu.Unlock()
	stockUp(s, photos, "AU", maxHeld)
	s.mu.Lock()
	s.recent = nil // AU is no longer kept: its photos are surplus
	s.noteKey("europe", clk.Now())
	s.mu.Unlock()
	for range 10 {
		key, ok, _ := s.nextSearch(time.Now())
		if !ok {
			break
		}
		stockUp(s, photos, key, 1)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if eu, plain, au, held := s.stocked("europe"), s.stocked(""), s.stocked("AU"), s.held(); eu != need(3) || plain != s.cfg.Buffer || au != maxHeld-need(3)-s.cfg.Buffer || held != maxHeld {
		t.Errorf("%d in Europe, %d plain, %d in AU, %d in all", eu, plain, au, held)
	}
}

// Photos a game returns go to a game still gathering photos, which starts
// at once.
func TestRestockGatherer(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	c := fetchClient(t, t.TempDir(), clk, &fakeFinder{off: true}, 3, false)
	stockUp(c.s, &fakeFinder{}, "europe", need(3))
	if r := c.geo("europe 1m", dmFrom(clk, "c_b", alice)); len(r.Content) != 2 {
		t.Fatalf("/geo europe %q", words(r))
	}
	if got := words(c.geo("europe 1m", dmFrom(clk, "c_a", bob))); got != "🔎 Fetching spots for 3 rounds…" {
		t.Fatalf("/geo europe %q", got)
	}
	c.geo("end", dmFrom(clk, "c_b", alice))
	opened(t, c.s, "c_a", 1)
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if n, stock := c.s.reserved("c_a"), len(c.s.stock); n != 2 || stock != 0 {
		t.Errorf("%d reserved, %d in the stock", n, stock)
	}
}

// A search only refilling the stock is stopped for a game that starts
// gathering photos of another kind, and doesn't count as a failure.
func TestSearchInterrupted(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := &fakeFinder{off: true}
	c := fetchClient(t, t.TempDir(), clk, photos, 3, false)
	asked := func(key string) func() bool {
		return func() bool {
			photos.mu.Lock()
			defer photos.mu.Unlock()
			return slices.Contains(photos.asked, key)
		}
	}
	waitFor(t, c.s, "a refill", asked(""))
	c.geo("europe 1m", dmFrom(clk, "c_dm", alice))
	waitFor(t, c.s, "the game's search", asked("europe"))
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if _, failed := c.s.retries[""]; failed {
		t.Error("the stopped refill counts as a failure")
	}
}

// The photos of a game still starting are saved as stock, so a restart
// keeps them.
func TestStartingSaved(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	photos := &fakeFinder{}
	s := testServer(t, dir, clk, photos, io.Discard)
	s.perGame = 3
	stockUp(s, photos, "", need(3))
	s.mu.Lock()
	r := &round{ID: "r1", Chat: "c_dm", Of: 3, Waiting: true, starting: true}
	s.rounds[r.Chat] = r
	s.reserveLocked(r)
	s.mu.Unlock()
	s.save()
	s = testServer(t, dir, clk, photos, io.Discard)
	if len(s.stock) != need(3) || len(s.rounds) != 0 {
		t.Errorf("after a restart: %d in the stock, %d games", len(s.stock), len(s.rounds))
	}
	if files := photoFiles(t, dir); len(files) != need(3) {
		t.Errorf("%d files", len(files))
	}
}
