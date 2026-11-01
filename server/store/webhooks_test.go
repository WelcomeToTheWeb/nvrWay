package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

func TestWebhookCreateAndSecretRule(t *testing.T) {
	st := newTestStore(t)

	hook := &models.Webhook{
		Name:    "oncall",
		URL:     "https://hooks.example/nvr",
		Secret:  "shared-signing-secret",
		Events:  `["event.close"]`,
		Enabled: true,
	}
	if err := st.Webhooks.Create(hook); err != nil {
		t.Fatalf("create: %v", err)
	}
	if hook.ID == "" {
		t.Fatal("expected the store to mint the webhook id")
	}
	if !hook.Enabled {
		t.Fatal("expected the endpoint enabled")
	}
	// The console may know a signing secret is set. The secret itself has no
	// path out of the API.
	if !hook.SecretSet() {
		t.Fatal("expected SecretSet to report the signing secret")
	}

	if hook.Delivers("event.close") != true {
		t.Fatal("the v1 trigger is the event close")
	}
	if hook.Delivers("camera.created") {
		t.Fatal("an endpoint only subscribed to event closes must not receive camera events")
	}

	dup := &models.Webhook{Name: "oncall", URL: "https://other"}
	if err := st.Webhooks.Create(dup); err == nil {
		t.Fatal("expected the unique endpoint name to be refused")
	}
}

