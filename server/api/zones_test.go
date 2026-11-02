package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"nvr/server/models"
)

// Zone writes: the console draws a path over the snapshot, the worker tests a
// centroid against it, and this suite is the place where the two agree. The
// status body is the plan's literal `{"error":"zone polygon invalid"}`.

func writeZones(t *testing.T, fx *cameraFixture, cameraID, bearer string, zones any) (int, map[string]any, string) {
	t.Helper()
	return do(t, fx.Router, request{
		method: http.MethodPut, path: "/api/v1/cameras/" + cameraID + "/zones",
		bearer: bearer, body: zones,
	})
}

func storedZones(t *testing.T, fx *cameraFixture, cameraID string) []models.Zone {
	t.Helper()
	camera, err := fx.Store.Cameras.GetByID(cameraID)
	if err != nil {
		t.Fatalf("reading the camera: %v", err)
	}
	zones, err := models.ParseZones(camera.ZonesJSON)
	if err != nil {
		t.Fatalf("the stored zones must parse: %v", err)
	}
	return zones
}

// TestZoneWriteClosesAnOpenPathImplicitly: the editor sends a path, not a ring.
// Three vertices are a triangle, and requiring the client to repeat the first
// vertex would make every drawn square a 4-point polygon.
func TestZoneWriteClosesAnOpenPathImplicitly(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	status, out, raw := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zoneBody("z1")})
	if status != http.StatusOK {
		t.Fatalf("a triangle is a polygon: got %d %s", status, raw)
	}
	zones := storedZones(t, fx, id)
	if len(zones) != 1 {
		t.Fatalf("one zone stored: %+v", zones)
	}
	if len(zones[0].Polygon) != 3 {
		t.Fatalf("an open path stays 3 vertices: %+v", zones[0].Polygon)
	}
	if zones[0].Rule != models.ZoneRuleIntrusion {
		t.Fatalf("rule: %+v", zones[0])
	}
	if out["camera_id"] != id {
		t.Fatalf("the answer names the camera: %v", out)
	}
	answer, ok := out["zones"].([]any)
	if !ok || len(answer) != 1 {
		t.Fatalf("the answer carries the stored zones: %v", out["zones"])
	}
}

// TestZoneWriteAcceptsAnExplicitCloseAsTheSameShape: a client that repeats the
// first vertex drew the same triangle. Storing it verbatim means two
// installations hold the same rule with two different vertex counts.
func TestZoneWriteAcceptsAnExplicitCloseAsTheSameShape(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	zone := zoneBody("z1")
	zone["polygon"] = [][]float64{{0.1, 0.3}, {0.8, 0.3}, {0.8, 0.9}, {0.1, 0.3}}

	status, _, raw := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zone})
	if status != http.StatusOK {
		t.Fatalf("an explicit close is the same shape: got %d %s", status, raw)
	}
	if zones := storedZones(t, fx, id); len(zones[0].Polygon) != 3 {
		t.Fatalf("the closing repeat is dropped: %+v", zones[0].Polygon)
	}
}

func TestZoneWriteEnforcesTheVertexCount(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	// Two vertices enclose nothing.
	zone := zoneBody("z1")
	zone["polygon"] = [][]float64{{0.1, 0.2}, {0.8, 0.9}}
	status, out, _ := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zone})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a 2-vertex polygon: got %d, want 422", status)
	}
	if out["error"] != "zone polygon invalid" {
		t.Fatalf("the body is the contract's literal: %v", out)
	}
	detail, _ := out["detail"].(string)
	if detail == "" || !strings.Contains(detail, "z1") {
		t.Fatalf("the detail names the zone to fix: %v", out)
	}

	// 32 is the ceiling.
	wide := make([][]float64, 0, 33)
	for i := 0; i < 33; i++ {
		angle := float64(i) / 33.0
		wide = append(wide, []float64{0.5 + 0.4*angle, 0.5 - 0.4*angle})
	}
	zone2 := zoneBody("z2")
	zone2["polygon"] = wide
	if status, out, _ := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zone2}); status != http.StatusUnprocessableEntity {
		t.Fatalf("a 33-vertex polygon: got %d %v", status, out)
	}
}

