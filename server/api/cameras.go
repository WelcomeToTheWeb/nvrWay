package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"nvr/server/go2rtc"
	"nvr/server/models"
	"nvr/server/store"
)

// The camera surface: the rows, the probe, the snapshot, and the one write path
// to go2rtc.
//
// Three rules shape this file.
//
//   - **The slug is validated on write, in the words the operator can act on.**
//     The slug is the go2rtc stream name, the RTSP path the recorder pulls, and
//     the segment file prefix. A slug that is not `^[a-z0-9][a-z0-9-]{1,31}$` is
//     a camera that records into a file nothing can name, so it is refused at the
//     door with the pattern in the message — and a collision is a 409, because
//     two rows on one slug record into each other.
//   - **Every write re-renders go2rtc's config** (S3-05). One path, so the
//     sidecar cannot drift from the rows: create, update, delete and the boot
//     sequence all call the same function, and a write whose config the sidecar
//     did not apply is answered as a failure, not as a success with a log line.
//   - **A PUT carries what it means.** `Store.Update` writes every mutable column
//     deliberately, so that a client omitting a column cannot zero it; the handler
//     therefore binds into pointers and merges onto the stored row. A PUT that
//     omits `remote_retention_days` keeps the retention the operator set, which is
//     the difference between a console bug and a camera whose footage is deleted.

// registerCameraRoutes mounts the camera surface on the router.
//
// The role floor is the plan's (Step 3): `viewer` reads cameras, events and
// clips; `operator` adds camera CRUD and the probe; `admin` adds the delete,
// because deleting a camera destroys its footage ledger.
func (h *Handlers) registerCameraRoutes(r *gin.Engine) {
	read := r.Group("/api/v1/cameras", AuthMiddleware(h.JWT))
	read.GET("", h.ListCameras)
	read.GET("/:id", h.GetCamera)
	read.GET("/:id/snapshot", h.CameraSnapshot)

	write := read.Group("", RequireAnyRole(h.St.Audit, models.RoleOperator, models.RoleAdmin))
	write.POST("", h.CreateCamera)
	write.PUT("/:id", h.UpdateCamera)
	write.PUT("/:id/zones", h.PutCameraZones)
	write.POST("/:id/probe", h.ProbeCamera)

	admin := read.Group("", RequireRole(models.RoleAdmin, h.St.Audit))
	admin.DELETE("/:id", h.DeleteCamera)
}

// Go2rtcBootReport is what the boot step has to say: the sidecar's state, how
// many streams it reported, and whether the rendered config landed.
type Go2rtcBootReport struct {
	Probed   bool   `json:"probed"`
	Applied  bool   `json:"applied"`
	Streams  int    `json:"streams"`
	Cameras  int    `json:"cameras"`
	Detected int    `json:"streams_detection_taps"`
	Reason   string `json:"reason"`
}

// BootstrapGo2rtc is the boot step (S3-05): probe the sidecar, then apply the
// config rendered from the camera rows.
//
// A failure is returned, never fatal. A server that refuses to boot because its
// media sidecar is down is a server you cannot use to find out which of the two
// is down — and the camera rows, the accounts and the API are all usable without
// go2rtc. The log line says what happened and what the installation is running
// as, and the camera write path re-applies on the next save.
//
// The probe comes first because it is the cheap question: it says whether the
// sidecar answers at all, before a whole-file config write spends its retries
// against a host that is not there.
func (h *Handlers) BootstrapGo2rtc(ctx context.Context) (*Go2rtcBootReport, error) {
	report := &Go2rtcBootReport{}
	if h.Go2rtc == nil {
		report.Reason = "no go2rtc sidecar is configured (GO2RTC_URL)"
		return report, errors.New(report.Reason)
	}

	if streams, err := h.Go2rtc.Streams(ctx); err != nil {
		report.Reason = fmt.Sprintf("go2rtc did not answer the probe: %v", err)
	} else {
		report.Probed = true
		report.Streams = len(streams)
		for name := range streams {
			if strings.HasSuffix(name, go2rtc.DetectionSuffix) {
				report.Detected++
			}
		}
	}

	cameras, err := h.St.Cameras.ListActive()
	if err != nil {
		report.Reason = fmt.Sprintf("the camera rows are unreadable: %v", err)
		return report, errors.New(report.Reason)
	}
	report.Cameras = len(cameras)

	if err := h.ApplyCameraConfig(ctx); err != nil {
		if report.Reason != "" {
			report.Reason = fmt.Sprintf("%s; config not applied: %v", report.Reason, err)
		} else {
			report.Reason = fmt.Sprintf("config not applied: %v", err)
		}
		return report, errors.New(report.Reason)
	}
	report.Applied = true
	return report, nil
}

