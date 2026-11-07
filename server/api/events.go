// The event surface (S6-02, S6-04): the list, the detail, the picture, the
// clip, and the one act an operator can take on an event — deleting it.
//
// Two rules drive the shape of everything here.
//
// **The event is the fact and its media is evidence attached to it.** An event
// with no snapshot is a normal outcome — a worker that could not encode a
// frame, a picture over the cap, a disk that refused the write — and an event
// with no clip is a normal outcome too: a camera in `record_mode=off` produces
// exactly that, and so does a window that held no segments. Neither turns the
// event into a 404. The event answers, and the media route that has nothing
// answers a 404 with a name on it.
//
// **The row stores ids; the person reading the list knows names.** The event
// row accumulates zone *ids* (the id is what the worker's signal carries and
// what survives a zone being redrawn), while the console draws zone chips and an
// operator types "porch" into a search box. The camera row holds the pair, so
// this file resolves it in both directions: ids to names on the way out, names
// to ids for `q` on the way in. A zone that has since been deleted falls back to
// its id, which is ugly and true — the event did happen in a zone that no longer
// has a name.
//
// The delete is the only act on the surface, so it is the only route above
// `viewer`, and the only one that writes a ledger row. Reading events is what a
// viewer is for.

package api

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"nvr/server/models"
	"nvr/server/storage"
	"nvr/server/store"
)

// registerEventRoutes mounts the event surface.
//
// The read group carries only `AuthMiddleware(h.JWT)`, exactly like the camera
// and segment reads: every authenticated role reads events (a viewer's whole
// job is to look), and a read is not an act — there is no ledger row for it, and
// a 401 here has no actor to name in one.
//
// The delete sits above it on `admin`, not `operator`. An operator can change
// what is recorded; only an admin removes footage that has already been
// recorded, and the row is what the storage tier's purge jobs and the ledger
// point at afterwards.
func (h *Handlers) registerEventRoutes(r *gin.Engine) {
	read := r.Group("/api/v1/events", AuthMiddleware(h.JWT))
	read.GET("", h.ListEvents)
	read.GET("/:id", h.GetEvent)
	read.GET("/:id/snapshot", h.EventSnapshot)
	read.GET("/:id/clip", h.EventClip)

	admin := read.Group("", RequireRole(models.RoleAdmin, h.St.Audit))
	admin.DELETE("/:id", h.DeleteEvent)
}

// EventSnapshot serves the frame an event was detected on.
//
// GET /api/v1/events/:id/snapshot → image/jpeg
//
// The bytes are the file the worker's `open` signal wrote, at
// `<SNAPSHOTS_DIR>/<event_id>.jpg`, and the row's `local_snapshot_path` is the
// only thing that says where it is: the console never gets a path, playback is
// server-proxied, and a name on this server's disk is not a name the browser can
// dial. When that file is gone and the row says the picture reached the storage
// tier, the route streams it from the tier instead (S6-05) — the same order the
// clip route uses, so the two cannot disagree inside one drawer.
//
// The answers a caller can get are each named, because the console draws each one
// differently:
//
//	200 image/jpeg                            the picture
//	404 {"error":"event not found"}           no such event
//	404 {"error":"snapshot not available"}    the event stands, without a picture
//	502 {"error":"…","tier":"<name>"}         the tier is there and not usable
//
// The 404 is the interesting case and it is not an error the operator has to fix:
// it is the state an event enters when its evidence is absent or aged out, and the
// plan's rule for the clip ("the UI shows a named 404 for events whose media is
// gone") is applied to the snapshot identically.
//
// `Cache-Control: private, max-age=3600` is safe because an event's picture is
// written once and never rewritten — a replayed `open` fills a missing one and
// overwrites nothing — and `private` because the frame shows a person's property
// to whoever holds a console session.
func (h *Handlers) EventSnapshot(c *gin.Context) {
	event, ok := h.eventByID(c)
	if !ok {
		return
	}
	if h.serveLocalSnapshot(c, event) {
		return
	}
	if event.SnapshotUploaded() && h.streamFromTier(c, event.SnapshotKey, "image/jpeg") {
		return
	}
	jsonError(c, http.StatusNotFound, "snapshot not available")
}

