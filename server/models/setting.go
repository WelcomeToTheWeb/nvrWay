package models

import "time"

// Setting is one installation fact the server owns and re-reads.
//
// It is a key/value table rather than a column per fact because these are
// process state, not entity data: a recorder's segment numbering must survive a
// restart, and "did this installation already create its break-glass admin"
// must survive an operator deleting that account. Both are one row with a
// timestamp, and both are read by code that must not guess.
type Setting struct {
	Key   string `gorm:"primaryKey;size:128" json:"key"`
	Value string `gorm:"type:text;not null;default:''" json:"value"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `gorm:"index" json:"updated_at"`
}

// TableName pins the table name.
func (Setting) TableName() string { return "settings" }

// Setting keys the server writes. They are constants so a key cannot be
// invented twice with two spellings.
const (
	// SettingBootstrapAdminSeededAt records when the break-glass local admin was
	// created. Seeding is gated on this row, not on the user table: an operator
	// who deletes the admin account must not get it back on the next boot.
	SettingBootstrapAdminSeededAt = "bootstrap_admin_seeded_at"

	// SettingDetectorHeartbeat is the last time the detection worker reported
	// in. Detector liveness is data the server answers with, not a port it
	// probes: GET /api/healthz reads this row.
	SettingDetectorHeartbeat = "detector_heartbeat_at"

	// RecorderGenPrefix + camera id is the recorder's per-camera spawn counter,
	// so a restart continues segment numbering instead of colliding with files
	// that already exist.
	RecorderGenPrefix = "recorder_gen_"

	// SFTPHostKeyPrefix + backend id is a pinned SSH host key, recorded on
	// first use. A changed pin fails Ping: a host key that changed is a
	// different machine, and footage must not be uploaded to it.
	SFTPHostKeyPrefix = "sftp_hostkey_"
)

// RecorderGenKey builds the recorder counter key for a camera.
func RecorderGenKey(cameraID string) string { return RecorderGenPrefix + cameraID }

// SFTPHostKeyKey builds the host-key pin key for a storage backend.
func SFTPHostKeyKey(backendID string) string { return SFTPHostKeyPrefix + backendID }
