package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"nhooyr.io/websocket"

	"nvr/server/auth"
	"nvr/server/models"
)

// bareSockets carries sockets accepted by the /bare route to whichever test
// asked for one.
var bareSockets = make(chan *websocket.Conn, 16)

const (
	testSecret  = "unit-test-secret-not-a-production-one"
	allowedHost = "https://nvr.example"
	foreignHost = "https://not-my-console.example"
)

// newTestHub is the hub behind a real listener: the exit criteria are about the
// socket (a bad token, a foreign origin, fan-out to every client), and a hub
// exercised through a function call cannot answer them.
func newTestHub(t *testing.T, origins ...string) (*Hub, *auth.JWTAuth, *httptest.Server) {
	t.Helper()
	return newTestHubAt(t, time.Now, origins...)
}

func newTestHubAt(t *testing.T, now func() time.Time, origins ...string) (*Hub, *auth.JWTAuth, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	jwt := auth.NewJWTAuth(testSecret)
	hub := NewHubWithClock(jwt, origins, now)

	engine := gin.New()
	engine.GET("/ws", hub.Handle)
	// A socket the hub does not own: the drop-path test needs a client whose
	// queue nobody drains, which is a client with no writer goroutine, and the
	// only honest way to get one is to accept a connection and keep it.
	engine.GET("/bare", func(c *gin.Context) {
		conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
			CompressionMode:    websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		bareSockets <- conn
		<-c.Request.Context().Done()
	})
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		for _, c := range hub.clientList() {
			c.close(websocket.StatusNormalClosure, "test over")
		}
	})
	return hub, jwt, server
}

func socketURL(server *httptest.Server, token string) string {
	return strings.Replace(server.URL, "http://", "ws://", 1) + "/ws?token=" + token
}

// dial opens one console socket. The dial context has a deadline; the read
// context never does. nhooyr ends a connection when the context of a Read
// expires, so a per-read timeout is a self-inflicted disconnect, and a harness
// that disconnects its own console reports failures the product does not have.
func dial(t *testing.T, server *httptest.Server, token, origin string) (*websocket.Conn, *feed, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	conn, _, err := websocket.Dial(ctx, socketURL(server, token), &websocket.DialOptions{
		HTTPHeader:      header,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, nil, err
	}
	return conn, newFeed(conn), nil
}

// feed is one socket's frames, read by one goroutine into a channel. The
// timeouts happen on the channel, on the test's side, never on the socket.
type feed struct {
	frames chan Envelope
}

func newFeed(conn *websocket.Conn) *feed {
	f := &feed{frames: make(chan Envelope, 256)}
	go func() {
		defer close(f.frames)
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var envelope Envelope
			if err := json.Unmarshal(data, &envelope); err != nil {
				continue
			}
			f.frames <- envelope
		}
	}()
	return f
}

func validToken(t *testing.T, jwt *auth.JWTAuth) string {
	t.Helper()
	token, err := jwt.Issue("user-1", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}
	return token
}