// serveLocalSnapshot is the clip route's counterpart over `SNAPSHOTS_DIR`: it
// writes the response when the row names a readable file, and returns false when
// it did not, so the tier gets its turn.
//
// The tier branch is not in this card's exit criteria, and it is here anyway for
// a reason: an event whose footage aged out locally still has its picture on the
// tier, and a drawer that plays a clip around a broken `<img>` is a bug the
// console cannot fix from the event view alone. The two media routes resolve
// "where are the bytes" the same way, or they disagree in the same drawer.
func (h *Handlers) serveLocalSnapshot(c *gin.Context, event *models.Event) bool {
	if event.LocalSnapshotPath == "" {
		// The absent-snapshot case, filed that way on purpose by the signal path
		// (no snapshot on the signal, over the cap, or a write that failed). The
		// event is still an event.
		return false
	}
	if !h.snapshotIsInPlace(event.LocalSnapshotPath) {
		// The row names a file this server will not read: outside SNAPSHOTS_DIR
		// (a row restored from another installation), or unreadable. Serving it
		// anyway would make the route a general file reader, and the log says
		// which of the two happened.
		log.Printf("events: snapshot for %s names %s, which is not inside %s; trying the tier",
			event.ID, event.LocalSnapshotPath, h.Cfg.SnapshotsDir)
		return false
	}

	f, err := os.Open(event.LocalSnapshotPath)
	if err != nil {
		// The row says the picture is here and the disk says otherwise: the
		// sweeper's local delete (S7-09) or a volume that came back without it.
		// The event survives its media, so the tier gets a turn and then this is a
		// named 404 and not a 500.
		log.Printf("events: snapshot for %s could not be opened at %s: %v", event.ID, event.LocalSnapshotPath, err)
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		log.Printf("events: snapshot for %s could not be stat'd at %s: %v", event.ID, event.LocalSnapshotPath, err)
		return false
	}

	c.Header("Cache-Control", "private, max-age=3600")
	// ServeContent, not ServeFile: the explicit type is what `docs/api.md`
	// promises (`image/jpeg`), and it is the response that answers a Range
	// request and a conditional GET, which a console redrawing a detail drawer
	// sends without being clever about it.
	http.ServeContent(c.Writer, c.Request, filepath.Base(event.LocalSnapshotPath), info.ModTime(), f)
	return true
}

// snapshotIsInPlace reports that a row's snapshot path is a file this server is
// willing to read: under SNAPSHOTS_DIR, after both sides are made absolute and
// cleaned. The write path builds the name with `filepath.Base(event_id)` for the
// same reason; this is the read half of that guard.
func (h *Handlers) snapshotIsInPlace(path string) bool {
	return fileIsUnder(h.Cfg.SnapshotsDir, path)
}

// clipIsInPlace is the read half of the same guard on CLIPS_DIR.
func (h *Handlers) clipIsInPlace(path string) bool {
	return fileIsUnder(h.Cfg.ClipsDir, path)
}

// fileIsUnder reports that `path` sits inside `dir`, once both are made absolute
// and cleaned. Both media write paths build the name from the event id, so a row
// that names a path outside its directory came from another installation, and
// serving it anyway would turn a media route into a general file reader.
func fileIsUnder(dir, path string) bool {
	base, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	return strings.HasPrefix(target, base+string(filepath.Separator))
}

// -- the list and the detail (S6-04) --------------------------------------

// cameraRefs is what the event surface needs from the camera rows: the slug to
// label a row with, and the zone id ↔ name pair the event row does not carry.
type cameraRefs struct {
	slug string
	// zoneNames maps a zone id to the name the operator gave it.
	zoneNames map[string]string
	// zoneIDs maps a lower-cased zone name to the ids that carry it. Two cameras
	// can each have a zone called "porch", and one camera can hold two, so it is
	// a list of ids rather than one id.
	zoneIDs map[string][]string
}

// refsOf decodes one camera row into the pair the event surface reads.
func refsOf(camera *models.Camera) *cameraRefs {
	out := &cameraRefs{
		slug:      camera.Slug,
		zoneNames: map[string]string{},
		zoneIDs:   map[string][]string{},
	}
	zones, err := models.ParseZones(camera.ZonesJSON)
	if err != nil {
		// A row whose zones_json this server cannot decode still has footage, and
		// still has events; its zones are simply answered by id.
		log.Printf("events: camera %s has a zones_json this server cannot decode (%v); its zones are answered by id", camera.Slug, err)
		return out
	}
	for _, z := range zones {
		if z.ID == "" {
			continue
		}
		out.zoneNames[z.ID] = z.Name
		if key := strings.ToLower(strings.TrimSpace(z.Name)); key != "" {
			out.zoneIDs[key] = append(out.zoneIDs[key], z.ID)
		}
	}
	return out
}

