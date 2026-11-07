// Package api's half of the detection worker's contract (S5-10): the config the
// worker polls, and the signals it posts back.
//
// Both routes sit behind `InternalAuthMiddleware` and nothing else. The worker is
// not a user, has no JWT, and is not going to log in; the shared secret in
// `WORKER_TOKEN` is the whole of its identity, and an empty token answers
// "internal api disabled" rather than letting an unauthenticated process write
// events.
//
// The decisions in here:
//
//   - **The config payload is built from the rows, on every poll.** No cache, no
//     push. A camera added, renamed, or switched off in the console is reflected
//     in the worker's next poll within five seconds, and the worker needs no
//     restart and no message. `ListDetectionEnabled` already answers only
//     `active AND detection_enabled`, so the payload *is* the set the worker may
//     watch — which is why the card's "only DetectionEnabled && Active cameras are
//     processed" is a property of this query, not of the worker's arithmetic.
//   - **A refusal is a 200 with an `error` on the signal, not a 4xx.** The batch
//     answers per signal: "unknown camera" and "detection disabled" are facts
//     about rows that a retry cannot fix, and a worker that retried them would
//     buffer signals forever. The one case that *is* retried is a server-side
//     failure, which answers 500 so the worker keeps its buffer — safe, because
//     the store dedupes on `event_key` (S6-03).
//   - **The worker's `ts` is validated and then thrown away.** A detection box on
//     a machine whose clock has drifted must not be able to file an event in the
//     future, so `EventInput.TS` is this process's clock at receipt. The shape
//     still has to parse: a signal whose timestamp will not parse is a signal this
//     server refuses, and says which one it refused.
//   - **A snapshot over the cap is stored as absent, and the event survives.**
//     The event is the fact; the picture is evidence. Dropping an event because
//     its JPEG was large would lose the fact to keep the evidence, and the log
//     line says which signal lost its picture.
//   - **`record_mode=events` windows open here.** There is no `/window` route:
//     the first signal of an event is the moment recording becomes interesting,
//     and the recorder's window (S4-01) is extended by every later signal for the
//     same camera.
//
// No ledger row per signal. The ledger records acts by an actor with an identity
// (S2-08); a worker posting detections is a machine doing its job, and the event
// rows are the record of it.

package api

import (
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"nvr/server/go2rtc"
	"nvr/server/models"
	"nvr/server/store"
)

// MaxSignalSnapshotBytes is the ceiling on a snapshot this server will keep: the
// worker already caps its own at 200 KB, so anything materially larger is a
// worker that is not the one this contract was written for. Over the cap the
// picture is dropped and the event is filed.
const MaxSignalSnapshotBytes = 250 * 1024

// DetStreamSuffix names the detection tap on go2rtc: `<slug>_det` (S3-05). It is
// here rather than in the worker because this server writes the go2rtc config, so
// the name is decided once and read twice.
const DetStreamSuffix = "_det"

// registerDetectorRoutes mounts the worker's routes on the internal group.
func (h *Handlers) registerDetectorRoutes(g *gin.RouterGroup) {
	g.GET("/detector/config", h.DetectorConfig)
	g.POST("/signals", h.IngestSignals)
	// S9-07: the beat that makes detector liveness answerable as data.
	g.POST("/heartbeat", h.DetectorHeartbeat)
}

// -- GET /api/internal/v1/detector/config ---------------------------------

// detectorZonePayload is one zone rule, as the row's `zones_json` carries it.
// The worker reads the same field names the console wrote, so a zone drawn in the
// browser and a zone inferred on the worker cannot drift apart.
type detectorZonePayload struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Polygon       [][]float64 `json:"polygon"`
	Classes       []string    `json:"classes"`
	Rule          string      `json:"rule"`
	DwellSeconds  int         `json:"dwell_seconds"`
	Severity      int         `json:"severity"`
	MinConfidence float64     `json:"min_confidence"`
	MinAreaRatio  float64     `json:"min_area_ratio"`
	Enabled       bool        `json:"enabled"`
}

// detectorCameraPayload is one camera the worker should watch.
type detectorCameraPayload struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	DetStream string `json:"det_stream"`

	// FPS is the row's `detection_fps`, or the installation's tap rate when the
	// row left it at 0. The worker paces its loop to this, so a 15 fps tap
	// pointed at a 5 fps camera row costs five inferences a second and not
	// fifteen.
	FPS    int `json:"fps"`
	Width  int `json:"width"`
	Height int `json:"height"`

	Sensitivity  float64               `json:"sensitivity"`
	MinAreaRatio float64               `json:"min_area_ratio"`
	Zones        []detectorZonePayload `json:"zones"`

	RecordMode string   `json:"record_mode"`
	Providers  []string `json:"providers"`
}

