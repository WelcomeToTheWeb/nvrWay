package store

import (
	"testing"
	"time"

	"nvr/server/models"
)

func auditRow(actor models.AuditActor, action, targetType, targetID, outcome string, details map[string]any, at time.Time) *models.Audit {
	return &models.Audit{
		ID:            models.NewAuditID(),
		ActorUserID:   actor.UserID,
		ActorUsername: actor.Username,
		ActorRoles:    actor.Roles,
		ActorProvider: actor.Provider,
		Action:        action,
		TargetType:    targetType,
		TargetID:      targetID,
		Outcome:       outcome,
		Details:       EncodeDetails(details),
		SourceIP:      "10.0.0.5",
		CreatedAt:     at,
	}
}

func TestAuditRecordMintsIDAndTimestamp(t *testing.T) {
	st := newTestStore(t)

	row := &models.Audit{
		ActorUserID:   "user-1",
		ActorUsername: "admin",
		Action:        models.AuditCameraCreate,
		TargetType:    "camera",
		TargetID:      "cam-1",
		Outcome:       models.AuditOutcomeOK,
	}
	if err := st.Audit.Record(row); err != nil {
		t.Fatalf("record: %v", err)
	}
	if row.ID == "" {
		t.Fatal("expected the ledger to mint its own id")
	}
	if row.CreatedAt.IsZero() {
		t.Fatal("expected the ledger to stamp the act")
	}
}

func TestAuditRefusalsAreRows(t *testing.T) {
	st := newTestStore(t)

	// A log that records only successes is a list of what the installation
	// allowed. The event an operator wants is the one it refused: a viewer
	// trying to delete a camera is the row that settles an argument.
	if err := st.Audit.Record(auditRow(
		models.AuditActor{UserID: "u-viewer", Username: "watcher", Roles: "viewer", Provider: models.ProviderLocal},
		models.AuditCameraDelete, "camera", "cam-1", models.AuditOutcomeRefused,
		map[string]any{"required_role": models.RoleAdmin}, time.Now().UTC(),
	)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := st.Audit.Record(auditRow(
		models.AuditActor{}, // a failed login has no authenticated user
		models.AuditAuthLoginFailed, "user", "someone", models.AuditOutcomeRefused,
		map[string]any{"username": "someone"}, time.Now().UTC(),
	)); err != nil {
		t.Fatalf("record: %v", err)
	}

	rows, err := st.Audit.List(AuditFilter{Outcome: models.AuditOutcomeRefused})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected both refusals, got %d", len(rows))
	}

	// The failed login's actor columns are legitimately empty, and a `uuid`
	// column would have rejected that at migration time on Postgres.
	var empty bool
	for _, row := range rows {
		if row.Action == models.AuditAuthLoginFailed && row.ActorUserID == "" {
			empty = true
		}
	}
	if !empty {
		t.Fatal("a failed login must be recordable with no authenticated actor id")
	}
}

