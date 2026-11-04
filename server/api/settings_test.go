package api

// S6-07 — the webhook settings surface.
//
// The three exit criteria are all about what a response says and what the ledger
// knows:
//
//   - the signing secret has no path out: `secret_set: true`, and the secret's
//     bytes are not in any response
//   - every write is a ledger row, including the writes that were refused
//   - the deliveries list answers "why did my hook fail"
//
// The delivery loop itself is `webhooks/webhooks_test.go`; this file is the surface
// an operator drives, and the one place a secret can leak out of the installation.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"nvr/server/models"
	"nvr/server/storage"
	"nvr/server/store"
)

// webhookSecret is what an operator types into the secret field. It is deliberately
// a string no other test data contains, so "the bytes of the response do not contain
// it" is a check that cannot pass by accident.
const webhookSecret = "sk-nvr-webhook-6f0f-a1b2c3d4e5f6"

// webhookFixture is the admin surface with one route table and three roles' tokens.
type webhookFixture struct {
	st     *store.Store
	h      *Handlers
	router interface {
		ServeHTTP(http.ResponseWriter, *http.Request)
	}
	admin    string
	operator string
	viewer   string
}

func newWebhookFixture(t *testing.T) *webhookFixture {
	t.Helper()
	st := newStore(t)
	h := newHandlers(t, st)

	f := &webhookFixture{st: st, h: h, router: routerFor(t, h)}
	f.admin, _ = usersToken(t, h)
	f.viewer = webhookToken(t, st, h, "hook-viewer", models.RoleViewer)
	f.operator = webhookToken(t, st, h, "hook-operator", models.RoleOperator)
	return f
}

func webhookToken(t *testing.T, st *store.Store, h *Handlers, username, role string) string {
	t.Helper()
	u := newUser(t, st, username, role)
	return accessToken(t, st, h.JWT, u)
}

// call runs one request as the admin.
func (f *webhookFixture) call(t *testing.T, r request) (int, map[string]any, string) {
	t.Helper()
	r.bearer = f.admin
	return do(t, f.router, r)
}

// create posts an endpoint exactly as the test described it. It fills in nothing:
// the route's refusals are keyed on the fields that are missing, and a helper that
// supplied a default URL would make "no url" untestable.
func (f *webhookFixture) create(t *testing.T, body map[string]any) (int, map[string]any, string) {
	t.Helper()
	return f.call(t, request{method: http.MethodPost, path: "/api/settings/webhooks", body: body})
}

// createHook posts a working endpoint: the URL is a plain http one because the
// route refuses anything else, and those refusals are a separate case.
func (f *webhookFixture) createHook(t *testing.T, name string) map[string]any {
	t.Helper()
	status, body, raw := f.create(t, map[string]any{
		"name":   name,
		"url":    "http://192.168.1.50:8123/api/webhook/abc",
		"secret": webhookSecret,
	})
	if status != http.StatusCreated {
		t.Fatalf("creating %s: got %d %s, want 201", name, status, raw)
	}
	return body
}

func (f *webhookFixture) idOf(t *testing.T, body map[string]any) string {
	t.Helper()
	id, ok := body["id"].(string)
	if !ok || id == "" {
		t.Fatalf("the created row has no id: %v", body)
	}
	return id
}

// seedDelivery files one attempt row directly. The timestamps are explicit and a
// second apart because the history is ordered by `created_at`, and rows written in
// the same millisecond would make "newest first" a question the database answers
// differently per query.
func (f *webhookFixture) seedDelivery(t *testing.T, webhookID string, attempt int, status string, code int, reason string, at time.Time) *models.WebhookDelivery {
	t.Helper()
	row := &models.WebhookDelivery{
		WebhookID:  webhookID,
		Event:      "event.close",
		Payload:    fmt.Sprintf(`{"id":"evt-1","attempt":%d}`, attempt),
		Status:     status,
		StatusCode: code,
		Attempt:    attempt,
		Error:      reason,
		CreatedAt:  at.UTC(),
	}
	if status == models.DeliveryStatusDelivered {
		when := at.UTC()
		row.DeliveredAt = &when
	}
	if err := f.st.WebhookDeliveries.Create(row); err != nil {
		t.Fatalf("seeding the attempt: %v", err)
	}
	return row
}

