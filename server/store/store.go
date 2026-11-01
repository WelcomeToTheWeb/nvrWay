package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store provides database access through separate repositories.
type Store struct {
	DB *gorm.DB

	Settings *SettingStore

	Cameras  *CameraStore
	Events   *EventStore
	Segments *SegmentStore

	// Storage is the media tier: the destinations, and the queue that carries
	// objects to them.
	StorageBackends *StorageBackendStore
	StorageJobs     *StorageJobStore

	Webhooks          *WebhookStore
	WebhookDeliveries *WebhookDeliveryStore

	Users             *UserStore
	Roles             *RoleStore
	UserRoles         *UserRoleStore
	Sessions          *SessionStore
	SSOProviders      *SSOProviderStore
	PendingIdentities *PendingIdentityStore

	// Audit is the record of mutating acts, written by the handlers.
	Audit *AuditStore
}

// Zero values against a column default. gorm omits a zero-valued field from an
// INSERT when the column declares `default:`, and the database then supplies
// the default — so for a `not null;default:true` column, a row created with
// false arrives as true. Every Create below that can legitimately take a false
// value writes it explicitly after the insert, so "created disabled" means
// disabled, and an operator's setting is not decided by a schema default.
func writeZeroColumns(db *gorm.DB, row any, columns map[string]any) error {
	if len(columns) == 0 {
		return nil
	}
	return db.Model(row).Updates(columns).Error
}

// OpenDB opens a database from a DSN, choosing the driver from the scheme:
//
//	postgresql://… | postgres://…   Postgres (the deployment)
//	sqlite://path                  sqlite at a path (scripts/dev.sh, e2e)
//	file:… | :memory:              sqlite, verbatim (tests)
//
// sqlite is not a development convenience hidden behind a flag: it is the
// documented second driver, the one every store test runs on, so the SQL the
// handlers issue is written to work on both. Where the two genuinely differ
// (a row lock), the method says so in its comment.
func OpenDB(dsn string) (*gorm.DB, error) {
	dsn = strings.TrimSpace(dsn)
	switch {
	case dsn == "":
		return nil, errors.New("store: database url is empty (set DATABASE_URL)")
	case strings.HasPrefix(dsn, "sqlite://"):
		return openSQLite(strings.TrimPrefix(dsn, "sqlite://"))
	case dsn == ":memory:", strings.HasPrefix(dsn, "file:"):
		return openSQLite(dsn)
	default:
		return openPostgres(dsn)
	}
}

func openPostgres(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	sqlDB, err := db.DB()
	if err == nil {
		sqlDB.SetMaxOpenConns(100)
		sqlDB.SetMaxIdleConns(20)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
		sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	}
	return db, nil
}

func openSQLite(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}
	// One connection, always. Two things depend on it: an in-memory database is
	// per-connection, so a pool would hand each caller a different (empty)
	// database; and sqlite has one writer, so a pool trades a queue for
	// "database is locked" errors at the worst possible moment (the uploader
	// tick claiming a job while the recorder inserts a segment).
	sqlDB, err := db.DB()
	if err == nil {
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file::memory:") {
		// A file-backed database is shared with scripts/ and the dev stack, so
		// a writer that finds the lock waits instead of failing the insert.
		if err := db.Exec("PRAGMA busy_timeout = 5000").Error; err != nil {
			return nil, fmt.Errorf("sqlite busy_timeout: %w", err)
		}
	}
	return db, nil
}

// NewWithDB builds a Store over an existing *gorm.DB, migrating it and seeding
// the built-in roles. Tests use this; so does any caller that already owns a
// database handle.
func NewWithDB(db *gorm.DB) (*Store, error) {
	return buildStore(db)
}

// New connects to the database named by dsn, migrates it, and returns a Store.
func New(dsn string) (*Store, error) {
	db, err := OpenDB(dsn)
	if err != nil {
		return nil, err
	}
	return buildStore(db)
}

// buildStore is the single place a Store is assembled, so `New` and
// `NewWithDB` cannot drift apart — a Store built one way with migrations and
// seeded roles and the other way without them is the difference between a
// passing test suite and a production install with no roles.
func buildStore(db *gorm.DB) (*Store, error) {
	if err := Migrate(db); err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	st := &Store{
		DB:                db,
		Settings:          &SettingStore{db: db},
		Cameras:           &CameraStore{db: db},
		Events:            &EventStore{db: db},
		Segments:          &SegmentStore{db: db},
		StorageBackends:   &StorageBackendStore{db: db},
		StorageJobs:       &StorageJobStore{db: db},
		Webhooks:          &WebhookStore{db: db},
		WebhookDeliveries: &WebhookDeliveryStore{db: db},
		Users:             &UserStore{db: db},
		Roles:             &RoleStore{db: db},
		UserRoles:         &UserRoleStore{db: db},
		Sessions:          &SessionStore{db: db},
		SSOProviders:      &SSOProviderStore{db: db},
		PendingIdentities: &PendingIdentityStore{db: db},
		Audit:             &AuditStore{db: db},
	}

	if err := st.Roles.SeedBuiltinRoles(); err != nil {
		return nil, fmt.Errorf("failed to seed builtin roles: %w", err)
	}
	return st, nil
}
