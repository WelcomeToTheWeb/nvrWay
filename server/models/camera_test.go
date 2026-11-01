package models

import (
	"strings"
	"testing"
)

func TestSlugIsTheStreamName(t *testing.T) {
	// The slug is dropped into a YAML key, an RTSP path and a URL query
	// verbatim, and there is no mapping table: so the character set and length
	// are asserted, not left to "looks fine".
	valid := []string{"a1", "front-door", "demo-doorbell", "cam0", "z0", "9x", "a-b-c-d", "1start"}
	for _, slug := range valid {
		if !ValidSlug(slug) {
			t.Fatalf("slug %q must be valid", slug)
		}
	}

	invalid := []string{
		"", "a", "-lead", "has space", "has_underscore", "UPPER-cam", "A1", "frontDoor",
		strings.Repeat("a", 33), ".dot", "slash/es", "q?mark", "y!ield",
	}
	for _, slug := range invalid {
		if ValidSlug(slug) {
			t.Fatalf("slug %q must be refused (go2rtc stream name, RTSP path, YAML key)", slug)
		}
	}
}

func TestRecordModes(t *testing.T) {
	for _, mode := range RecordModes {
		if !ValidRecordMode(mode) {
			t.Fatalf("record mode %q must be valid", mode)
		}
	}
	if len(RecordModes) != 3 {
		t.Fatalf("v1 has three recording modes, got %v", RecordModes)
	}
	for _, mode := range []string{"", "always", "detect", "CONTINUOUS", "off "} {
		if ValidRecordMode(mode) {
			t.Fatalf("record mode %q must be refused", mode)
		}
	}
}

func TestZoneContractAcceptsThePlanShape(t *testing.T) {
	// The plan's exact wire contract, asserted verbatim: three readers (the
	// console, the API, the Python worker) agree on this shape, and a field
	// renamed in one of them is a silent no-op in another.
	const contract = `[{"id":"z1","name":"driveway","polygon":[[0.12,0.35],[0.8,0.35],[0.8,0.95],[0.12,0.95]],"classes":["person","car"],"rule":"intrusion","dwell_seconds":30,"severity":3,"min_confidence":0.35,"min_area_ratio":0.004,"enabled":true}]`

	zones, err := ParseZones(contract)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(zones) != 1 {
		t.Fatalf("expected one zone, got %d", len(zones))
	}
	zone := zones[0]
	if zone.ID != "z1" || zone.Name != "driveway" {
		t.Fatalf("id/name: %+v", zone)
	}
	if zone.Rule != ZoneRuleIntrusion {
		t.Fatalf("rule: %q", zone.Rule)
	}
	if zone.DwellSec != 30 {
		t.Fatalf("dwell_seconds decodes to DwellSec: %d", zone.DwellSec)
	}
	if zone.Severity != 3 || zone.MinConfidence != 0.35 || zone.MinAreaRatio != 0.004 {
		t.Fatalf("floors: %+v", zone)
	}
	if !zone.Enabled {
		t.Fatal("enabled must decode")
	}
	if len(zone.Polygon) != 4 || zone.Polygon[0][0] != 0.12 || zone.Polygon[3][1] != 0.95 {
		t.Fatalf("polygon: %v", zone.Polygon)
	}
	if err := ValidateZonesJSON(contract); err != nil {
		t.Fatalf("the plan's shape must validate, got %v", err)
	}

	// Round-tripping keeps the shape byte-compatible for a console that reads
	// the row and writes it back.
	encoded, err := ZonesToJSON(zones)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := ValidateZonesJSON(encoded); err != nil {
		t.Fatalf("round-trip must validate: %v (encoded %s)", err, encoded)
	}
}