// collectFrames returns every frame that arrives within the window. Silence is a
// result: "exactly one frame" is only an assertion if the test waited to see
// whether a second one came.
// awaitClients waits until the hub has registered n consoles. A dial returns
// when the handshake completes, and the hub registers the socket a moment later
// on the handler goroutine: a broadcast issued in that window goes to the
// consoles that are connected, which is none of them. Tests that broadcast
// without waiting are testing a race, not the product.
func awaitClients(t *testing.T, hub *Hub, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for hub.Count() != n {
		if time.Now().After(deadline) {
			t.Fatalf("hub reports %d consoles, want %d", hub.Count(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func collectFrames(t *testing.T, f *feed, within time.Duration) []Envelope {
	t.Helper()
	var frames []Envelope
	deadline := time.Now().Add(within)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return frames
		}
		select {
		case envelope, ok := <-f.frames:
			if !ok {
				return frames
			}
			frames = append(frames, envelope)
		case <-time.After(remaining):
			return frames
		}
	}
}

// awaitFrame returns the first frame of the wanted type. Presence is skipped on
// purpose: the hub announces it when a console connects and when one leaves, and
// a test waiting for event.new must not mistake that for it.
func awaitFrame(t *testing.T, f *feed, typ string, within time.Duration) Envelope {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("no %s frame arrived within %s", typ, within)
		}
		select {
		case envelope, ok := <-f.frames:
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

func TestMessageTypeNamesAreExact(t *testing.T) {
	want := []string{
		"event.new",
		"event.update",
		"event.close",
		"camera.status",
		"recorder.status",
		"storage.queue",
		"presence",
	}
	got := MessageTypes()
	if len(got) != len(want) {
		t.Fatalf("message types: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("message type %d is %q, want %q (the console switches on these strings)", i, got[i], want[i])
		}
	}
}

func TestEnvelopeShapeIsTypePayloadTS(t *testing.T) {
	fixed := time.Date(2026, 11, 1, 12, 34, 56, 0, time.UTC)
	stamped := fixed.Add(90 * time.Second)
	hub, jwt, server := newTestHubAt(t, func() time.Time { return stamped }, allowedHost)

	conn, f, err := dial(t, server, validToken(t, jwt), allowedHost)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	awaitClients(t, hub, 1, 5*time.Second)

	if err := hub.EventNew(map[string]any{"event_id": "e-1", "camera_slug": "front"}); err != nil {
		t.Fatalf("broadcast: %v", err)
	}

	frame := awaitFrame(t, f, TypeEventNew, 5*time.Second)
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("re-encode the frame: %v", err)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatalf("frame is not JSON: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("envelope keys: got %v, want exactly type, payload, ts", keys)
	}
	for _, key := range []string{"type", "payload", "ts"} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("envelope has no %q key: %s", key, data)
		}
	}

	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Type != TypeEventNew {
		t.Fatalf("type: got %q, want %q", envelope.Type, TypeEventNew)
	}
	if !envelope.TS.Equal(stamped) {
		t.Fatalf("ts: got %s, want the server clock %s", envelope.TS, stamped)
	}
	if envelope.TS.Location() != time.UTC {
		t.Fatalf("ts must be stamped UTC, got location %v", envelope.TS.Location())
	}
	var payload map[string]any
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	if payload["event_id"] != "e-1" {
		t.Fatalf("payload did not carry the event: %v", payload)
	}
}

func TestBroadcastRefusesAnUnknownType(t *testing.T) {
	hub, _, _ := newTestHub(t, allowedHost)

	if err := hub.Broadcast("event.opened", nil); err == nil {
		t.Fatal("a type the console has no case for was broadcast")
	}
	for _, known := range MessageTypes() {
		if err := hub.Broadcast(known, nil); err != nil {
			t.Fatalf("known type %s refused: %v", known, err)
		}
	}
}

func TestFanOutReachesEveryConnectedClient(t *testing.T) {
	hub, jwt, server := newTestHub(t, allowedHost)
	token := validToken(t, jwt)

	conns := make([]*websocket.Conn, 3)
	feeds := make([]*feed, 3)
	for i := range conns {
		conn, f, err := dial(t, server, token, allowedHost)
		if err != nil {
			t.Fatalf("dial client %d: %v", i, err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		conns[i] = conn
		feeds[i] = f
	}
	awaitClients(t, hub, 3, 5*time.Second)

	if err := hub.CameraStatus(map[string]any{"camera_slug": "front", "status": "recording"}); err != nil {
		t.Fatalf("broadcast: %v", err)
	}

	for i := range conns {
		frame := awaitFrame(t, feeds[i], TypeCameraStatus, 5*time.Second)
		var payload map[string]any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			t.Fatalf("client %d payload: %v", i, err)
		}
		if payload["status"] != "recording" {
			t.Fatalf("client %d got %v", i, payload)
		}
	}
}

func TestBadTokenIsRefusedAndNoFeedArrives(t *testing.T) {
	_, _, server := newTestHub(t, allowedHost)

	for _, token := range []string{"", "not-a-token", "a.b.c", "1.2.3"} {
		conn, _, err := dial(t, server, token, allowedHost)
		if err == nil {
			conn.Close(websocket.StatusNormalClosure, "closed by the test")
			t.Fatalf("token %q opened the live feed", token)
		}
		if !strings.Contains(err.Error(), "401") {
			t.Fatalf("token %q refused with %v, want a 401 at the handshake", token, err)
		}
	}
}

func TestRefreshTokenCannotOpenTheLiveFeed(t *testing.T) {
	_, jwt, server := newTestHub(t, allowedHost)

	refresh, err := jwt.IssueRefresh("user-1", "admin", []string{"admin"}, 0)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}
	conn, _, err := dial(t, server, refresh, allowedHost)
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "closed by the test")
		t.Fatal("a refresh token opened the live feed")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("refresh token refused with %v, want a 401", err)
	}
}