func TestWebhooksGateIsAdminOnly(t *testing.T) {
	// A webhook is a door out of the house and a URL this server will POST to, so
	// the surface sits behind the same gate as the storage backends. The denial is
	// audited by the middleware that answers it, which is why no handler here has to
	// think about the role.
	f := newWebhookFixture(t)
	created := f.createHook(t, "front-door-hook")
	id := f.idOf(t, created)

	routes := []request{
		{method: http.MethodGet, path: "/api/settings/webhooks"},
		{method: http.MethodPost, path: "/api/settings/webhooks", body: map[string]any{"name": "x", "url": "http://h/x"}},
		{method: http.MethodPut, path: "/api/settings/webhooks/" + id, body: map[string]any{"enabled": false}},
		{method: http.MethodDelete, path: "/api/settings/webhooks/" + id},
		{method: http.MethodGet, path: "/api/settings/webhooks/" + id + "/deliveries"},
	}

	for _, role := range []struct {
		name  string
		token string
	}{
		{"viewer", f.viewer},
		{"operator", f.operator},
	} {
		for _, r := range routes {
			r.bearer = role.token
			status, _, raw := do(t, f.router, r)
			if status != http.StatusForbidden {
				t.Errorf("%s %s %s answered %d %s, want 403", role.name, r.method, r.path, status, raw)
			}
		}
	}

	// No token at all is a 401, not a 403: the question "who are you" is answered
	// before the question "may you".
	for _, r := range routes {
		status, _, _ := do(t, f.router, r)
		if status != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s answered %d, want 401", r.method, r.path, status)
		}
	}
}

