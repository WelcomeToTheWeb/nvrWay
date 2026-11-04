// S6-08's other half: the signal path's broadcasts.
//
// `ws/events_test.go` proves the hub throttles and fans out. What lives here is the
// claim that matters to the wall: that the route the worker posts to is what puts an
// event on the live feed, with the fields a LIVE card reads, and that a storm of
// extends over that route still costs one frame per five seconds per console.
//
// The console here is a real WebSocket. A spy on `Hub` would prove the call is made;
// it would not prove the frame arrives, in order, with a payload that can be drawn.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"nvr/server/models"
	"nvr/server/ws"
)

// liveOrigin is the console origin the hub's allowlist carries. It is the fixture
// config's own WEB_URL, which is what a single-host installation falls back to.
const liveOrigin = "http://localhost:3000"

// liveEnvelope is one frame as the console receives it.
type liveEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	TS      time.Time       `json:"ts"`
}

// liveConsole is the worker's signal route with one console connected to /ws.
type liveConsole struct {
	f      *detectorFixture
	frames chan liveEnvelope
}

func newLiveConsole(t *testing.T) *liveConsole {
	t.Helper()

	f := newDetectorFixture(t, testWorkerToken)

	// The hub is mounted by Register, and the fixture's router was built before a
	// hub existed. A second router over the same handlers is the cheap way to get
	// the /ws route without rebuilding the signal fixture.
	hub := ws.NewHub(f.Handlers.JWT, []string{liveOrigin})
	f.Handlers.Hub = hub
	server := httptest.NewServer(routerFor(t, f.Handlers))
	t.Cleanup(server.Close)

	token, _ := usersToken(t, f.Handlers)
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()

	header := http.Header{}
	header.Set("Origin", liveOrigin)
	conn, _, err := websocket.Dial(dialCtx,
		strings.Replace(server.URL, "http://", "ws://", 1)+"/ws?token="+token,
		&websocket.DialOptions{HTTPHeader: header, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		t.Fatalf("dialing /ws: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "test over") })

	frames := make(chan liveEnvelope, 128)
	go func() {
		defer close(frames)
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var envelope liveEnvelope
			if json.Unmarshal(data, &envelope) == nil {
				frames <- envelope
			}
		}
	}()

	// The hub registers a socket a moment after the handshake returns, and a
	// broadcast sent in that window goes to the consoles that are connected — which
	// is none of them. A test that broadcasts into a socket the hub has not
	// registered is testing a race.
	deadline := time.Now().Add(2 * time.Second)
	for hub.Count() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the hub reports %d consoles, want 1", hub.Count())
		}
		time.Sleep(10 * time.Millisecond)
	}

	return &liveConsole{f: f, frames: frames}
}

// await returns the first frame of one type. Presence is skipped on purpose: the
// hub announces it when consoles join and leave, and a test waiting for event.new
// must not mistake it for the event.
func (c *liveConsole) await(t *testing.T, typ string, within time.Duration) liveEnvelope {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("no %s frame arrived within %s", typ, within)
		}
		select {
		case envelope, ok := <-c.frames:
			if !ok {
				t.Fatalf("the socket ended while waiting for %s", typ)
			}
			if envelope.Type == typ {
				return envelope
			}
		case <-time.After(remaining):
			t.Fatalf("no %s frame arrived within %s", typ, within)
		}
	}
}

// eventFrames returns the event frames that arrive within the window. Silence is a
// result: "the throttle sent one frame" is only an assertion if the test waited to
// see whether a second one came.
func (c *liveConsole) eventFrames(t *testing.T, within time.Duration) []liveEnvelope {
	t.Helper()
	var out []liveEnvelope
	deadline := time.Now().Add(within)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return out
		}
		select {
		case envelope, ok := <-c.frames:
			if !ok {
				return out
			}
			if strings.HasPrefix(envelope.Type, "event.") {
				out = append(out, envelope)
			}
		case <-time.After(remaining):
			return out
		}
	}
}