func TestForeignOriginIsRejectedAndAllowedOriginIsNot(t *testing.T) {
	_, jwt, server := newTestHub(t, allowedHost)
	token := validToken(t, jwt)

	conn, _, err := dial(t, server, token, foreignHost)
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "closed by the test")
		t.Fatal("a foreign origin opened the live feed")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("foreign origin refused with %v, want a 403", err)
	}

	// The allowlist entry itself must work, and in the shape an operator writes
	// in .env: with a trailing slash, with a path, with mixed case.
	for _, origin := range []string{allowedHost, allowedHost + "/", allowedHost + "/console", "https://NVR.EXAMPLE"} {
		conn, _, err := dial(t, server, token, origin)
		if err != nil {
			t.Fatalf("allowed origin %q refused: %v", origin, err)
		}
		conn.Close(websocket.StatusNormalClosure, "done")
	}

	// No Origin header is a non-browser client (the CLI, curl, a worker): the
	// token is what decides, and it decides.
	conn, _, err = dial(t, server, token, "")
	if err != nil {
		t.Fatalf("a client with no Origin header was refused: %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "done")
}

func TestOriginAllowlistIsTheConfiguredList(t *testing.T) {
	hub, jwt, server := newTestHub(t, "https://a.example, https://b.example/", "https://c.example")
	token := validToken(t, jwt)

	for _, origin := range []string{"https://a.example", "https://b.example", "https://c.example"} {
		conn, _, err := dial(t, server, token, origin)
		if err != nil {
			t.Fatalf("configured origin %q refused: %v", origin, err)
		}
		conn.Close(websocket.StatusNormalClosure, "done")
	}
	if _, _, err := dial(t, server, token, "https://d.example"); err == nil {
		t.Fatal("an origin that is not on the list opened the live feed")
	}

	if got := len(hub.Origins()); got != 3 {
		t.Fatalf("allowlist normalised to %d entries, want 3: %v", got, hub.Origins())
	}
}

func TestEachMutationHandlerBroadcastsExactlyOneType(t *testing.T) {
	cases := []struct {
		name string
		call func(h *Hub) error
		typ  string
	}{
		{"EventNew", func(h *Hub) error { return h.EventNew(map[string]any{"event_id": "e-1"}) }, TypeEventNew},
		{"EventUpdate", func(h *Hub) error { return h.EventUpdate(map[string]any{"event_id": "e-1"}) }, TypeEventUpdate},
		{"EventClose", func(h *Hub) error { return h.EventClose(map[string]any{"event_id": "e-1"}) }, TypeEventClose},
		{"CameraStatus", func(h *Hub) error { return h.CameraStatus(map[string]any{"camera_slug": "front"}) }, TypeCameraStatus},
		{"RecorderStatus", func(h *Hub) error { return h.RecorderStatus(map[string]any{"camera_slug": "front"}) }, TypeRecorderStatus},
		{"StorageQueue", func(h *Hub) error { return h.StorageQueue(map[string]any{"pending": 3}) }, TypeStorageQueue},
		{"Presence", func(h *Hub) error { return h.Presence() }, TypePresence},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, jwt, server := newTestHub(t, allowedHost)
			conn, f, err := dial(t, server, validToken(t, jwt), allowedHost)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close(websocket.StatusNormalClosure, "done")

			awaitClients(t, hub, 1, 5*time.Second)
			// Settle the presence announcements the connect itself caused, so
			// what is counted below is the handler's doing.
			collectFrames(t, f, 300*time.Millisecond)

			if err := tc.call(hub); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			frames := collectFrames(t, f, 700*time.Millisecond)
			if len(frames) != 1 {
				t.Fatalf("%s broadcast %d frames (%v), want exactly 1", tc.name, len(frames), typesOf(frames))
			}
			if frames[0].Type != tc.typ {
				t.Fatalf("%s broadcast %q, want %q", tc.name, frames[0].Type, tc.typ)
			}
		})
	}
}

