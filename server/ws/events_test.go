package ws

// S6-08 — the event fan-out: `event.new`, a throttled `event.update`, and
// `event.close`.
//
// The three exit criteria are the throttle asserted against a storm, two consoles
// both receiving the sequence, and an open event being renderable as LIVE from
// these frames alone. The throttle is the only one of the three that is a behaviour
// of the hub rather than of its callers, and it is the one that needs a clock the
// test can turn: the window is five seconds, and a suite that slept through it would
// be slow and would assert the same thing.

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"nvr/server/models"
)

// liveCard is the payload the signal path sends for an event — the event view the
// list and the drawer answer, reduced here to the fields a LIVE card reads. The hub
// passes the payload through untouched, and these tests use the real shape rather
// than a map so "the console can draw this" is a claim about fields, not about
// whatever a test invented.
type liveCard struct {
	ID          string   `json:"id"`
	CameraID    string   `json:"camera_id"`
	CameraSlug  string   `json:"camera_slug"`
	StartedAt   string   `json:"started_at"`
	EndedAt     string   `json:"ended_at"`
	Seconds     float64  `json:"seconds"`
	Open        bool     `json:"open"`
	Classes     []string `json:"classes"`
	Zones       []string `json:"zones"`
	Severity    int      `json:"severity"`
	SignalCount int      `json:"signal_count"`
	HasSnapshot bool     `json:"has_snapshot"`
	HasClip     bool     `json:"has_clip"`
}

func newLiveCard(signalCount int) liveCard {
	return liveCard{
		ID:          "evt-1",
		CameraID:    "cam-1",
		CameraSlug:  "front-door",
		StartedAt:   "2026-11-04T09:15:03.400Z",
		EndedAt:     "2026-11-04T09:15:23.400Z",
		Seconds:     20,
		Open:        true,
		Classes:     []string{"person"},
		Zones:       []string{"porch"},
		Severity:    models.SeverityHigh,
		SignalCount: signalCount,
		HasSnapshot: true,
	}
}

// mutableClock is the hub's clock with a dial the test turns.
type mutableClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *mutableClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *mutableClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func newClock() *mutableClock {
	return &mutableClock{at: time.Date(2026, 11, 4, 9, 15, 0, 0, time.UTC)}
}

// oneConsole is a hub with one connected client and its feed.
func oneConsole(t *testing.T, clock *mutableClock) (*Hub, *feed) {
	t.Helper()

	var now func() time.Time = time.Now
	if clock != nil {
		now = clock.now
	}
	hub, jwt, server := newTestHubAt(t, now, allowedHost)
	token := validToken(t, jwt)

	conn, f, err := dial(t, server, token, allowedHost)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "test over") })
	awaitClients(t, hub, 1, 2*time.Second)
	return hub, f
}

// eventFramesOf keeps the event frames and drops the presence announcements the
// hub sends when consoles come and go. Every count in this file is a count of event
// frames: a test that counted raw frames would be counting who else is watching.
func eventFramesOf(frames []Envelope) []Envelope {
	out := []Envelope{}
	for _, frame := range frames {
		switch frame.Type {
		case TypeEventNew, TypeEventUpdate, TypeEventClose:
			out = append(out, frame)
		}
	}
	return out
}

// eventTypesOf is eventFramesOf reduced to the types, in the order they arrived.
func eventTypesOf(frames []Envelope) []string {
	out := []string{}
	for _, frame := range eventFramesOf(frames) {
		out = append(out, frame.Type)
	}
	return out
}

func decodeCard(t *testing.T, frame Envelope) liveCard {
	t.Helper()
	var card liveCard
	if err := json.Unmarshal(frame.Payload, &card); err != nil {
		t.Fatalf("the %s payload is not the event view: %v (%s)", frame.Type, err, frame.Payload)
	}
	return card
}

