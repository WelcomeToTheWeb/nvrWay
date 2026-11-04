// Package ws is the live feed: the one WebSocket the console opens, and the fan
// -out that reaches every client of it.
//
// The hub is local and the installation is single-instance (plan decision 8). No
// Redis, no sticky sessions, no shared state: the server that recorded the
// segment is the server that tells the console about it, so the hub is a map of
// connected clients in this process.
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"nhooyr.io/websocket"

	"nvr/server/auth"
)

// The seven message types. The console switches on these exact strings, so they
// are constants here and the card's exit criterion is a test: a type name that
// moves is a console that stops seeing events.
const (
	TypeEventNew       = "event.new"
	TypeEventUpdate    = "event.update"
	TypeEventClose     = "event.close"
	TypeCameraStatus   = "camera.status"
	TypeRecorderStatus = "recorder.status"
	TypeStorageQueue   = "storage.queue"
	TypePresence       = "presence"
)

// MessageTypes is every type the hub will broadcast, in the order the console's
// legend lists them.
func MessageTypes() []string {
	return []string{
		TypeEventNew,
		TypeEventUpdate,
		TypeEventClose,
		TypeCameraStatus,
		TypeRecorderStatus,
		TypeStorageQueue,
		TypePresence,
	}
}

// Envelope is the frame every message arrives in: {type, payload, ts}.
//
// ts is the server clock, formatted UTC. A browser clock is a thing that drifts
// and changes with the user's timezone, and "when did the event start" must be
// comparable with "when did the segment land" — so the timestamp is the one the
// server put on the row.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	TS      time.Time       `json:"ts"`
}

const (
	// writeDeadline bounds one write to one client. A client that cannot take a
	// frame in this long is gone: the tab is backgrounded, the phone is asleep,
	// the network died. The hub drops it rather than let one dead reader slow a
	// broadcast for everyone.
	writeDeadline = 10 * time.Second

	// sendBuffer is one client's queue depth. A broadcast is a notification, not
	// a stream with a delivery guarantee; a client that falls this far behind is
	// resyncing from REST on reconnect, which is what the console does anyway.
	sendBuffer = 64

	// pingEvery keeps a half-open connection from looking live forever. nhooyr
	// answers peer pings itself; these are the hub's own.
	pingEvery = 30 * time.Second
)

// client is one connected socket: its own send queue, and the writer goroutine
// that drains it.
//
// `mu` + `closed` are what make fan-out safe. Broadcast snapshots the client set
// and then hands frames to each client, and a client can be dropped between the
// snapshot and the hand-off. A closed Go channel is a panic in that window, so
// the send channel is never closed at all: `closed` is the gate, and it is taken
// under `mu`, which every sender holds.
type client struct {
	hub    *Hub
	conn   *websocket.Conn
	send   chan []byte
	userID string

	mu     sync.Mutex
	closed bool
}

// close ends the connection once, however many places decide it should end.
//
// The client leaves the hub immediately and the socket close happens on its own
// goroutine: Conn.Close waits for the peer to echo the close frame, up to five
// seconds, and a hub that unregisters only after that keeps telling a console
// "1 watching" to a console that is gone, and keeps the count for its presence
// announcements stale.
func (c *client) close(code websocket.StatusCode, reason string) {
	if !c.markClosed() {
		return
	}
	c.hub.remove(c)
	go func() { _ = c.conn.Close(code, reason) }()
}

// markClosed flips the gate, answering whether this caller is the one that
// closed it.
func (c *client) markClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.closed = true
	return true
}

// Hub is the fan-out.
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}

	jwt     *auth.JWTAuth
	origins []string

	// now is the clock stamped into every envelope. It is a field so a test can
	// put a known clock in and assert the ts the console receives, not a call
	// site that happens to read the wall clock.
	now func() time.Time

	// updates is the plan's throttle on `event.update`: at most one send per event
	// per EventUpdateWindow. It lives on the hub because the hub is the thing that
	// decides what reaches every console at once — a throttle per caller would let
	// two callers each send their own five-second stream.
	updates *updateThrottle
}

// NewHub builds the fan-out for one server.
//
// origins is the allowlist (config's WS_ORIGINS, falling back to WEB_URL). A
// blank allowlist accepts the browser's default same-origin requests only, which
// for a server with no web UI is: nothing useful, and nothing unintended.
func NewHub(jwt *auth.JWTAuth, origins []string) *Hub {
	return NewHubWithClock(jwt, origins, time.Now)
}