// cameraView is a camera row as the console reads it. `zones` is the parsed rule
// set, not the JSON text: the console draws polygons, and a client that has to
// parse a string column to draw one is a contract with two shapes for one thing.
type cameraView struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	SourceURI string `json:"source_uri"`

	RecordMode       string `json:"record_mode"`
	DetectionEnabled bool   `json:"detection_enabled"`
	DetectionFPS     int    `json:"detection_fps"`
	DetectionWidth   int    `json:"detection_width"`
	DetectionHeight  int    `json:"detection_height"`

	Sensitivity  float64 `json:"sensitivity"`
	MinAreaRatio float64 `json:"min_area_ratio"`

	Zones []models.Zone `json:"zones"`

	LocalRetentionDays  int  `json:"local_retention_days"`
	RemoteRetentionDays int  `json:"remote_retention_days"`
	Active              bool `json:"active"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (h *Handlers) cameraView(camera *models.Camera) cameraView {
	zones, err := models.ParseZones(camera.ZonesJSON)
	if err != nil {
		// A row whose zones text does not parse is a row the zone editor cannot
		// draw. Answer the cameras it does have, and say nothing about the zones:
		// the write path validates, so this is a damaged row, not a request.
		zones = []models.Zone{}
	}
	return cameraView{
		ID:                  camera.ID,
		Slug:                camera.Slug,
		Name:                camera.Name,
		SourceURI:           camera.SourceURI,
		RecordMode:          camera.RecordMode,
		DetectionEnabled:    camera.DetectionEnabled,
		DetectionFPS:        camera.DetectionFPS,
		DetectionWidth:      camera.DetectionWidth,
		DetectionHeight:     camera.DetectionHeight,
		Sensitivity:         camera.Sensitivity,
		MinAreaRatio:        camera.MinAreaRatio,
		Zones:               zones,
		LocalRetentionDays:  camera.LocalRetentionDays,
		RemoteRetentionDays: camera.RemoteRetentionDays,
		Active:              camera.Active,
		CreatedAt:           camera.CreatedAt.Format(timeLayout),
		UpdatedAt:           camera.UpdatedAt.Format(timeLayout),
	}
}

// cameraInput is the write payload, every field a pointer: an omitted field is
// absent, not zero. See the file comment.
type cameraInput struct {
	Slug                *string        `json:"slug"`
	Name                *string        `json:"name"`
	SourceURI           *string        `json:"source_uri"`
	RecordMode          *string        `json:"record_mode"`
	DetectionEnabled    *bool          `json:"detection_enabled"`
	DetectionFPS        *int           `json:"detection_fps"`
	DetectionWidth      *int           `json:"detection_width"`
	DetectionHeight     *int           `json:"detection_height"`
	Sensitivity         *float64       `json:"sensitivity"`
	MinAreaRatio        *float64       `json:"min_area_ratio"`
	Zones               *[]models.Zone `json:"zones"`
	LocalRetentionDays  *int           `json:"local_retention_days"`
	RemoteRetentionDays *int           `json:"remote_retention_days"`
	Active              *bool          `json:"active"`
}

// ListCameras answers every camera, newest first.
//
// GET /api/v1/cameras
func (h *Handlers) ListCameras(c *gin.Context) {
	cameras, err := h.St.Cameras.ListAll()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "cameras are unreadable")
		return
	}
	out := make([]cameraView, 0, len(cameras))
	for i := range cameras {
		out = append(out, h.cameraView(&cameras[i]))
	}
	// The list is an object, not a bare array: every collection this API answers
	// is `{"<thing>": [...]}` (users, roles, backends), so a console reads one
	// shape for "here is the list" and the response can grow a total later
	// without becoming a different endpoint.
	c.JSON(http.StatusOK, gin.H{"cameras": out})
}

// GetCamera answers one camera.
//
// GET /api/v1/cameras/:id
func (h *Handlers) GetCamera(c *gin.Context) {
	camera, ok := h.cameraByID(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.cameraView(camera))
}

// CreateCamera adds a camera, then re-renders go2rtc's config.
//
// POST /api/v1/cameras (operator, admin)
func (h *Handlers) CreateCamera(c *gin.Context) {
	var in cameraInput
	if err := c.BindJSON(&in); err != nil {
		jsonError(c, http.StatusBadRequest, "the camera body is not JSON this route understands")
		return
	}

	camera := &models.Camera{
		RecordMode:   models.RecordModeContinuous,
		Sensitivity:  0.45,
		MinAreaRatio: 0.004,
		Active:       true,
		ZonesJSON:    "[]",
	}
	applyCameraInput(camera, &in)

	if err := h.validateCamera(camera); err != nil {
		jsonError(c, http.StatusUnprocessableEntity, err.Error())
		return
	}

	if err := h.St.Cameras.Create(camera); err != nil {
		if errors.Is(err, store.ErrSlugTaken) {
			h.audit(c, models.AuditCameraCreate, http.StatusConflict,
				AuditTarget{Type: "camera", ID: ""}, map[string]any{"slug": camera.Slug})
			jsonError(c, http.StatusConflict, fmt.Sprintf("slug taken: %s", camera.Slug))
			return
		}
		jsonError(c, http.StatusInternalServerError, "the camera row could not be written")
		return
	}

	h.audit(c, models.AuditCameraCreate, http.StatusOK,
		AuditTarget{Type: "camera", ID: camera.ID}, map[string]any{
			"slug":              camera.Slug,
			"record_mode":       camera.RecordMode,
			"detection_enabled": camera.DetectionEnabled,
		})

	// The config write comes after the row and after the ledger row: the rows are
	// the truth, and the sidecar is a projection of them. A camera that is in the
	// database and not in go2rtc is a camera whose absence from the sidecar is
	// reportable; the reverse is a stream nobody owns.
	if err := h.ApplyCameraConfig(c.Request.Context()); err != nil {
		h.reportConfigFailure(c, http.StatusCreated, camera, err)
		return
	}
	h.broadcastCamera(c.Request.Context(), camera)

	c.JSON(http.StatusCreated, h.cameraView(camera))
}

// UpdateCamera replaces a camera row, keeping the fields the body omits.
//
// PUT /api/v1/cameras/:id (operator, admin)
func (h *Handlers) UpdateCamera(c *gin.Context) {
	camera, ok := h.cameraByID(c)
	if !ok {
		return
	}
	before := *camera

	var in cameraInput
	if err := c.BindJSON(&in); err != nil {
		jsonError(c, http.StatusBadRequest, "the camera body is not JSON this route understands")
		return
	}
	applyCameraInput(camera, &in)

	if err := h.validateCamera(camera); err != nil {
		jsonError(c, http.StatusUnprocessableEntity, err.Error())
		return
	}

	if err := h.St.Cameras.Update(camera); err != nil {
		if errors.Is(err, store.ErrSlugTaken) {
			h.audit(c, models.AuditCameraUpdate, http.StatusConflict,
				AuditTarget{Type: "camera", ID: camera.ID}, map[string]any{"slug": camera.Slug})
			jsonError(c, http.StatusConflict, fmt.Sprintf("slug taken: %s", camera.Slug))
			return
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no camera with that id")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the camera row could not be written")
		return
	}

	fresh, err := h.St.Cameras.GetByID(camera.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the camera row is unreadable after the write")
		return
	}

	h.audit(c, models.AuditCameraUpdate, http.StatusOK,
		AuditTarget{Type: "camera", ID: camera.ID}, map[string]any{
			"slug_before":       before.Slug,
			"slug":              fresh.Slug,
			"record_mode":       fresh.RecordMode,
			"detection_enabled": fresh.DetectionEnabled,
		})

	if err := h.ApplyCameraConfig(c.Request.Context()); err != nil {
		h.reportConfigFailure(c, http.StatusOK, fresh, err)
		return
	}
	h.broadcastCamera(c.Request.Context(), fresh)

	c.JSON(http.StatusOK, h.cameraView(fresh))
}

// ProbeCamera answers what go2rtc says about this camera right now.
//
// POST /api/v1/cameras/:id/probe (operator, admin) → `{"online": bool, "producers": [...]}`
//
// The answer comes from the sidecar's live stream map, never from the row: a
// camera row always says it exists. `producers` is go2rtc's own words for what is
// feeding the stream, which is what tells an operator the difference between a
// camera that is not on the network and a camera whose RTSP credentials are wrong.
func (h *Handlers) ProbeCamera(c *gin.Context) {
	camera, ok := h.cameraByID(c)
	if !ok {
		return
	}
	online, producers, err := h.probeSlug(c.Request.Context(), camera.Slug)
	if err != nil {
		h.audit(c, models.AuditCameraProbe, http.StatusBadGateway,
			AuditTarget{Type: "camera", ID: camera.ID}, map[string]any{"slug": camera.Slug, "reason": err.Error()})
		jsonError(c, http.StatusBadGateway, fmt.Sprintf("go2rtc did not answer the probe: %v", err))
		return
	}
	h.audit(c, models.AuditCameraProbe, http.StatusOK,
		AuditTarget{Type: "camera", ID: camera.ID}, map[string]any{
			"slug":      camera.Slug,
			"online":    online,
			"producers": len(producers),
		})
	c.JSON(http.StatusOK, gin.H{
		"camera_id": camera.ID,
		"slug":      camera.Slug,
		"online":    online,
		"producers": producers,
	})
}

// CameraSnapshot proxies one JPEG from go2rtc.
//
// GET /api/v1/cameras/:id/snapshot → image/jpeg
//
// `cache=5s` and `width=640` are the plan's: the zone editor draws polygons over
// this frame, so it wants a small frame and a short freshness window, and go2rtc's
// own cache is what stops a console polling snapshots from pinning a camera's slow
// producer. `Cache-Control: private, max-age=5` is the same promise made to the
// browser, so a polling console and a shared proxy cannot each add a cache layer
// longer than the frame's freshness — and `private` because the frame shows a
// person's property to whoever holds a console session.
func (h *Handlers) CameraSnapshot(c *gin.Context) {
	camera, ok := h.cameraByID(c)
	if !ok {
		return
	}
	if h.Go2rtc == nil {
		jsonError(c, http.StatusServiceUnavailable, "no go2rtc sidecar is configured")
		return
	}
	frame, err := h.Go2rtc.Frame(c.Request.Context(), camera.Slug, snapshotWidth, snapshotCache)
	if err != nil {
		if errors.Is(err, go2rtc.ErrStreamRejected) {
			// The sidecar answered: the camera is not producing. This is not a
			// server fault, and 404 is the answer a console can draw ("no snapshot
			// yet") without reading it as an outage.
			jsonError(c, http.StatusNotFound, fmt.Sprintf("no snapshot from %s: %v", camera.Slug, err))
			return
		}
		jsonError(c, http.StatusBadGateway, fmt.Sprintf("go2rtc did not answer the snapshot: %v", err))
		return
	}
	c.Header("Cache-Control", "private, max-age=5")
	c.Header("Content-Type", "image/jpeg")
	c.Data(http.StatusOK, "image/jpeg", frame)
}

// The snapshot's shape, the plan's: a 640px frame, cached for 5 seconds.
const (
	snapshotWidth = 640
	snapshotCache = "5s"
)

// DeleteCamera removes a camera, its footage rows, its local files, and queues
// the remote copies for deletion.
//
// DELETE /api/v1/cameras/:id (admin)
//
// The order is the one that cannot lose data:
//
//  1. queue the remote purge jobs, because a job carries its own target key and
//     backend, and once the rows are gone the queue is the only record that those
//     objects ever existed. An uploaded segment with no row and no job is a file
//     on someone's S3 bucket costing money with nobody pointing at it.
//  2. delete the local files,
//  3. delete the rows — events, then segments, then the camera,
//  4. re-render go2rtc's config, so the deleted camera's stream stops being pulled.
//
// `Store.Cameras.Delete` deliberately does not touch media: the handler owns the
// order, because a camera deleted while its rows are still referenced by events
// loses the events' media bookkeeping.
func (h *Handlers) DeleteCamera(c *gin.Context) {
	camera, ok := h.cameraByID(c)
	if !ok {
		return
	}

	segments, err := h.allSegmentsFor(camera.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the camera's segments are unreadable")
		return
	}
	events, err := h.allEventsFor(camera.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the camera's events are unreadable")
		return
	}

	queued, queueErr := h.enqueueCameraPurge(c.Request.Context(), camera, segments, events)

	local := h.removeLocalCameraMedia(camera, segments, events)

	segmentIDs := make([]string, 0, len(segments))
	for _, seg := range segments {
		segmentIDs = append(segmentIDs, seg.ID)
	}
	if _, err := h.St.Segments.DeleteByIDs(segmentIDs); err != nil {
		jsonError(c, http.StatusInternalServerError, "the segment rows could not be deleted")
		return
	}
	for _, event := range events {
		if err := h.St.Events.Delete(event.ID); err != nil {
			jsonError(c, http.StatusInternalServerError, "the event rows could not be deleted")
			return
		}
	}
	if err := h.St.Cameras.Delete(camera.ID); err != nil {
		jsonError(c, http.StatusInternalServerError, "the camera row could not be deleted")
		return
	}

	h.audit(c, models.AuditCameraDelete, http.StatusOK,
		AuditTarget{Type: "camera", ID: camera.ID}, map[string]any{
			"slug":           camera.Slug,
			"segments":       len(segments),
			"events":         len(events),
			"local_files":    local,
			"remote_jobs":    queued,
			"queue_warnings": queueErr,
		})

	if err := h.ApplyCameraConfig(c.Request.Context()); err != nil {
		// The row is gone: the deletion is not undone by a sidecar that will not
		// take the config, and boot re-applies. Say it, do not fail the delete.
		h.broadcastCameraStatus(c.Request.Context(), camera.ID, camera.Slug, false, []string{})
		c.JSON(http.StatusOK, gin.H{
			"deleted":        camera.ID,
			"slug":           camera.Slug,
			"segments":       len(segments),
			"events":         len(events),
			"local_files":    local,
			"remote_jobs":    queued,
			"config_applied": false,
			"config_warning": err.Error(),
		})
		return
	}
	h.broadcastCameraStatus(c.Request.Context(), camera.ID, camera.Slug, false, []string{})

	c.JSON(http.StatusOK, gin.H{
		"deleted":        camera.ID,
		"slug":           camera.Slug,
		"segments":       len(segments),
		"events":         len(events),
		"local_files":    local,
		"remote_jobs":    queued,
		"config_applied": true,
	})
}

// enqueueCameraPurge queues the remote deletion of everything this camera's
// footage reached on the tier: uploaded segments, uploaded snapshots, uploaded
// clips.
//
// The queue is per-object, and a job names its own backend and target, so these
// rows can be deleted right after this and the jobs still say what to remove.
// With no storage backend configured there is nothing remote to purge — that is
// a local-only installation, and it is not an error.
func (h *Handlers) enqueueCameraPurge(ctx context.Context, camera *models.Camera,
	segments []models.Segment, events []models.Event) (int, string) {

	backend, err := h.St.StorageBackends.GetDefault()
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, "no storage tier is configured: nothing remote to purge"
		}
		return 0, fmt.Sprintf("the storage tier is unreadable: %v", err)
	}

	queued := 0
	var problems []string
	for _, seg := range segments {
		if !seg.Uploaded() {
			continue
		}
		target := seg.RemoteKey
		if target == "" {
			target = filepath.Base(seg.Path)
		}
		if err := h.St.StorageJobs.Enqueue(store.JobInput{
			ID:        seg.ID,
			Kind:      models.JobKindSegment,
			Target:    target,
			BackendID: backend.ID,
			Action:    models.JobActionDeleteRemote,
			Bytes:     seg.Bytes,
		}); err != nil {
			problems = append(problems, fmt.Sprintf("segment %s: %v", seg.ID, err))
			continue
		}
		queued++
	}

	for _, event := range events {
		if event.SnapshotUploadedAt != nil {
			target := event.SnapshotKey
			if target == "" {
				target = filepath.Base(event.LocalSnapshotPath)
			}
			if err := h.St.StorageJobs.Enqueue(store.JobInput{
				ID:        event.ID + ":snapshot",
				Kind:      models.JobKindSnapshot,
				Target:    target,
				BackendID: backend.ID,
				Action:    models.JobActionDeleteRemote,
			}); err != nil {
				problems = append(problems, fmt.Sprintf("snapshot %s: %v", event.ID, err))
			} else {
				queued++
			}
		}
		if event.ClipUploadedAt != nil {
			target := event.ClipKey
			if target == "" {
				target = filepath.Base(event.LocalClipPath)
			}
			if err := h.St.StorageJobs.Enqueue(store.JobInput{
				ID:        event.ID + ":clip",
				Kind:      models.JobKindClip,
				Target:    target,
				BackendID: backend.ID,
				Action:    models.JobActionDeleteRemote,
			}); err != nil {
				problems = append(problems, fmt.Sprintf("clip %s: %v", event.ID, err))
			} else {
				queued++
			}
		}
	}

	return queued, strings.Join(problems, "; ")
}

// removeLocalCameraMedia deletes this camera's files under the media tree.
//
// Two passes, deliberately. The segment rows name the files the database knows
// about; the slug sweep catches the ones it does not — a segment ffmpeg wrote and
// the CSV never reported, the CSV itself, a segment written after a crash. A
// camera deleted with files still under SEGMENTS_DIR is disk that nothing will
// ever reclaim, because the sweeper walks rows, not directories.
//
// Only files whose name starts with the slug are touched. The slug is
// `[a-z0-9][a-z0-9-]{1,31}`, so it cannot contain a path separator, and matching
// by prefix is what keeps this from becoming a recursive delete over a directory
// shared with other cameras.
func (h *Handlers) removeLocalCameraMedia(camera *models.Camera, segments []models.Segment, events []models.Event) int {
	removed := 0
	prefixes := []string{camera.Slug + "-", camera.Slug + ".csv", camera.Slug + "_"}

	for _, seg := range segments {
		if seg.Path == "" {
			continue
		}
		if err := os.Remove(seg.Path); err == nil {
			removed++
		}
	}

	for _, dir := range []string{h.Cfg.SegmentsDir, h.Cfg.ClipsDir, h.Cfg.SnapshotsDir} {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			for _, prefix := range prefixes {
				if strings.HasPrefix(name, prefix) {
					if err := os.Remove(filepath.Join(dir, name)); err == nil {
						removed++
					}
					break
				}
			}
		}
	}

	for _, event := range events {
		for _, path := range []string{event.LocalSnapshotPath, event.LocalClipPath} {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err == nil {
				removed++
			}
		}
	}
	return removed
}

// ApplyCameraConfig re-renders go2rtc's config from the active camera rows and
// applies it. S3-05: this is the only path that writes the sidecar's config, and
// the boot sequence calls this same function.
func (h *Handlers) ApplyCameraConfig(ctx context.Context) error {
	if h.Go2rtc == nil {
		return errors.New("no go2rtc sidecar is configured (GO2RTC_URL)")
	}
	cameras, err := h.St.Cameras.ListActive()
	if err != nil {
		return fmt.Errorf("the camera rows are unreadable: %w", err)
	}
	yml, err := go2rtc.RenderConfig(cameras, h.Cfg)
	if err != nil {
		return err
	}
	return h.Go2rtc.ApplyConfig(ctx, yml)
}

// reportConfigFailure answers a write whose rows landed and whose config did not.
//
// The row is not rolled back: the recorder and the worker poll rows, and boot
// re-applies the config, so a saved row with an unapplied config heals. What it
// must not do is report success — a 200 here is an operator who believes the
// camera is live. The status is 502 and the body says what is true: the camera is
// saved, the sidecar did not take the config, and why.
func (h *Handlers) reportConfigFailure(c *gin.Context, savedStatus int, camera *models.Camera, err error) {
	_ = savedStatus
	c.JSON(http.StatusBadGateway, gin.H{
		"error":          fmt.Sprintf("camera saved, go2rtc config not applied: %v", err),
		"camera":         h.cameraView(camera),
		"config_applied": false,
	})
}

// probeSlug reads one stream's live state out of the sidecar's stream map.
func (h *Handlers) probeSlug(ctx context.Context, slug string) (bool, []string, error) {
	if h.Go2rtc == nil {
		return false, nil, errors.New("no go2rtc sidecar is configured (GO2RTC_URL)")
	}
	streams, err := h.Go2rtc.Streams(ctx)
	if err != nil {
		return false, nil, err
	}
	info, present := streams[slug]
	if !present {
		// Not in the map at all: the config the sidecar is running does not
		// contain this camera, which is the drift S3-05 exists to prevent, and it
		// is a different fact from "configured and not connecting".
		return false, []string{}, nil
	}
	producers := info.Producers
	if producers == nil {
		producers = []string{}
	}
	return info.Online(), producers, nil
}

// broadcastCamera reports this camera's live state to the console, from the same
// probe the config confirm already makes. A camera write that does not move the
// tile is a tile that lies for the next 30 seconds.
func (h *Handlers) broadcastCamera(ctx context.Context, camera *models.Camera) {
	online, producers, err := h.probeSlug(ctx, camera.Slug)
	if err != nil {
		online, producers = false, []string{}
	}
	h.broadcastCameraStatus(ctx, camera.ID, camera.Slug, online, producers)
}

func (h *Handlers) broadcastCameraStatus(ctx context.Context, cameraID, slug string, online bool, producers []string) {
	if h.Hub == nil {
		return
	}
	_ = h.Hub.CameraStatus(map[string]any{
		"camera_id": cameraID,
		"slug":      slug,
		"online":    online,
		"producers": producers,
	})
}

// cameraByID resolves :id, answering the request and returning false when there
// is no such camera.
func (h *Handlers) cameraByID(c *gin.Context) (*models.Camera, bool) {
	id := strings.TrimSpace(c.Param("id"))
	camera, err := h.St.Cameras.GetByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no camera with that id")
			return nil, false
		}
		jsonError(c, http.StatusInternalServerError, "the camera row is unreadable")
		return nil, false
	}
	return camera, true
}

// applyCameraInput copies the payload onto a row, skipping absent fields.
func applyCameraInput(camera *models.Camera, in *cameraInput) {
	if in.Slug != nil {
		camera.Slug = strings.TrimSpace(*in.Slug)
	}
	if in.Name != nil {
		camera.Name = strings.TrimSpace(*in.Name)
	}
	if in.SourceURI != nil {
		camera.SourceURI = strings.TrimSpace(*in.SourceURI)
	}
	if in.RecordMode != nil {
		camera.RecordMode = strings.TrimSpace(*in.RecordMode)
	}
	if in.DetectionEnabled != nil {
		camera.DetectionEnabled = *in.DetectionEnabled
	}
	if in.DetectionFPS != nil {
		camera.DetectionFPS = *in.DetectionFPS
	}
	if in.DetectionWidth != nil {
		camera.DetectionWidth = *in.DetectionWidth
	}
	if in.DetectionHeight != nil {
		camera.DetectionHeight = *in.DetectionHeight
	}
	if in.Sensitivity != nil {
		camera.Sensitivity = *in.Sensitivity
	}
	if in.MinAreaRatio != nil {
		camera.MinAreaRatio = *in.MinAreaRatio
	}
	if in.LocalRetentionDays != nil {
		camera.LocalRetentionDays = *in.LocalRetentionDays
	}
	if in.RemoteRetentionDays != nil {
		camera.RemoteRetentionDays = *in.RemoteRetentionDays
	}
	if in.Active != nil {
		camera.Active = *in.Active
	}
	if in.Zones != nil {
		encoded, err := models.ZonesToJSON(*in.Zones)
		if err == nil {
			camera.ZonesJSON = encoded
		}
	}
}

// validateCamera checks a row against the contract before it can be written: the
// slug pattern, the recording mode, the numbers, the zones — and, at the end, the
// row run through the renderer.
//
// The renderer check is the one that saves an operator a day of staring at a dead
// camera: a row whose detection tap go2rtc cannot open (a frame rate of zero, an
// unusable size) is a camera that is configured, green in the tile, and detecting
// nothing forever. Asking the renderer at write time turns that into a 422 naming
// the knob to turn.
func (h *Handlers) validateCamera(camera *models.Camera) error {
	if !models.ValidSlug(camera.Slug) {
		return fmt.Errorf("slug %q is not a usable stream name (^[a-z0-9][a-z0-9-]{1,31}$)", camera.Slug)
	}
	if strings.TrimSpace(camera.Name) == "" {
		return errors.New("name is required")
	}
	if strings.TrimSpace(camera.SourceURI) == "" {
		return errors.New("source_uri is required")
	}
	if !models.ValidRecordMode(camera.RecordMode) {
		return fmt.Errorf("record_mode %q is not a recording mode (continuous, events, off)", camera.RecordMode)
	}
	if camera.DetectionFPS < 0 {
		return errors.New("detection_fps cannot be negative")
	}
	if camera.DetectionWidth < 0 || camera.DetectionHeight < 0 {
		return errors.New("detection_width and detection_height cannot be negative")
	}
	if camera.Sensitivity < 0 || camera.Sensitivity > 1 {
		return errors.New("sensitivity must be 0..1")
	}
	if camera.MinAreaRatio < 0 || camera.MinAreaRatio > 1 {
		return errors.New("min_area_ratio must be 0..1")
	}
	if camera.LocalRetentionDays < 0 || camera.RemoteRetentionDays < 0 {
		return errors.New("retention days cannot be negative")
	}
	if err := models.ValidateZonesJSON(camera.ZonesJSON); err != nil {
		return err
	}

	rendered, err := go2rtc.RenderConfig([]models.Camera{*camera}, h.Cfg)
	if err != nil {
		return err
	}
	if len(rendered) == 0 {
		return errors.New("the camera row renders no stream")
	}
	return nil
}

// allSegmentsFor walks a camera's segments in pages: a camera recorded for a month
// has thousands, and a delete that silently only saw the first page would leave
// the rest as rows on a camera that no longer exists.
func (h *Handlers) allSegmentsFor(cameraID string) ([]models.Segment, error) {
	var (
		all  []models.Segment
		seen = map[string]bool{}
	)
	for page := 0; page < 50; page++ {
		batch, err := h.St.Segments.ListRecentByCamera(cameraID, store.SegmentListMaxLimit)
		if err != nil {
			return nil, err
		}
		fresh := 0
		for _, seg := range batch {
			if seen[seg.ID] {
				continue
			}
			seen[seg.ID] = true
			all = append(all, seg)
			fresh++
		}
		if fresh == 0 {
			break
		}
	}
	return all, nil
}

// allEventsFor walks a camera's events in pages, for the same reason.
func (h *Handlers) allEventsFor(cameraID string) ([]models.Event, error) {
	var all []models.Event
	for offset := 0; offset < 10000; offset += store.EventListMaxLimit {
		batch, err := h.St.Events.List(store.EventFilter{CameraID: cameraID}, offset)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
	}
	return all, nil
}