func TestZoneWriteEnforcesTheUnitBounds(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	// Coordinates are normalized 0..1 of the detection frame, so a resolution
	// change cannot invalidate a rule (S5-05). A pixel value is the usual mistake.
	for _, bad := range [][][]float64{
		{{0, 0}, {640, 0}, {640, 360}},
		{{0.1, 0.3}, {0.8, 0.3}, {1.2, 0.9}},
		{{0.1, 0.3}, {0.8, 0.3}, {-0.1, 0.9}},
	} {
		zone := zoneBody("z1")
		zone["polygon"] = bad
		status, out, _ := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zone})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("polygon %v: got %d, want 422: %v", bad, status, out)
		}
		if out["error"] != "zone polygon invalid" {
			t.Fatalf("the body is the contract's literal: %v", out)
		}
	}
	if zones := storedZones(t, fx, id); len(zones) != 0 {
		t.Fatal("nothing was written")
	}
}

// TestZoneWriteRefusesAPolygonWithNoArea: collinear and repeated-point polygons
// enclose nothing, so a rule like that never fires. The operator believes it is
// armed.
func TestZoneWriteRefusesAPolygonWithNoArea(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	for _, degenerate := range [][][]float64{
		{{0.1, 0.1}, {0.5, 0.5}, {0.9, 0.9}},         // collinear
		{{0.2, 0.2}, {0.2, 0.2}, {0.7, 0.7}},         // repeated point, no area
		{{0.1, 0.1}, {0.4, 0.4}, {0.7, 0.7}, {1, 1}}, // collinear run
	} {
		zone := zoneBody("z1")
		zone["polygon"] = degenerate
		status, out, _ := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zone})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("polygon %v: got %d, want 422: %v", degenerate, status, out)
		}
		detail, _ := out["detail"].(string)
		if detail == "" {
			t.Fatalf("say why it is invalid: %v", out)
		}
	}
}

// TestZoneWriteAcceptsOnlyIntrusionAndLoiter.
func TestZoneWriteAcceptsOnlyIntrusionAndLoiter(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	for _, rule := range []string{"containment", "loiter_once", "", "INTRUSION"} {
		zone := zoneBody("z1")
		zone["rule"] = rule
		status, out, _ := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zone})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("rule %q: got %d, want 422: %v", rule, status, out)
		}
		if out["error"] != "zone polygon invalid" {
			t.Fatalf("the body is the contract's literal: %v", out)
		}
	}

	for _, rule := range []string{models.ZoneRuleIntrusion, models.ZoneRuleLoiter} {
		zone := zoneBody("z1")
		zone["rule"] = rule
		if rule == models.ZoneRuleLoiter {
			zone["dwell_seconds"] = 30
		}
		if status, _, raw := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zone}); status != http.StatusOK {
			t.Fatalf("rule %q: got %d %s", rule, status, raw)
		}
	}
}

