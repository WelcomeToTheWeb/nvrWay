// The ledger read (S8-09): `GET /api/audit`.
//
// What is pinned here is the answer the console's AuditPage is built on:
// **who did what, to what, and what the server answered** — including the
// refusals, because a ledger that only records successes cannot settle an
// argument about an act that never happened.
package api

import (
	"net/http"
	"testing"
	"time"

	"nvr/server/models"
	"nvr/server/store"
)

// record writes one ledger row with an explicit timestamp, so "newest first"
// is a fact the test controls rather than a race between inserts.
func record(t *testing.T, st *store.Store, row *models.Audit) {
	t.Helper()
	if err := st.Audit.Record(row); err != nil {
		t.Fatalf("recording a ledger row: %v", err)
	}
}

func baseTime() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) }

func TestAuditListAnswersWhoDidWhat(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)
	admin, _ := usersToken(t, h)

	record(t, st, &models.Audit{
		ActorUserID: "u-alice", ActorUsername: "alice", ActorRoles: "admin", ActorProvider: "local",
		Action: models.AuditCameraDelete, TargetType: "camera", TargetID: "c1",
		Outcome:   models.AuditOutcomeOK,
		Details:   store.EncodeDetails(map[string]any{"name": "front-door"}),
		CreatedAt: baseTime().Add(time.Minute),
	})
	record(t, st, &models.Audit{
		ActorUsername: "bob", ActorRoles: "viewer", ActorProvider: "local",
		Action: models.AuditCameraDelete, TargetType: "camera", TargetID: "c2",
		Outcome:   models.AuditOutcomeRefused,
		CreatedAt: baseTime(),
	})

	status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/audit", bearer: admin})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	audits, ok := body["audits"].([]any)
	if !ok || len(audits) != 2 {
		t.Fatalf("audits = %v, want two rows", body["audits"])
	}
	first := audits[0].(map[string]any)
	// Newest first: alice's delete closed after bob's refusal.
	if first["actor_username"] != "alice" || first["action"] != models.AuditCameraDelete {
		t.Fatalf("newest row = %v, want alice's camera.delete", first)
	}
	if first["outcome"] != models.AuditOutcomeOK || first["target_id"] != "c1" {
		t.Fatalf("newest row lost the what/answer: %v", first)
	}
	details, ok := first["details"].(map[string]any)
	if !ok || details["name"] != "front-door" {
		t.Fatalf("details = %v, want the parsed map", first["details"])
	}
	second := audits[1].(map[string]any)
	// The refusal is a row, and it reads as one: no user id (the gate refused
	// before the handler knew the account), but the name and the answer stand.
	if second["outcome"] != models.AuditOutcomeRefused || second["actor_username"] != "bob" {
		t.Fatalf("refusal row = %v", second)
	}
	if second["details"] != nil {
		t.Fatalf("a row with no details must answer null, got %v", second["details"])
	}
}

func TestAuditListFilters(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)
	admin, _ := usersToken(t, h)

	record(t, st, &models.Audit{
		ActorUsername: "alice", Action: models.AuditCameraCreate, TargetType: "camera", TargetID: "c1",
		Outcome: models.AuditOutcomeOK, CreatedAt: baseTime().Add(time.Minute),
	})
	record(t, st, &models.Audit{
		ActorUsername: "bob", Action: models.AuditCameraDelete, TargetType: "camera", TargetID: "c1",
		Outcome: models.AuditOutcomeRefused, CreatedAt: baseTime(),
	})
	record(t, st, &models.Audit{
		ActorUsername: "alice", Action: models.AuditAuthLoginFailed, TargetType: "", TargetID: "",
		Outcome: models.AuditOutcomeRefused, CreatedAt: baseTime().Add(2 * time.Minute),
	})

	cases := []struct {
		query string
		want  int
	}{
		{"actor=alice", 2},
		{"outcome=refused", 2},
		{"action=" + models.AuditCameraDelete, 1},
		{"target_id=c1", 2},
		{"actor=alice&outcome=refused", 1},
		{"from=" + baseTime().Add(30*time.Second).Format(time.RFC3339), 2},
		{"to=" + baseTime().Format(time.RFC3339), 1},
	}
	for _, tc := range cases {
		status, body, raw := do(t, r, request{method: http.MethodGet, path: "/api/audit", bearer: admin, query: tc.query})
		if status != http.StatusOK {
			t.Fatalf("query %q: status = %d, want 200 (%s)", tc.query, status, raw)
		}
		if got := len(body["audits"].([]any)); got != tc.want {
			t.Fatalf("query %q: %d rows, want %d", tc.query, got, tc.want)
		}
	}
}

func TestAuditListRejectsBadFilters(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)
	admin, _ := usersToken(t, h)

	cases := []struct{ query, want string }{
		{"outcome=maybe", `outcome must be "ok", "refused", "not_found" or "error"`},
		{"from=yesterday", "from must be an RFC3339 timestamp"},
		{"to=soon", "to must be an RFC3339 timestamp"},
		{"limit=0", "limit must be a positive integer"},
		{"offset=-1", "offset must be a non-negative integer"},
	}
	for _, tc := range cases {
		status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/audit", bearer: admin, query: tc.query})
		if status != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want 400", tc.query, status)
		}
		if body["error"] != tc.want {
			t.Fatalf("query %q: error = %v, want %q", tc.query, body["error"], tc.want)
		}
	}
}

func TestAuditListPagesAndReportsTruncation(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)
	admin, _ := usersToken(t, h)

	for i := 0; i < 3; i++ {
		record(t, st, &models.Audit{
			ActorUsername: "alice", Action: models.AuditSettingUpdate,
			Outcome:   models.AuditOutcomeOK,
			CreatedAt: baseTime().Add(time.Duration(i) * time.Minute),
		})
	}

	status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/audit", bearer: admin, query: "limit=2"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["truncated"] != true || body["count"].(float64) != 2 {
		t.Fatalf("page = count %v truncated %v, want 2/true", body["count"], body["truncated"])
	}

	status, body, _ = do(t, r, request{method: http.MethodGet, path: "/api/audit", bearer: admin, query: "limit=2&offset=2"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["truncated"] != false || body["count"].(float64) != 1 {
		t.Fatalf("last page = count %v truncated %v, want 1/false", body["count"], body["truncated"])
	}
}

func TestAuditListIsAdminOnly(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	for _, role := range []string{models.RoleViewer, models.RoleOperator} {
		token := tokenForRole(t, st, h, "audit-"+role, role)
		status, _, _ := do(t, r, request{method: http.MethodGet, path: "/api/audit", bearer: token})
		if status != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", role, status)
		}
	}
	status, _, _ := do(t, r, request{method: http.MethodGet, path: "/api/audit"})
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous: status = %d, want 401", status)
	}
}