// detectorSettings is the installation-wide part of the payload. The worker's
// event ceiling lives here because `EVENT_MAX_SECONDS` is a server setting: the
// worker may end an event early, but the row's ceiling is this process's answer.
type detectorSettings struct {
	EventMaxSeconds int `json:"event_max_seconds"`
	DetWidth        int `json:"det_width"`
	DetHeight       int `json:"det_height"`
	DetFPS          int `json:"det_fps"`
}

// detectorConfigPayload is the whole answer.
type detectorConfigPayload struct {
	Providers []string                `json:"providers"`
	Settings  detectorSettings        `json:"settings"`
	Cameras   []detectorCameraPayload `json:"cameras"`
}

// DetectorConfig answers the worker's poll.
func (h *Handlers) DetectorConfig(c *gin.Context) {
	cameras, err := h.St.Cameras.ListDetectionEnabled()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "camera rows could not be read"})
		return
	}

	providers := h.Cfg.DetProviderList()
	payload := detectorConfigPayload{
		Providers: providers,
		Settings: detectorSettings{
			EventMaxSeconds: h.Cfg.EventMaxSeconds,
			DetWidth:        h.Cfg.Go2rtcMjpegWidth,
			DetHeight:       h.Cfg.Go2rtcMjpegHeight,
			DetFPS:          h.Cfg.Go2rtcMjpegFPS,
		},
		// Never null: a worker that iterates `cameras` on an installation with no
		// detection switched on should iterate nothing, not fail on a null.
		Cameras: []detectorCameraPayload{},
	}

	for i := range cameras {
		cam := &cameras[i]
		fps := cam.DetectionFPS
		if fps <= 0 {
			fps = h.Cfg.Go2rtcMjpegFPS
		}
		payload.Cameras = append(payload.Cameras, detectorCameraPayload{
			ID:           cam.ID,
			Slug:         cam.Slug,
			Name:         cam.Name,
			DetStream:    cam.Slug + DetStreamSuffix,
			FPS:          fps,
			Width:        h.Cfg.Go2rtcMjpegWidth,
			Height:       h.Cfg.Go2rtcMjpegHeight,
			Sensitivity:  cam.Sensitivity,
			MinAreaRatio: cam.MinAreaRatio,
			Zones:        zonesForPayload(cam),
			RecordMode:   cam.RecordMode,
			Providers:    providers,
		})
	}

	c.JSON(http.StatusOK, payload)
}

// zonesForPayload reads a camera's rule set. A row whose JSON cannot be decoded
// contributes no rules rather than failing the whole poll: one damaged row should
// cost one camera its zones, not every camera its detection.
func zonesForPayload(cam *models.Camera) []detectorZonePayload {
	zones, err := models.ParseZones(cam.ZonesJSON)
	if err != nil {
		log.Printf("detector: camera %s has a zones_json this server cannot decode (%v); it is sent no rules", cam.Slug, err)
		return []detectorZonePayload{}
	}
	out := make([]detectorZonePayload, 0, len(zones))
	for _, z := range zones {
		if !z.Enabled {
			// The worker would skip it anyway; sending the rule set the operator
			// can see keeps the payload and the console saying the same thing.
			continue
		}
		out = append(out, detectorZonePayload{
			ID:            z.ID,
			Name:          z.Name,
			Polygon:       z.Polygon,
			Classes:       z.Classes,
			Rule:          z.Rule,
			DwellSeconds:  z.DwellSec,
			Severity:      z.Severity,
			MinConfidence: z.MinConfidence,
			MinAreaRatio:  z.MinAreaRatio,
			Enabled:       z.Enabled,
		})
	}
	return out
}

// -- POST /api/internal/v1/signals ----------------------------------------

// signalPayload is one signal, in the plan's key order.
type signalPayload struct {
	CameraID       string               `json:"camera_id"`
	EventKey       string               `json:"event_key"`
	Action         string               `json:"action"`
	Rule           string               `json:"rule"`
	ZoneID         string               `json:"zone_id"`
	ZoneName       string               `json:"zone_name"`
	Severity       int                  `json:"severity"`
	Classes        []string             `json:"classes"`
	TS             string               `json:"ts"`
	Objects        []models.EventObject `json:"objects"`
	SnapshotBase64 string               `json:"snapshot_base64"`
	RecordMode     string               `json:"record_mode"`
}

// signalsPayload is the request body.
type signalsPayload struct {
	Signals []signalPayload `json:"signals"`
}

// signalReply is one signal's answer. `error` is present for a signal that was
// skipped or refused, and its absence is what `accepted` counts.
type signalReply struct {
	EventKey string `json:"event_key,omitempty"`
	ID       string `json:"id,omitempty"`
	Action   string `json:"action,omitempty"`
	Error    string `json:"error,omitempty"`
}