// cameraRefsFor loads the cameras an event query can name: the one row when the
// request named a camera, the table when it did not. A `camera_id` that names no
// row answers an empty map, and the handler turns that into the 404 — the same
// split the segment manifest draws between "no camera" and "no events".
func (h *Handlers) cameraRefsFor(cameraID string) (map[string]*cameraRefs, error) {
	if cameraID != "" {
		camera, err := h.St.Cameras.GetByID(cameraID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return map[string]*cameraRefs{}, nil
			}
			return nil, err
		}
		return map[string]*cameraRefs{camera.ID: refsOf(camera)}, nil
	}
	cameras, err := h.St.Cameras.ListAll()
	if err != nil {
		return nil, err
	}
	out := make(map[string]*cameraRefs, len(cameras))
	for i := range cameras {
		out[cameras[i].ID] = refsOf(&cameras[i])
	}
	return out, nil
}

// MaxQZoneIDs caps how many zone ids one `q` may resolve to. The cap bounds the
// query, not the answer: a search for "a" across a hundred cameras names more
// zones than anyone is looking for, and an unbounded OR list is a statement that
// grows with the number of zones drawn.
const MaxQZoneIDs = 32

// zoneIDsForName resolves a search word to the ids of the zones whose **name**
// contains it.
//
// The match runs in Go rather than as a LIKE against the camera rows for one
// reason: sqlite's LIKE is case-insensitive for ASCII and Postgres' is not, so a
// search that finds "Porch" on the test database and not on the deployment is a
// bug nobody can reproduce. The row's own `q` match stays in SQL because the
// values it compares — class names, rule names, zone ids — are written by this
// server in a known case.
func zoneIDsForName(refs map[string]*cameraRefs, q string) []string {
	needle := strings.ToLower(strings.TrimSpace(q))
	if needle == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, cam := range refs {
		for name, ids := range cam.zoneIDs {
			if !strings.Contains(name, needle) {
				continue
			}
			for _, id := range ids {
				if seen[id] {
					continue
				}
				seen[id] = true
				out = append(out, id)
				if len(out) >= MaxQZoneIDs {
					return out
				}
			}
		}
	}
	return out
}

// eventView is one event as the API answers it.
//
// No paths: a snapshot path and a clip path are names on this server's disk, and
// playback is server-proxied, so the console gets two routes and two flags that
// say whether they will answer.
type eventView struct {
	ID          string  `json:"id"`
	EventKey    string  `json:"event_key"`
	CameraID    string  `json:"camera_id"`
	CameraSlug  string  `json:"camera_slug"`
	Action      string  `json:"action"`
	Open        bool    `json:"open"`
	StartedAt   string  `json:"started_at"`
	EndedAt     string  `json:"ended_at"`
	Seconds     float64 `json:"seconds"`
	Severity    int     `json:"severity"`
	SignalCount int     `json:"signal_count"`

	// Classes and rules are the worker's words. Zones is the names the operator
	// typed, with ZoneIDs alongside because a name can be reused and an id
	// cannot — and because the `zone_id=` filter takes an id.
	Classes []string `json:"classes"`
	Zones   []string `json:"zones"`
	ZoneIDs []string `json:"zone_ids"`
	Rules   []string `json:"rules"`

	// The wall answers `objects: []` and the count; the drawer answers the boxes.
	// Fifty events at two hundred boxes each is a payload no wall draws, and a
	// count is what a card shows.
	Objects     []models.EventObject `json:"objects"`
	ObjectCount int                  `json:"object_count"`

	// The two flags are the row's claim, not the disk's. A flag can be true and
	// its route still answer a named 404 — the sweeper took the file, a volume
	// came back without it — and the route is the truth.
	HasSnapshot      bool    `json:"has_snapshot"`
	HasClip          bool    `json:"has_clip"`
	ClipSeconds      float64 `json:"clip_seconds"`
	SnapshotUploaded bool    `json:"snapshot_uploaded"`
	ClipUploaded     bool    `json:"clip_uploaded"`

	CreatedAt string `json:"created_at"`
}

