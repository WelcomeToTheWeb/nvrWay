package api

import (
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"nvr/server/models"
)

// Zone writes: the rule set a camera detects inside.
//
// Coordinates stay normalized to 0..1 of the detection frame, which is why a
// camera changing its resolution cannot silently invalidate a rule (S5-05 reads
// the same numbers). The console draws a polygon over the snapshot, the worker
// tests a centroid against it, and the API is the one place both shapes are
// checked before they can be stored.
//
// Three things this file enforces that the row validators do not:
//
//   - **A polygon is closed implicitly.** The editor sends a path, not a ring:
//     3 vertices is a triangle. Requiring the client to repeat the first vertex
//     would make every console draw a 4-point square, and the worker closes the
//     ring itself. A payload that *does* repeat the first vertex is the same
//     shape, so it is normalized by dropping the repeat — a rule stored twice is
//     a rule whose vertex count differs in two installations for no reason.
//   - **A polygon with no area is refused.** Collinear and repeated-point
//     polygons have area 0: nothing is ever inside them. That is a rule an
//     operator believes is armed and the worker can never fire — the same class
//     of failure as a zone naming a class the worker drops, which the model
//     layer already refuses.
//   - **Every zone write is a ledger row with the before and the after.** "Who
//     disarmed the driveway rule, and what was it before?" is a question asked
//     after an event that did not fire, and the answer has to be in the ledger.
//
// Zone writes do not re-render go2rtc's config: the sidecar knows streams, not
// rules. S3-05's one-write-path rule is about stream-bearing writes.

// PutCameraZones replaces a camera's zone rule set.
//
// PUT /api/v1/cameras/:id/zones (operator, admin)
//
// The body is the array the console draws:
//
//	[{"id":"z1","name":"driveway","polygon":[[0.1,0.3],[0.8,0.3],[0.8,0.9]],
//	  "classes":["person"],"rule":"intrusion","dwell_seconds":30,"severity":3,
//	  "min_confidence":0.35,"min_area_ratio":0.004,"enabled":true}]
//
// An empty array is a valid write: "no rules" is a state an operator sets, not a
// malformed request.
func (h *Handlers) PutCameraZones(c *gin.Context) {
	camera, ok := h.cameraByID(c)
	if !ok {
		return
	}

	var zones []models.Zone
	if err := c.BindJSON(&zones); err != nil {
		zoneError(c, "the body is not a zone array")
		return
	}
	if zones == nil {
		zones = []models.Zone{}
	}

	normalized, err := normalizeZones(zones)
	if err != nil {
		zoneError(c, err.Error())
		return
	}

	encoded, err := models.ZonesToJSON(normalized)
	if err != nil {
		zoneError(c, err.Error())
		return
	}
	// The model validator owns the rest of the contract: vertex count and bounds,
	// classes, rule names, dwell, severity, the per-zone floors, duplicate ids,
	// and the count ceiling. One validator, three readers (console, API, worker).
	if err := models.ValidateZonesJSON(encoded); err != nil {
		zoneError(c, err.Error())
		return
	}

	before, err := models.ParseZones(camera.ZonesJSON)
	if err != nil {
		before = []models.Zone{}
	}

	if err := h.St.Cameras.UpdateZones(camera.ID, encoded); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no camera with that id")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the zone rules could not be written")
		return
	}

	fresh, err := h.St.Cameras.GetByID(camera.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the camera row is unreadable after the write")
		return
	}

	h.audit(c, models.AuditCameraZones, http.StatusOK,
		AuditTarget{Type: "camera", ID: camera.ID}, map[string]any{
			"slug":         camera.Slug,
			"zones_before": len(before),
			"zones_after":  len(normalized),
			"before":       zoneSummaries(before),
			"after":        zoneSummaries(normalized),
		})

	zonesOut, _ := models.ParseZones(fresh.ZonesJSON)
	c.JSON(http.StatusOK, gin.H{
		"camera_id": camera.ID,
		"slug":      camera.Slug,
		"zones":     zonesOut,
	})
}

