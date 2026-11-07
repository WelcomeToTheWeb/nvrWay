package api

// S8-04 — GET /api/v1/settings/live.
//
// The live player builds its WebSocket URL from this answer, so the console
// never hard-codes the media host. `GO2RTC_PUBLIC_URL` is a deployment fact: a
// bundle that baked it in could not move between a laptop, a LAN box and a
// reverse proxy without a rebuild.
//
// The card's two exit criteria, plus the two shapes the player meets when the
// deployment is wrong.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"nvr/server/config"
	"nvr/server/models"
	"nvr/server/store"
)

// liveFixture builds the router with one knob: the configured media host.
func liveFixture(t *testing.T, publicURL string) (*store.Store, *gin.Engine) {
	t.Helper()
	st := newStore(t)
	cfg := &config.Config{
		ServerPort:         ":8080",
		JWTSecret:          "unit-test-signing-secret-not-a-deployment-one",
		DataDir:            t.TempDir(),
		WebURL:             "http://localhost:3000",
		Go2rtcPublicURL:    publicURL,
		RateLimitPerMinute: 120,
		RateLimitBurst:     12,
		SegmentSeconds:     5,
		TZ:                 "UTC",
	}
	h := NewHandlers(context.Background(), st, nil, nil, cfg)
	return st, routerFor(t, h)
}

func TestLiveSettingsAnswersThePublicURLVerbatim(t *testing.T) {
	// Distinctive on purpose: a non-default scheme, a non-default port, and a
	// path segment. Each is a place a handler could quietly "help" — strip the
	// port, drop the path, append a slash — and each one breaks the player's
	// `url.replace('http','ws') + '/api/ws?src='` in a way that reads as a
	// media bug rather than a settings bug.
	const want = "https://cam.example.net:1984/media"
	st, r := liveFixture(t, want)

	viewer := newUser(t, st, "wall", models.RoleViewer)
	status, body, raw := do(t, r, request{
		method: http.MethodGet,
		path:   "/api/v1/settings/live",
		bearer: accessToken(t, st, newJWT(), viewer),
	})
	if status != http.StatusOK {
		t.Fatalf("status: got %d %s, want 200", status, raw)
	}
	got, ok := body["go2rtc_public_url"]
	if !ok {
		t.Fatalf("body has no go2rtc_public_url: %s", raw)
	}
	if got != want {
		t.Errorf("go2rtc_public_url: got %q, want the configured value verbatim %q", got, want)
	}
	if strings.HasSuffix(got.(string), "/") {
		t.Errorf("go2rtc_public_url gained a trailing slash: %q", got)
	}
}

func TestLiveSettingsAllowsTheViewerRole(t *testing.T) {
	// The plan's role floor: `viewer` reads the wall. This is the answer the
	// wall needs in order to draw itself, so a gate that demands more than a
	// viewer makes the feature unreachable for the role it is built for.
	st, r := liveFixture(t, "http://media.lan:1984")

	viewer := newUser(t, st, "watcher", models.RoleViewer)
	status, body, raw := do(t, r, request{
		method: http.MethodGet,
		path:   "/api/v1/settings/live",
		bearer: accessToken(t, st, newJWT(), viewer),
	})
	if status != http.StatusOK {
		t.Fatalf("viewer denied: got %d %s, want 200", status, raw)
	}
	if body["go2rtc_public_url"] != "http://media.lan:1984" {
		t.Errorf("body: %s", raw)
	}
}

func TestLiveSettingsRefusesAnAnonymousBrowser(t *testing.T) {
	// The card's second exit criterion, and the shape the console's client
	// reacts to. The refusal is the middleware's own words, not a new one.
	_, r := liveFixture(t, "http://media.lan:1984")

	status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/v1/settings/live"})
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous: got %d, want 401", status)
	}
	if body["error"] != "missing authorization header" {
		t.Errorf("error: got %v, want %q", body["error"], "missing authorization header")
	}
}

func TestLiveSettingsRefusesARefreshToken(t *testing.T) {
	// A binding convention of this API: a refresh token is not a bearer
	// credential at an access gate, even though it is a well-formed signed
	// token for the same account.
	st, r := liveFixture(t, "http://media.lan:1984")

	viewer := newUser(t, st, "watcher", models.RoleViewer)
	status, body, _ := do(t, r, request{
		method: http.MethodGet,
		path:   "/api/v1/settings/live",
		bearer: refreshToken(t, newJWT(), viewer),
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("refresh token at an access gate: got %d, want 401", status)
	}
	if body["error"] != "access token is not valid" {
		t.Errorf("error: got %v, want %q", body["error"], "access token is not valid")
	}
}

func TestLiveSettingsNamesAnUnconfiguredMediaHost(t *testing.T) {
	// A deployment that never set GO2RTC_PUBLIC_URL and a Config built without
	// it answer the same question, and an empty string is the worst possible
	// answer: the console would build `ws:///api/ws?src=…` and fail as a media
	// fault. The route says what is missing instead of handing over a blank.
	st, r := liveFixture(t, "")

	viewer := newUser(t, st, "watcher", models.RoleViewer)
	status, body, raw := do(t, r, request{
		method: http.MethodGet,
		path:   "/api/v1/settings/live",
		bearer: accessToken(t, st, newJWT(), viewer),
	})
	if status == http.StatusOK {
		t.Fatalf("an unconfigured media host answered 200: %s", raw)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d %s, want 503", status, raw)
	}
	reason, _ := body["error"].(string)
	if !strings.Contains(strings.ToLower(reason), "go2rtc_public_url") {
		t.Errorf("error should name the setting that is missing: got %q", reason)
	}
	if _, present := body["go2rtc_public_url"]; present {
		t.Errorf("a refusal must not also answer a blank media host: %s", raw)
	}
}


// P-11 — GET /api/v1/settings/media: the roll and the segment length, told.
//
// The drawer's strip pads the event window by these numbers so the band it
// draws is the window the clip was cut from; the retention panel reads the
// same door so the console cannot show two numbers for one setting.
func TestMediaSettingsAnswersTheRollAndTheSegmentLength(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	// Distinctive non-defaults: the answer must be the config, not a restated
	// default, and the strip does arithmetic with these numbers.
	h.Cfg.PreRollSeconds = 8
	h.Cfg.PostRollSeconds = 15
	h.Cfg.SegmentSeconds = 5
	r := routerFor(t, h)

	viewer := newUser(t, st, "wall", models.RoleViewer)
	status, body, raw := do(t, r, request{
		method: http.MethodGet,
		path:   "/api/v1/settings/media",
		bearer: accessToken(t, st, h.JWT, viewer),
	})
	if status != http.StatusOK {
		t.Fatalf("a viewer reads the roll: %d %s", status, raw)
	}
	for key, want := range map[string]float64{
		"pre_roll_seconds":  8,
		"post_roll_seconds": 15,
		"segment_seconds":   5,
	} {
		if body[key] != want {
			t.Errorf("%s = %v, want %v (%s)", key, body[key], want, raw)
		}
	}
}

func TestMediaSettingsNeedsASessionLikeItsNeighbour(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	status, _, _ := do(t, r, request{method: http.MethodGet, path: "/api/v1/settings/media"})
	if status != http.StatusUnauthorized {
		t.Errorf("the route is behind the read gate, not public: got %d", status)
	}
}