// NewHubWithClock builds the fan-out with a named clock.
//
// The clock is a constructor argument, not a field a caller assigns later: every
// broadcast reads it, a broadcast happens on other goroutines, and a hub whose
// clock can be swapped under them is a data race with a timestamp in it.
func NewHubWithClock(jwt *auth.JWTAuth, origins []string, now func() time.Time) *Hub {
	if jwt == nil {
		jwt = auth.NewJWTAuth("")
	}
	if now == nil {
		now = time.Now
	}
	return &Hub{
		clients: make(map[*client]struct{}),
		jwt:     jwt,
		origins: normalizeOrigins(origins),
		now:     now,
		updates: newUpdateThrottle(EventUpdateWindow, now),
	}
}

// normalizeOrigins turns the configured list into comparable origins: lowercase,
// scheme required, no path, no trailing slash. "https://nvr.example/" and
// "https://nvr.example" are the same origin to a browser, and a comparison that
// does not know that is an allowlist with surprises in it.
func normalizeOrigins(raw []string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, item := range raw {
		for _, piece := range strings.Split(item, ",") {
			piece = strings.TrimSpace(piece)
			if piece == "" {
				continue
			}
			if !strings.Contains(piece, "://") {
				piece = "https://" + piece
			}
			parsed, err := url.Parse(piece)
			if err != nil || parsed.Host == "" {
				continue
			}
			origin := strings.ToLower(parsed.Scheme + "://" + parsed.Host)
			if _, ok := seen[origin]; ok {
				continue
			}
			seen[origin] = struct{}{}
			out = append(out, origin)
		}
	}
	return out
}

// Origins answers the effective allowlist, for the boot log and the tests.
func (h *Hub) Origins() []string {
	out := make([]string, len(h.origins))
	copy(out, h.origins)
	return out
}

// Count answers how many consoles are watching.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Broadcast serialises one envelope and hands it to every connected client.
//
// It never blocks on a client: a full queue means that client is dropped, and
// the others get the news on time. An unknown type is refused here, so a handler
// that invents a type fails at the call, not in the console.
func (h *Hub) Broadcast(typ string, payload any) error {
	if !isMessageType(typ) {
		return fmt.Errorf("ws: %q is not a message type the console knows", typ)
	}
	raw, err := encodePayload(payload)
	if err != nil {
		return err
	}
	frame, err := json.Marshal(Envelope{Type: typ, Payload: raw, TS: h.now().UTC()})
	if err != nil {
		return fmt.Errorf("ws: cannot encode the %s envelope: %w", typ, err)
	}

	h.mu.RLock()
	targets := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	for _, c := range targets {
		h.deliver(c, frame)
	}
	return nil
}

// deliver hands one frame to one client without waiting. A full queue means the
// client is not reading, and the drop is the honest answer: the console that
// comes back resyncs from REST, which is what it does on a reconnect anyway.
//
// The client's gate is checked and set under its own lock, so a client that was
// dropped after Broadcast took its snapshot is skipped, and a drop from two
// broadcasts in the same instant closes one socket.
func (h *Hub) deliver(c *client, frame []byte) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	select {
	case c.send <- frame:
		c.mu.Unlock()
		return
	default:
		// The queue is full: this client is not reading.
		c.closed = true
		c.mu.Unlock()
		// Unregister now, close the socket on a goroutine of its own. Conn.Close
		// waits for the peer to echo the close frame, up to five seconds, and a
		// broadcast that pays that cost per dead console is a signal-intake
		// handler stalling on its own POST.
		h.remove(c)
		go func() {
			_ = c.conn.Close(websocket.StatusTryAgainLater, "send queue full")
		}()
		return
	}
}

// clientList snapshots the connected clients. Exported to tests only: it hands
// them a client whose queue they can fill, which is the only way to watch the
// drop path happen rather than argue that it would.
func (h *Hub) clientList() []*client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, c)
	}
	return out
}

// isMessageType answers whether the console has a case for this type.
func isMessageType(typ string) bool {
	for _, known := range MessageTypes() {
		if typ == known {
			return true
		}
	}
	return false
}

// encodePayload marshals a payload, and maps "no payload" to an empty object so
// the console never has to handle a missing key.
func encodePayload(payload any) (json.RawMessage, error) {
	if payload == nil {
		return json.RawMessage("{}"), nil
	}
	if raw, ok := payload.(json.RawMessage); ok {
		if len(raw) == 0 || !json.Valid(raw) {
			return nil, errors.New("ws: payload is not valid JSON")
		}
		return raw, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ws: cannot encode the payload: %w", err)
	}
	return raw, nil
}

