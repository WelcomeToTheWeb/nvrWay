// The webhook settings surface (S6-07): the endpoints, and the history that
// answers the one question an operator brings to them.
//
// Everything here is admin-only. A webhook is a door out of the house — an
// endpoint that receives pictures of people on a person's property, and a URL
// this server will POST to on someone's say-so — so it sits behind the same gate
// as the storage backends and the OIDC providers, and every write is a ledger row.
//
// Two rules shape the shape of the answers.
//
// **The signing secret has no path out.** The console writes it and reads back
// `secret_set`, exactly like a storage backend's credentials, and the suite proves
// it the strong way: the bytes of every response do not contain the secret. A route
// that echoed it would put it in the browser's history, in the console's state, and
// in whatever log the console writes.
//
// **The history is the answer to "why did my hook fail".** The endpoint row carries
// `last_error` for the one-line version; the deliveries route carries the sequence,
// because "it answered 500 twice then 200" and "it has never answered" are different
// problems that the last row alone cannot tell apart.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"nvr/server/models"
	"nvr/server/storage"
	"nvr/server/webhooks"
)

const (
	// webhookDeliveriesDefault/Max bound the history page. Two hundred attempts is
	// more than an operator reads; the ladder is three rows per event, so a page is
	// sixty-odd events, which is a week of a busy camera.
	webhookDeliveriesDefault = 50
	webhookDeliveriesMax     = 200
)

// registerWebhookRoutes mounts the admin-only webhook surface.
func (h *Handlers) registerWebhookRoutes(admin *gin.RouterGroup) {
	admin.GET("/settings/webhooks", h.ListWebhooks)
	admin.POST("/settings/webhooks", h.CreateWebhook)
	admin.PUT("/settings/webhooks/:id", h.UpdateWebhook)
	admin.DELETE("/settings/webhooks/:id", h.DeleteWebhook)
	admin.GET("/settings/webhooks/:id/deliveries", h.ListWebhookDeliveries)
}

