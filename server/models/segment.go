package models

import "time"

// Segment kinds. `continuous` is what the always-on recorder writes; `event`
// is a segment written inside an event window (`record_mode=events`). The clip
// builder selects overlapping segments of **any** kind, which is why the kind
// is recorded and never used to exclude footage.
const (
	SegmentKindContinuous = "continuous"
	SegmentKindEvent      = "event"
)

// ValidSegmentKind reports whether kind is a segment kind.
func ValidSegmentKind(kind string) bool {
	return kind == SegmentKindContinuous || kind == SegmentKindEvent
}

// Segment is one mp4 the recorder wrote, with the window of time it covers.
//
// StartTS/EndTS are **server-measured**: the recorder takes ffmpeg's per-segment
// start/end offsets from the segment list ffmpeg writes and adds them to the
// time its own supervisor started. No camera clock is involved, and no clock is
// parsed out of a file name, so the timeline an operator scrubs is the timeline
// the server recorded.
type Segment struct {
	ID string `gorm:"type:uuid;primaryKey" json:"id"`
	// Generation is the recorder supervisor's per-camera spawn counter, so
	// restarts after a crash continue numbering instead of colliding.
	// idx_seg_cam_gen_start is the recorder's own read: "what is the last
	// segment this camera's current generation wrote".
	CameraID   string `gorm:"type:uuid;not null;index:idx_seg_cam_gen_start" json:"camera_id"`
	Generation int    `gorm:"not null;index:idx_seg_cam_gen_start" json:"generation"`

	Path string `gorm:"not null" json:"path"`

	StartTS    time.Time  `gorm:"not null;index:idx_seg_cam_gen_start" json:"start_ts"`
	EndTS      time.Time  `gorm:"not null" json:"end_ts"`
	Seconds    float64    `gorm:"not null;default:0" json:"seconds"`
	Bytes      int64      `gorm:"not null;default:0" json:"bytes"`
	Kind       string     `gorm:"not null;default:'continuous'" json:"kind"`
	UploadedAt *time.Time `json:"uploaded_at"`
	RemoteKey  string     `gorm:"not null;default:''" json:"remote_key"`

	// LocalDeletedAt is set when the local file is gone and the row is kept for
	// the events that reference it: the timeline can show what was recorded
	// after the footage itself aged out.
	LocalDeletedAt *time.Time `json:"local_deleted_at"`

	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

// TableName pins the table name.
func (Segment) TableName() string { return "segments" }

// HasLocalFile reports whether the media is readable from disk.
func (s *Segment) HasLocalFile() bool { return s != nil && s.LocalDeletedAt == nil }

// Uploaded reports whether the segment is safe on the storage tier.
func (s *Segment) Uploaded() bool { return s != nil && s.UploadedAt != nil }