func TestEventUpdateStormSendsAtMostOneFramePerFiveSeconds(t *testing.T) {
	// The exit criterion. A worker extends on every detection tick — five a second
	// on a busy camera — and sixty such cameras would put three thousand frames a
	// second on every console to say nothing an operator can read.
	clock := newClock()
	hub, f := oneConsole(t, clock)

	const storm = 25
	for i := 1; i <= storm; i++ {
		if err := hub.EventUpdateThrottled("evt-1", newLiveCard(i)); err != nil {
			t.Fatalf("event.update %d: %v", i, err)
		}
	}

	// The leading edge goes; the other twenty-four do not. `collectFrames` waits
	// for a second one rather than assuming silence, which is the whole assertion.
	frames := eventFramesOf(collectFrames(t, f, 400*time.Millisecond))
	if len(frames) != 1 {
		t.Fatalf("a storm of %d extends put %d frames on the console, want 1", storm, len(frames))
	}
	card := decodeCard(t, frames[0])
	if card.SignalCount != 1 {
		t.Errorf("the frame that went carries signal_count %d, want the first extend (1): the throttle is the leading edge", card.SignalCount)
	}

	// Still inside the window: nothing new arrives, however many extends come.
	clock.advance(EventUpdateWindow - time.Millisecond)
	for i := storm + 1; i <= storm*2; i++ {
		_ = hub.EventUpdateThrottled("evt-1", newLiveCard(i))
	}
	if extra := eventFramesOf(collectFrames(t, f, 300*time.Millisecond)); len(extra) != 0 {
		t.Errorf("%d event frames arrived inside the window, want silence", len(extra))
	}

	// The window closes: the next extend goes, and it carries the state as it now
	// reads rather than the state that was dropped.
	clock.advance(2 * time.Millisecond)
	if err := hub.EventUpdateThrottled("evt-1", newLiveCard(storm*2+1)); err != nil {
		t.Fatalf("event.update after the window: %v", err)
	}
	next := awaitFrame(t, f, TypeEventUpdate, 2*time.Second)
	if got := decodeCard(t, next).SignalCount; got != storm*2+1 {
		t.Errorf("the frame after the window carries signal_count %d, want %d", got, storm*2+1)
	}
}

func TestEventUpdateThrottleIsPerEventNotGlobal(t *testing.T) {
	// Two events extending at once are two cards on the wall. A throttle keyed on
	// nothing (or on something coarser than the event) would let one camera's storm
	// silence another's, which is a livelock with a friendly name.
	clock := newClock()
	hub, f := oneConsole(t, clock)

	first := newLiveCard(1)
	other := first
	other.ID = "evt-2"

	for _, card := range []liveCard{first, other, first, other, first} {
		if err := hub.EventUpdateThrottled(card.ID, card); err != nil {
			t.Fatalf("event.update for %s: %v", card.ID, err)
		}
	}

	frames := eventFramesOf(collectFrames(t, f, 400*time.Millisecond))
	if len(frames) != 2 {
		t.Fatalf("%d event frames arrived for two events, want one per event", len(frames))
	}
	ids := map[string]bool{}
	for _, frame := range frames {
		ids[decodeCard(t, frame).ID] = true
	}
	if !ids["evt-1"] || !ids["evt-2"] {
		t.Errorf("the frames name %v, want one of each event", ids)
	}

	// And the window is per event: evt-2's first update is not spent by evt-1's
	// storm, so advancing past evt-1's window is not needed to hear from evt-2.
	clock.advance(EventUpdateWindow + time.Second)
	if err := hub.EventUpdateThrottled("evt-1", first); err != nil {
		t.Fatalf("event.update after the window: %v", err)
	}
	if got := awaitFrame(t, f, TypeEventUpdate, 2*time.Second); decodeCard(t, got).ID != "evt-1" {
		t.Error("the frame after the window is not evt-1's")
	}
}