func TestWebhooksSecretIsNeverEchoedByAnyRoute(t *testing.T) {
	// The exit criterion, in the strongest form: not "the view has no secret
	// field" but "the bytes of every response do not contain the secret". That also
	// catches a handler that puts it in an error message, which is how a write-only
	// field ends up in the browser's console.
	f := newWebhookFixture(t)

	const other = "sk-nvr-webhook-0000-000000000000"
	status, created, raw := f.create(t, map[string]any{"name": "signed", "url": "http://h/x", "secret": webhookSecret})
	if status != http.StatusCreated {
		t.Fatalf("create answered %d %s", status, raw)
	}
	id := f.idOf(t, created)
	if created["secret_set"] != true {
		t.Errorf("the created row says secret_set=%v, want true", created["secret_set"])
	}
	for _, response := range []string{raw} {
		if strings.Contains(response, webhookSecret) {
			t.Errorf("a response carries the secret: %s", response)
		}
	}

	// The column really holds sealed text that opens with this installation's key —
	// a route that stored the plaintext would pass every byte check below.
	stored, err := f.st.Webhooks.GetByID(id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Secret == webhookSecret {
		t.Error("the secret column holds plaintext")
	}
	key, err := f.h.Cfg.SealKey()
	if err != nil {
		t.Fatalf("SealKey: %v", err)
	}
	opened, err := storage.Unseal(stored.Secret, key)
	if err != nil {
		t.Fatalf("the stored secret does not open: %v", err)
	}
	if opened != webhookSecret {
		t.Errorf("the column opens to %q, want the secret the operator sent", opened)
	}

	// Every route that can name this endpoint. The delete is last because it is the
	// one that stops the others.
	pages := []request{
		{method: http.MethodGet, path: "/api/settings/webhooks"},
		{method: http.MethodPut, path: "/api/settings/webhooks/" + id, body: map[string]any{"name": "signed-2"}},
		{method: http.MethodGet, path: "/api/settings/webhooks/" + id + "/deliveries"},
		{method: http.MethodDelete, path: "/api/settings/webhooks/" + id},
	}
	for _, r := range pages {
		_, _, raw := f.call(t, r)
		if strings.Contains(raw, webhookSecret) {
			t.Errorf("%s %s leaks the secret: %s", r.method, r.path, raw)
		}
	}

	// A second endpoint with a different secret, so a check that one response is
	// clean cannot be an accident of which secret was used.
	status, _, raw = f.create(t, map[string]any{"name": "other", "url": "http://h/y", "secret": other})
	if status != http.StatusCreated {
		t.Fatalf("create answered %d %s", status, raw)
	}
	if strings.Contains(raw, other) || strings.Contains(raw, webhookSecret) {
		t.Errorf("the second create leaks a secret: %s", raw)
	}
}

func TestWebhooksEveryWriteIsALedgerRow(t *testing.T) {
	f := newWebhookFixture(t)

	status, created, raw := f.create(t, map[string]any{"name": "audited", "url": "http://h/x", "secret": webhookSecret})
	if status != http.StatusCreated {
		t.Fatalf("create answered %d %s", status, raw)
	}
	id := f.idOf(t, created)

	if rows := auditRows(t, f.st, models.AuditWebhookCreate); len(rows) != 1 {
		t.Errorf("the create left %d ledger rows, want 1", len(rows))
	} else {
		row := rows[0]
		if row.TargetID != id {
			t.Errorf("the create row targets %q, want the endpoint's id", row.TargetID)
		}
		// The details name the secret's state and never its value: the ledger is
		// read by anyone who can see the Audit page.
		if strings.Contains(row.Details, webhookSecret) {
			t.Errorf("the ledger row carries the secret: %s", row.Details)
		}
	}

	status, _, raw = f.call(t, request{
		method: http.MethodPut, path: "/api/settings/webhooks/" + id,
		body: map[string]any{"enabled": false},
	})
	if status != http.StatusOK {
		t.Fatalf("update answered %d %s", status, raw)
	}
	if rows := auditRows(t, f.st, models.AuditWebhookUpdate); len(rows) != 1 {
		t.Errorf("the update left %d ledger rows, want 1", len(rows))
	}

	status, _, raw = f.call(t, request{method: http.MethodDelete, path: "/api/settings/webhooks/" + id})
	if status != http.StatusOK {
		t.Fatalf("delete answered %d %s", status, raw)
	}
	if rows := auditRows(t, f.st, models.AuditWebhookDelete); len(rows) != 1 {
		t.Errorf("the delete left %d ledger rows, want 1", len(rows))
	}
}

func TestWebhooksRefusalIsALedgerRowToo(t *testing.T) {
	// A write that was refused is the one an operator asks about. The storage
	// surface records its refusals the same way, and the denial is only half the
	// story without the reason that was answered.
	f := newWebhookFixture(t)

	status, body, _ := f.create(t, map[string]any{"name": "no-url", "url": ""})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("an endpoint with no url answered %d %v, want 422", status, body)
	}
	rows := auditRows(t, f.st, models.AuditWebhookCreate)
	if len(rows) != 1 {
		t.Fatalf("the refusal left %d ledger rows, want 1", len(rows))
	}
	if !strings.Contains(rows[0].Details, "url is required") {
		t.Errorf("the refusal row does not carry the reason: %s", rows[0].Details)
	}
	if rows[0].Outcome != models.AuditOutcomeRefused {
		t.Errorf("the refusal row has outcome %q, want %q", rows[0].Outcome, models.AuditOutcomeRefused)
	}
}

func TestWebhooksCreateRefusesAnEndpointThatWouldHearNothing(t *testing.T) {
	// Each of these is a row that would save cleanly and then never deliver, which
	// is the failure mode with no symptom on screen.
	f := newWebhookFixture(t)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no name", map[string]any{"url": "http://h/x"}, "name is required"},
		{"no url", map[string]any{"name": "a"}, "url is required"},
		{"no scheme", map[string]any{"name": "a", "url": "not-a-url"}, "http or https"},
		{"wrong scheme", map[string]any{"name": "a", "url": "ftp://h/x"}, "http or https"},
		{"no host", map[string]any{"name": "a", "url": "http://"}, "no host"},
		{"credentials in the url", map[string]any{"name": "a", "url": "http://u:p@h/x"}, "must not carry credentials"},
		{"no triggers", map[string]any{"name": "a", "url": "http://h/x", "events": []string{}}, "at least one trigger"},
		{"unknown trigger", map[string]any{"name": "a", "url": "http://h/x", "events": []string{"event.open"}}, "event.close"},
		{"unknown camera", map[string]any{"name": "a", "url": "http://h/x", "camera_id": "00000000-0000-4000-8000-000000000000"}, "names no camera"},
	}

	for _, tc := range cases {
		status, body, raw := f.create(t, tc.body)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("%s answered %d %v, want 422", tc.name, status, body)
			continue
		}
		message, _ := body["error"].(string)
		if !strings.Contains(message, tc.want) {
			t.Errorf("%s answered %q, want it to say %q (%s)", tc.name, message, tc.want, raw)
		}
	}
}

