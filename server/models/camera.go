package models

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Recording modes (plan decision 3). `continuous` records always; `events`
// records inside detect-triggered windows; `off` records nothing and yields
// snapshot-only events, which is a stated behaviour, not a failure.
const (
	RecordModeContinuous = "continuous"
	RecordModeEvents     = "events"
	RecordModeOff        = "off"
)

// RecordModes is every recording mode.
var RecordModes = []string{RecordModeContinuous, RecordModeEvents, RecordModeOff}

// ValidRecordMode reports whether mode is a recording mode.
func ValidRecordMode(mode string) bool {
	for _, m := range RecordModes {
		if m == mode {
			return true
		}
	}
	return false
}

// slugPattern is the camera slug contract: the slug **is** the go2rtc stream
// name, so it must be safe to drop into a YAML key, an RTSP path and a URL
// query without escaping — lowercase alphanumerics, starting with one, 2–32
// chars, hyphens allowed inside. There is no mapping table, so nothing else
// can rename it.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// ValidSlug reports whether slug can be used verbatim as a stream name.
func ValidSlug(slug string) bool { return slugPattern.MatchString(slug) }

// Camera is a video source, its recording mode, and its detection rules.
type Camera struct {
	ID   string `gorm:"type:uuid;primaryKey" json:"id"`
	Slug string `gorm:"uniqueIndex;not null;size:32" json:"slug"`
	Name string `gorm:"not null" json:"name"`

	// SourceURI is handed to go2rtc verbatim as the stream source
	// (`rtsp://user:pass@host/…`, `ffmpeg:…`, `exec:…`).
	SourceURI string `gorm:"not null" json:"source_uri"`

	RecordMode       string `gorm:"not null;default:'continuous';index" json:"record_mode"`
	DetectionEnabled bool   `gorm:"not null;default:false" json:"detection_enabled"`

	// The detection tap: go2rtc transcodes the camera to this mjpeg stream
	// (<slug>_det) and the worker reads frames from it. Zero falls back to the
	// GO2RTC_MJPEG_* defaults, so a camera row does not have to repeat them.
	DetectionFPS    int `gorm:"not null;default:0" json:"detection_fps"`
	DetectionWidth  int `gorm:"not null;default:0" json:"detection_width"`
	DetectionHeight int `gorm:"not null;default:0" json:"detection_height"`

	// Camera-wide floors a zone may raise but not lower: sensitivity is the
	// confidence floor and MinAreaRatio the size floor for a box to count.
	Sensitivity  float64 `gorm:"not null;default:0.45" json:"sensitivity"`
	MinAreaRatio float64 `gorm:"not null;default:0.004" json:"min_area_ratio"`

	// ZonesJSON is the zone rule set as JSON text (see Zone / ValidateZonesJSON
	// for the exact wire shape). Text, not JSONB: the server reads it, the
	// worker reads it, and the console writes it — one shape, three readers.
	ZonesJSON string `gorm:"type:text;not null;default:'[]'" json:"zones_json"`

	// Per-camera retention. Zero means "use the installation default".
	LocalRetentionDays  int `gorm:"not null;default:0" json:"local_retention_days"`
	RemoteRetentionDays int `gorm:"not null;default:0" json:"remote_retention_days"`

	Active bool `gorm:"not null;default:true;index" json:"active"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the table name.
func (Camera) TableName() string { return "cameras" }

// Retention: the camera wins, the installation default is the floor. Zero on a
// camera row is "not set", not "delete immediately" — which is why the fallback
// lives here rather than in each caller.
func (c *Camera) EffectiveLocalRetention(defaultDays int) int {
	if c == nil || c.LocalRetentionDays <= 0 {
		return defaultDays
	}
	return c.LocalRetentionDays
}

// EffectiveRemoteRetention resolves the camera's remote window. The
// installation default may be 0 ("keep remote forever"), and a camera that set
// its own number is honoured exactly, so 0 means the same here as there.
func (c *Camera) EffectiveRemoteRetention(defaultDays int) int {
	if c == nil || c.RemoteRetentionDays <= 0 {
		return defaultDays
	}
	return c.RemoteRetentionDays
}

// Zone is one detection rule of one camera. The JSON shape below **is** the wire
// contract between the console, the API and the detection worker:
//
//	[{"id":"z1","name":"driveway","polygon":[[0.12,0.35],[0.8,0.35],[0.8,0.95],[0.12,0.95]],
//	  "classes":["person","car"],"rule":"intrusion","dwell_seconds":30,"severity":3,
//	  "min_confidence":0.35,"min_area_ratio":0.004,"enabled":true}]
//
// Polygon coordinates are **normalized** to 0..1 of the detection frame, so
// changing a camera's resolution does not silently invalidate a rule. Polygons
// are closed implicitly and need 3–32 vertices.
type Zone struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Polygon  [][]float64 `json:"polygon"`
	Classes  []string    `json:"classes"`
	Rule     string      `json:"rule"`
	DwellSec int         `json:"dwell_seconds"`
	Severity int         `json:"severity"`
	// MinConfidence / MinAreaRatio are per-zone floors a zone may **raise**
	// above the camera's Sensitivity / MinAreaRatio. 0 means "use the camera's".
	MinConfidence float64 `json:"min_confidence"`
	MinAreaRatio  float64 `json:"min_area_ratio"`
	Enabled       bool    `json:"enabled"`
}

// Zone rules. `intrusion` fires when a matched class centroid is inside the
// polygon; `loiter` fires when the same track stays inside continuously for
// `dwell_seconds` (leaving resets the timer).
const (
	ZoneRuleIntrusion = "intrusion"
	ZoneRuleLoiter    = "loiter"
)

// ZoneRules is every zone rule.
var ZoneRules = []string{ZoneRuleIntrusion, ZoneRuleLoiter}

// ValidZoneRule reports whether rule is a zone rule.
func ValidZoneRule(rule string) bool {
	for _, r := range ZoneRules {
		if r == rule {
			return true
		}
	}
	return false
}

// Detection classes: the COCO subset detection is allowed to act on. A zone
// naming any other COCO class is refused at write time, because the worker
// drops those boxes (nvr_detector.model.IGNORED_CLASSES) and a zone that can
// never fire is a rule an operator believes is armed.
var DetectionClasses = []string{
	"person", "bicycle", "car", "motorcycle", "bus", "truck", "dog", "cat", "bear",
}

// ValidDetectionClass reports whether a zone may be armed on that class.
func ValidDetectionClass(class string) bool {
	for _, c := range DetectionClasses {
		if c == class {
			return true
		}
	}
	return false
}

// Zone bounds, stated as constants so the API, the console and the worker
// validate against the same numbers.
const (
	ZoneMinVertices = 3
	ZoneMaxVertices = 32
	ZoneMaxCount    = 16
	ZoneMaxSeverity = 3
)

// ZoneError names a zone and the reason it is invalid, so the API's 422 body
// can say which zone to fix instead of "your JSON is wrong".
type ZoneError struct {
	ZoneID string
	Reason string
}

func (e ZoneError) Error() string {
	if e.ZoneID == "" {
		return e.Reason
	}
	return fmt.Sprintf("zone %s: %s", e.ZoneID, e.Reason)
}

// ParseZones decodes a ZonesJSON payload. An empty string is the empty rule
// set, which is what a camera with no zones holds.
func ParseZones(zonesJSON string) ([]Zone, error) {
	trimmed := strings.TrimSpace(zonesJSON)
	if trimmed == "" || trimmed == "null" {
		return []Zone{}, nil
	}
	var zones []Zone
	if err := json.Unmarshal([]byte(trimmed), &zones); err != nil {
		return nil, fmt.Errorf("zone json: %w", err)
	}
	return zones, nil
}

// ZonesToJSON encodes a zone list the way the column stores it: `[]` when
// empty, so the column never holds an empty string no reader agrees on.
func ZonesToJSON(zones []Zone) (string, error) {
	if len(zones) == 0 {
		return "[]", nil
	}
	encoded, err := json.Marshal(zones)
	if err != nil {
		return "", fmt.Errorf("zone json: %w", err)
	}
	return string(encoded), nil
}

// ValidateZonesJSON validates a whole rule set: every zone, and the set as
// something a camera can actually hold. It answers the first problem found,
// named by zone, so a write either lands or says what to fix.
func ValidateZonesJSON(zonesJSON string) error {
	zones, err := ParseZones(zonesJSON)
	if err != nil {
		return err
	}
	if len(zones) > ZoneMaxCount {
		return ZoneError{Reason: fmt.Sprintf("a camera holds at most %d zones, got %d", ZoneMaxCount, len(zones))}
	}
	seen := map[string]bool{}
	for i := range zones {
		if err := zones[i].Validate(); err != nil {
			return err
		}
		if seen[zones[i].ID] {
			return ZoneError{ZoneID: zones[i].ID, Reason: "duplicate zone id"}
		}
		seen[zones[i].ID] = true
	}
	return nil
}

// Validate checks one zone against the contract.
func (z Zone) Validate() error {
	id := strings.TrimSpace(z.ID)
	if id == "" {
		return ZoneError{Reason: "id is required"}
	}
	if len(z.Polygon) < ZoneMinVertices {
		return ZoneError{ZoneID: id, Reason: fmt.Sprintf("polygon needs %d-%d vertices, got %d",
			ZoneMinVertices, ZoneMaxVertices, len(z.Polygon))}
	}
	if len(z.Polygon) > ZoneMaxVertices {
		return ZoneError{ZoneID: id, Reason: fmt.Sprintf("polygon needs %d-%d vertices, got %d",
			ZoneMinVertices, ZoneMaxVertices, len(z.Polygon))}
	}
	for vi, vertex := range z.Polygon {
		if len(vertex) != 2 {
			return ZoneError{ZoneID: id, Reason: fmt.Sprintf("vertex %d is not an [x,y] pair", vi)}
		}
		for _, v := range vertex {
			if v < 0 || v > 1 {
				return ZoneError{ZoneID: id, Reason: fmt.Sprintf(
					"vertex %d coordinate %g is outside 0..1 (polygon coordinates are normalized)", vi, v)}
			}
		}
	}
	if len(z.Classes) == 0 {
		return ZoneError{ZoneID: id, Reason: "at least one class is required"}
	}
	for _, class := range z.Classes {
		if !ValidDetectionClass(class) {
			return ZoneError{ZoneID: id, Reason: fmt.Sprintf(
				"class %q is not detection-eligible (person, bicycle, car, motorcycle, bus, truck, dog, cat, bear)", class)}
		}
	}
	if !ValidZoneRule(z.Rule) {
		return ZoneError{ZoneID: id, Reason: fmt.Sprintf("rule %q is not a zone rule (intrusion, loiter)", z.Rule)}
	}
	if z.Rule == ZoneRuleLoiter && z.DwellSec < 1 {
		return ZoneError{ZoneID: id, Reason: "loiter needs dwell_seconds >= 1"}
	}
	if z.Severity < 1 || z.Severity > ZoneMaxSeverity {
		return ZoneError{ZoneID: id, Reason: fmt.Sprintf("severity must be 1..%d, got %d", ZoneMaxSeverity, z.Severity)}
	}
	if z.MinConfidence < 0 || z.MinConfidence > 1 {
		return ZoneError{ZoneID: id, Reason: fmt.Sprintf("min_confidence must be 0..1, got %g", z.MinConfidence)}
	}
	if z.MinAreaRatio < 0 || z.MinAreaRatio > 1 {
		return ZoneError{ZoneID: id, Reason: fmt.Sprintf("min_area_ratio must be 0..1, got %g", z.MinAreaRatio)}
	}
	return nil
}