// TestZoneWriteAuditsBeforeAndAfter: "who disarmed the driveway rule, and what
// was it before?" is asked after an event that did not fire.
func TestZoneWriteAuditsBeforeAndAfter(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	first := zoneBody("z1")
	if status, _, raw := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{first}); status != http.StatusOK {
		t.Fatalf("first write: %d %s", status, raw)
	}

	// Replace it with a loiter rule and a second zone: the ledger has to show the
	// shape before the write and after it.
	loiter := zoneBody("z1")
	loiter["rule"] = models.ZoneRuleLoiter
	loiter["dwell_seconds"] = 45
	loiter["enabled"] = false
	second := zoneBody("z2")
	second["classes"] = []string{"car"}
	if status, _, raw := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{loiter, second}); status != http.StatusOK {
		t.Fatalf("second write: %d %s", status, raw)
	}

	rows := auditRows(t, fx.Store, models.AuditCameraZones)
	if len(rows) != 2 {
		t.Fatalf("two writes, two ledger rows: %d", len(rows))
	}
	details := map[string]any{}
	if err := json.Unmarshal([]byte(rows[0].Details), &details); err != nil {
		t.Fatalf("the zone write's details are JSON: %v", err)
	}
	if rows[0].TargetID != id {
		t.Fatalf("the row points at the camera: %+v", rows[0])
	}
	if details["zones_before"] != float64(1) || details["zones_after"] != float64(2) {
		t.Fatalf("before and after counts: %v", details)
	}
	before, ok := details["before"].([]any)
	if !ok || len(before) != 1 {
		t.Fatalf("the row carries the rules as they were: %v", details["before"])
	}
	beforeZone := before[0].(map[string]any)
	if beforeZone["rule"] != models.ZoneRuleIntrusion || beforeZone["id"] != "z1" {
		t.Fatalf("the before is the intrusion rule: %v", beforeZone)
	}
	after, ok := details["after"].([]any)
	if !ok || len(after) != 2 {
		t.Fatalf("the row carries the rules as they became: %v", details["after"])
	}
	firstAfter := after[0].(map[string]any)
	if firstAfter["rule"] != models.ZoneRuleLoiter || firstAfter["enabled"] != false ||
		firstAfter["dwell_seconds"] != float64(45) {
		t.Fatalf("the after shows the disarmed loiter rule: %v", firstAfter)
	}
	if rows[0].ActorUsername != "cam-operator" {
		t.Fatalf("the row names who wrote it: %+v", rows[0])
	}
}

func TestZoneWriteEmptyArrayClearsTheRules(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zoneBody("z1")})

	status, out, raw := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("\"no rules\" is a state an operator sets: got %d %s", status, raw)
	}
	if zones := storedZones(t, fx, id); len(zones) != 0 {
		t.Fatalf("the rules are cleared: %+v", zones)
	}
	if list, ok := out["zones"].([]any); !ok || len(list) != 0 {
		t.Fatalf("the answer says so: %v", out["zones"])
	}
	rows := auditRows(t, fx.Store, models.AuditCameraZones)
	details := map[string]any{}
	_ = json.Unmarshal([]byte(rows[0].Details), &details)
	if details["zones_before"] != float64(1) || details["zones_after"] != float64(0) {
		t.Fatalf("the clear is recorded: %v", details)
	}
}

func TestZoneWriteRefusesAZoneThatCanNeverFire(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://554/live"), fx.OperatorTok)

	// The worker drops classes outside its set, so a zone armed on `boat` is a
	// rule that can never fire.
	zone := zoneBody("z1")
	zone["classes"] = []string{"boat"}
	status, out, _ := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zone})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("an unarmable class: got %d, want 422: %v", status, out)
	}
	if !strings.Contains(out["detail"].(string), "z1") {
		t.Fatalf("name the zone: %v", out)
	}
}

func TestZoneWriteRefusesDuplicateZoneIDs(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	status, out, _ := writeZones(t, fx, id, fx.OperatorTok, []map[string]any{zoneBody("z1"), zoneBody("z1")})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("two rules cannot share an id: got %d %v", status, out)
	}
}

func TestZoneWriteRequiresAnExistingCamera(t *testing.T) {
	fx := newCameraFixture(t)
	status, _, _ := writeZones(t, fx, "no-such-camera", fx.OperatorTok, []map[string]any{zoneBody("z1")})
	if status != http.StatusNotFound {
		t.Fatalf("no such camera: got %d, want 404", status)
	}
}

func TestZoneWriteIsOperatorWrite(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	status, out, _ := writeZones(t, fx, id, fx.ViewerTok, []map[string]any{zoneBody("z1")})
	if status != http.StatusForbidden {
		t.Fatalf("a viewer cannot arm a rule: got %d, want 403", status)
	}
	if out["error"] != "insufficient role" {
		t.Fatalf("the refusal is the named one: %v", out)
	}
	if zones := storedZones(t, fx, id); len(zones) != 0 {
		t.Fatal("the refusal wrote nothing")
	}
}