func TestEventNewAndUpdateAndCloseReachBothClientsInOrder(t *testing.T) {
	// The exit criterion: two consoles, one sequence, both of them seeing it. The
	// hub is one fan-out, so a console that missed a close is a bug in this file and
	// not in the caller.
	clock := newClock()
	hub, jwt, server := newTestHubAt(t, clock.now, allowedHost)
	token := validToken(t, jwt)

	feeds := make([]*feed, 0, 2)
	for i := 0; i < 2; i++ {
		conn, f, err := dial(t, server, token, allowedHost)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "test over") })
		feeds = append(feeds, f)
	}
	awaitClients(t, hub, 2, 2*time.Second)

	opening := newLiveCard(1)
	if err := hub.EventNew(opening); err != nil {
		t.Fatalf("event.new: %v", err)
	}
	// The extend is throttled and the close is not, so the sequence a console sees
	// has exactly one update in it and a close that cannot be swallowed.
	if err := hub.EventUpdateThrottled(opening.ID, opening); err != nil {
		t.Fatalf("event.update: %v", err)
	}
	// Dropped: inside the window, and the close carries the final state anyway.
	if err := hub.EventUpdateThrottled(opening.ID, opening); err != nil {
		t.Fatalf("event.update (dropped): %v", err)
	}
	closed := opening
	closed.Open = false
	closed.Seconds = 42
	if err := hub.EventClose(closed); err != nil {
		t.Fatalf("event.close: %v", err)
	}

	for i, f := range feeds {
		frames := collectFrames(t, f, 500*time.Millisecond)
		got := eventTypesOf(frames)
		want := []string{TypeEventNew, TypeEventUpdate, TypeEventClose}
		if len(got) != len(want) {
			t.Errorf("console %d saw %v, want %v", i, got, want)
			continue
		}
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("console %d frame %d is %s, want %s", i, j, got[j], want[j])
			}
		}
		last := frames[len(frames)-1]
		if card := decodeCard(t, last); card.Open {
			t.Errorf("console %d received a close that still says open", i)
		}
	}
}

func TestEventCloseIsNotThrottledAndTheCallerForgetsTheGate(t *testing.T) {
	// A close inside an update's window still goes out: the badge coming off is not
	// something a five-second window may swallow, and the close is where the final
	// state of the event arrives.
	clock := newClock()
	hub, f := oneConsole(t, clock)

	if err := hub.EventUpdateThrottled("evt-1", newLiveCard(1)); err != nil {
		t.Fatalf("event.update: %v", err)
	}
	if err := hub.EventClose(newLiveCard(9)); err != nil {
		t.Fatalf("event.close: %v", err)
	}

	frames := eventFramesOf(collectFrames(t, f, 400*time.Millisecond))
	if got := eventTypesOf(frames); len(got) != 2 || got[1] != TypeEventClose {
		t.Fatalf("the console saw %v, want an update then a close", got)
	}

	// The gate outlives the close until the caller forgets the event, and that is
	// the shape of this hub: it broadcasts payloads it does not parse, so it cannot
	// know which id a close names. An event that closed will not extend again, so
	// the gate is only memory — which is why the signal path calls ForgetEvent, and
	// why the throttle evicts hour-old gates as a belt for the caller that forgot.
	if !hub.updates.gated("evt-1") {
		t.Error("the update's gate vanished on its own")
	}
	hub.ForgetEvent("evt-1")
	if hub.updates.gated("evt-1") {
		t.Error("ForgetEvent left the gate behind")
	}

	// So a late extend for an id that has closed (a replay, or a row someone
	// re-opened by hand) is not held back by a window belonging to a finished event.
	clock.advance(time.Millisecond)
	if err := hub.EventUpdateThrottled("evt-1", newLiveCard(10)); err != nil {
		t.Fatalf("event.update after the close: %v", err)
	}
	if got := awaitFrame(t, f, TypeEventUpdate, 2*time.Second); decodeCard(t, got).SignalCount != 10 {
		t.Error("the update after the close did not arrive")
	}
}

