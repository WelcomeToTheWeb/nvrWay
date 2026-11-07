// The ledger read (S8-09): `GET /api/audit`, admin-only.
//
// The audit *writer* has lived in the handlers since S2-08 — every mutating
// act and every refusal is a row. What was missing is the reader: the plan
// promises the console an answer to "who did what", and a ledger nobody can
// query is a file, not a surface.
//
// The read is deliberately **not** audited. The verb vocabulary lives in
// `models/audit.go` ("declared here so a handler cannot invent a verb"), and
// no read verb is declared for the ledger itself — and the recursion is the
// reason: a read that writes a row makes the page count move under the
// operator reading it, and an admin paging through the ledger to audit
// themselves would be authoring the very rows they are hunting. Reads that
// *are* audited (`storage.backend_read`, `user.read`) earn it because they
// surface credentials; the ledger surfaces no secret the server has not
// already written down.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"nvr/server/models"
	"nvr/server/store"
)

// registerAuditRoutes mounts the ledger read on the admin group. The gate is
// the group's `RequireRole(admin)` — the ledger carries usernames, source IPs
// and the shape of every refusal, which is exactly what a lower role has no
// business reading.
func (h *Handlers) registerAuditRoutes(admin *gin.RouterGroup) {
	admin.GET("/audit", h.ListAudit)
}

// auditView is one row as the console reads it. The model's `details` is a
// JSON *text* column (the same convention every JSON column in this schema
// follows); the view parses it so the console renders a map instead of
// re-parsing a string, and an unparseable value arrives as null rather than
// as a lie about its shape.
type auditView struct {
	ID            string         `json:"id"`
	ActorUserID   string         `json:"actor_user_id"`
	ActorUsername string         `json:"actor_username"`
	ActorRoles    string         `json:"actor_roles"`
	ActorProvider string         `json:"actor_provider"`
	Action        string         `json:"action"`
	TargetType    string         `json:"target_type"`
	TargetID      string         `json:"target_id"`
	Outcome       string         `json:"outcome"`
	Details       map[string]any `json:"details"`
	SourceIP      string         `json:"source_ip"`
	CreatedAt     time.Time      `json:"created_at"`
}

func auditViewOf(row *models.Audit) auditView {
	view := auditView{
		ID:            row.ID,
		ActorUserID:   row.ActorUserID,
		ActorUsername: row.ActorUsername,
		ActorRoles:    row.ActorRoles,
		ActorProvider: row.ActorProvider,
		Action:        row.Action,
		TargetType:    row.TargetType,
		TargetID:      row.TargetID,
		Outcome:       row.Outcome,
		SourceIP:      row.SourceIP,
		CreatedAt:     row.CreatedAt,
	}
	if row.Details != "" {
		var details map[string]any
		if err := json.Unmarshal([]byte(row.Details), &details); err == nil {
			view.Details = details
		}
	}
	return view
}

const (
	auditDefaultLimit = 50
	auditMaxLimit     = 200
)

// ListAudit answers `GET /api/audit?actor=&action=&target_id=&outcome=&from=&to=&limit=&offset=`.
//
// The filters are the ledger's two indexed shapes — an action (or outcome)
// inside a time window, and one actor's acts inside a time window — plus the
// target, because "everything that happened to camera X" is the question a
// deleted camera leaves behind. `outcome` is validated against the four
// stored words, because a filter that silently matches nothing is worse than
// a 400 that names the vocabulary.
func (h *Handlers) ListAudit(c *gin.Context) {
	filter := store.AuditFilter{
		ActorUsername: strings.TrimSpace(c.Query("actor")),
		Action:        strings.TrimSpace(c.Query("action")),
		TargetID:      strings.TrimSpace(c.Query("target_id")),
	}

	if outcome := strings.TrimSpace(c.Query("outcome")); outcome != "" {
		switch outcome {
		case models.AuditOutcomeOK, models.AuditOutcomeRefused,
			models.AuditOutcomeNotFound, models.AuditOutcomeError:
			filter.Outcome = outcome
		default:
			jsonError(c, http.StatusBadRequest,
				`outcome must be "ok", "refused", "not_found" or "error"`)
			return
		}
	}

	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonError(c, http.StatusBadRequest, "from must be an RFC3339 timestamp")
			return
		}
		filter.From = &at
	}
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonError(c, http.StatusBadRequest, "to must be an RFC3339 timestamp")
			return
		}
		filter.To = &at
	}

	limit := auditDefaultLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			jsonError(c, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > auditMaxLimit {
			n = auditMaxLimit
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
	filter.Limit = limit + 1 // one row more, so `truncated` is a fact not a guess
	filter.Offset = offset

	rows, err := h.St.Audit.List(filter)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the ledger is unreadable")
		return
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}

	out := make([]auditView, 0, len(rows))
	for i := range rows {
		out = append(out, auditViewOf(&rows[i]))
	}
	c.JSON(http.StatusOK, gin.H{
		"audits":    out,
		"count":     len(out),
		"limit":     limit,
		"offset":    offset,
		"truncated": truncated,
	})
}