func TestZoneValidationNamesTheZoneAndTheReason(t *testing.T) {
	// The API answers 422 {"error":"zone polygon invalid"}; the reason and the
	// zone are what the operator acts on, so the error is named at the model.
	cases := []struct {
		name       string
		json       string
		wantReason string
	}{
		{"two vertices is not a polygon",
			`[{"id":"z1","name":"garden","polygon":[[0,0],[1,1]],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`,
			"vertices"},
		{"a coordinate outside 0..1",
			`[{"id":"z1","name":"garden","polygon":[[0,0],[1.2,0],[1.2,1],[0,1]],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`,
			"0..1"},
		{"a negative coordinate",
			`[{"id":"z1","name":"garden","polygon":[[-0.1,0],[1,0],[1,1],[0,1]],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`,
			"0..1"},
		{"a vertex that is not an [x,y] pair",
			`[{"id":"z1","name":"garden","polygon":[[0,0],[1,0],[1]],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`,
			"[x,y]"},
		{"no classes",
			`[{"id":"z1","name":"garden","polygon":[[0,0],[1,0],[1,1]],"classes":[],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`,
			"at least one class"},
		{"a class the worker drops",
			`[{"id":"z1","name":"garden","polygon":[[0,0],[1,0],[1,1]],"classes":["airplane"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`,
			"not detection-eligible"},
		{"an unknown rule",
			`[{"id":"z1","name":"garden","polygon":[[0,0],[1,0],[1,1]],"classes":["person"],"rule":"presence","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`,
			"not a zone rule"},
		{"loiter with no dwell",
			`[{"id":"z1","name":"garden","polygon":[[0,0],[1,0],[1,1]],"classes":["person"],"rule":"loiter","dwell_seconds":0,"severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`,
			"dwell_seconds >= 1"},
		{"severity out of range",
			`[{"id":"z1","name":"garden","polygon":[[0,0],[1,0],[1,1]],"classes":["person"],"rule":"intrusion","severity":9,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`,
			"severity"},
		{"confidence floor out of range",
			`[{"id":"z1","name":"garden","polygon":[[0,0],[1,0],[1,1]],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":1.5,"min_area_ratio":0.004,"enabled":true}]`,
			"min_confidence"},
		{"no id",
			`[{"id":"","name":"garden","polygon":[[0,0],[1,0],[1,1]],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`,
			"id is required"},
	}
	for _, tc := range cases {
		err := ValidateZonesJSON(tc.json)
		if err == nil {
			t.Fatalf("%s: expected a refusal", tc.name)
		}
		if !strings.Contains(err.Error(), tc.wantReason) {
			t.Fatalf("%s: expected the reason %q, got %q", tc.name, tc.wantReason, err.Error())
		}
		if !strings.Contains(err.Error(), "z1") && tc.wantReason != "id is required" {
			t.Fatalf("%s: the message must name the zone, got %q", tc.name, err.Error())
		}
	}
}

func TestZoneVertexBounds(t *testing.T) {
	poly := func(n int) string {
		var b strings.Builder
		b.WriteString(`[{"id":"z1","name":"garden","polygon":[`)
		for i := 0; i < n; i++ {
			x := float64(i) / float64(n)
			b.WriteString("[")
			b.WriteString(strings.TrimRight(strings.TrimRight(formatFloat(x), "0"), "."))
			b.WriteString(",")
			b.WriteString(strings.TrimRight(strings.TrimRight(formatFloat(1-x), "0"), "."))
			b.WriteString("]")
			if i < n-1 {
				b.WriteString(",")
			}
		}
		b.WriteString(`],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`)
		return b.String()
	}

	if err := ValidateZonesJSON(poly(ZoneMinVertices)); err != nil {
		t.Fatalf("3 vertices is a polygon: %v", err)
	}
	if err := ValidateZonesJSON(poly(ZoneMaxVertices)); err != nil {
		t.Fatalf("32 vertices is the documented ceiling: %v", err)
	}
	if err := ValidateZonesJSON(poly(ZoneMaxVertices + 1)); err == nil {
		t.Fatal("33 vertices must be refused")
	}
	if err := ValidateZonesJSON(poly(ZoneMinVertices - 1)); err == nil {
		t.Fatal("2 vertices must be refused")
	}
}