// postSignal sends one signal as the worker and fails the test on a non-200.
func (c *liveConsole) postSignal(t *testing.T, cam *models.Camera, key, action string) {
	t.Helper()
	s := signal(cam.ID, key, action)
	status, body, raw := postSignals(t, c.f, []map[string]any{s}, testWorkerToken)
	if status != http.StatusOK {
		t.Fatalf("%s answered %d: %s", action, status, raw)
	}
	// A signal the store refused answers 200 with an `error`, so the status alone
	// does not prove the event moved.
	if msg, ok := body["error"].(string); ok && msg != "" {
		t.Fatalf("%s was refused: %s", action, msg)
	}
}

// liveEventView is the payload's shape as the console sees it: the event view the
// list and the drawer answer, which is what the wall draws a card from.
type liveEventView struct {
	ID               string   `json:"id"`
	CameraID         string   `json:"camera_id"`
	CameraSlug       string   `json:"camera_slug"`
	Open             bool     `json:"open"`
	Seconds          float64  `json:"seconds"`
	Classes          []string `json:"classes"`
	Zones            []string `json:"zones"`
	Rules            []string `json:"rules"`
	Severity         int      `json:"severity"`
	SignalCount      int      `json:"signal_count"`
	ObjectCount      int      `json:"object_count"`
	HasSnapshot      bool     `json:"has_snapshot"`
	HasClip          bool     `json:"has_clip"`
	ClipUploaded     bool     `json:"clip_uploaded"`
	SnapshotUploaded bool     `json:"snapshot_uploaded"`
}

func decodeLive(t *testing.T, frame liveEnvelope) liveEventView {
	t.Helper()
	var view liveEventView
	if err := json.Unmarshal(frame.Payload, &view); err != nil {
		t.Fatalf("the %s payload is not the event view: %v (%s)", frame.Type, err, frame.Payload)
	}
	return view
}

func liveCamera(t *testing.T, f *detectorFixture) *models.Camera {
	t.Helper()
	return f.addCamera(t, "porch", true, true, models.RecordModeContinuous,
		`[{"id":"z1","name":"front step","polygon":[[0,0],[1,0],[1,1],[0,1]],`+
			`"classes":["person"],"rule":"loiter","dwell_seconds":30,"severity":3,`+
			`"min_confidence":0.4,"min_area_ratio":0.004,"enabled":true}]`)
}

func TestEventSignalsPutNewUpdateCloseOnTheLiveFeed(t *testing.T) {
	// The sequence a console sees is the sequence the store applied, and nothing
	// else sends it. That is what lets the wall show LIVE from these frames alone.
	c := newLiveConsole(t)
	cam := liveCamera(t, c.f)
	key := "live-sequence-1"

	c.postSignal(t, cam, key, models.SignalActionOpen)
	opened := c.await(t, "event.new", 3*time.Second)
	view := decodeLive(t, opened)
	if !view.Open {
		t.Error("event.new says the event is closed")
	}
	if view.ID == "" || view.CameraID != cam.ID {
		t.Errorf("the frame names event %q on camera %q, want the event that just opened", view.ID, view.CameraID)
	}
	if view.CameraSlug != "porch" {
		t.Errorf("camera_slug is %q, want the slug an operator recognises", view.CameraSlug)
	}
	// The zone arrives by the name the operator drew, not by the id the row stores:
	// the console has no way to resolve an id it has never been given.
	if len(view.Zones) != 1 || view.Zones[0] != "front step" {
		t.Errorf("zones is %v, want the zone name the operator typed", view.Zones)
	}
	if len(view.Classes) == 0 || view.Severity < models.SeverityLow {
		t.Errorf("the frame carries classes %v and severity %d, want what the wall draws", view.Classes, view.Severity)
	}
	if view.ObjectCount < 0 {
		t.Errorf("object_count is %d, want the count the wall shows without the boxes", view.ObjectCount)
	}
	eventID := view.ID

	c.postSignal(t, cam, key, models.SignalActionExtend)
	updated := c.await(t, "event.update", 3*time.Second)
	if got := decodeLive(t, updated); got.ID != eventID || !got.Open {
		t.Errorf("event.update names %q open=%v, want the same event still open", got.ID, got.Open)
	}

	c.postSignal(t, cam, key, models.SignalActionClose)
	closed := c.await(t, "event.close", 3*time.Second)
	if got := decodeLive(t, closed); got.Open {
		t.Error("event.close still says open, and the LIVE badge would never come off")
	}

	// The order, not just the presence: a close that arrived before its open would
	// put a card on the wall that never comes off.
	c.f.Handlers.Hub.ForgetEvent(eventID)
}