func TestPresenceTracksConsolesJoiningAndLeaving(t *testing.T) {
	hub, jwt, server := newTestHub(t, allowedHost)
	token := validToken(t, jwt)

	// One console stays open as the observer; a second joins and leaves. The
	// observer is the one whose frames are read, so its feed is named.
	observer, watching, err := dial(t, server, token, allowedHost)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer observer.Close(websocket.StatusNormalClosure, "done")

	frame := awaitFrame(t, watching, TypePresence, 5*time.Second)
	var payload map[string]any
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatalf("presence payload: %v", err)
	}
	if payload["clients"] != float64(1) {
		t.Fatalf("the first presence reported %v, want 1", payload["clients"])
	}

	visitor, _, err := dial(t, server, token, allowedHost)
	if err != nil {
		t.Fatalf("dial the second console: %v", err)
	}
	frame = awaitFrame(t, watching, TypePresence, 5*time.Second)
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatalf("presence payload: %v", err)
	}
	if payload["clients"] != float64(2) {
		t.Fatalf("presence after the second console joined reported %v, want 2", payload["clients"])
	}
	if got := hub.Count(); got != 2 {
		t.Fatalf("hub reports %d clients, want 2", got)
	}

	visitor.Close(websocket.StatusNormalClosure, "bye")
	frame = awaitFrame(t, watching, TypePresence, 5*time.Second)
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatalf("presence payload: %v", err)
	}
	if payload["clients"] != float64(1) {
		t.Fatalf("presence after the second console left reported %v, want 1", payload["clients"])
	}
	if got := hub.Count(); got != 1 {
		t.Fatalf("hub reports %d clients after a departure, want 1", got)
	}
}

func TestBroadcastDoesNotBlockAndDropsAClientThatStopsReading(t *testing.T) {
	hub, _, server := newTestHub(t, allowedHost)

	// A socket with no writer goroutine draining it: the state a console reaches
	// when its reader stops, and the only way to watch the drop happen rather
	// than argue that it would.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(server.URL, "http://", "ws://", 1)+"/bare", &websocket.DialOptions{
		HTTPHeader:      http.Header{"Origin": []string{allowedHost}},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	var accepted *websocket.Conn
	select {
	case accepted = <-bareSockets:
	case <-time.After(5 * time.Second):
		t.Fatal("the /bare route never accepted the socket")
	}

	stuck := &client{hub: hub, conn: accepted, send: make(chan []byte, 2), userID: "user-1"}
	hub.add(stuck)
	stuck.send <- []byte("{}")
	stuck.send <- []byte("{}")

	start := time.Now()
	for range 20 {
		if err := hub.Broadcast(TypeEventNew, map[string]any{"event_id": "e"}); err != nil {
			t.Fatalf("broadcast: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Broadcast waited %s for a client that is not reading", elapsed)
	}
	// The drop is housekeeping, so it lands asynchronously: the socket close
	// waits for the peer's close echo and must not be paid for on the broadcast
	// path. The queue-full client is gone within a second.
	deadline := time.Now().Add(2 * time.Second)
	for hub.Count() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := hub.Count(); got != 0 {
		t.Fatalf("a client with a full queue is still connected: count %d", got)
	}
}

func typesOf(frames []Envelope) []string {
	out := make([]string, 0, len(frames))
	for _, frame := range frames {
		out = append(out, frame.Type)
	}
	return out
}