// signalsReply is the response body.
type signalsReply struct {
	Accepted int           `json:"accepted"`
	Events   []signalReply `json:"events"`
}

// IngestSignals applies a batch of worker signals to the event rows.
func (h *Handlers) IngestSignals(c *gin.Context) {
	var in signalsPayload
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "signals body is not valid json"})
		return
	}

	// One clock reading for the batch. Every event this request touches is
	// stamped from here, so two signals in one batch cannot disagree about when
	// they arrived.
	now := h.now().UTC()

	out := signalsReply{Events: make([]signalReply, 0, len(in.Signals))}
	for _, s := range in.Signals {
		reply, fatal := h.applySignal(c, s, now)
		if fatal != "" {
			// The worker keeps the batch and retries it. That is safe because the
			// store dedupes on `event_key`, and it is better than answering 200 for
			// a signal the database never got.
			c.JSON(http.StatusInternalServerError, gin.H{"error": fatal, "events": out.Events})
			return
		}
		if reply.Error == "" {
			out.Accepted++
		}
		out.Events = append(out.Events, reply)
	}

	c.JSON(http.StatusOK, out)
}

// applySignal handles one signal. The second return is a server-side failure that
// should fail the whole batch; an empty one means the reply is the answer.
func (h *Handlers) applySignal(c *gin.Context, s signalPayload, now time.Time) (signalReply, string) {
	reply := signalReply{EventKey: s.EventKey}

	if !models.ValidSignalAction(s.Action) {
		reply.Error = `action must be "open", "extend" or "close"`
		return reply, ""
	}
	if s.EventKey == "" {
		reply.Error = "event_key is required"
		return reply, ""
	}
	if s.CameraID == "" {
		reply.Error = "camera_id is required"
		return reply, ""
	}
	// The shape is checked and the value discarded. A worker whose clock is an
	// hour ahead cannot move an event, but a timestamp this server cannot parse
	// is a worker that is not speaking this contract.
	if _, err := time.Parse(time.RFC3339, s.TS); err != nil {
		reply.Error = `ts must be RFC3339`
		return reply, ""
	}
	if !models.ValidSeverity(s.Severity) {
		// Clamped, not refused: a worker that reports 7 is reporting "high", and
		// losing the event over the number on the dial is the wrong trade.
		if s.Severity < models.SeverityLow {
			s.Severity = models.SeverityLow
		} else {
			s.Severity = models.SeverityHigh
		}
	}

	cam, err := h.St.Cameras.GetByID(s.CameraID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		reply.Error = "unknown camera"
		return reply, ""
	}
	if err != nil {
		return reply, "camera rows could not be read"
	}
	if !cam.DetectionEnabled {
		// The camera exists and its detection was switched off, probably a moment
		// before this signal left a worker that had already seen the frame. The
		// signal is dropped, and the worker is told why.
		reply.Error = "detection disabled"
		return reply, ""
	}

	// The frame these boxes are pixels of (P-10): the detection tap's size,
	// resolved by the one rule that renders the tap. Stored on the event so
	// the console and any API reader can place the boxes without borrowing
	// the camera row's knobs — which default to 0.
	detW, detH := go2rtc.DetectionFrameSize(*cam, h.Cfg)

	in := store.EventInput{
		EventKey:  s.EventKey,
		CameraID:  s.CameraID,
		Action:    s.Action,
		Rule:      s.Rule,
		ZoneID:    s.ZoneID,
		Severity:  s.Severity,
		Classes:   s.Classes,
		Objects:   s.Objects,
		DetWidth:  detW,
		DetHeight: detH,
		TS:        now,
	}

	event, _, err := h.St.Events.UpsertByEventKey(c.Request.Context(), in)
	if errors.Is(err, store.ErrEventClosed) {
		// 200, not 4xx: a worker re-sending a buffered signal has nothing left to
		// retry, and a 4xx would tell it the ingest is broken.
		reply.Error = "event closed"
		if event != nil {
			reply.ID, reply.Action = event.ID, event.Action
		}
		return reply, ""
	}
	if err != nil {
		return reply, "event could not be written"
	}

	reply.ID = event.ID
	reply.Action = event.Action

	if s.Action == models.SignalActionOpen && s.SnapshotBase64 != "" && event.LocalSnapshotPath == "" {
		// The snapshot rides on the open alone — an extend or a close never
		// carries one — so this is its only chance to be written. Two details keep
		// that rule true under a retry:
		//
		//   - the gate is "this row has no picture", not "this signal created the
		//     row". A worker that re-sends an open whose picture the first attempt
		//     lost — over the cap, a disk that refused the write — gets a second
		//     chance, because the picture is still missing;
		//   - a row that already has a picture is never overwritten, so a replay
		//     cannot swap the frame that shows the trigger for a later one.
		//
		// A failure here costs the picture, never the event.
		h.writeEventSnapshot(event.ID, s.SnapshotBase64)
	}

	if recordModeForWindow(cam.RecordMode, s.RecordMode) == models.RecordModeEvents && h.Recorder != nil {
		// The window is the recorder's, and the signal is the only reason it
		// exists: an `events` camera records nothing until something is detected.
		h.Recorder.OpenWindow(cam.ID)
	}

	if h.Clips != nil {
		// S6-01: every applied signal re-states the clip's deadline. An open event
		// gets one armed assembly (the ceiling, so a worker that dies without a
		// close still gets a clip); the close cancels that one and arms the real
		// one at `ended_at + POST_ROLL_SECONDS`, which is when the footage the
		// clip needs is all on disk.
		h.Clips.Schedule(event)
	}

	if h.Webhooks != nil && event.Action == models.SignalActionClose {
		// S6-06: the close is the trigger, and it is the only one v1 sends. The
		// deliverer resolves the endpoints here and returns without waiting for any
		// of them — an endpoint that is down would otherwise hold the worker's
		// request open for the ladder's 72 seconds, and the signal has already been
		// accepted into the database by this point.
		//
		// The camera row goes along because the payload names things a receiver
		// knows — the slug, and the zone names the operator typed — and the event
		// row stores only ids.
		h.Webhooks.NotifyClose(event, cam)
	}

	// S6-08: the same applied signal that may notify a webhook is what the consoles
	// watching the wall are waiting for. The action on the row decides which of the
	// three messages goes out, and the extend is throttled inside the hub.
	h.broadcastEvent(event)

	return reply, ""
}