// viewEvent builds one row's answer. `detail` is the difference between the wall
// and the drawer: the drawer draws boxes, the wall does not.
func (h *Handlers) viewEvent(ev *models.Event, refs map[string]*cameraRefs, detail bool) eventView {
	zoneIDs := models.JSONStringList(ev.ZonesJSON)
	cam := refs[ev.CameraID]

	names := make([]string, 0, len(zoneIDs))
	for _, id := range zoneIDs {
		name := id
		if cam != nil {
			if named := cam.zoneNames[id]; named != "" {
				name = named
			}
		}
		names = append(names, name)
	}

	slug := ""
	if cam != nil {
		slug = cam.slug
	}

	boxes := ev.Objects()
	out := eventView{
		ID:               ev.ID,
		EventKey:         ev.EventKey,
		CameraID:         ev.CameraID,
		CameraSlug:       slug,
		Action:           ev.Action,
		Open:             ev.Open,
		StartedAt:        ev.StartedAt.UTC().Format(timeLayout),
		EndedAt:          ev.EndedAt.UTC().Format(timeLayout),
		Seconds:          ev.EndedAt.Sub(ev.StartedAt).Seconds(),
		Severity:         ev.Severity,
		SignalCount:      ev.SignalCount,
		Classes:          orEmpty(models.JSONStringList(ev.ClassesJSON)),
		Zones:            names,
		ZoneIDs:          orEmpty(zoneIDs),
		Rules:            orEmpty(models.JSONStringList(ev.RulesJSON)),
		Objects:          []models.EventObject{},
		ObjectCount:      len(boxes),
		HasSnapshot:      ev.LocalSnapshotPath != "",
		HasClip:          ev.LocalClipPath != "",
		ClipSeconds:      ev.ClipSeconds,
		SnapshotUploaded: ev.SnapshotUploaded(),
		ClipUploaded:     ev.ClipUploaded(),
		CreatedAt:        ev.CreatedAt.UTC().Format(timeLayout),
	}
	if detail {
		out.Objects = orEmptyObjects(boxes)
	}
	return out
}

