package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

func TestSettingSetIsAnUpsert(t *testing.T) {
	st := newTestStore(t)

	// The recorder's generation counter is written from ticks and restarts. A
	// first-write-only path would silently drop every write after the first,
	// and a restart would resume numbering from a value nobody wrote.
	for i := 1; i <= 5; i++ {
		if err := st.Settings.Set(models.RecorderGenKey("cam-1"), itoa(i)); err != nil {
			t.Fatalf("set: %v", err)
		}
	}

	value, err := st.Settings.Get(models.RecorderGenKey("cam-1"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if value != "5" {
		t.Fatalf("expected the last write to win, got %q", value)
	}

	var rows int64
	if err := st.DB.Model(&models.Setting{}).Count(&rows).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected one row for five writes, got %d", rows)
	}

	if err := st.Settings.Set("", "value"); err == nil {
		t.Fatal("expected a keyless write to be refused")
	}
}

func TestSettingGetAndGetInt(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.Settings.Get("never-written"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found for an absent key, got %v", err)
	}

	fallback, err := st.Settings.GetOr("never-written", "default")
	if err != nil || fallback != "default" {
		t.Fatalf("expected the fallback for an absent key, got %q (%v)", fallback, err)
	}

	if err := st.Settings.Set(models.SettingBootstrapAdminSeededAt, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("set: %v", err)
	}
	value, err := st.Settings.GetOr(models.SettingBootstrapAdminSeededAt, "default")
	if err != nil || value != "2026-01-01T00:00:00Z" {
		t.Fatalf("expected the stored value, got %q (%v)", value, err)
	}

	if err := st.Settings.Set(models.RecorderGenKey("cam-2"), "42"); err != nil {
		t.Fatalf("set: %v", err)
	}
	n, err := st.Settings.GetInt(models.RecorderGenKey("cam-2"))
	if err != nil || n != 42 {
		t.Fatalf("expected 42, got %d (%v)", n, err)
	}

	// A generation counter read as 0 starts numbering over and overwrites
	// files, so an unparseable value is an error, not a default.
	if err := st.Settings.Set(models.RecorderGenKey("cam-3"), "not-a-number"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := st.Settings.GetInt(models.RecorderGenKey("cam-3")); err == nil {
		t.Fatal("expected an unparseable counter to be an error")
	}
	if _, err := st.Settings.GetInt("absent"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found for an absent counter, got %v", err)
	}
}

func TestSettingListKeysIsPrefixScoped(t *testing.T) {
	st := newTestStore(t)

	for _, key := range []string{
		models.RecorderGenKey("cam-1"),
		models.RecorderGenKey("cam-2"),
		models.SFTPHostKeyKey("backend-1"),
		models.SettingDetectorHeartbeat,
	} {
		if err := st.Settings.Set(key, "x"); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}

	gens, err := st.Settings.ListKeys(models.RecorderGenPrefix)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(gens) != 2 {
		t.Fatalf("expected the two recorder counters, got %+v", gens)
	}
	pins, err := st.Settings.ListKeys(models.SFTPHostKeyPrefix)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("expected the one pinned host key, got %+v", pins)
	}

	if err := st.Settings.Delete(models.SettingDetectorHeartbeat); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.Settings.Delete(models.SettingDetectorHeartbeat); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found on the second delete, got %v", err)
	}
}

func TestSettingKeyNamesAreTheOnesThePlanNames(t *testing.T) {
	// Step 7's sweeper, Step 4's recorder and Step 9's health endpoint read
	// these keys by name. A spelling change here is a silent behaviour change
	// there, so the strings are asserted.
	if models.RecorderGenKey("cam-1") != "recorder_gen_cam-1" {
		t.Fatalf("unexpected recorder key %q", models.RecorderGenKey("cam-1"))
	}
	if models.SFTPHostKeyKey("b-1") != "sftp_hostkey_b-1" {
		t.Fatalf("unexpected host-key key %q", models.SFTPHostKeyKey("b-1"))
	}
	if models.SettingBootstrapAdminSeededAt != "bootstrap_admin_seeded_at" {
		t.Fatalf("unexpected bootstrap key %q", models.SettingBootstrapAdminSeededAt)
	}
	if models.SettingDetectorHeartbeat != "detector_heartbeat_at" {
		t.Fatalf("unexpected heartbeat key %q", models.SettingDetectorHeartbeat)
	}
}

func TestSettingUpdatedAtIsReadable(t *testing.T) {
	st := newTestStore(t)

	// "when was the break-glass admin seeded" is a fact an operator reads in
	// the settings page, so the row carries its own timestamp.
	if err := st.Settings.Set(models.SettingBootstrapAdminSeededAt, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("set: %v", err)
	}
	var row models.Setting
	if err := st.DB.First(&row, "key = ?", models.SettingBootstrapAdminSeededAt).Error; err != nil {
		t.Fatalf("read row: %v", err)
	}
	if row.UpdatedAt.IsZero() {
		t.Fatal("expected the row to stamp itself")
	}
}
