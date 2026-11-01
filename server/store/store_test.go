package store

import (
	"path/filepath"
	"testing"

	"nvr/server/models"
)

func TestNewOnMemorySqliteMigratesAndSeedsRoles(t *testing.T) {
	st := newTestStore(t)

	if st.DB.Dialector.Name() != "sqlite" {
		t.Fatalf("expected the sqlite driver for :memory:, got %q", st.DB.Dialector.Name())
	}

	versions, err := AppliedVersions(st.DB)
	if err != nil {
		t.Fatalf("applied versions: %v", err)
	}
	if len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("expected exactly migration v1 applied, got %v", versions)
	}

	roles, err := st.Roles.ListRoles()
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	names := map[string]bool{}
	for _, r := range roles {
		names[r.Name] = true
	}
	for _, want := range []string{models.RoleAdmin, models.RoleOperator, models.RoleViewer} {
		if !names[want] {
			t.Fatalf("expected the built-in role %q to be seeded, got %v", want, names)
		}
	}
}

func TestOpenDBChoosesDriverFromScheme(t *testing.T) {
	t.Run("sqlite scheme", func(t *testing.T) {
		db, err := OpenDB("sqlite://" + filepath.Join(t.TempDir(), "nvr.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		if db.Dialector.Name() != "sqlite" {
			t.Fatalf("expected sqlite, got %q", db.Dialector.Name())
		}
	})

	t.Run("bare memory dsn", func(t *testing.T) {
		db, err := OpenDB(":memory:")
		if err != nil {
			t.Fatalf("open :memory:: %v", err)
		}
		if db.Dialector.Name() != "sqlite" {
			t.Fatalf("expected sqlite, got %q", db.Dialector.Name())
		}
	})

	t.Run("empty dsn is refused", func(t *testing.T) {
		if _, err := OpenDB("  "); err == nil {
			t.Fatal("expected an empty DATABASE_URL to be refused, not opened")
		}
	})

	t.Run("postgres scheme reaches postgres", func(t *testing.T) {
		// Port 1 is closed on this host, so the point of the statement is the
		// driver that reports the failure: a Postgres DSN must not be quietly
		// opened as sqlite, which is how a deployment ends up running on an
		// embedded database nobody chose.
		_, err := OpenDB("postgresql://nvr:nvr@127.0.0.1:1/nvr?sslmode=disable")
		if err == nil {
			t.Fatal("expected a connection failure")
		}
	})
}

func TestNewWithDBMigrates(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st, err := NewWithDB(db)
	if err != nil {
		t.Fatalf("new with db: %v", err)
	}
	camera := &models.Camera{Slug: "front-door", Name: "Front Door", SourceURI: "rtsp://x", RecordMode: models.RecordModeContinuous}
	if err := st.Cameras.Create(camera); err != nil {
		t.Fatalf("create camera: %v", err)
	}
	if camera.ID == "" {
		t.Fatal("expected the store to mint the camera id")
	}
}

func TestEveryRepositoryIsWired(t *testing.T) {
	// A Store field left nil is a nil-pointer panic at the first request that
	// touches it, in production, on a path no test of that store exercises.
	st := newTestStore(t)
	checks := []struct {
		name string
		ptr  any
	}{
		{"Settings", st.Settings},
		{"Cameras", st.Cameras},
		{"Events", st.Events},
		{"Segments", st.Segments},
		{"StorageBackends", st.StorageBackends},
		{"StorageJobs", st.StorageJobs},
		{"Webhooks", st.Webhooks},
		{"WebhookDeliveries", st.WebhookDeliveries},
		{"Users", st.Users},
		{"Roles", st.Roles},
		{"UserRoles", st.UserRoles},
		{"Sessions", st.Sessions},
		{"SSOProviders", st.SSOProviders},
		{"PendingIdentities", st.PendingIdentities},
		{"Audit", st.Audit},
	}
	for _, c := range checks {
		if c.ptr == nil {
			t.Fatalf("Store.%s is nil", c.name)
		}
	}
}