func TestWebhooksCreateDefaultsToEnabledAndEventClose(t *testing.T) {
	// An operator who has just described a destination expects notifications to
	// start, and `event.close` is the only trigger v1 sends — so the defaults are
	// the two fields the form does not have.
	f := newWebhookFixture(t)

	created := f.createHook(t, "defaults")
	if created["enabled"] != true {
		t.Errorf("a new endpoint is enabled=%v, want true", created["enabled"])
	}
	events, _ := created["events"].([]any)
	if len(events) != 1 || events[0] != "event.close" {
		t.Errorf("a new endpoint subscribes to %v, want [event.close]", events)
	}
	if created["camera_id"] != "" {
		t.Errorf("a new endpoint is scoped to camera %v, want every camera", created["camera_id"])
	}
	if created["last_delivered_at"] != nil {
		t.Errorf("a new endpoint has last_delivered_at %v, want null: it has never delivered", created["last_delivered_at"])
	}
}

func TestWebhooksUpdateKeepsAnOmittedSecretAndClearsAnEmptyOne(t *testing.T) {
	// The one case the pointer fields exist for. The console reads `secret_set` and
	// never the secret, so every save it performs omits the field; a route that read
	// an omitted secret as "clear it" would un-sign every endpoint the moment someone
	// renamed it.
	f := newWebhookFixture(t)
	id := f.idOf(t, f.createHook(t, "secret-semantics"))

	openedSecret := func(t *testing.T) string {
		t.Helper()
		stored, err := f.st.Webhooks.GetByID(id)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if stored.Secret == "" {
			return ""
		}
		key, err := f.h.Cfg.SealKey()
		if err != nil {
			t.Fatalf("SealKey: %v", err)
		}
		opened, err := storage.Unseal(stored.Secret, key)
		if err != nil {
			t.Fatalf("the stored secret does not open: %v", err)
		}
		return opened
	}

	// An omitted secret keeps the stored one. The column's bytes change (the seal
	// uses a fresh nonce), so the assertion is on what it opens to.
	status, body, raw := f.call(t, request{
		method: http.MethodPut, path: "/api/settings/webhooks/" + id,
		body: map[string]any{"name": "renamed"},
	})
	if status != http.StatusOK {
		t.Fatalf("update answered %d %s", status, raw)
	}
	if opened := openedSecret(t); opened != webhookSecret {
		t.Errorf("a PUT that omitted the secret left %q, want the stored one", opened)
	}
	if body["secret_set"] != true {
		t.Errorf("after an omitted secret the view says secret_set=%v, want true", body["secret_set"])
	}

	// An explicitly empty secret clears it, and the ledger records the removal —
	// the next person's question will be why the receiver started rejecting
	// signatures.
	status, body, raw = f.call(t, request{
		method: http.MethodPut, path: "/api/settings/webhooks/" + id,
		body: map[string]any{"secret": ""},
	})
	if status != http.StatusOK {
		t.Fatalf("clearing answered %d %s", status, raw)
	}
	if opened := openedSecret(t); opened != "" {
		t.Errorf("a PUT that sent an empty secret left %q, want the column cleared", opened)
	}
	if body["secret_set"] != false {
		t.Errorf("after clearing, the view says secret_set=%v, want false", body["secret_set"])
	}
	rows := auditRows(t, f.st, models.AuditWebhookUpdate)
	if len(rows) == 0 {
		t.Fatal("clearing the secret left no ledger row")
	}
	if !strings.Contains(rows[0].Details, "cleared") {
		t.Errorf("the ledger row does not record the removal: %s", rows[0].Details)
	}

	// A new secret replaces it.
	const replaced = "sk-nvr-webhook-9999-999999999999"
	status, _, raw = f.call(t, request{
		method: http.MethodPut, path: "/api/settings/webhooks/" + id,
		body: map[string]any{"secret": replaced},
	})
	if status != http.StatusOK {
		t.Fatalf("replacing answered %d %s", status, raw)
	}
	if opened := openedSecret(t); opened != replaced {
		t.Errorf("the secret column holds %q, want the one the PUT sent", opened)
	}
}

