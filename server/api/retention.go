package api

// The retention surface (admin, read-only): the windows the installation runs,
// as the server states them.
//
// Retention is authored by the environment — `LOCAL_RETENTION_DAYS`,
// `REMOTE_RETENTION_DAYS`, `EVENT_RETENTION_DAYS` — and the sweeper is the one
// who honours them. There is deliberately **no write path here**: a console
// that edited retention would fork the policy into an env file and a settings
// row, and the sweep would keep reading the env while the screen showed the
// row. The honest v1 answer is a read of the truth plus the name of the knob
// that changes it, which is what `docs/configuration.md` and the installation
// guide's retention math table are for.
//
// The three housekeeping windows (delivery history, audit, queue rows) are not
// retention policy — nobody looks for a delivery row from day 29 — so they are
// the sweeper's constants, reported through the same door for the same reason
// the rest is: the console shows what the machine will do, never what someone
// hoped it would.

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"nvr/server/sweeper"
)

// registerRetentionRoutes mounts the read-only retention surface on the admin
// group. The Settings page is the only reader, and the page is admin-only.
func (h *Handlers) registerRetentionRoutes(admin *gin.RouterGroup) {
	admin.GET("/settings/retention", h.RetentionSettings)
}

// RetentionSettings answers the retention policy this process will sweep by.
//
// GET /api/settings/retention (admin)
//
// `remote_forever` is spelled out beside `remote_days` because `0` means
// "keep forever" here and "delete everything now" almost everywhere else; a
// console that guessed the meaning would be one refactor away from telling an
// operator their tier is emptying.
//
// `unuploaded_protection_days` comes from the sweeper's own formula, not a
// restatement: it is the number that decides whether footage that never
// reached the tier is still on disk, and the sweep and the screen must never
// disagree about it.
func (h *Handlers) RetentionSettings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"local_days":     h.Cfg.LocalRetentionDays,
		"remote_days":    h.Cfg.RemoteRetentionDays,
		"remote_forever": h.Cfg.RemoteRetentionDays == 0,
		"event_days":     h.Cfg.EventRetentionDays,

		"unuploaded_protection_days": sweeper.UnuploadedProtectionDays(h.Cfg.LocalRetentionDays),

		"webhook_delivery_days": sweeper.DefaultWebhookDeliveryRetentionDays,
		"audit_days":            sweeper.DefaultAuditRetentionDays,
		"job_days":              sweeper.DefaultJobRetentionDays,

		// Where the policy is authored. The console has no write path for
		// these numbers, and saying so is the difference between a settings
		// page and a status page.
		"author": "environment",
	})
}