func TestWebhookCreatedDisabledStaysDisabled(t *testing.T) {
	st := newTestStore(t)

	// The store writes what the row says: `Enabled: false` means disabled, and
	// the schema's `default: true` does not win. "The payload did not mention
	// it" is a request-shape question, answered by the API layer's `*bool` DTO
	// in Step 3, not by the store guessing from Go's zero value.
	hook := &models.Webhook{Name: "parked", URL: "https://hooks.example/parked", Enabled: false}
	if err := st.Webhooks.Create(hook); err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err := st.Webhooks.GetByID(hook.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Enabled {
		t.Fatal("an endpoint saved disabled arrived enabled (schema default won)")
	}

	on := &models.Webhook{Name: "live", URL: "https://hooks.example/live", Enabled: true}
	if err := st.Webhooks.Create(on); err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err = st.Webhooks.GetByID(on.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !loaded.Enabled {
		t.Fatal("an endpoint saved enabled arrived disabled")
	}
}

func TestWebhookUpdateKeepsTheSigningSecret(t *testing.T) {
	st := newTestStore(t)
	hook := &models.Webhook{Name: "oncall", URL: "https://hooks.example/nvr", Secret: "shared-signing-secret"}
	if err := st.Webhooks.Create(hook); err != nil {
		t.Fatalf("create: %v", err)
	}
	secret := hook.Secret

	// The console never reads the secret back, so a PUT carries the fields the
	// operator edited. Clearing the signing secret from a URL edit would make
	// every delivery unverifiable at the receiver, silently.
	edit := &models.Webhook{ID: hook.ID, Name: "oncall-2", URL: "https://hooks.example/nvr-2", Secret: ""}
	if err := st.Webhooks.Update(edit, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	loaded, err := st.Webhooks.GetByID(hook.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Name != "oncall-2" || loaded.URL != "https://hooks.example/nvr-2" {
		t.Fatalf("expected the edit to land, got %+v", loaded)
	}
	if loaded.Secret != secret {
		t.Fatal("the signing secret was wiped by an edit that did not carry it")
	}

	rotate := &models.Webhook{ID: hook.ID, Name: "oncall-2", URL: "https://hooks.example/nvr-2", Secret: "rotated-secret"}
	if err := st.Webhooks.Update(rotate, true); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	loaded, err = st.Webhooks.GetByID(hook.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Secret != "rotated-secret" {
		t.Fatal("expected the rotated signing secret")
	}

	if err := st.Webhooks.Update(&models.Webhook{ID: "missing"}, false); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestWebhookListEnabledForCamera(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	other := seedCamera(t, st, "side-gate")

	global := &models.Webhook{Name: "everywhere", URL: "https://hooks.example/all", Enabled: true}
	scoped := &models.Webhook{Name: "doorbell-only", URL: "https://hooks.example/door", CameraID: camera.ID, Enabled: true}
	parked := &models.Webhook{Name: "parked", URL: "https://hooks.example/parked", Enabled: false}
	for _, h := range []*models.Webhook{global, scoped, parked} {
		if err := st.Webhooks.Create(h); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// The delivery loop's set, resolved in one query so a close does not scan
	// the table per signal.
	targets, err := st.Webhooks.ListEnabledForCamera(camera.ID)
	if err != nil {
		t.Fatalf("list enabled for camera: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected the global endpoint plus the camera's own, got %d %+v", len(targets), targets)
	}

	otherTargets, err := st.Webhooks.ListEnabledForCamera(other.ID)
	if err != nil {
		t.Fatalf("list enabled for camera: %v", err)
	}
	if len(otherTargets) != 1 || otherTargets[0].Name != "everywhere" {
		t.Fatalf("the doorbell's endpoint must not receive the gate's events, got %+v", otherTargets)
	}

	all, err := st.Webhooks.ListAll()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("the settings page shows the parked endpoint too, got %d", len(all))
	}
}

func TestWebhookDeliveryRowsAreTheAttemptSequence(t *testing.T) {
	st := newTestStore(t)
	hook := &models.Webhook{Name: "oncall", URL: "https://hooks.example/nvr"}
	if err := st.Webhooks.Create(hook); err != nil {
		t.Fatalf("create: %v", err)
	}

	// "Did it get through" is answered by the sequence, not the last entry:
	// 502, 502, 200 is a receiver that recovered, and a table with only the
	// last attempt says "failed" for two of the three attempts and hides the
	// recovery.
	for i, attempt := range []struct {
		status  string
		code    int
		message string
	}{{models.DeliveryStatusFailed, 502, "bad gateway"}, {models.DeliveryStatusFailed, 502, "bad gateway"}, {models.DeliveryStatusDelivered, 200, ""}} {
		if err := st.WebhookDeliveries.Create(&models.WebhookDelivery{
			WebhookID:  hook.ID,
			Event:      "event.close",
			Payload:    `{"id":"e1"}`,
			Status:     attempt.status,
			StatusCode: attempt.code,
			Attempt:    i + 1,
			Error:      attempt.message,
		}); err != nil {
			t.Fatalf("create delivery %d: %v", i, err)
		}
	}

	rows, err := st.WebhookDeliveries.ListForWebhook(hook.ID, 10)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 attempt rows, got %d", len(rows))
	}
	if rows[0].Attempt != 3 {
		t.Fatalf("expected newest-first, got attempt %d first", rows[0].Attempt)
	}
	if rows[0].Status != models.DeliveryStatusDelivered {
		t.Fatalf("expected the recovery recorded, got %+v", rows[0])
	}
}

func TestWebhookMarkDelivered(t *testing.T) {
	st := newTestStore(t)
	hook := &models.Webhook{Name: "oncall", URL: "https://hooks.example/nvr"}
	if err := st.Webhooks.Create(hook); err != nil {
		t.Fatalf("create: %v", err)
	}

	at := time.Now().UTC()
	if err := st.Webhooks.MarkDelivered(hook.ID, "", at); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	loaded, err := st.Webhooks.GetByID(hook.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.LastDeliveredAt == nil || loaded.LastError != "" {
		t.Fatalf(`expected the clean delivery stamp, got %+v`, loaded)
	}

	if err := st.Webhooks.MarkDelivered(hook.ID, "dial tcp: connection refused", at); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	loaded, err = st.Webhooks.GetByID(hook.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.LastError == "" {
		t.Fatal(`the console answers "is this webhook working" from the row it already has`)
	}
}

func TestWebhookDeleteTakesItsHistory(t *testing.T) {
	st := newTestStore(t)
	hook := &models.Webhook{Name: "oncall", URL: "https://hooks.example/nvr"}
	if err := st.Webhooks.Create(hook); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.WebhookDeliveries.Create(&models.WebhookDelivery{
		WebhookID: hook.ID, Event: "event.close", Payload: "{}", Status: models.DeliveryStatusDelivered, Attempt: 1,
	}); err != nil {
		t.Fatalf("create delivery: %v", err)
	}

	if err := st.Webhooks.Delete(hook.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var rows int64
	if err := st.DB.Model(&models.WebhookDelivery{}).Where("webhook_id = ?", hook.ID).Count(&rows).Error; err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if rows != 0 {
		t.Fatal("delivery rows naming an endpoint that no longer exists are a history nobody can act on")
	}

	if err := st.Webhooks.Delete(hook.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found on the second delete, got %v", err)
	}
}

func TestWebhookDeliveryPrune(t *testing.T) {
	st := newTestStore(t)
	hook := &models.Webhook{Name: "oncall", URL: "https://hooks.example/nvr"}
	if err := st.Webhooks.Create(hook); err != nil {
		t.Fatalf("create: %v", err)
	}

	old := &models.WebhookDelivery{WebhookID: hook.ID, Event: "event.close", Payload: "{}", Status: models.DeliveryStatusDelivered, Attempt: 1}
	if err := st.WebhookDeliveries.Create(old); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.DB.Model(&models.WebhookDelivery{}).Where("id = ?", old.ID).
		Update("created_at", daysAgo(40)).Error; err != nil {
		t.Fatalf("age the row: %v", err)
	}
	fresh := &models.WebhookDelivery{WebhookID: hook.ID, Event: "event.close", Payload: "{}", Status: models.DeliveryStatusDelivered, Attempt: 1}
	if err := st.WebhookDeliveries.Create(fresh); err != nil {
		t.Fatalf("create: %v", err)
	}

	pruned, err := st.WebhookDeliveries.PruneOlderThan(daysAgo(30))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected the one old delivery pruned, got %d", pruned)
	}
}