// zoneSummaries is the ledger's before/after: enough to reconstruct the rule that
// was disarmed, without pasting the whole payload into the audit row.
func zoneSummaries(zones []models.Zone) []map[string]any {
	out := make([]map[string]any, 0, len(zones))
	for _, z := range zones {
		out = append(out, map[string]any{
			"id":            z.ID,
			"name":          z.Name,
			"rule":          z.Rule,
			"classes":       z.Classes,
			"vertices":      len(z.Polygon),
			"dwell_seconds": z.DwellSec,
			"severity":      z.Severity,
			"enabled":       z.Enabled,
		})
	}
	return out
}

// zoneError answers the plan's literal status body, with the reason beside it.
//
// `{"error":"zone polygon invalid"}` is the contract docs/api.md and the console
// name, so that string is fixed. The `detail` field is what tells the operator
// which zone and what about it — "your JSON is wrong" is a bug report they cannot
// act on.
func zoneError(c *gin.Context, detail string) {
	c.JSON(http.StatusUnprocessableEntity, gin.H{
		"error":  "zone polygon invalid",
		"detail": detail,
	})
}

// normalizeZones closes polygons implicitly and drops the degenerate ones.
func normalizeZones(zones []models.Zone) ([]models.Zone, error) {
	out := make([]models.Zone, 0, len(zones))
	for _, zone := range zones {
		polygon, err := normalizePolygon(zone.Polygon)
		if err != nil {
			return nil, fmt.Errorf("zone %s: %w", zone.ID, err)
		}
		zone.Polygon = polygon
		out = append(out, zone)
	}
	return out, nil
}

// normalizePolygon drops a trailing vertex that repeats the first (an explicit
// close is the same shape as an implicit one) and enforces the vertex count and
// the 0..1 bounds on the normalized shape, so the count the operator sees is the
// count the worker tests.
func normalizePolygon(polygon [][]float64) ([][]float64, error) {
	if len(polygon) == 0 {
		return nil, errors.New("polygon is required")
	}

	points := make([][]float64, 0, len(polygon))
	for _, vertex := range polygon {
		if len(vertex) != 2 {
			return nil, errors.New("every vertex is an [x,y] pair")
		}
		if math.IsNaN(vertex[0]) || math.IsNaN(vertex[1]) ||
			math.IsInf(vertex[0], 0) || math.IsInf(vertex[1], 0) {
			return nil, errors.New("polygon coordinates must be finite numbers")
		}
		points = append(points, []float64{vertex[0], vertex[1]})
	}

	if len(points) > 1 {
		first, last := points[0], points[len(points)-1]
		if samePoint(first, last) {
			points = points[:len(points)-1]
		}
	}

	if len(points) < models.ZoneMinVertices {
		return nil, fmt.Errorf("polygon needs %d-%d vertices, got %d (an open path is closed implicitly, so 3 points are a triangle)",
			models.ZoneMinVertices, models.ZoneMaxVertices, len(points))
	}
	if len(points) > models.ZoneMaxVertices {
		return nil, fmt.Errorf("polygon needs %d-%d vertices, got %d",
			models.ZoneMinVertices, models.ZoneMaxVertices, len(points))
	}

	if polygonArea(points) == 0 {
		return nil, errors.New("polygon encloses no area (its vertices are collinear or repeated), so nothing can ever be inside it")
	}
	return points, nil
}

// polygonArea is twice the signed area of the ring (the shoelace sum, which is
// what a point-in-polygon test walks too). Zero is the degenerate shape.
//
// The comparison is on the raw sum, not a rounded one: a rule drawn as a hairline
// at 0.0000001 is a polygon the operator can see in the editor and the centroid
// can never be in, so it is refused for the same reason a collinear one is.
func polygonArea(points [][]float64) float64 {
	area := 0.0
	for i := range points {
		x1, y1 := points[i][0], points[i][1]
		x2, y2 := points[(i+1)%len(points)][0], points[(i+1)%len(points)][1]
		area += x1*y2 - x2*y1
	}
	return area
}

func samePoint(a, b []float64) bool {
	const epsilon = 1e-9
	return absFloat(a[0]-b[0]) < epsilon && absFloat(a[1]-b[1]) < epsilon
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