// orEmpty answers an empty list for a nil one. The console maps over these
// fields, and `[]` is an answer while `null` is a crash it has to defend against
// on every row.
func orEmpty(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

func orEmptyObjects(items []models.EventObject) []models.EventObject {
	if items == nil {
		return []models.EventObject{}
	}
	return items
}

// ListEvents is the event wall.
//
// GET /api/v1/events?camera_id=&from=&to=&class=&zone_id=&severity=&q=&limit=&offset=
//
// Newest first, with `id` breaking the tie so two events that opened on the same
// tick cannot be split across a page boundary. The window means overlap — an
// event belongs to a window if any part of it happened inside it — which is the
// segment manifest's rule and the clip builder's rule for the same reason: a
// list that hid the thing in progress is a list with a hole where the event is.
//
// `limit` defaults to 50 and is clamped at 200, in the handler and again in the
// store, so a caller cannot get more by asking twice. The response echoes the
// limit, the offset, and whether the page held more than it answered.
func (h *Handlers) ListEvents(c *gin.Context) {
	cameraID := strings.TrimSpace(c.Query("camera_id"))
	refs, err := h.cameraRefsFor(cameraID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the camera rows are unreadable")
		return
	}
	if cameraID != "" && refs[cameraID] == nil {
		// A request about a camera that is not there is not a request that found
		// nothing.
		jsonError(c, http.StatusNotFound, "no camera with that id")
		return
	}

	filter := store.EventFilter{CameraID: cameraID}

	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		moment, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonError(c, http.StatusBadRequest, "to must be an RFC3339 timestamp")
			return
		}
		to := moment.UTC()
		filter.To = &to
	}
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		moment, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonError(c, http.StatusBadRequest, "from must be an RFC3339 timestamp")
			return
		}
		from := moment.UTC()
		filter.From = &from
	}
	if filter.From != nil && filter.To != nil && !filter.From.Before(*filter.To) {
		jsonError(c, http.StatusBadRequest, "from must name a moment before to")
		return
	}

	filter.Class = strings.TrimSpace(c.Query("class"))
	filter.ZoneID = strings.TrimSpace(c.Query("zone_id"))

	severity := 0
	if raw := strings.TrimSpace(c.Query("severity")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || !models.ValidSeverity(n) {
			// Refused rather than clamped: `severity` is a filter here, and a
			// console that typed 9 and silently got "everything" would read a quiet
			// property out of a typo.
			jsonError(c, http.StatusBadRequest, "severity must be 1, 2 or 3")
			return
		}
		filter.Severity = &n
		severity = n
	}

	q := strings.TrimSpace(c.Query("q"))
	filter.Q = q
	filter.QZoneIDs = zoneIDsForName(refs, q)

	limit := store.EventListDefaultLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			jsonError(c, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		if n > store.EventListMaxLimit {
			n = store.EventListMaxLimit
		}
		limit = n
	}

	offset := 0
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			jsonError(c, http.StatusBadRequest, "offset must be zero or a positive number")
			return
		}
		offset = n
	}

	// The page is asked for at its limit, and a full page is probed with one row
	// at the next offset. Asking for `limit+1` instead would be cheaper, but the
	// store clamps the limit at 200, so at the ceiling that extra row is clamped
	// away — and a page of 200 out of 250 would answer `truncated: false`, a
	// timeline that looks complete exactly where it is cut.
	filter.Limit = limit

	events, err := h.St.Events.List(filter, offset)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the event rows are unreadable")
		return
	}
	truncated := false
	if len(events) == limit {
		probe := filter
		probe.Limit = 1
		more, err := h.St.Events.List(probe, offset+limit)
		if err != nil {
			jsonError(c, http.StatusInternalServerError, "the event rows are unreadable")
			return
		}
		truncated = len(more) > 0
	}

	out := make([]eventView, 0, len(events))
	for i := range events {
		out = append(out, h.viewEvent(&events[i], refs, false))
	}

	c.JSON(http.StatusOK, gin.H{
		"events": out,
		"count":  len(out),
		// The filters are echoed in the words the query used, so a console can
		// show what it asked for and a script can tell a filter that matched
		// nothing from a filter it forgot to send.
		"camera_id": cameraID,
		"class":     filter.Class,
		"zone_id":   filter.ZoneID,
		"severity":  severity,
		"q":         q,
		"from":      echoMoment(filter.From),
		"to":        echoMoment(filter.To),
		"limit":     limit,
		"offset":    offset,
		"truncated": truncated,
	})
}

// echoMoment answers the empty string for a window edge the request did not
// name: "no lower bound" is not the same statement as a moment.
func echoMoment(moment *time.Time) string {
	if moment == nil {
		return ""
	}
	return moment.UTC().Format(timeLayout)
}

// GetEvent is the detail drawer: the row, its boxes, and the two routes its
// media is served from.
//
// GET /api/v1/events/:id
func (h *Handlers) GetEvent(c *gin.Context) {
	event, ok := h.eventByID(c)
	if !ok {
		return
	}
	refs, err := h.cameraRefsFor(event.CameraID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the camera rows are unreadable")
		return
	}
	c.JSON(http.StatusOK, h.viewEvent(event, refs, true))
}

// -- the delete (S6-04) ---------------------------------------------------

// DeleteEvent removes one event: the row, its local files, and — for the copies
// that reached a storage tier — the objects out there too.
//
// DELETE /api/v1/events/:id
//
// The order is the one that cannot lose footage: cancel the pending assembly,
// queue the remote purge, remove the local files, delete the row. A row that
// outlives its files is a named 404 a console draws; files that outlive their
// row are bytes nothing will ever reclaim, because the sweeper walks rows and
// never directories.
//
// The remote copies go into the queue rather than being deleted inline: the tier
// may be a WebDAV share behind a tunnel, and deleting one event should not block
// on it — nor fail because it is down. A job names its own backend and key, so
// it outlives the row it came from.
func (h *Handlers) DeleteEvent(c *gin.Context) {
	event, ok := h.eventByID(c)
	if !ok {
		return
	}

	// An assembly that fires after the row is gone cuts a clip for an event
	// nobody can ask for and leaves a file no row points at.
	if h.Clips != nil {
		h.Clips.Cancel(event.ID)
	}
	if h.Hub != nil {
		// The row is about to stop existing, and so is its update throttle gate.
		h.Hub.ForgetEvent(event.ID)
	}

	queued, warning := h.enqueueEventPurge(event)
	removed := h.removeLocalEventMedia(event)

	if err := h.St.Events.Delete(event.ID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "event not found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the event row could not be deleted")
		return
	}

	h.audit(c, models.AuditEventDelete, http.StatusOK,
		AuditTarget{Type: "event", ID: event.ID}, map[string]any{
			"camera_id":     event.CameraID,
			"event_key":     event.EventKey,
			"started_at":    event.StartedAt.UTC().Format(timeLayout),
			"local_files":   removed,
			"remote_jobs":   queued,
			"queue_warning": warning,
		})

	c.JSON(http.StatusOK, gin.H{
		"deleted":       event.ID,
		"camera_id":     event.CameraID,
		"local_files":   removed,
		"remote_jobs":   queued,
		"queue_warning": warning,
	})
}