func TestAuditListShapes(t *testing.T) {
	st := newTestStore(t)

	admin := models.AuditActor{UserID: "u-admin", Username: "admin", Roles: "admin", Provider: models.ProviderLocal}
	watcher := models.AuditActor{UserID: "u-viewer", Username: "watcher", Roles: "viewer", Provider: "keycloak"}

	// Newest first, and the two queries the ledger exists to answer:
	// "every failed login, this week", and "everything this person did".
	entries := []struct {
		actor   models.AuditActor
		action  string
		target  string
		outcome string
		at      time.Time
	}{
		{admin, models.AuditCameraCreate, "cam-1", models.AuditOutcomeOK, minutesAgo(60)},
		{admin, models.AuditCameraZones, "cam-1", models.AuditOutcomeOK, minutesAgo(45)},
		{watcher, models.AuditCameraUpdate, "cam-1", models.AuditOutcomeRefused, minutesAgo(30)},
		{watcher, models.AuditAuthLoginFailed, "", models.AuditOutcomeRefused, minutesAgo(10)},
	}
	for _, e := range entries {
		if err := st.Audit.Record(auditRow(e.actor, e.action, "camera", e.target, e.outcome, nil, e.at)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	byActor, err := st.Audit.List(AuditFilter{ActorUsername: "watcher"})
	if err != nil {
		t.Fatalf("list by actor: %v", err)
	}
	if len(byActor) != 2 {
		t.Fatalf("expected the watcher's two acts, got %d", len(byActor))
	}
	if byActor[0].CreatedAt.Before(byActor[1].CreatedAt) {
		t.Fatal("the ledger reads newest-first")
	}

	byAction, err := st.Audit.List(AuditFilter{Action: models.AuditCameraZones})
	if err != nil {
		t.Fatalf("list by action: %v", err)
	}
	if len(byAction) != 1 {
		t.Fatalf("expected the one zone write, got %d", len(byAction))
	}

	byWindow, err := st.Audit.List(AuditFilter{From: &entries[2].at})
	if err != nil {
		t.Fatalf("list by window: %v", err)
	}
	if len(byWindow) != 2 {
		t.Fatalf("expected the two acts inside the window, got %d", len(byWindow))
	}

	byOutcome, err := st.Audit.List(AuditFilter{Outcome: models.AuditOutcomeRefused, Action: models.AuditAuthLoginFailed})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(byOutcome) != 1 {
		t.Fatalf("expected the one failed login, got %d", len(byOutcome))
	}

	paged, err := st.Audit.List(AuditFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("list paged: %v", err)
	}
	if len(paged) != 2 {
		t.Fatalf("expected the second page of two, got %d", len(paged))
	}
}

func TestAuditEncodeDetails(t *testing.T) {
	st := newTestStore(t)

	if got := EncodeDetails(nil); got != "" {
		t.Fatalf(`expected an empty column for no details, got %q`, got)
	}
	if got := EncodeDetails(map[string]any{}); got != "" {
		t.Fatalf(`expected an empty column for no details, got %q`, got)
	}
	if got := EncodeDetails(map[string]any{"slug": "front-door"}); got == "" {
		t.Fatalf("expected the details serialised, got %q", got)
	}

	// A bulk delete is one row with a count, not 400 rows.
	row := auditRow(models.AuditActor{UserID: "u", Username: "admin"},
		models.AuditEventDelete, "event", "e1", models.AuditOutcomeOK,
		map[string]any{"count": 400, "purged_media": true}, time.Now().UTC())
	if err := st.Audit.Record(row); err != nil {
		t.Fatalf("record: %v", err)
	}
	if row.Details == "" {
		t.Fatal("expected the count on the row")
	}
}

func TestAuditPruneOlderThan(t *testing.T) {
	st := newTestStore(t)

	old := auditRow(models.AuditActor{UserID: "u", Username: "admin"},
		models.AuditCameraCreate, "camera", "cam-1", models.AuditOutcomeOK, nil, daysAgo(400))
	fresh := auditRow(models.AuditActor{UserID: "u", Username: "admin"},
		models.AuditCameraCreate, "camera", "cam-2", models.AuditOutcomeOK, nil, time.Now().UTC())
	for _, row := range []*models.Audit{old, fresh} {
		if err := st.Audit.Record(row); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	pruned, err := st.Audit.PruneOlderThan(time.Now().UTC().Add(-time.Duration(models.AuditRetentionDays) * 24 * time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected the one row past the 365-day window pruned, got %d", pruned)
	}

	count, err := st.Audit.Count()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the ledger to keep the recent row, got %d", count)
	}
}

func TestAuditOutcomeIsRenderedFromTheStatusCode(t *testing.T) {
	// The outcome is a column with a rendered value, so a filter is a filter
	// and not a free-text reason three handlers spell differently.
	cases := map[int]string{
		200: models.AuditOutcomeOK,
		204: models.AuditOutcomeOK,
		404: models.AuditOutcomeNotFound,
		403: models.AuditOutcomeRefused,
		422: models.AuditOutcomeRefused,
		429: models.AuditOutcomeRefused,
		500: models.AuditOutcomeError,
		503: models.AuditOutcomeError,
	}
	for status, want := range cases {
		if got := models.AuditOutcomeFor(status); got != want {
			t.Fatalf("status %d: expected %q, got %q", status, want, got)
		}
	}
}

func TestAuditActionNamesAreStableStrings(t *testing.T) {
	// The verbs the plan names for the mutating routes, asserted as strings: a
	// filter is only a filter if the verb is not invented per handler.
	want := []string{
		models.AuditAuthLogin, models.AuditAuthLoginFailed, models.AuditAuthLogout,
		models.AuditAuthPasswordChange, models.AuditAuthRegister, models.AuditSSOLink,
		models.AuditCameraCreate, models.AuditCameraUpdate, models.AuditCameraDelete,
		models.AuditCameraZones, models.AuditCameraProbe,
		models.AuditEventDelete,
		models.AuditStorageCreate, models.AuditStorageUpdate, models.AuditStorageDelete,
		models.AuditStorageDefault, models.AuditStorageEnable, models.AuditStorageTest, models.AuditStorageRequeue,
		models.AuditWebhookCreate, models.AuditWebhookUpdate, models.AuditWebhookDelete, models.AuditWebhookTest,
		models.AuditOIDCCreate, models.AuditOIDCUpdate, models.AuditOIDCDelete, models.AuditOIDCTest,
		models.AuditUserCreate, models.AuditUserUpdate, models.AuditUserDelete, models.AuditUserRoleChange,
		models.AuditRoleCreate, models.AuditRoleUpdate, models.AuditRoleDelete,
		models.AuditSettingUpdate, models.AuditIdentityLink, models.AuditIdentityMerge, models.AuditIdentityRefuse,
	}
	seen := map[string]bool{}
	for _, action := range want {
		if action == "" {
			t.Fatal("an audit action name is empty")
		}
		if seen[action] {
			t.Fatalf("audit action %q declared twice under two names", action)
		}
		seen[action] = true
	}
	if len(seen) != len(want) {
		t.Fatalf("expected %d distinct audit actions, got %d", len(want), len(seen))
	}
}
