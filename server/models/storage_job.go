package models

import "time"

// Job kinds name the kind of media the job carries.
const (
	JobKindSegment  = "segment"
	JobKindClip     = "clip"
	JobKindSnapshot = "snapshot"
)

// ValidJobKind reports whether kind is a job kind.
func ValidJobKind(kind string) bool {
	return kind == JobKindSegment || kind == JobKindClip || kind == JobKindSnapshot
}

// Job actions: what to do with the object named by `ID`.
const (
	JobActionUpload       = "upload"
	JobActionDeleteLocal  = "delete_local"
	JobActionDeleteRemote = "delete_remote"
)

// ValidJobAction reports whether action is a job action.
func ValidJobAction(action string) bool {
	return action == JobActionUpload || action == JobActionDeleteLocal || action == JobActionDeleteRemote
}

// Job states. `pending` is queued (and retryable), `uploading` is claimed by a
// worker, `uploaded` is done, `failed` is the queue's dead-letter state after
// JobMaxAttempts failures — visible on GET /api/storage/queue?state=failed and
// in the storage.queue WebSocket message.
const (
	JobStatePending   = "pending"
	JobStateUploading = "uploading"
	JobStateUploaded  = "uploaded"
	JobStateFailed    = "failed"
)

// ValidJobState reports whether the name is a queue state. The queue endpoint's
// `?state=` filter is validated with it, so a typo answers 400 instead of an
// empty page that reads as a healthy queue.
func ValidJobState(state string) bool {
	return state == JobStatePending || state == JobStateUploading ||
		state == JobStateUploaded || state == JobStateFailed
}

// JobMaxAttempts is the point a job stops being retried and becomes a number
// an operator sees. The uploader's backoff is exponential and capped, so 20
// attempts is days of trying, not minutes.
const JobMaxAttempts = 20

// StorageJob is one object's next obligation to the storage tier.
//
// `ID` **is** the segment's or event's id, so an object has one job row that
// states what the queue owes it (upload, then delete_local, then delete_remote)
// and enqueue is idempotent: enqueuing the same object twice cannot create a
// pile of duplicate uploads, which is what a tick-driven queue left to itself
// becomes.
//
// The column is text, not uuid: an event's two objects are `<event_id>:clip`
// and `<event_id>:snapshot`, and those ids are not uuids. The baseline shipped
// the column as uuid, sqlite never enforced it, and Postgres rejected every
// clip and snapshot job — migration v2 widens existing schemas, and this tag
// keeps new ones born right (P-08).
type StorageJob struct {
	ID   string `gorm:"type:text;primaryKey" json:"id"`
	Kind string `gorm:"not null;index" json:"kind"`

	// Target is the local path (upload) or the remote key (delete) the action
	// applies to.
	Target    string `gorm:"not null;default:''" json:"target"`
	BackendID string `gorm:"type:uuid;not null;index" json:"backend_id"`
	Action    string `gorm:"not null" json:"action"`

	State    string `gorm:"not null;default:'pending';index:idx_jobs_claimable" json:"state"`
	Attempts int    `gorm:"not null;default:0" json:"attempts"`
	// WorkerID is the uploader tick that claimed the job. It is written by the
	// claim statement, so a job stuck in `uploading` names who took it.
	WorkerID      string     `gorm:"not null;default:''" json:"worker_id"`
	LastError     string     `gorm:"type:text;not null;default:''" json:"last_error"`
	NextAttemptAt *time.Time `gorm:"index:idx_jobs_claimable" json:"next_attempt_at"`

	RemoteKey string `gorm:"not null;default:''" json:"remote_key"`
	Bytes     int64  `gorm:"not null;default:0" json:"bytes"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the table name.
func (StorageJob) TableName() string { return "storage_jobs" }