func TestEventExtendStormOverTheRouteSendsOneUpdatePerFiveSeconds(t *testing.T) {
	// The exit criterion through the door the worker actually uses. A worker extends
	// on every detection tick; without the throttle this route would put a frame per
	// signal on every console, and the wall would be a slideshow of numbers nobody
	// can read.
	c := newLiveConsole(t)
	cam := liveCamera(t, c.f)
	key := "live-storm-1"

	c.postSignal(t, cam, key, models.SignalActionOpen)
	c.await(t, "event.new", 3*time.Second)

	for i := 0; i < 12; i++ {
		c.postSignal(t, cam, key, models.SignalActionExtend)
	}

	frames := c.eventFrames(t, 500*time.Millisecond)
	updates := 0
	for _, frame := range frames {
		if frame.Type == "event.update" {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("twelve extends put %d event.update frames on the console, want 1", updates)
	}

	// The open and the close are not throttled, so the sequence still closes: the
	// badge comes off whenever the event ends, however recently an update went out.
	c.postSignal(t, cam, key, models.SignalActionClose)
	if got := c.await(t, "event.close", 3*time.Second); decodeLive(t, got).Open {
		t.Error("the close after a storm still says open")
	}
}

func TestEventLiveFeedIsUnaffectedWhenThereIsNoHub(t *testing.T) {
	// `Hub` nil is a stated state — a process built without the live feed — and the
	// signal path must not notice. The fixture without a hub is the one every other
	// suite in this package uses, so this is the case that keeps those honest.
	f := newDetectorFixture(t, testWorkerToken)
	f.Handlers.Hub = nil
	cam := liveCamera(t, f)

	for _, action := range []string{models.SignalActionOpen, models.SignalActionExtend, models.SignalActionClose} {
		s := signal(cam.ID, "live-no-hub", action)
		status, _, raw := postSignals(t, f, []map[string]any{s}, testWorkerToken)
		if status != http.StatusOK {
			t.Fatalf("%s with no hub answered %d: %s", action, status, raw)
		}
	}
}

func TestClipAssemblyPutsTheFinishedEventOnTheLiveFeed(t *testing.T) {
	// The clip is the one fact about a closed event that still moves, and it lands on
	// a timer rather than in a request. Without this the console's player waits on a
	// clip that already exists until someone reloads the wall.
	c := newLiveConsole(t)
	cam := liveCamera(t, c.f)

	event := &models.Event{
		EventKey:    "cam:zone:loiter:1762247703",
		CameraID:    cam.ID,
		Action:      models.SignalActionClose,
		StartedAt:   time.Now().UTC().Add(-time.Minute),
		EndedAt:     time.Now().UTC().Add(-45 * time.Second),
		Open:        false,
		ClassesJSON: `["person"]`,
		ZonesJSON:   `["z1"]`,
		RulesJSON:   `["loiter"]`,
		Severity:    2,
	}
	if err := c.f.Store.DB.Create(event).Error; err != nil {
		t.Fatalf("creating the event: %v", err)
	}
	// `open` is a zero value with a schema default of true, so a direct Create
	// writes the default and hands it back on the struct. The row is a closed one —
	// an assembly only runs after the close — and the view mirrors the row.
	event.Open = false

	// What the clip builder's callback does, on the row as it reads once the file
	// exists.
	event.LocalClipPath = "/var/lib/nvr/clips/" + event.ID + ".mp4"
	event.ClipSeconds = 23.5
	c.f.Handlers.BroadcastEventUpdate(event)

	frame := c.await(t, "event.update", 3*time.Second)
	view := decodeLive(t, frame)
	if view.ID != event.ID {
		t.Errorf("the frame names %q, want the event whose clip landed", view.ID)
	}
	if !view.HasClip || view.Seconds == 0 {
		t.Errorf("the frame says has_clip=%v seconds=%.2f, want the clip the console can now play", view.HasClip, view.Seconds)
	}
	if view.Open {
		t.Error("an assembly update says the event is open again")
	}
}