func TestZoneIDSAreUniquePerCamera(t *testing.T) {
	const dup = `[{"id":"z1","name":"a","polygon":[[0,0],[1,0],[1,1]],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true},` +
		`{"id":"z1","name":"b","polygon":[[0,0],[1,0],[1,1]],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}]`
	err := ValidateZonesJSON(dup)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("two zones with one id leave the worker's event_key ambiguous: %v", err)
	}
}

func TestZoneCountIsBounded(t *testing.T) {
	var parts []string
	for i := 0; i < ZoneMaxCount; i++ {
		parts = append(parts, `{"id":"z`+itoa(i)+`","name":"zone","polygon":[[0,0],[1,0],[1,1]],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}`)
	}
	if err := ValidateZonesJSON("[" + strings.Join(parts, ",") + "]"); err != nil {
		t.Fatalf("%d zones is the documented limit: %v", ZoneMaxCount, err)
	}
	parts = append(parts, `{"id":"overflow","name":"zone","polygon":[[0,0],[1,0],[1,1]],"classes":["person"],"rule":"intrusion","severity":2,"min_confidence":0.3,"min_area_ratio":0.004,"enabled":true}`)
	if err := ValidateZonesJSON("[" + strings.Join(parts, ",") + "]"); err == nil {
		t.Fatal("a rule set past the limit must be refused at the write")
	}
}

func TestZoneEmptyAndMalformedInput(t *testing.T) {
	// A camera with no zones holds `[]`, and an empty string is the same thing:
	// two spellings for "no rules" is a reader bug waiting to happen.
	for _, empty := range []string{"", "  ", "[]", "null"} {
		zones, err := ParseZones(empty)
		if err != nil {
			t.Fatalf("ParseZones(%q): %v", empty, err)
		}
		if len(zones) != 0 {
			t.Fatalf("ParseZones(%q) must answer no zones", empty)
		}
		if err := ValidateZonesJSON(empty); err != nil {
			t.Fatalf("ValidateZonesJSON(%q): %v", empty, err)
		}
	}

	if _, err := ParseZones(`{"id":"z1"}`); err == nil {
		t.Fatal("an object where an array belongs must be refused")
	}
	if _, err := ParseZones(`not json`); err == nil {
		t.Fatal("malformed JSON must be refused, not read as no zones")
	}

	encoded, err := ZonesToJSON(nil)
	if err != nil {
		t.Fatalf("ZonesToJSON(nil): %v", err)
	}
	if encoded != "[]" {
		t.Fatalf(`an empty rule set is stored as [], got %q`, encoded)
	}
}

func TestDetectionEligibleClasses(t *testing.T) {
	// The worker drops every other COCO class (nvr_detector IGNORED_CLASSES).
	// A zone armed on a dropped class can never fire, and an operator believes
	// it is armed, so the eligible set is validated at the write.
	for _, class := range DetectionClasses {
		if !ValidDetectionClass(class) {
			t.Fatalf("class %q must be eligible", class)
		}
	}
	for _, class := range []string{"airplane", "bird", "traffic light", "fire hydrant", "stop sign", "", "Person", "person "} {
		if ValidDetectionClass(class) {
			t.Fatalf("class %q must not be zone-eligible", class)
		}
	}
}

func TestZoneRulesAreTheTwoThePlanNames(t *testing.T) {
	if len(ZoneRules) != 2 || !ValidZoneRule(ZoneRuleIntrusion) || !ValidZoneRule(ZoneRuleLoiter) {
		t.Fatalf("v1 arms %v", ZoneRules)
	}
	if ValidZoneRule("enter") || ValidZoneRule("exit") || ValidZoneRule("") {
		t.Fatal("a rule the worker does not implement must not be armable")
	}
}

func formatFloat(f float64) string {
	// Fixed-point, enough digits for the test polygons.
	whole := int(f)
	frac := int((f - float64(whole)) * 1000)
	return itoa(whole) + "." + itoa(frac)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	if neg {
		return "-" + digits
	}
	return digits
}
