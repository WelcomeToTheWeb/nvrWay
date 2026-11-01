package models

import "time"

// Signal actions the detection worker reports, and the only values `Action`
// holds. The worker's state machine (open → extend → close) is mirrored by the
// server's event row; the action on the row is the last one it applied.
const (
	SignalActionOpen   = "open"
	SignalActionExtend = "extend"
	SignalActionClose  = "close"
)

// ValidSignalAction reports whether action is one the state machine understands.
func ValidSignalAction(action string) bool {
	return action == SignalActionOpen || action == SignalActionExtend || action == SignalActionClose
}

// Severities. A number, not a word, so a filter is a comparison and the web
// hook payload says the same thing the console draws.
const (
	SeverityLow    = 1
	SeverityMedium = 2
	SeverityHigh   = 3
)

// ValidSeverity reports whether severity is in 1..3.
func ValidSeverity(severity int) bool { return severity >= SeverityLow && severity <= SeverityHigh }

// Event is one detected occurrence on one camera, assembled from the worker's
// open/extend/close signals under a single `event_key`.
//
// The row is written on the **server's** clock: a signal's own timestamp never
// reaches the database, so host clock skew on the detection box cannot create
// events in the future. `StartedAt`/`EndedAt` are therefore comparable with
// segment times, which are also server-measured.
type Event struct {
	ID string `gorm:"type:uuid;primaryKey" json:"id"`
	// EventKey is the worker's identity for this occurrence
	// (`<camera_id>:<zone_id>:<rule>:<open_unix>`), and the dedupe key: the
	// worker buffers signals and re-sends, so the same open arrives twice.
	EventKey string `gorm:"uniqueIndex;not null;size:255" json:"event_key"`
	CameraID string `gorm:"type:uuid;not null;index" json:"camera_id"`

	// Action is the last signal applied, Open the state machine's state.
	Action    string    `gorm:"not null" json:"action"`
	StartedAt time.Time `gorm:"not null;index:idx_events_cam_started" json:"started_at"`
	EndedAt   time.Time `gorm:"not null;index:idx_events_cam_started" json:"ended_at"`
	Open      bool      `gorm:"not null;default:true;index" json:"open"`

	// Accumulated facts, as JSON text: the union of classes, zones and rules
	// that contributed, and the object boxes (capped, oldest trimmed first).
	ClassesJSON string `gorm:"type:text;not null;default:'[]'" json:"classes_json"`
	ZonesJSON   string `gorm:"type:text;not null;default:'[]'" json:"zones_json"`
	RulesJSON   string `gorm:"type:text;not null;default:'[]'" json:"rules_json"`
	ObjectsJSON string `gorm:"type:text;not null;default:'[]'" json:"objects_json"`

	Severity    int `gorm:"not null;default:1" json:"severity"`
	SignalCount int `gorm:"not null;default:0" json:"signal_count"`

	// Media: the remote key once uploaded, and the local path while it exists.
	// A snapshot with no clip is a normal outcome (a camera in `record_mode=off`
	// produces exactly that), not an error.
	SnapshotKey       string `gorm:"not null;default:''" json:"snapshot_key"`
	ClipKey           string `gorm:"not null;default:''" json:"clip_key"`
	LocalSnapshotPath string `gorm:"not null;default:''" json:"local_snapshot_path"`
	LocalClipPath     string `gorm:"not null;default:''" json:"local_clip_path"`

	// Nil is "not uploaded". The sweeper keeps local files while any of these
	// is nil, so an upload outage cannot delete the only copy.
	SnapshotUploadedAt *time.Time `json:"snapshot_uploaded_at"`
	ClipUploadedAt     *time.Time `json:"clip_uploaded_at"`

	ClipSeconds float64 `gorm:"not null;default:0" json:"clip_seconds"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the table name.
func (Event) TableName() string { return "events" }

// EventObject is one detected box as the worker reported it, kept on the event
// so the console can draw what the model saw at `ts = started_at`:
//
//	{"class":"person","conf":0.81,"box":[100,50,140,150]}
//
// Boxes are **pixels of the detection frame** (that is what the worker measures),
// so drawing them needs the frame size, which the camera row carries.
type EventObject struct {
	Class string    `json:"class"`
	Conf  float64   `json:"conf"`
	Box   []float64 `json:"box"`
}

// MaxEventObjects caps the boxes stored on one event, oldest trimmed first: a
// loitering person tracked for five minutes produces far more boxes than anyone
// will look at, and an event row that grows without bound is an event table
// that grows with it.
const MaxEventObjects = 200

// MediaUploadedAt/ClipUploaded return the upload stamps the sweeper and the
// playback proxy read. Kept as helpers because "is the media safe yet" is one
// question with two answers, and both are nil-checked in three places.
func (e *Event) SnapshotUploaded() bool { return e != nil && e.SnapshotUploadedAt != nil }
func (e *Event) ClipUploaded() bool     { return e != nil && e.ClipUploadedAt != nil }

// Objects answers the boxes recorded on the row, in the order the worker sent
// them. The console draws them on the snapshot; a column that is not readable
// JSON answers none, so one damaged row cannot make the event list unreadable.
func (e *Event) Objects() []EventObject {
	if e == nil {
		return nil
	}
	return JSONObjects(e.ObjectsJSON)
}