func TestEventFramesCarryWhatALiveCardNeedsWithoutAnyRESTCall(t *testing.T) {
	// The exit criterion, stated as the console's contract: an open event shows
	// LIVE from these frames alone. So the frame has to name the event, the camera
	// by the slug an operator recognises, the window it is running, the classes and
	// zones it accumulated, and how many signals produced it — and it has to say
	// `open`, because that is the badge.
	clock := newClock()
	hub, f := oneConsole(t, clock)

	if err := hub.EventNew(newLiveCard(1)); err != nil {
		t.Fatalf("event.new: %v", err)
	}
	frame := awaitFrame(t, f, TypeEventNew, 2*time.Second)

	if frame.TS.UTC().Format(time.RFC3339) != clock.now().UTC().Format(time.RFC3339) {
		t.Errorf("the envelope's ts is %v, want the hub's clock", frame.TS)
	}
	card := decodeCard(t, frame)
	if card.ID == "" || card.CameraSlug != "front-door" {
		t.Errorf("the card names %q on camera %q, want an event and a slug", card.ID, card.CameraSlug)
	}
	if !card.Open {
		t.Error("an event.new frame says the event is closed")
	}
	if card.Seconds <= 0 {
		t.Errorf("the card carries %.2fs, and the LIVE timer needs the window", card.Seconds)
	}
	if len(card.Classes) == 0 || len(card.Zones) == 0 {
		t.Errorf("the card carries classes %v and zones %v, want both: they are what the wall draws", card.Classes, card.Zones)
	}
	if card.Severity < models.SeverityLow || card.Severity > models.SeverityHigh {
		t.Errorf("the card carries severity %d, want the 1..3 the console colours by", card.Severity)
	}
	for _, ts := range []string{card.StartedAt, card.EndedAt} {
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("the card's timestamp %q is not RFC3339: %v", ts, err)
		}
	}
	// The media flags decide whether the card shows a picture and a player. Without
	// them the console would have to ask the REST surface, and "from these messages
	// alone" would be false.
	if !card.HasSnapshot {
		t.Error("the card carries no has_snapshot, so the console cannot draw the frame")
	}
	if card.HasClip {
		t.Error("an event that has just opened reports a clip")
	}
}

func TestEventUpdateWindowIsThePlansFiveSeconds(t *testing.T) {
	if EventUpdateWindow != 5*time.Second {
		t.Errorf("the throttle window is %s, want the plan's 5s", EventUpdateWindow)
	}
}

func TestStaleEventThrottleGatesAreForgotten(t *testing.T) {
	// The close path clears a gate, and an event that closed while nobody was
	// watching still clears it. The eviction is the belt for the case where the row
	// stopped existing without the hub being told — a restart, a delete by hand — so
	// the map stays the size of the events open right now.
	clock := newClock()
	hub, _ := oneConsole(t, clock)

	if err := hub.EventUpdateThrottled("evt-gone", newLiveCard(1)); err != nil {
		t.Fatalf("event.update: %v", err)
	}
	if !hub.updates.gated("evt-gone") {
		t.Fatal("the first update did not arm the gate")
	}

	// An hour of nothing, then a different event's update: the stale gate is gone.
	clock.advance(gateStaleAfter + time.Minute)
	other := newLiveCard(1)
	other.ID = "evt-alive"
	if err := hub.EventUpdateThrottled(other.ID, other); err != nil {
		t.Fatalf("event.update for the live event: %v", err)
	}
	if hub.updates.gated("evt-gone") {
		t.Error("a gate an hour old survived")
	}
	if !hub.updates.gated("evt-alive") {
		t.Error("the eviction took the gate that was just armed")
	}
}

func TestEventUpdateThrottleWithoutAnEventIDSends(t *testing.T) {
	// A caller with no id to key on gets its message sent rather than silently
	// dropped: sharing one global window with every other event is the bug the
	// throttle exists to prevent, and dropping a frame nobody can attribute is
	// worse than sending one.
	clock := newClock()
	hub, f := oneConsole(t, clock)

	for i := 1; i <= 3; i++ {
		if err := hub.EventUpdateThrottled("", newLiveCard(i)); err != nil {
			t.Fatalf("event.update with no id: %v", err)
		}
	}
	if frames := eventFramesOf(collectFrames(t, f, 400*time.Millisecond)); len(frames) != 3 {
		t.Errorf("unkeyed updates put %d frames on the console, want 3: the drop needs a key", len(frames))
	}
}