// enqueueEventPurge queues the remote deletion of the event's media, for the
// copies that actually reached a tier.
//
// Two jobs, one per key, with ids the queue can hold side by side
// (`<event_id>:clip`, `<event_id>:snapshot`): a job row is keyed by its object,
// so two ids are what keeps a clip purge from replacing a snapshot purge. This is
// the shape the camera purge already uses (S3-03), for the same reason.
//
// With no storage backend configured there is nothing remote to purge — that is
// a local-only installation, and it is not an error.
func (h *Handlers) enqueueEventPurge(event *models.Event) (int, string) {
	backend, err := h.St.StorageBackends.GetDefault()
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, "no storage tier is configured: nothing remote to purge"
		}
		return 0, fmt.Sprintf("the storage tier is unreadable: %v", err)
	}

	type media struct {
		jobID, kind, key, local string
		uploaded                bool
	}
	pieces := []media{
		{store.EventJobID(event.ID, models.JobKindClip), models.JobKindClip, event.ClipKey, event.LocalClipPath, event.ClipUploaded()},
		{store.EventJobID(event.ID, models.JobKindSnapshot), models.JobKindSnapshot, event.SnapshotKey, event.LocalSnapshotPath, event.SnapshotUploaded()},
	}

	queued := 0
	var problems []string
	for _, m := range pieces {
		if !m.uploaded {
			continue
		}
		target := m.key
		if target == "" {
			target = filepath.Base(m.local)
		}
		if err := h.St.StorageJobs.Enqueue(store.JobInput{
			ID:        m.jobID,
			Kind:      m.kind,
			Target:    target,
			BackendID: backend.ID,
			Action:    models.JobActionDeleteRemote,
		}); err != nil {
			problems = append(problems, fmt.Sprintf("%s %s: %v", m.kind, event.ID, err))
			continue
		}
		queued++
	}
	return queued, strings.Join(problems, "; ")
}

// removeLocalEventMedia deletes the files this event owns.
//
// Unlike the camera delete, this names exactly the two paths the row holds: the
// clip builder and the signal path each write one file per event id, so an event
// has no local media the database does not know about.
func (h *Handlers) removeLocalEventMedia(event *models.Event) int {
	removed := 0
	for _, path := range []string{event.LocalClipPath, event.LocalSnapshotPath} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed
}

// -- the clip (S6-04; the remote copy is S6-05) ---------------------------

// EventClip serves the assembled clip, from whichever copy still exists.
//
// GET /api/v1/events/:id/clip → video/mp4
//
//	200 video/mp4                       the clip (206 for a Range request)
//	404 {"error":"event not found"}     no such event
//	404 {"error":"clip not available"}  the event stands, without a clip
//	502 {"error":"…","tier":"<name>"}   the tier is there and not usable
//
// **Local first, then the tier.** A file in `CLIPS_DIR` is served by
// `ServeContent`, which is what makes `Range: bytes=…` answer 206 — how a
// scrubber seeks into a clip without downloading it — and what answers a
// conditional GET for a drawer that redraws. With no local copy and the row’s
// upload stamp set, the bytes are on the storage tier and the route streams them
// from byte 0 (S6-05, `streamFromTier`): no seek, the whole object, the same
// type.
//
// "clip not available" covers the states the console draws identically — no clip
// was ever cut (a camera in `record_mode=off`, or a window that held no
// segments), and the local copy aged out *and* the tier copy aged out with it.
// The log names which one it was, because the operator’s question and the
// browser’s answer are different questions.
func (h *Handlers) EventClip(c *gin.Context) {
	event, ok := h.eventByID(c)
	if !ok {
		return
	}
	if h.serveLocalClip(c, event) {
		return
	}
	if event.ClipUploaded() && h.streamFromTier(c, event.ClipKey, "video/mp4") {
		return
	}
	jsonError(c, http.StatusNotFound, "clip not available")
}