// The seven mutation handlers. Each broadcasts exactly one type — that is the
// rule the card tests, and the reason they exist as named methods rather than
// leaving every caller to type a string at the call site: a handler that
// broadcast two types would put the console's counters out by one, forever.

// EventNew announces an opened event.
func (h *Hub) EventNew(payload any) error { return h.Broadcast(TypeEventNew, payload) }

// EventUpdate announces an extended event.
func (h *Hub) EventUpdate(payload any) error { return h.Broadcast(TypeEventUpdate, payload) }

// EventClose announces a closed event.
func (h *Hub) EventClose(payload any) error { return h.Broadcast(TypeEventClose, payload) }

// EventUpdateWindow is the plan's throttle on `event.update`: one send per event
// every 5 s. A worker extends an event every detection tick — five per second on a
// busy camera — and a wall of sixty events would put three thousand frames a second
// on every console to say nothing the operator can read.
const EventUpdateWindow = 5 * time.Second

// gateStaleAfter is the belt on the throttle's memory. The signal path forgets a
// gate when its event closes, and an event cannot stay open past the ceiling, so an
// entry that has not been touched for an hour belongs to an event that stopped
// existing without anyone telling the hub — a restart, a row deleted by hand.
const gateStaleAfter = time.Hour

// updateThrottle is the leading edge of the window, per event.
//
// The leading edge is enough here because of what the messages carry: all three
// event types send the whole event view, and `event.close` is not throttled. So the
// state an extend loses to the window is back on the wire within five seconds, and
// the last state of all arrives with the close. A trailing-edge flush would need a
// timer per event to deliver, on the same schedule, something the close already
// delivers.
type updateThrottle struct {
	mu     sync.Mutex
	window time.Duration
	now    func() time.Time
	last   map[string]time.Time
}

func newUpdateThrottle(window time.Duration, now func() time.Time) *updateThrottle {
	if window <= 0 {
		window = EventUpdateWindow
	}
	if now == nil {
		now = time.Now
	}
	return &updateThrottle{window: window, now: now, last: map[string]time.Time{}}
}

// allow reports whether an update for one event may go out now, and records the
// instant when it may not go out again.
func (t *updateThrottle) allow(eventID string) bool {
	if eventID == "" {
		// No event to key on: an unkeyed update would let one event's storm spend
		// another's window, which is the bug the throttle exists to prevent. Send it
		// rather than silently dropping it, and let the caller own the key.
		return true
	}

	moment := t.now().UTC()

	t.mu.Lock()
	defer t.mu.Unlock()

	if sent, ok := t.last[eventID]; ok && moment.Sub(sent) < t.window {
		return false
	}

	// An allowed send is the only moment the map is guaranteed to be looked at, so
	// it is when the stale gates go — and it has to run for a *new* id too, which is
	// the case that would otherwise leak: the event that closed while nobody was
	// watching is exactly the one no longer in the map.
	//
	// The scan is over the events open right now, once per window per event: a
	// hundred entries every few seconds, which is not a cost worth a second data
	// structure.
	for id, seen := range t.last {
		if id != eventID && moment.Sub(seen) > gateStaleAfter {
			delete(t.last, id)
		}
	}
	t.last[eventID] = moment
	return true
}

// forget drops one event's gate. The close path calls it, because an event that has
// closed will never extend again and its gate is memory with no purpose.
func (t *updateThrottle) forget(eventID string) {
	if eventID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, eventID)
}

func (t *updateThrottle) gated(eventID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.last[eventID]
	return ok
}

// EventUpdateThrottled sends `event.update` for one event at most once per
// EventUpdateWindow, and returns nil when the window dropped it. The drop is not an
// error: the plan asked for the throttle, and a caller that treated it as a failure
// would log a line for every extend of a busy camera.
func (h *Hub) EventUpdateThrottled(eventID string, payload any) error {
	if !h.updates.allow(eventID) {
		return nil
	}
	return h.Broadcast(TypeEventUpdate, payload)
}

// ForgetEvent stops throttling one event. The event close and the event delete both
// call it; nothing else needs to, because the gate is only ever armed by an extend.
func (h *Hub) ForgetEvent(eventID string) { h.updates.forget(eventID) }

// CameraStatus announces a camera's state.
func (h *Hub) CameraStatus(payload any) error { return h.Broadcast(TypeCameraStatus, payload) }