func TestWebhooksNameCollisionAnswersConflict(t *testing.T) {
	// The name is the handle the list and the history are read from, so two
	// endpoints sharing one is a row an operator cannot tell apart.
	f := newWebhookFixture(t)
	f.createHook(t, "dup")

	status, body, _ := f.create(t, map[string]any{"name": "dup", "url": "http://h/y"})
	if status != http.StatusConflict {
		t.Fatalf("a duplicate name answered %d %v, want 409", status, body)
	}
	if !strings.Contains(fmt.Sprint(body["error"]), "dup") {
		t.Errorf("the 409 says %v, want it to name the name that is taken", body["error"])
	}
	if rows := auditRows(t, f.st, models.AuditWebhookCreate); len(rows) != 2 {
		t.Errorf("the collision left %d ledger rows, want 2: a refused write is on the record", len(rows))
	}
}

func TestWebhooksDeliveriesAnswerWhyTheHookFailed(t *testing.T) {
	// The exit criterion. The endpoint row answers "is it working"; this answers
	// "why not", and the answer is the sequence — 500, 500, then 200 — because the
	// last row alone cannot tell "it recovered" from "it has never worked".
	f := newWebhookFixture(t)
	id := f.idOf(t, f.createHook(t, "flaky"))

	base := time.Now().UTC().Add(-time.Hour)
	f.seedDelivery(t, id, 1, models.DeliveryStatusFailed, http.StatusInternalServerError, "the endpoint answered 500: nginx is bored", base)
	f.seedDelivery(t, id, 2, models.DeliveryStatusFailed, http.StatusBadGateway, "the endpoint did not answer: dial tcp 192.168.1.50:8123: connect: connection refused", base.Add(2*time.Second))
	f.seedDelivery(t, id, 1, models.DeliveryStatusDelivered, http.StatusOK, "", base.Add(time.Hour))

	status, body, raw := f.call(t, request{method: http.MethodGet, path: "/api/settings/webhooks/" + id + "/deliveries"})
	if status != http.StatusOK {
		t.Fatalf("the history answered %d %s", status, raw)
	}

	if body["count"] != float64(3) {
		t.Fatalf("the history carries %v rows, want 3", body["count"])
	}
	list, _ := body["deliveries"].([]any)
	if len(list) != 3 {
		t.Fatalf("the deliveries array holds %d rows, want 3", len(list))
	}

	newest, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("the newest row is not an object: %v", list[0])
	}
	if newest["status"] != models.DeliveryStatusDelivered {
		t.Errorf("the newest row is %v, want the delivered one: the history is newest-first", newest["status"])
	}
	if newest["delivered_at"] == nil {
		t.Error("a delivered row carries no delivered_at")
	}

	// The payload travels as the JSON object it is, so the console can answer "what
	// did we send it" without a second request.
	if payload, ok := newest["payload"].(map[string]any); !ok || payload["id"] != "evt-1" {
		t.Errorf("payload is %v, want the sent object rather than a string inside a string", newest["payload"])
	}

	// The second row is the one that answers the question, and it carries both the
	// status code and the sentence.
	older, ok := list[1].(map[string]any)
	if !ok {
		t.Fatalf("the second row is not an object: %v", list[1])
	}
	if older["status_code"] != float64(http.StatusBadGateway) {
		t.Errorf("the second row carries status_code %v, want 502", older["status_code"])
	}
	if reason, _ := older["error"].(string); !strings.Contains(reason, "connection refused") {
		t.Errorf("the second row's error is %q, want the sentence that says what to fix", reason)
	}
	if older["attempt"] != float64(2) {
		t.Errorf("the second row is attempt %v, want 2", older["attempt"])
	}
	if older["event"] != "event.close" {
		t.Errorf("the row is filed under %v, want the trigger that sent it", older["event"])
	}

	// The endpoint comes back inline: the drawer shows whether the thing is switched
	// on and what its last error was, and one request that answers both is one the
	// console cannot get out of sync.
	hook, ok := body["webhook"].(map[string]any)
	if !ok {
		t.Fatal("the response carries no webhook")
	}
	if hook["name"] != "flaky" {
		t.Errorf("the inline endpoint is %v, want the one whose history this is", hook["name"])
	}
	if hook["secret_set"] != true {
		t.Errorf("the inline endpoint says secret_set=%v, want true", hook["secret_set"])
	}
	if strings.Contains(raw, webhookSecret) {
		t.Errorf("the history response leaks the secret: %s", raw)
	}
}

