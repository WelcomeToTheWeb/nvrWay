// Package api's timeline manifest: the segment rows the scrubber draws.
//
// One route, and the decisions in it are the ones the console would otherwise
// have to invent:
//
//   - **The floor is `viewer`.** Footage is what an operator shows a person who
//     is not an operator, so reading the timeline is the lowest thing an account
//     can do. There is no gate above `AuthMiddleware` because there is nothing
//     here to change.
//   - **The window is overlap, not containment.** `Segments.ListRange` answers
//     `end_ts > from AND start_ts < to`, which is the clip builder's rule
//     (S6-01): a scrub that starts in the middle of a segment needs that segment,
//     and a manifest that hid it would show a hole where the footage is.
//   - **A request that names no window gets the server's.** `to` is now on this
//     process's clock and `from` is an hour before it, and the response says
//     which window it answered. The alternative — making every caller do date
//     arithmetic to ask "is this camera recording" — is how a console ends up
//     with nine different answers to one question.
//   - **An empty window is `[]`, not a 404.** "Nothing was recorded then" is an
//     answer about the property, and the scrubber draws it. A `camera_id` that
//     names no camera is the different case: that is a request about a thing that
//     does not exist, and it says so.
//   - **The limit is stated and its effect is stated.** `store.SegmentListMaxLimit`
//     (2000) is the ceiling; the response echoes the limit it used and says
//     whether the window held more rows than it answered. A truncated manifest
//     that looks complete is a timeline with a silent edge.
//
// No ledger row: scrubbing a timeline is not an act. The ledger records acts
// (S2-08, S2-09), and the console polls this route.
package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"nvr/server/models"
	"nvr/server/store"
)

// DefaultManifestWindow is the window answered when the request names a camera
// and no window. An hour of footage at the plan's 5 s segment is 720 rows,
// inside the 2000-row ceiling, and it is wide enough that "is this camera
// recording" is answerable without the caller naming a date.
const DefaultManifestWindow = time.Hour

// registerSegmentRoutes mounts the timeline manifest.
//
// The read group carries the whole surface: `GET /api/v1/segments` is a query,
// and the only gate it needs is the one every `/api/v1` route runs.
func (h *Handlers) registerSegmentRoutes(r *gin.Engine) {
	read := r.Group("/api/v1/segments", AuthMiddleware(h.JWT))
	read.GET("", h.ListSegments)
}

// segmentView is one manifest row as the console draws it.
//
// `path` is deliberately absent: it is a name on this server's disk, and playback
// is server-proxied (S6-05), so the scrubber needs the row's `id` and nothing
// else. `slug` is present because the wall labels a strip with the camera's name,
// and a manifest that spans cameras cannot ask for it per row.
type segmentView struct {
	ID         string  `json:"id"`
	CameraID   string  `json:"camera_id"`
	Slug       string  `json:"slug"`
	Generation int     `json:"generation"`
	StartTS    string  `json:"start_ts"`
	EndTS      string  `json:"end_ts"`
	Seconds    float64 `json:"seconds"`
	Bytes      int64   `json:"bytes"`
	Kind       string  `json:"kind"`

	// Uploaded says the footage is safe on a storage tier as well as on this
	// disk. The console shows it because the local tree is a working copy, and a
	// strip the operator has not yet pushed off the box is one they should know
	// is still only here.
	Uploaded bool `json:"uploaded"`
}

func (h *Handlers) viewSegment(seg *models.Segment, slug string) segmentView {
	return segmentView{
		ID:         seg.ID,
		CameraID:   seg.CameraID,
		Slug:       slug,
		Generation: seg.Generation,
		StartTS:    seg.StartTS.UTC().Format(timeLayout),
		EndTS:      seg.EndTS.UTC().Format(timeLayout),
		Seconds:    seg.Seconds,
		Bytes:      seg.Bytes,
		Kind:       seg.Kind,
		Uploaded:   seg.Uploaded(),
	}
}

// ListSegments is the manifest.
//
// GET /api/v1/segments?camera_id=&from=&to=&limit=
//
// Rows come back in `start_ts` order — the order a scrubber draws them — and the
// window is half-open overlap: a segment that ended exactly at `from` is out, a
// segment that starts exactly at `to` is out, and anything crossing either edge
// is in.
func (h *Handlers) ListSegments(c *gin.Context) {
	cameraID := strings.TrimSpace(c.Query("camera_id"))
	if cameraID != "" {
		if _, err := h.St.Cameras.GetByID(cameraID); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				jsonError(c, http.StatusNotFound, "no camera with that id")
				return
			}
			jsonError(c, http.StatusInternalServerError, "the camera row is unreadable")
			return
		}
	}

	to := h.now().UTC()
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		moment, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonError(c, http.StatusBadRequest, "to must be an RFC3339 timestamp")
			return
		}
		to = moment.UTC()
	}

	from := to.Add(-DefaultManifestWindow)
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		moment, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonError(c, http.StatusBadRequest, "from must be an RFC3339 timestamp")
			return
		}
		from = moment.UTC()
	}
	if !from.Before(to) {
		// A window with no inside has no rows, and answering [] for it would let
		// a console with its operands swapped believe the property was quiet.
		jsonError(c, http.StatusBadRequest, "from must name a moment before to")
		return
	}

	limit := store.SegmentListMaxLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			jsonError(c, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		if n > store.SegmentListMaxLimit {
			n = store.SegmentListMaxLimit
		}
		limit = n
	}

	// One row past the limit is asked for, so "the window held more" is a fact
	// this handler observed rather than a guess it makes from the count it
	// happened to return.
	ask := limit
	if limit < store.SegmentListMaxLimit {
		ask = limit + 1
	}

	segments, err := h.St.Segments.ListRange(cameraID, from, to, ask)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the segment manifest is unreadable")
		return
	}
	truncated := len(segments) > limit
	if truncated {
		segments = segments[:limit]
	}

	slugs, err := h.slugsByCamera(cameraID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the camera rows are unreadable")
		return
	}

	out := make([]segmentView, 0, len(segments))
	for i := range segments {
		seg := &segments[i]
		out = append(out, h.viewSegment(seg, slugs[seg.CameraID]))
	}

	c.JSON(http.StatusOK, gin.H{
		"segments": out,
		"count":    len(out),
		// Empty means "every camera", and the response says so in the same words
		// the request used.
		"camera_id": cameraID,
		"from":      from.Format(timeLayout),
		"to":        to.Format(timeLayout),
		"limit":     limit,
		"truncated": truncated,
	})
}

// slugsByCamera maps camera id to slug for the rows this manifest can name.
//
// A single-camera request reads that one row; a request across cameras reads the
// table once. A segment whose camera row is gone answers with an empty slug —
// the footage is still footage, and the row that named the camera is the thing
// that went away.
func (h *Handlers) slugsByCamera(cameraID string) (map[string]string, error) {
	if cameraID != "" {
		camera, err := h.St.Cameras.GetByID(cameraID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return map[string]string{}, nil
			}
			return nil, err
		}
		return map[string]string{camera.ID: camera.Slug}, nil
	}
	cameras, err := h.St.Cameras.ListAll()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(cameras))
	for i := range cameras {
		out[cameras[i].ID] = cameras[i].Slug
	}
	return out, nil
}