// recordModeForWindow answers which recording mode decides the recorder's window.
//
// The row wins. The signal carries a copy of the mode from the worker's last
// config poll, and the window is a property of the installation as it is now: a
// camera switched to `off` or back to `continuous` in the console stops opening
// windows on the next signal, whatever a worker still believes about it.
func recordModeForWindow(fromRow, fromSignal string) string {
	if models.ValidRecordMode(fromRow) {
		return fromRow
	}
	if models.ValidRecordMode(fromSignal) {
		return fromSignal
	}
	return models.RecordModeContinuous
}

// writeEventSnapshot decodes a signal's snapshot and files it at
// `<SNAPSHOTS_DIR>/<event_id>.jpg`, then records the path on the row.
//
// Over `MaxSignalSnapshotBytes` the picture is dropped and the event keeps
// standing. Every failure here is a log line, never a refusal: the event is the
// fact, and the snapshot is evidence attached to it.
func (h *Handlers) writeEventSnapshot(eventID, snapshotBase64 string) {
	raw, err := base64.StdEncoding.DecodeString(snapshotBase64)
	if err != nil {
		log.Printf("events: snapshot for %s is not valid base64: %v; the event is filed without it", eventID, err)
		return
	}
	if len(raw) > MaxSignalSnapshotBytes {
		log.Printf(
			"events: snapshot for %s is %d bytes, over the %d byte cap; stored as absent and the event survives",
			eventID, len(raw), MaxSignalSnapshotBytes,
		)
		return
	}

	// `Base` is not decoration: the name is built from the event id, and a name
	// that escaped SNAPSHOTS_DIR would turn the snapshot route (S6-02) into a
	// general file reader. The id is a server-minted UUID today; the guard is what
	// keeps it that way if a row is ever restored from elsewhere.
	path := filepath.Join(h.Cfg.SnapshotsDir, filepath.Base(eventID)+".jpg")
	if err := os.MkdirAll(h.Cfg.SnapshotsDir, 0o755); err != nil {
		log.Printf("events: snapshots dir %s could not be created: %v", h.Cfg.SnapshotsDir, err)
		return
	}
	// 0o640, like the recorder's segments: these are pictures of someone's
	// property, and the only reader is this process.
	if err := os.WriteFile(path, raw, 0o640); err != nil {
		log.Printf("events: snapshot for %s could not be written to %s: %v", eventID, path, err)
		return
	}
	if err := h.St.Events.SetLocalSnapshot(eventID, path); err != nil {
		// The file exists and the row does not point at it. The sweeper treats a
		// file no row claims as garbage, so this is a lost picture and not a leak.
		log.Printf("events: snapshot written for %s but the row could not record it: %v", eventID, err)
		return
	}
	// The picture is filed and named on the row, so it owes the tier a copy.
	// The queue is filled by whoever finalizes an object — recorder, clip
	// builder, and here.
	if _, err := h.St.EnqueueUpload(models.JobKindSnapshot, store.EventJobID(eventID, models.JobKindSnapshot), path); err != nil {
		log.Printf("events: snapshot for %s could not be queued for upload: %v", eventID, err)
	}
}