func TestWebhooksDeliveriesFilterToTheFailures(t *testing.T) {
	// `?status=failed` is the filter the question asks for, and it is a filter
	// rather than a computation over the page because the successes are not the
	// thing being read.
	f := newWebhookFixture(t)
	id := f.idOf(t, f.createHook(t, "mixed"))

	base := time.Now().UTC().Add(-time.Hour)
	f.seedDelivery(t, id, 1, models.DeliveryStatusFailed, http.StatusInternalServerError, "the endpoint answered 500", base)
	f.seedDelivery(t, id, 1, models.DeliveryStatusDelivered, http.StatusOK, "", base.Add(time.Minute))

	status, body, raw := f.call(t, request{
		method: http.MethodGet, path: "/api/settings/webhooks/" + id + "/deliveries",
		query: "status=failed",
	})
	if status != http.StatusOK {
		t.Fatalf("the filtered history answered %d %s", status, raw)
	}
	list, _ := body["deliveries"].([]any)
	if len(list) != 1 {
		t.Fatalf("the filtered history holds %d rows, want the one failure", len(list))
	}
	row, _ := list[0].(map[string]any)
	if row["status"] != models.DeliveryStatusFailed {
		t.Errorf("the filtered row is %v, want failed", row["status"])
	}

	// A status that is not one of the three the model defines is a typo, and an
	// empty list is how a typo would otherwise be answered.
	status, body, _ = f.call(t, request{
		method: http.MethodGet, path: "/api/settings/webhooks/" + id + "/deliveries",
		query: "status=broken",
	})
	if status != http.StatusBadRequest {
		t.Errorf("status=broken answered %d %v, want 400", status, body)
	}
}

func TestWebhooksDeliveriesPaginateAndSayWhenThereIsMore(t *testing.T) {
	f := newWebhookFixture(t)
	id := f.idOf(t, f.createHook(t, "busy"))

	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		f.seedDelivery(t, id, 1, models.DeliveryStatusFailed, http.StatusInternalServerError, "the endpoint answered 500", base.Add(time.Duration(i)*time.Second))
	}

	status, first, raw := f.call(t, request{
		method: http.MethodGet, path: "/api/settings/webhooks/" + id + "/deliveries",
		query: "limit=2",
	})
	if status != http.StatusOK {
		t.Fatalf("the first page answered %d %s", status, raw)
	}
	if first["count"] != float64(2) || first["truncated"] != true {
		t.Errorf("the first page says count=%v truncated=%v, want 2 and true", first["count"], first["truncated"])
	}

	status, second, raw := f.call(t, request{
		method: http.MethodGet, path: "/api/settings/webhooks/" + id + "/deliveries",
		query: "limit=2&offset=4",
	})
	if status != http.StatusOK {
		t.Fatalf("the last page answered %d %s", status, raw)
	}
	if second["count"] != float64(1) || second["truncated"] != false {
		t.Errorf("the last page says count=%v truncated=%v, want 1 and false", second["count"], second["truncated"])
	}

	// The ceiling. The store clamps its own limit above this route's maximum
	// precisely so the probe for one more row cannot be the thing that answers
	// "there is nothing more" at the ceiling.
	status, atCeiling, raw := f.call(t, request{
		method: http.MethodGet, path: "/api/settings/webhooks/" + id + "/deliveries",
		query: "limit=200",
	})
	if status != http.StatusOK {
		t.Fatalf("limit=200 answered %d %s", status, raw)
	}
	if atCeiling["limit"] != float64(webhookDeliveriesMax) {
		t.Errorf("limit is %v, want the ceiling echoed back", atCeiling["limit"])
	}
	if atCeiling["truncated"] != false {
		t.Errorf("a page that held everything says truncated=%v", atCeiling["truncated"])
	}

	for _, bad := range []string{"limit=0", "limit=-3", "limit=many", "offset=-1"} {
		status, body, _ := f.call(t, request{
			method: http.MethodGet, path: "/api/settings/webhooks/" + id + "/deliveries", query: bad,
		})
		if status != http.StatusBadRequest {
			t.Errorf("%s answered %d %v, want 400", bad, status, body)
		}
	}
}