// RecorderStatus announces a recorder's state.
func (h *Hub) RecorderStatus(payload any) error { return h.Broadcast(TypeRecorderStatus, payload) }

// StorageQueue announces queue depth.
func (h *Hub) StorageQueue(payload any) error { return h.Broadcast(TypeStorageQueue, payload) }

// Presence announces how many consoles are watching.
func (h *Hub) Presence() error {
	return h.Broadcast(TypePresence, map[string]any{"clients": h.Count()})
}

// The accept side: the origin guard, the token gate, and the two pumps that
// make one connection.

// Handle is the /ws route: it checks the origin, checks the access token from
// ?token=, and then keeps the connection open until either side ends it.
//
// The token is a query parameter because a browser WebSocket cannot set a header
// — the one place nvrWay accepts a bearer token outside Authorization, and the
// reason the access token is short-lived (30 minutes) and the console re-opens
// with a fresh one after a refresh.
func (h *Hub) Handle(c *gin.Context) {
	origin := c.Request.Header.Get("Origin")
	if !h.originAllowed(origin) {
		// A cross-origin page that can open a socket to this server can read the
		// event stream with a token it obtained from a logged-in browser. The
		// allowlist is the answer, and it is why WS_ORIGINS exists.
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "origin is not allowed"})
		return
	}

	token := c.Query("token")
	claims, err := h.jwt.Verify(token)
	if err != nil {
		// Refuse before the upgrade. A socket closed with a policy-violation code
		// tells the console "re-authenticate", where a 401 page cannot: the
		// console is already a WebSocket by the time it learns the token is stale.
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "access token is not valid"})
		return
	}
	if claims.Type != auth.TokenTypeAccess {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "a refresh token cannot open the live feed"})
		return
	}

	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
		// nhooyr's own origin rule compares the Origin header with RemoteAddr,
		// which behind a proxy is the proxy: it cannot express WS_ORIGINS. The
		// hub's check above is the rule, and it is the one that runs.
		InsecureSkipVerify: true,
		// The console sends no frames; it only reads. Nothing arrives that is
		// worth a compression negotiation, and a compression negotiation on a
		// feed nobody writes to is cost without benefit.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		// Accept failure after the headers are written: nothing left to answer.
		return
	}

	client := &client{
		hub:    h,
		conn:   conn,
		send:   make(chan []byte, sendBuffer),
		userID: claims.UserID,
	}
	h.add(client)
	go client.writePump(c.Request.Context())

	// The count changed, and the console's "N watching" is a fact about this
	// hub, so the hub announces it. A presence failure is not a connection
	// failure: the socket is open and the feed works.
	_ = h.Presence()

	// The read pump runs on this goroutine, so the handler does not return while
	// the socket is live. The response is already upgraded, and a handler that
	// returns hands the connection back to net/http: the request context is
	// cancelled and the socket dies, which is how a live feed ends up with a
	// console that disconnects the instant it connects.
	client.readPump(c.Request.Context())
}

// originAllowed answers the allowlist. An empty Origin is the CLI's, curl's, or
// a server-to-server socket: a header a browser cannot forge, so a non-browser
// client is accepted, and the token gate is what decides.
func (h *Hub) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	normalized := strings.ToLower(parsed.Scheme + "://" + parsed.Host)
	for _, allowed := range h.origins {
		if allowed == normalized {
			return true
		}
	}
	return false
}

// add registers a client.
func (h *Hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

// remove unregisters a client. Safe to call twice.
func (h *Hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	// The count changed, so the consoles watching it are told. Broadcast takes
	// the lock itself, which is why this runs after the unlock above.
	_ = h.Presence()
}

// writePump is the only goroutine that writes to this socket.
func (c *client) writePump(ctx context.Context) {
	ticker := time.NewTicker(pingEvery)
	defer func() {
		ticker.Stop()
		c.close(websocket.StatusNormalClosure, "connection closed")
	}()
	for {
		select {
		case frame := <-c.send:
			writeCtx, cancel := context.WithTimeout(ctx, writeDeadline)
			err := c.conn.Write(writeCtx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, writeDeadline)
			err := c.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// readPump exists to notice the other side ending. The console sends no
// application frames, so what arrives is a pong, a close, or a protocol error —
// and each of them ends the connection, which is the fact the hub needs.
func (c *client) readPump(ctx context.Context) {
	defer c.close(websocket.StatusNormalClosure, "read ended")
	for {
		_, _, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
	}
}
