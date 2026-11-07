package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

// baseModels is the baseline schema. Only migration 001 may use AutoMigrate;
// every later migration must be a versioned, explicit change, so production
// schemas evolve reproducibly instead of diverging silently. An AutoMigrate
// baseline is a fine way to start; it is a bad way to keep going.
//
// The order is load-bearing in one place: `User` precedes `UserRole`. User's
// many2many association creates the `user_roles` join table (keyed on
// user_id+role_id), and the UserRole model then **adds its columns to that
// table**. In the other order, UserRole's own primary key lands as NOT NULL on
// a table the association writes without an id, and assigning a role fails.
var baseModels = []any{
	&models.User{},
	&models.Role{},
	&models.UserRole{},
	&models.Session{},
	&models.SSOProvider{},
	&models.PendingIdentity{},
	&models.Camera{},
	&models.Event{},
	&models.Segment{},
	&models.StorageBackend{},
	&models.StorageJob{},
	&models.Webhook{},
	&models.WebhookDelivery{},
	&models.Audit{},
	&models.Setting{},
}

// migration is one versioned, ordered schema change. Version numbers are
// contiguous and never reused; a migration, once shipped, is never edited.
type migration struct {
	version int64
	name    string
	fn      func(*gorm.DB) error
}

// appliedMigration is a row in schema_migrations.
type appliedMigration struct {
	Version   int64     `gorm:"primaryKey"`
	Name      string    `gorm:"not null"`
	AppliedAt time.Time `gorm:"not null"`
}

func (appliedMigration) TableName() string { return "schema_migrations" }

// migrations is the ordered, append-only list of schema changes. v1 is the
// AutoMigrate baseline; a new column arrives as v2, never as an edit to v1.
var migrations = []migration{
	{version: 1, name: "baseline_auto_migrate", fn: func(db *gorm.DB) error {
		return db.AutoMigrate(baseModels...)
	}},
	// v2 widens storage_jobs.id from uuid to text. A job row is keyed by its
	// object, and an event has two objects, so the queue holds them as
	// `<event_id>:clip` and `<event_id>:snapshot` — ids that are not uuids.
	// sqlite never enforced the column type, so the suite could not see it;
	// Postgres answered every clip and snapshot enqueue with
	// `invalid input syntax for type uuid`, which is P-08 arriving in the
	// dev stack exactly as the card predicted. The ALTER is a no-op where the
	// column is already text, and sqlite has no ALTER COLUMN, hence the
	// dialect guard.
	{version: 2, name: "storage_jobs_id_is_text", fn: func(db *gorm.DB) error {
		if db.Dialector.Name() != "postgres" {
			return nil
		}
		return db.Exec("ALTER TABLE storage_jobs ALTER COLUMN id TYPE text").Error
	}},
	// v3 names the frame the event boxes are pixels of (P-10). The worker
	// sends pixels of the detection tap and the row stored them without ever
	// saying which frame; the console could only borrow the size from the
	// camera row, which defaults to 0. The intake now resolves the tap size
	// (camera row, falling back to GO2RTC_MJPEG_*) and stores it per event,
	// so the frame survives a config change and outlives the row that named
	// it. Rows written before this migration keep 0 — "not named" — and a
	// consumer that finds 0 must not place the boxes.
	{version: 3, name: "events_det_frame", fn: func(db *gorm.DB) error {
		// A fresh database already has the columns — v1's AutoMigrate reads the
		// model, which carries them — so each ADD runs only where the column is
		// genuinely missing. sqlite and Postgres both accept ADD COLUMN with a
		// constant default; neither needs the table rebuilt for it.
		for _, col := range []string{"det_width", "det_height"} {
			if db.Migrator().HasColumn(&models.Event{}, col) {
				continue
			}
			if err := db.Exec("ALTER TABLE events ADD COLUMN " + col + " integer NOT NULL DEFAULT 0").Error; err != nil {
				return err
			}
		}
		return nil
	}},
}

// Migrate applies every pending migration, each inside its own transaction,
// recording it in schema_migrations. It is safe to call repeatedly.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&appliedMigration{}); err != nil {
		return fmt.Errorf("failed to create schema_migrations table: %w", err)
	}
	for _, m := range migrations {
		applied, err := isApplied(db, m.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		err = db.Transaction(func(tx *gorm.DB) error {
			if err := m.fn(tx); err != nil {
				return fmt.Errorf("migration %d (%s) failed: %w", m.version, m.name, err)
			}
			return tx.Create(&appliedMigration{Version: m.version, Name: m.name, AppliedAt: time.Now().UTC()}).Error
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// isApplied reports whether the given migration version is recorded.
func isApplied(db *gorm.DB, version int64) (bool, error) {
	var row appliedMigration
	err := db.Where("version = ?", version).First(&row).Error
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("failed to read schema_migrations: %w", err)
}

// AppliedVersions returns the versions recorded in schema_migrations, for
// diagnostics and tests.
func AppliedVersions(db *gorm.DB) ([]int64, error) {
	var rows []appliedMigration
	if err := db.Order("version ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Version)
	}
	return out, nil
}

// BaseModels exposes the baseline model set for the migration test, which
// asserts every one of them has a table: an unregistered model is a schema that
// works until the first request touches the missing table.
func BaseModels() []any { return baseModels }