func TestWebhooksDeleteReportsTheHistoryThatWentWithIt(t *testing.T) {
	// The rows go with the endpoint — a history naming an endpoint that no longer
	// exists is a history nobody can act on — and the count is reported, because an
	// operator who deletes mid-investigation should see how much went with it.
	f := newWebhookFixture(t)
	id := f.idOf(t, f.createHook(t, "doomed"))

	base := time.Now().UTC().Add(-time.Hour)
	f.seedDelivery(t, id, 1, models.DeliveryStatusFailed, 500, "the endpoint answered 500", base)
	f.seedDelivery(t, id, 2, models.DeliveryStatusFailed, 502, "the endpoint answered 502", base.Add(time.Second))

	status, body, raw := f.call(t, request{method: http.MethodDelete, path: "/api/settings/webhooks/" + id})
	if status != http.StatusOK {
		t.Fatalf("delete answered %d %s", status, raw)
	}
	if body["deliveries_removed"] != float64(2) {
		t.Errorf("the delete reports %v rows removed, want 2", body["deliveries_removed"])
	}

	rows, err := f.st.WebhookDeliveries.ListForWebhook(id, "", 50, 0)
	if err != nil {
		t.Fatalf("read the history: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d delivery rows survived the endpoint", len(rows))
	}

	// And the endpoint is gone, so the history route answers 404 rather than an
	// empty page that reads as "it never failed".
	status, body, _ = f.call(t, request{method: http.MethodGet, path: "/api/settings/webhooks/" + id + "/deliveries"})
	if status != http.StatusNotFound {
		t.Errorf("the history of a deleted endpoint answered %d %v, want 404", status, body)
	}
}

func TestWebhooksRoutesAnswerForAnIdThatIsNotThere(t *testing.T) {
	f := newWebhookFixture(t)
	const missing = "00000000-0000-4000-8000-000000000000"

	for _, r := range []request{
		{method: http.MethodPut, path: "/api/settings/webhooks/" + missing, body: map[string]any{"enabled": false}},
		{method: http.MethodDelete, path: "/api/settings/webhooks/" + missing},
		{method: http.MethodGet, path: "/api/settings/webhooks/" + missing + "/deliveries"},
	} {
		status, body, raw := f.call(t, r)
		if status != http.StatusNotFound {
			t.Errorf("%s %s answered %d %s, want 404", r.method, r.path, status, raw)
			continue
		}
		if body["error"] != "no such webhook" {
			t.Errorf("%s %s says %v, want the named 404", r.method, r.path, body["error"])
		}
	}
}

func TestWebhooksListIsAnArrayEvenWithNothingInIt(t *testing.T) {
	f := newWebhookFixture(t)

	status, body, raw := f.call(t, request{method: http.MethodGet, path: "/api/settings/webhooks"})
	if status != http.StatusOK {
		t.Fatalf("the list answered %d %s", status, raw)
	}
	list, ok := body["webhooks"].([]any)
	if !ok {
		t.Fatalf("webhooks is %v, want an array", body["webhooks"])
	}
	if len(list) != 0 {
		t.Errorf("a fresh installation lists %d endpoints, want none", len(list))
	}
	if !strings.HasPrefix(raw, `{"webhooks":[]`) && !strings.Contains(raw, `"webhooks":[]`) {
		t.Errorf("the empty list marshalled as %s, want an empty array rather than null", raw)
	}
}

