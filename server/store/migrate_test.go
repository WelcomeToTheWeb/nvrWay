package store

import (
	"testing"

	"nvr/server/models"
)

func TestMigrateIsIdempotent(t *testing.T) {
	st := newTestStore(t)

	for i := 0; i < 3; i++ {
		if err := Migrate(st.DB); err != nil {
			t.Fatalf("migrate pass %d: %v", i, err)
		}
	}

	// Every migration is recorded once, in order. A migration re-running on a
	// live install is a schema change applied twice, and "CREATE INDEX IF NOT
	// EXISTS" is not the reason the ledger exists.
	versions, err := AppliedVersions(st.DB)
	if err != nil {
		t.Fatalf("applied versions: %v", err)
	}
	if len(versions) != len(migrations) {
		t.Fatalf("expected %d recorded migrations after 4 passes, got %v", len(migrations), versions)
	}

	var count int64
	if err := st.DB.Model(&models.Camera{}).Count(&count).Error; err != nil {
		t.Fatalf("count cameras after re-migration: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the re-migration to leave rows alone, found %d cameras", count)
	}
}

func TestEveryBaseModelHasATable(t *testing.T) {
	st := newTestStore(t)

	// An unregistered model is a schema that works until the first request
	// touches the table nobody created.
	for _, model := range BaseModels() {
		if !st.DB.Migrator().HasTable(model) {
			t.Fatalf("model %T has no table", model)
		}
	}
}

func TestSegmentsCarriesTheRecorderIndex(t *testing.T) {
	st := newTestStore(t)

	// idx_seg_cam_gen_start is the recorder's read after a restart ("what did
	// this generation write"), and the plan names its columns: a restart that
	// scans the whole segment table is a restart that stalls the first minute of
	// recording on a busy install.
	indexes, err := st.DB.Migrator().GetIndexes(&models.Segment{})
	if err != nil {
		t.Fatalf("get indexes: %v", err)
	}
	var fields []string
	for _, idx := range indexes {
		if idx.Name() != "idx_seg_cam_gen_start" {
			continue
		}
		fields = idx.Columns()
	}
	if len(fields) == 0 {
		t.Fatalf("idx_seg_cam_gen_start missing from %v", indexes)
	}
	want := []string{"camera_id", "generation", "start_ts"}
	if len(fields) != len(want) {
		t.Fatalf("idx_seg_cam_gen_start covers %v, want %v", fields, want)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Fatalf("idx_seg_cam_gen_start covers %v, want %v", fields, want)
		}
	}
}

func TestUserRoleJoinKeepsTheAssociationWritable(t *testing.T) {
	st := newTestStore(t)

	// The migration order comment in migrate.go is about this: User creates the
	// many2many join table, UserRole adds columns to it. If that inverts, an
	// INSERT into user_roles without an id hits a NOT NULL primary key and
	// assigning a role fails at runtime, not at migration time.
	if err := st.DB.Table("user_roles").Create(map[string]any{
		"user_id": "11111111-1111-1111-1111-111111111111",
		"role_id": "22222222-2222-2222-2222-222222222222",
	}).Error; err != nil {
		t.Fatalf("the many2many write path must not need an id: %v", err)
	}

	var count int64
	if err := st.DB.Model(&models.UserRole{}).Count(&count).Error; err != nil {
		t.Fatalf("count user_roles: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the raw association insert to land, count = %d", count)
	}
}
