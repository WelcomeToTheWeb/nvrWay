// Liveness and readiness, the two endpoints that answer without an identity
// (S9-07).
//
// `/health` is the container healthcheck's line. It answers from the process's
// own memory and touches nothing — not the database, not go2rtc, not the disk.
// That is the whole design: a healthcheck that queries the database can be the
// thing that restarts a server out from under a recording, because a slow
// database reads as a dead process to Docker. The handler does not take a
// store, so "never touches the DB" is a fact about the signature, not a
// promise about the code path.
//
// `/api/healthz` is the question an operator (and verify.sh) asks: is the
// system doing its job? Detector liveness is answered as *data* — the worker's
// last heartbeat, a settings row — not as a port probe. The detector has no
// listener to probe; its life is a POST every 30 seconds, and the honest answer
// about a worker that died an hour ago is "stale", which is a 200 with a
// degraded status, not a 500: the server itself is fine, and a healthcheck that
// restarts the *server* because the *detector* is down would replace one dead
// thing with two.
//
// The fresh window is 90 s — three missed heartbeats. Under a 30 s beat, one
// lost POST (a restart, a blip) must not flap the wall green-to-amber; three
// consecutive misses cannot be a blip.

package api

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"nvr/server/config"
	"nvr/server/models"
)

// DetectorFreshWindow is how long a heartbeat keeps the detector "fresh". The
// worker beats every 30 s; this is three beats.
const DetectorFreshWindow = 90 * time.Second

// heartbeatPayload is the worker's heartbeat body. `uptime_seconds` is
// validated — a worker that cannot say how long it has been up is not speaking
// this contract — and then not stored: the stored fact is *when* the worker was
// last heard from, and the server's clock stamps it, so a worker whose clock
// has drifted cannot make itself look fresh forever.
type heartbeatPayload struct {
	UptimeSeconds *float64 `json:"uptime_seconds"`
}

// Health answers `/health`. No store, no sidecar, no disk: see the package
// comment for why that is the point.
func (h *Handlers) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"version":    config.Version,
		"git_commit": config.GitCommit,
	})
}

// DetectorHeartbeat records the worker's beat. The timestamp is this process's
// clock at receipt — the same rule the signal ingest applies to `ts`.
func (h *Handlers) DetectorHeartbeat(c *gin.Context) {
	var in heartbeatPayload
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "heartbeat body is not valid json"})
		return
	}
	if in.UptimeSeconds == nil || *in.UptimeSeconds < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "uptime_seconds must be a non-negative number"})
		return
	}

	if err := h.St.Settings.Set(models.SettingDetectorHeartbeat, h.now().UTC().Format(time.RFC3339Nano)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings row could not be written"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "recorded"})
}

// Healthz answers `/api/healthz`: the server's own liveness plus the detector's
// freshness as read from the heartbeat row.
//
// A detector that has never reported is `stale`, not an error and not a
// missing field: "we have never heard from the worker" and "the worker stopped
// an hour ago" are the same fact from here, and the operator's action is the
// same. A value this process cannot parse is also stale — a damaged row must
// not turn the health endpoint into a 500, and the log line says which row is
// the problem.
func (h *Handlers) Healthz(c *gin.Context) {
	detector := gin.H{
		"status":         "stale",
		"last_seen":      nil,
		"age_seconds":    nil,
		"window_seconds": int(DetectorFreshWindow / time.Second),
	}

	last, err := h.St.Settings.Get(models.SettingDetectorHeartbeat)
	if err != nil {
		log.Printf("healthz: the detector heartbeat row could not be read: %v", err)
	} else if last != "" {
		at, perr := time.Parse(time.RFC3339Nano, last)
		if perr != nil {
			log.Printf("healthz: detector heartbeat %q does not parse as RFC3339; the detector reads stale", last)
		} else {
			age := h.now().Sub(at)
			detector["last_seen"] = at.UTC().Format(time.RFC3339Nano)
			detector["age_seconds"] = age.Seconds()
			if age < DetectorFreshWindow {
				detector["status"] = "fresh"
			}
		}
	}

	status := "ok"
	if detector["status"] != "fresh" {
		status = "degraded"
	}

	c.JSON(http.StatusOK, gin.H{
		"status":     status,
		"version":    config.Version,
		"git_commit": config.GitCommit,
		"detector":   detector,
	})
}
