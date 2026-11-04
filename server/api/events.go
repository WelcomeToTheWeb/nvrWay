// The event surface. This file is the first slice of the event API (S6-02): the
// picture an event came with. The list, the detail, the delete and the clip
// (S6-04, S6-05) mount on the same group, and they are deliberately not here
// yet — a route is shipped when the card that owns it is picked up, not
// pre-registered as a stub that answers 501.
//
// One rule drives the shape of what is here: **the event is the fact and the
// picture is evidence attached to it.** An event with no snapshot is a normal
// outcome — a worker that could not encode a frame, a picture over the cap, a
// disk that refused the write, a file the sweeper aged out — and the surface
// says so with a name, at a status a console can draw, without turning the
// event into a 404.

package api

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"nvr/server/models"
)

// registerEventRoutes mounts the event surface.
//
// The read group carries only `AuthMiddleware(h.JWT)`, exactly like the camera
// and segment reads: every authenticated role reads events (a viewer's whole
// job is to look), and a read is not an act — there is no ledger row for it, and
// a 401 here has no actor to name in one.
func (h *Handlers) registerEventRoutes(r *gin.Engine) {
	read := r.Group("/api/v1/events", AuthMiddleware(h.JWT))
	read.GET("/:id/snapshot", h.EventSnapshot)
}

// EventSnapshot serves the frame an event was detected on.
//
// GET /api/v1/events/:id/snapshot → image/jpeg
//
// The bytes are the file the worker's `open` signal wrote, at
// `<SNAPSHOTS_DIR>/<event_id>.jpg`, and the row's `local_snapshot_path` is the
// only thing that says where it is: the console never gets a path, playback is
// server-proxied, and a name on this server's disk is not a name the browser can
// dial.
//
// The three answers a caller can get are each named, because the console draws
// each one differently:
//
//	200 image/jpeg                            the picture
//	404 {"error":"event not found"}           no such event
//	404 {"error":"snapshot not available"}    the event stands, without a picture
//
// The last one is the interesting case and it is not an error the operator has
// to fix: it is the state an event enters when its evidence is absent or aged
// out, and the plan's rule for the clip ("the UI shows a named 404 for events
// whose media is gone") is applied to the snapshot identically.
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

	if event.LocalSnapshotPath == "" {
		// The absent-snapshot case, filed that way on purpose by the signal path
		// (no snapshot on the signal, over the cap, or a write that failed). The
		// event is still an event.
		jsonError(c, http.StatusNotFound, "snapshot not available")
		return
	}
	if !h.snapshotIsInPlace(event.LocalSnapshotPath) {
		// The row names a file this server will not read: outside SNAPSHOTS_DIR
		// (a row restored from another installation), or unreadable. Serving it
		// anyway would make the route a general file reader, and the log says
		// which of the two happened.
		log.Printf("events: snapshot for %s names %s, which is not inside %s; answering not available",
			event.ID, event.LocalSnapshotPath, h.Cfg.SnapshotsDir)
		jsonError(c, http.StatusNotFound, "snapshot not available")
		return
	}

	f, err := os.Open(event.LocalSnapshotPath)
	if err != nil {
		// The row says the picture is here and the disk says otherwise: the
		// sweeper's local delete (S7-09) or a volume that came back without it.
		// The event survives its media, so this is a named 404 and not a 500.
		log.Printf("events: snapshot for %s could not be opened at %s: %v", event.ID, event.LocalSnapshotPath, err)
		jsonError(c, http.StatusNotFound, "snapshot not available")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		log.Printf("events: snapshot for %s could not be stat'd at %s: %v", event.ID, event.LocalSnapshotPath, err)
		jsonError(c, http.StatusNotFound, "snapshot not available")
		return
	}

	c.Header("Cache-Control", "private, max-age=3600")
	// ServeContent, not ServeFile: the explicit type is what `docs/api.md`
	// promises (`image/jpeg`), and it is the response that answers a Range
	// request and a conditional GET, which a console redrawing a detail drawer
	// sends without being clever about it.
	http.ServeContent(c.Writer, c.Request, filepath.Base(event.LocalSnapshotPath), info.ModTime(), f)
}

// snapshotIsInPlace reports that a row's snapshot path is a file this server is
// willing to read: under SNAPSHOTS_DIR, after both sides are made absolute and
// cleaned. The write path builds the name with `filepath.Base(event_id)` for the
// same reason; this is the read half of that guard.
func (h *Handlers) snapshotIsInPlace(path string) bool {
	dir, err := filepath.Abs(h.Cfg.SnapshotsDir)
	if err != nil {
		return false
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	return strings.HasPrefix(target, dir+string(filepath.Separator))
}

// eventByID is the lookup every event route shares — the snapshot here, and the
// detail and delete that follow in S6-04 — so "no such event" is one sentence in
// one place rather than a phrase each handler invents.
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