// serveLocalClip answers from `CLIPS_DIR` when the row names a file this server
// can read, and returns false when it wrote nothing — so the caller can try the
// tier without unwinding a half-written response.
func (h *Handlers) serveLocalClip(c *gin.Context, event *models.Event) bool {
	if event.LocalClipPath == "" {
		return false
	}
	if !h.clipIsInPlace(event.LocalClipPath) {
		// The row names a file this server will not read: outside CLIPS_DIR (a row
		// restored from another installation), or unreadable. Serving it anyway
		// would make the route a general file reader, and the log says which of the
		// two happened.
		log.Printf("events: clip for %s names %s, which is not inside %s; trying the tier",
			event.ID, event.LocalClipPath, h.Cfg.ClipsDir)
		return false
	}

	f, err := os.Open(event.LocalClipPath)
	if err != nil {
		log.Printf("events: clip for %s could not be opened at %s: %v", event.ID, event.LocalClipPath, err)
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		log.Printf("events: clip for %s could not be stat'd at %s: %v", event.ID, event.LocalClipPath, err)
		return false
	}

	// The type is set explicitly because `docs/api.md` promises `video/mp4` and
	// the file’s extension is not what the contract rests on.
	c.Header("Content-Type", "video/mp4")
	c.Header("Cache-Control", "private, max-age=3600")
	http.ServeContent(c.Writer, c.Request, filepath.Base(event.LocalClipPath), info.ModTime(), f)
	return true
}

// -- the live feed (S6-08) ------------------------------------------------

// broadcastEvent puts one applied signal on the live feed.
//
// The three triggers are the console's LIVE card: `event.new` makes the event
// appear, `event.update` keeps it fresh while the worker extends it, and
// `event.close` retires the badge. Nothing else sends them, so the sequence a
// console sees is exactly the sequence the store applied — which is what lets the
// wall show "LIVE" from the messages alone, with no polling behind it.
//
// The payload is the same view the list and the drawer answer, whole rather than
// partial. That is what makes the plan's throttle safe on the leading edge: an
// extend that loses the window is back on the wire within five seconds, and the
// final state of every event arrives with its close, which is not throttled.
func (h *Handlers) broadcastEvent(ev *models.Event) {
	if h.Hub == nil || ev == nil {
		return
	}
	refs, err := h.cameraRefsFor(ev.CameraID)
	if err != nil {
		// The live feed is not the fact; the row is. A camera read that fails
		// costs the console one frame, and the next signal sends it again.
		return
	}
	view := h.viewEvent(ev, refs, false)

	switch ev.Action {
	case models.SignalActionOpen:
		_ = h.Hub.EventNew(view)
	case models.SignalActionExtend:
		// Throttled: a worker extends on every detection tick, and sixty busy
		// cameras would put three thousand frames a second on every console to
		// say nothing an operator can read.
		_ = h.Hub.EventUpdateThrottled(ev.ID, view)
	case models.SignalActionClose:
		_ = h.Hub.EventClose(view)
		// The event will never extend again, so its gate is memory with nothing
		// left to throttle.
		h.Hub.ForgetEvent(ev.ID)
	}
}

// BroadcastEventUpdate puts a changed event on the live feed for a change that did
// not arrive as a signal — the clip assembler's, which lands seconds after the close
// and is the one fact about a closed event that still moves.
//
// It is not throttled: one assembly is one message per event, and a player that
// lights up when the file exists is worth more than the frame it costs.
func (h *Handlers) BroadcastEventUpdate(ev *models.Event) {
	if h.Hub == nil || ev == nil {
		return
	}
	refs, err := h.cameraRefsFor(ev.CameraID)
	if err != nil {
		return
	}
	_ = h.Hub.EventUpdate(h.viewEvent(ev, refs, false))
}

// -- media that lives only on the storage tier (S6-05) --------------------

