package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"nvr/server/models"
)

// Shared fixtures for the camera and zone suites.

// doRaw is `do` for the assertions that need the response headers and the body
// bytes: the snapshot's cache promise and the config-failure body are both
// assertions on the wire, not on a decoded map.
func doRaw(t *testing.T, handler http.Handler, r request) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Buffer
	if r.body != nil {
		encoded, err := json.Marshal(r.body)
		if err != nil {
			t.Fatalf("encoding the request body: %v", err)
		}
		body = bytes.NewBuffer(encoded)
	} else {
		body = new(bytes.Buffer)
	}
	target := r.path
	if r.query != "" {
		target += "?" + r.query
	}
	req := httptest.NewRequest(r.method, target, body)
	req.Header.Set("Content-Type", "application/json")
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func snapshotRequest(t *testing.T, fx *cameraFixture, cameraID, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	return doRaw(t, fx.Router, request{method: http.MethodGet, path: "/api/v1/cameras/" + cameraID + "/snapshot", bearer: bearer})
}

// zoneBody is one valid zone of the shape the console sends: an open path
// (three vertices, implicitly closed).
func zoneBody(id string) map[string]any {
	return map[string]any{
		"id":            id,
		"name":          "Driveway " + id,
		"polygon":       [][]float64{{0.1, 0.3}, {0.8, 0.3}, {0.8, 0.9}},
		"classes":       []string{"person"},
		"rule":          models.ZoneRuleIntrusion,
		"dwell_seconds": 0,
		"severity":      3,
		"enabled":       true,
	}
}

// watchConsole opens one console socket on the fixture's router and returns a
// channel of decoded envelopes. The camera writes broadcast, and the only way to
// assert what a console is told is to be a console.
func watchConsole(t *testing.T, fx *cameraFixture) chan map[string]any {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := strings.Replace(fx.Server.URL, "http://", "ws://", 1) + "/ws?token=" + fx.ViewerTok
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:      http.Header{"Origin": {"http://localhost:3000"}},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatalf("dialing the console socket: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "test over") })
	conn.SetReadLimit(1 << 20)

	frames := make(chan map[string]any, 64)
	go func() {
		for {
			_, payload, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			frame := map[string]any{}
			if err := json.Unmarshal(payload, &frame); err != nil {
				continue
			}
			frames <- frame
		}
	}()
	return frames
}
