package api

// S8-08 — the read-only retention surface.
//
// The page this feeds is the Settings page's retention tab, and the tab is
// only honest if the numbers are the ones the sweeper will act on. So the
// suite pins three things:
//
//   - the windows come from the config this process loaded, not from defaults
//     the console could have typed itself;
//   - `remote_days: 0` arrives with `remote_forever: true` beside it, because
//     zero means "keep forever" here and nothing else;
//   - the protection window is the sweeper's formula, floor included — the
//     screen and the sweep must never disagree about whether un-uploaded
//     footage is still on disk.

import (
	"net/http"
	"testing"

	"nvr/server/models"
	"nvr/server/store"
)

func tokenForRole(t *testing.T, st *store.Store, h *Handlers, username, role string) string {
	t.Helper()
	u := newUser(t, st, username, role)
	return accessToken(t, st, h.JWT, u)
}

func TestRetentionAnswersTheConfiguredWindows(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	h.Cfg.LocalRetentionDays = 14
	h.Cfg.RemoteRetentionDays = 30
	h.Cfg.EventRetentionDays = 90
	r := routerFor(t, h)
	admin, _ := usersToken(t, h)

	status, body, _ := do(t, r, request{
		method: http.MethodGet, path: "/api/settings/retention", bearer: admin,
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	want := map[string]any{
		"local_days":     float64(14),
		"remote_days":    float64(30),
		"remote_forever": false,
		"event_days":     float64(90),
		// max(3 * 14, 7) — the sweeper's own arithmetic.
		"unuploaded_protection_days": float64(42),
		"webhook_delivery_days":      float64(30),
		"audit_days":                 float64(365),
		"job_days":                   float64(30),
		"author":                     "environment",
	}
	for key, expected := range want {
		if got := body[key]; got != expected {
			t.Errorf("%s = %v, want %v", key, got, expected)
		}
	}
}

func TestRetentionZeroRemoteMeansForever(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	h.Cfg.RemoteRetentionDays = 0
	r := routerFor(t, h)
	admin, _ := usersToken(t, h)

	status, body, _ := do(t, r, request{
		method: http.MethodGet, path: "/api/settings/retention", bearer: admin,
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["remote_days"] != float64(0) {
		t.Errorf("remote_days = %v, want 0", body["remote_days"])
	}
	if body["remote_forever"] != true {
		t.Errorf("remote_forever = %v, want true: zero is a promise, not an empty tier",
			body["remote_forever"])
	}
}

func TestRetentionProtectionFloorIsSevenDays(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	// A 1-day local window would protect un-uploaded footage for 3 days under
	// the multiplier alone; the floor is what makes it 7, and the Settings
	// page shows the floor, not the multiplication.
	h.Cfg.LocalRetentionDays = 1
	r := routerFor(t, h)
	admin, _ := usersToken(t, h)

	status, body, _ := do(t, r, request{
		method: http.MethodGet, path: "/api/settings/retention", bearer: admin,
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["unuploaded_protection_days"] != float64(7) {
		t.Errorf("unuploaded_protection_days = %v, want 7 (the floor)",
			body["unuploaded_protection_days"])
	}
}

func TestRetentionIsAdminOnly(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	for _, role := range []string{models.RoleViewer, models.RoleOperator} {
		token := tokenForRole(t, st, h, "retention-"+role, role)
		status, _, _ := do(t, r, request{
			method: http.MethodGet, path: "/api/settings/retention", bearer: token,
		})
		if status != http.StatusForbidden {
			t.Errorf("%s read retention: status = %d, want 403", role, status)
		}
	}

	// No token at all is a 401, not a 403: the gate answers "who" before
	// "may you", and the console's renewal path depends on that difference.
	status, _, _ := do(t, r, request{method: http.MethodGet, path: "/api/settings/retention"})
	if status != http.StatusUnauthorized {
		t.Errorf("anonymous read: status = %d, want 401", status)
	}
}