func TestWebhooksUpdateRefusesAChangeThatWouldSilenceTheEndpoint(t *testing.T) {
	// The same refusals as the create, applied to the *resulting* row: a PUT that
	// sends `{"url":""}` is the same broken endpoint as a POST with no URL, and a
	// rename must not be able to move an endpoint onto a trigger it will never hear.
	f := newWebhookFixture(t)
	id := f.idOf(t, f.createHook(t, "silence"))

	cases := []struct {
		name string
		body map[string]any
	}{
		{"empty url", map[string]any{"url": ""}},
		{"empty name", map[string]any{"name": "  "}},
		{"unknown trigger", map[string]any{"events": []string{"segment.closed"}}},
		{"unknown camera", map[string]any{"camera_id": "00000000-0000-4000-8000-000000000000"}},
	}
	for _, tc := range cases {
		status, body, raw := f.call(t, request{
			method: http.MethodPut, path: "/api/settings/webhooks/" + id, body: tc.body,
		})
		if status != http.StatusUnprocessableEntity {
			t.Errorf("%s answered %d %s, want 422", tc.name, status, raw)
			continue
		}
		if _, ok := body["error"].(string); !ok {
			t.Errorf("%s answered with no error sentence: %v", tc.name, body)
		}
	}

	// The refusals changed nothing: the endpoint is still the one that works.
	status, after, raw := f.call(t, request{method: http.MethodGet, path: "/api/settings/webhooks"})
	if status != http.StatusOK {
		t.Fatalf("the list answered %d %s", status, raw)
	}
	list, _ := after["webhooks"].([]any)
	row, _ := list[0].(map[string]any)
	if row["url"] != "http://192.168.1.50:8123/api/webhook/abc" {
		t.Errorf("a refused update moved the url to %v", row["url"])
	}
}

func TestWebhooksUpdateRecordsWhatChanged(t *testing.T) {
	// The ledger row is the answer to "who turned this off, and when", and a row
	// that says only "update" cannot answer it.
	f := newWebhookFixture(t)
	id := f.idOf(t, f.createHook(t, "watched"))

	status, _, raw := f.call(t, request{
		method: http.MethodPut, path: "/api/settings/webhooks/" + id,
		body: map[string]any{"enabled": false, "camera_id": f.addCameraForRow(t)},
	})
	if status != http.StatusOK {
		t.Fatalf("update answered %d %s", status, raw)
	}

	rows := auditRows(t, f.st, models.AuditWebhookUpdate)
	if len(rows) != 1 {
		t.Fatalf("the update left %d ledger rows, want 1", len(rows))
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(rows[0].Details), &details); err != nil {
		t.Fatalf("the ledger details are not JSON: %v (%s)", err, rows[0].Details)
	}
	changes, _ := details["changes"].(map[string]any)
	if changes == nil {
		t.Fatalf("the ledger row carries no changes: %s", rows[0].Details)
	}
	enabled, _ := changes["enabled"].([]any)
	if len(enabled) != 2 || enabled[0] != true || enabled[1] != false {
		t.Errorf("the enabled change is recorded as %v, want [true,false]", changes["enabled"])
	}
	if _, ok := changes["camera_id"]; !ok {
		t.Errorf("the ledger row does not record the camera change: %s", rows[0].Details)
	}
}

// addCameraForRow puts a camera row in for a webhook to be scoped to.
func (f *webhookFixture) addCameraForRow(t *testing.T) string {
	t.Helper()
	cam := &models.Camera{
		Slug: "gate", Name: "Gate", SourceURI: "rtsp://cam/gate",
		RecordMode: models.RecordModeContinuous, Active: true,
	}
	if err := f.st.Cameras.Create(cam); err != nil {
		t.Fatalf("create camera: %v", err)
	}
	return cam.ID
}