// webhookView is an endpoint as the Settings page reads it. No secret is in it:
// `secret_set` is all the API concedes, and it is enough because the form's secret
// field is write-only — it shows "already set" and sends whatever is typed into it.
type webhookView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	CameraID string   `json:"camera_id"`
	Events   []string `json:"events"`
	Enabled  bool     `json:"enabled"`

	SecretSet bool `json:"secret_set"`

	// The last outcome, so the list page answers "is this webhook working" without
	// a request per endpoint. The error text is the receiver's own words, capped by
	// the deliverer, and it is echoed because it is the operator's own endpoint
	// talking about itself — not a credential.
	LastError       string  `json:"last_error"`
	LastDeliveredAt *string `json:"last_delivered_at"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (h *Handlers) webhookView(w *models.Webhook) webhookView {
	return webhookView{
		ID:              w.ID,
		Name:            w.Name,
		URL:             w.URL,
		CameraID:        w.CameraID,
		Events:          models.JSONStringList(w.Events),
		Enabled:         w.Enabled,
		SecretSet:       w.SecretSet(),
		LastError:       w.LastError,
		LastDeliveredAt: formatTimePtr(w.LastDeliveredAt),
		CreatedAt:       w.CreatedAt.UTC().Format(timeLayout),
		UpdatedAt:       w.UpdatedAt.UTC().Format(timeLayout),
	}
}

// webhookInput is what the Settings page sends. Every field is a pointer because an
// omitted secret and an explicitly empty one are different acts — the first keeps
// the stored signing secret (the console never reads it back, so it *always* omits
// it), the second clears it — and a plain string cannot tell them apart.
type webhookInput struct {
	Name     *string   `json:"name"`
	URL      *string   `json:"url"`
	Secret   *string   `json:"secret"`
	CameraID *string   `json:"camera_id"`
	Events   *[]string `json:"events"`
	Enabled  *bool     `json:"enabled"`
}

// applyWebhookInput folds a payload onto a row and reports whether the caller sent a
// signing secret. The flag is what tells the store which column to rewrite, and it
// comes from what the payload *sent* rather than from what the row holds afterwards:
// a cleared secret is an empty column, so a flag computed from "is it non-empty"
// would say "do not write it" and the old secret would survive the operator
// deleting it.
func applyWebhookInput(w *models.Webhook, in *webhookInput) (sentSecret bool, clearedSecret bool) {
	if in.Name != nil {
		w.Name = strings.TrimSpace(*in.Name)
	}
	if in.URL != nil {
		w.URL = strings.TrimSpace(*in.URL)
	}
	if in.CameraID != nil {
		w.CameraID = strings.TrimSpace(*in.CameraID)
	}
	if in.Events != nil {
		// Deduplicated through the same helper the event row's unions use, so a form
		// that sends the same trigger twice stores it once — and the stored list is
		// what the Settings page shows back.
		w.Events = models.JSONStringListToJSON(models.AppendUnique(nil, *in.Events))
	}
	if in.Enabled != nil {
		w.Enabled = *in.Enabled
	}
	if in.Secret != nil {
		sentSecret = true
		plain := strings.TrimSpace(*in.Secret)
		if plain == "" {
			clearedSecret = true
		}
		w.Secret = plain
	}
	return sentSecret, clearedSecret
}

// webhookProblem is the reason a row could not be acted on, or "" for a row that
// could. It runs on the *resulting* row, so a PUT that sends only `{"url":""}` is
// refused for the same reason a POST with no URL is.
//
// The checks are the ones that leave an endpoint silently deaf. A URL with no host
// or a trigger this build never sends both produce an endpoint that looks configured
// and receives nothing, which is a bug the operator discovers by never being
// notified — the worst kind, because nothing is ever wrong on screen.
func (h *Handlers) webhookProblem(w *models.Webhook) string {
	if w.Name == "" {
		return "name is required: it is what the list shows and what the delivery history is read from"
	}
	if w.URL == "" {
		return "url is required"
	}
	parsed, err := url.Parse(w.URL)
	if err != nil {
		return "url is not a URL this server can dial"
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "url must be http or https: a delivery is a POST"
	}
	if parsed.Host == "" {
		return "url has no host"
	}
	if parsed.User != nil {
		// A user:pass in the URL would be a second credential on the same endpoint,
		// stored in a column the sealing covers and sent in a header the signature
		// does not cover. The signing secret is the credential this surface knows
		// about.
		return "url must not carry credentials: use the signing secret"
	}

	events := models.JSONStringList(w.Events)
	if len(events) == 0 {
		return "events must name at least one trigger"
	}
	for _, e := range events {
		if e != webhooks.TriggerClose {
			return fmt.Sprintf("this build sends only %q; %q would receive nothing", webhooks.TriggerClose, e)
		}
	}

	if w.CameraID != "" {
		if _, err := h.St.Cameras.GetByID(w.CameraID); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "camera_id names no camera: leave it empty for every camera"
			}
			return "the camera rows are unreadable"
		}
	}
	return ""
}

// ListWebhooks answers the Settings page.
//
// GET /api/settings/webhooks (admin)
func (h *Handlers) ListWebhooks(c *gin.Context) {
	hooks, err := h.St.Webhooks.ListAll()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the webhook rows are unreadable")
		return
	}
	out := make([]webhookView, 0, len(hooks))
	for i := range hooks {
		out = append(out, h.webhookView(&hooks[i]))
	}
	c.JSON(http.StatusOK, gin.H{"webhooks": out, "count": len(out)})
}

// CreateWebhook adds an endpoint.
//
// POST /api/settings/webhooks (admin)
//
// A new endpoint is enabled: an operator who has just described a destination
// expects notifications to start, and a row that sits disabled until a second
// request looks like a webhook that does not work.
func (h *Handlers) CreateWebhook(c *gin.Context) {
	var in webhookInput
	if err := c.BindJSON(&in); err != nil {
		jsonError(c, http.StatusBadRequest, "the webhook body is not JSON this route understands")
		return
	}

	hook := &models.Webhook{Enabled: true, Events: `["` + webhooks.TriggerClose + `"]`}
	sentSecret, _ := applyWebhookInput(hook, &in)

	if problem := h.webhookProblem(hook); problem != "" {
		h.audit(c, models.AuditWebhookCreate, http.StatusUnprocessableEntity,
			AuditTarget{Type: "webhook", ID: hook.Name}, map[string]any{"reason": problem})
		jsonError(c, http.StatusUnprocessableEntity, problem)
		return
	}
	if err := h.sealWebhookSecret(hook); err != nil {
		h.webhookSealFailure(c, models.AuditWebhookCreate, hook, err)
		return
	}

	if err := h.St.Webhooks.Create(hook); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			h.audit(c, models.AuditWebhookCreate, http.StatusConflict,
				AuditTarget{Type: "webhook", ID: hook.Name}, map[string]any{"name": hook.Name})
			jsonError(c, http.StatusConflict, "webhook name taken: "+hook.Name)
			return
		}
		jsonError(c, http.StatusInternalServerError, "the webhook row could not be written")
		return
	}

	fresh, err := h.St.Webhooks.GetByID(hook.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the webhook row is unreadable after the write")
		return
	}
	h.audit(c, models.AuditWebhookCreate, http.StatusCreated,
		AuditTarget{Type: "webhook", ID: fresh.ID}, map[string]any{
			"name":        fresh.Name,
			"url":         fresh.URL,
			"camera_id":   fresh.CameraID,
			"events":      models.JSONStringList(fresh.Events),
			"enabled":     fresh.Enabled,
			"secret_set":  fresh.SecretSet(),
			"secret_sent": sentSecret,
		})
	c.JSON(http.StatusCreated, h.webhookView(fresh))
}

// UpdateWebhook changes an endpoint.
//
// PUT /api/settings/webhooks/:id (admin)
//
// A secret the payload omits keeps its stored value; a secret the payload sends
// empty is cleared. That is not a nicety: the console reads `secret_set` and never
// the secret, so every save it performs omits the field, and a route that treated an
// omitted secret as "clear it" would un-sign every endpoint the moment someone
// renamed it.
func (h *Handlers) UpdateWebhook(c *gin.Context) {
	hook, ok := h.webhookByID(c)
	if !ok {
		return
	}
	before := *hook

	var in webhookInput
	if err := c.BindJSON(&in); err != nil {
		jsonError(c, http.StatusBadRequest, "the webhook body is not JSON this route understands")
		return
	}
	sentSecret, clearedSecret := applyWebhookInput(hook, &in)

	if problem := h.webhookProblem(hook); problem != "" {
		h.audit(c, models.AuditWebhookUpdate, http.StatusUnprocessableEntity,
			AuditTarget{Type: "webhook", ID: hook.ID}, map[string]any{"reason": problem})
		jsonError(c, http.StatusUnprocessableEntity, problem)
		return
	}
	if err := h.sealWebhookSecret(hook); err != nil {
		h.webhookSealFailure(c, models.AuditWebhookUpdate, hook, err)
		return
	}

	if err := h.St.Webhooks.Update(hook, sentSecret); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			h.audit(c, models.AuditWebhookUpdate, http.StatusConflict,
				AuditTarget{Type: "webhook", ID: hook.ID}, map[string]any{"name": hook.Name})
			jsonError(c, http.StatusConflict, "webhook name taken: "+hook.Name)
			return
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no such webhook")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the webhook row could not be written")
		return
	}

	fresh, err := h.St.Webhooks.GetByID(hook.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the webhook row is unreadable after the write")
		return
	}

	// The ledger records what changed, including the change nobody can see: a
	// signing secret that was removed. "The secret was cleared" is exactly what a
	// ledger row is for, because the next person's question will be why the
	// receiver started rejecting signatures.
	changes := map[string]any{}
	if before.Name != fresh.Name {
		changes["name"] = []any{before.Name, fresh.Name}
	}
	if before.URL != fresh.URL {
		changes["url"] = []any{before.URL, fresh.URL}
	}
	if before.CameraID != fresh.CameraID {
		changes["camera_id"] = []any{before.CameraID, fresh.CameraID}
	}
	if before.Events != fresh.Events {
		changes["events"] = []any{models.JSONStringList(before.Events), models.JSONStringList(fresh.Events)}
	}
	if before.Enabled != fresh.Enabled {
		changes["enabled"] = []any{before.Enabled, fresh.Enabled}
	}
	switch {
	case clearedSecret:
		changes["secret"] = "cleared"
	case sentSecret:
		changes["secret"] = "replaced"
	}
	h.audit(c, models.AuditWebhookUpdate, http.StatusOK,
		AuditTarget{Type: "webhook", ID: fresh.ID}, map[string]any{
			"name":    fresh.Name,
			"changes": changes,
		})
	c.JSON(http.StatusOK, h.webhookView(fresh))
}

// DeleteWebhook removes an endpoint and, with it, its delivery history.
//
// DELETE /api/settings/webhooks/:id (admin)
//
// The history goes because rows naming an endpoint that no longer exists are a
// history nobody can act on — but the count is reported, because an operator who
// deletes an endpoint mid-investigation is entitled to know how much of what they
// were reading went with it.
func (h *Handlers) DeleteWebhook(c *gin.Context) {
	hook, ok := h.webhookByID(c)
	if !ok {
		return
	}

	attempts, err := h.St.WebhookDeliveries.CountForWebhook(hook.ID)
	if err != nil {
		// The count is a detail of the answer, not a precondition of the delete.
		// A ledger row that says "unknown" is better than refusing to delete.
		attempts = -1
	}

	if err := h.St.Webhooks.Delete(hook.ID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no such webhook")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the webhook row could not be deleted")
		return
	}

	h.audit(c, models.AuditWebhookDelete, http.StatusOK,
		AuditTarget{Type: "webhook", ID: hook.ID}, map[string]any{
			"name":               hook.Name,
			"url":                hook.URL,
			"deliveries_removed": attempts,
			"secret_was_set":     hook.SecretSet(),
		})
	c.JSON(http.StatusOK, gin.H{
		"deleted":            hook.Name,
		"deliveries_removed": attempts,
	})
}

// ListWebhookDeliveries is the history: one row per attempt, newest first.
//
// GET /api/settings/webhooks/:id/deliveries?status=&limit=&offset= (admin)
//
// The response carries the endpoint inline, because the drawer that shows the
// history also shows whether the endpoint is switched on and what its last error
// was, and one request that answers both is one the console cannot get out of sync.
//
// `status=failed` is the filter the question asks for. It is a filter and not a
// computation over the page because the failures are what an operator reads, and a
// page of successes tells them nothing about why the hook failed.
func (h *Handlers) ListWebhookDeliveries(c *gin.Context) {
	hook, ok := h.webhookByID(c)
	if !ok {
		return
	}

	status := strings.TrimSpace(c.Query("status"))
	switch status {
	case "", models.DeliveryStatusDelivered, models.DeliveryStatusFailed, models.DeliveryStatusPending:
	default:
		jsonError(c, http.StatusBadRequest, `status must be "delivered", "failed" or "pending"`)
		return
	}

	limit := webhookDeliveriesDefault
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			jsonError(c, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > webhookDeliveriesMax {
			n = webhookDeliveriesMax
		}
		limit = n
	}
	offset := 0
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			jsonError(c, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = n
	}

	// One row more than the page, so `truncated` is a fact rather than a guess. The
	// store's own clamp sits above this route's ceiling precisely so the probe
	// cannot be the thing that answers "there is nothing more".
	rows, err := h.St.WebhookDeliveries.ListForWebhook(hook.ID, status, limit+1, offset)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the delivery history is unreadable")
		return
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}

	out := make([]webhookDeliveryView, 0, len(rows))
	for i := range rows {
		out = append(out, webhookDeliveryViewOf(&rows[i]))
	}
	c.JSON(http.StatusOK, gin.H{
		"webhook":    h.webhookView(hook),
		"deliveries": out,
		"count":      len(out),
		"limit":      limit,
		"offset":     offset,
		"truncated":  truncated,
	})
}

// webhookDeliveryView is one attempt. The payload travels as the JSON object it is
// rather than as a string inside a string, because the operator's second question
// after "why did it fail" is "what did we send it", and a console should be able to
// show that without a second request.
type webhookDeliveryView struct {
	ID          string          `json:"id"`
	Event       string          `json:"event"`
	Status      string          `json:"status"`
	StatusCode  int             `json:"status_code"`
	Attempt     int             `json:"attempt"`
	Error       string          `json:"error"`
	DeliveredAt *string         `json:"delivered_at"`
	CreatedAt   string          `json:"created_at"`
	Payload     json.RawMessage `json:"payload"`
}

func webhookDeliveryViewOf(d *models.WebhookDelivery) webhookDeliveryView {
	// The payload is the exact bytes that went out, so it is valid JSON by
	// construction. The guard is for the row that is not — a hand-edited database —
	// where dropping the field would hide the thing the operator is looking for, so
	// it goes out as a quoted string instead.
	payload := json.RawMessage(d.Payload)
	if !json.Valid(payload) {
		if encoded, err := json.Marshal(d.Payload); err == nil {
			payload = encoded
		} else {
			payload = json.RawMessage("null")
		}
	}

	return webhookDeliveryView{
		ID:          d.ID,
		Event:       d.Event,
		Status:      d.Status,
		StatusCode:  d.StatusCode,
		Attempt:     d.Attempt,
		Error:       d.Error,
		DeliveredAt: formatTimePtr(d.DeliveredAt),
		CreatedAt:   d.CreatedAt.UTC().Format(timeLayout),
		Payload:     payload,
	}
}

// webhookByID resolves the :id every webhook route starts from.
func (h *Handlers) webhookByID(c *gin.Context) (*models.Webhook, bool) {
	id := strings.TrimSpace(c.Param("id"))
	hook, err := h.St.Webhooks.GetByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no such webhook")
			return nil, false
		}
		jsonError(c, http.StatusInternalServerError, "the webhook rows are unreadable")
		return nil, false
	}
	return hook, true
}

// sealWebhookSecret puts the signing secret in the column's real form. The deliverer
// opens it again before signing (S6-06), so a route that stored it unsealed would
// still work — it would just be the one credential in this installation sitting in
// the database as plaintext.
func (h *Handlers) sealWebhookSecret(w *models.Webhook) error {
	if w.Secret == "" {
		return nil
	}
	if h.sealErr != nil {
		return fmt.Errorf("the seal key is unusable: %w", h.sealErr)
	}
	sealed, err := storage.Seal(w.Secret, h.sealKey)
	if err != nil {
		return err
	}
	w.Secret = sealed
	return nil
}

// webhookSealFailure answers a secret that could not be sealed. The sentence never
// contains the secret: this is the route that receives it, and an error message is
// a path into the browser's console.
func (h *Handlers) webhookSealFailure(c *gin.Context, action string, w *models.Webhook, err error) {
	h.audit(c, action, http.StatusInternalServerError,
		AuditTarget{Type: "webhook", ID: w.ID}, map[string]any{"reason": "the signing secret could not be sealed"})
	c.JSON(http.StatusInternalServerError, gin.H{
		"error":  "the signing secret could not be sealed with this installation's key",
		"detail": err.Error(),
	})
}

// formatTimePtr renders an optional timestamp in the layout every other read
// surface uses. Nil stays nil rather than becoming a zero date, because "never
// delivered" and "delivered in the year 1" are different answers.
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	value := t.UTC().Format(timeLayout)
	return &value
}