// streamFromTier copies one object off the storage tier into the response.
//
// It returns **false** when the tier has nothing for this row — no default
// backend at all (a local-only installation), or the object is not on it — which
// is the caller's named-404 case, and **true** when the response has been
// written, successfully or with a status of its own.
//
// Two things here are the plan's remote-playback gap, and neither is hidden:
//
//   - **There is no seek.** The tier's `Backend` reads an object from byte 0;
//     there is no ranged read to answer a `Range` with. A request that carries
//     one gets the whole object with a 200, which is the fallback HTTP sanctions
//     and browsers handle. Seek-on-remote is the v2 item the README's roadmap
//     names, and `docs/architecture.md` states the gap where the console author
//     will read it.
//   - **The size is asked for before the first byte.** `Stat` then `Get` is two
//     round trips where one could be enough, and the extra one buys a
//     `Content-Length` that is true and a 404 that arrives *before* a 200's
//     headers have gone — the difference between a drawer that says "no clip" and
//     one that shows a player that stopped halfway.
//
// The destination is the installation's default backend. The event row records
// the object's *key*, not which backend holds it, and every other path that
// touches an event's media — the delete, the camera purge — resolves the tier the
// same way. Changing the default does not move old clips; that is a fact about
// the row, not about this function.
func (h *Handlers) streamFromTier(c *gin.Context, key, contentType string) bool {
	if key == "" {
		return false
	}
	backend, err := h.St.StorageBackends.GetDefault()
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("events: the storage tier is unreadable for %s: %v", key, err)
		}
		// No default backend is not a failure: it is a local-only installation,
		// and the caller's named 404 is the right answer for it.
		return false
	}

	ctx := c.Request.Context()
	tier, err := storage.OpenWithSettings(ctx, backend, h.sealKey, h.St.Settings)
	if err != nil {
		h.tierFailure(c, key, err)
		return true
	}

	info, err := tier.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// The row says the object was uploaded and the tier does not have it:
			// aged out by the retention sweep, or removed by hand. The event
			// stands, its media does not, and the caller's named 404 is the truth.
			log.Printf("events: %s is not on the tier at %s, though the row says it was uploaded", key, backend.Name)
			return false
		}
		h.tierFailure(c, key, err)
		return true
	}

	body, err := tier.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false
		}
		h.tierFailure(c, key, err)
		return true
	}
	defer body.Close()

	if contentType == "" {
		contentType = info.ContentType
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Accept-Ranges", "bytes")
	c.Header("Cache-Control", "private, max-age=3600")
	c.Header("Content-Length", strconv.FormatInt(info.Size, 10))
	if !info.ModTime.IsZero() {
		c.Header("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}
	c.Status(http.StatusOK)

	if _, err := io.Copy(c.Writer, body); err != nil {
		// The headers are already gone, so the status cannot change. A client that
		// closed the connection is the normal case here and is not worth a line;
		// a tier that stopped answering mid-object is, because the player shows a
		// clip that ends early and the operator cannot tell why.
		if c.Request.Context().Err() != nil {
			return true
		}
		log.Printf("events: streaming %s from the tier stopped: %v", key, err)
	}
	return true
}

// tierFailure answers a tier that is there but not usable, with the named state
// beside the sentence so the console can say what to fix rather than that
// something is wrong.
func (h *Handlers) tierFailure(c *gin.Context, key string, err error) {
	status := http.StatusBadGateway
	message := "the storage tier is not reachable"

	switch {
	case errors.Is(err, storage.ErrBadKey):
		// Not the tier's fault and not the operator's setting: this process cannot
		// open the stored credential, which is an environment problem.
		status = http.StatusInternalServerError
		message = "the storage credential cannot be opened with this installation's key"
	case errors.Is(err, storage.ErrKindUnsupported):
		status = http.StatusInternalServerError
		message = "this build has no driver for the configured storage tier"
	}

	log.Printf("events: the storage tier refused %s: %v", key, err)
	c.JSON(status, gin.H{
		"error": message,
		"tier":  storage.ErrorCode(err),
	})
}

// eventByID is the lookup every event route shares — the detail, the snapshot,
// the clip and the delete — so "no such event" is one sentence in one place
// rather than a phrase each handler invents.
func (h *Handlers) eventByID(c *gin.Context) (*models.Event, bool) {
	id := strings.TrimSpace(c.Param("id"))
	event, err := h.St.Events.GetByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "event not found")
			return nil, false
		}
		jsonError(c, http.StatusInternalServerError, "the event row is unreadable")
		return nil, false
	}
	return event, true
}
