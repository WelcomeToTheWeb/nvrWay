package store

import (
	"context"
	"testing"
	"time"

	"nvr/server/models"
)

// newTestStore is every store test's fixture: the sqlite path the plan names
// (`store.New(":memory:")`), migrated and seeded exactly as a production boot
// does it. No test builds a schema by hand — a schema only tests build is a
// schema production does not have.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := st.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return st
}

// seedCamera inserts a camera row directly (store tests test stores, not the
// camera API, which has its own tests in Step 3).
func seedCamera(t *testing.T, st *Store, slug string) *models.Camera {
	t.Helper()
	camera := &models.Camera{
		Slug:             slug,
		Name:             "Camera " + slug,
		SourceURI:        "rtsp://cam.example/" + slug,
		RecordMode:       models.RecordModeContinuous,
		DetectionEnabled: true,
		DetectionFPS:     5,
		DetectionWidth:   640,
		DetectionHeight:  360,
		Sensitivity:      0.45,
		MinAreaRatio:     0.004,
		ZonesJSON:        `[{"id":"porch","name":"porch","polygon":[[0.05,0.35],[0.95,0.35],[0.95,0.98],[0.05,0.98]],"classes":["person"],"rule":"loiter","dwell_seconds":4,"severity":3,"min_confidence":0.35,"min_area_ratio":0.004,"enabled":true}]`,
		Active:           true,
	}
	if err := st.Cameras.Create(camera); err != nil {
		t.Fatalf("seed camera %s: %v", slug, err)
	}
	return camera
}

// daysAgo is the retention tests' clock: sweeps compare against a cutoff, and
// "15 days ago" must be readable in the test, not computed inline everywhere.
func daysAgo(n int) time.Time { return time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour) }

func minutesAgo(n int) time.Time { return time.Now().UTC().Add(-time.Duration(n) * time.Minute) }

// ctx is the context every store write in these tests takes: store methods take
// a context so a cancelled request stops its statement mid-flight.
func ctx() context.Context { return context.Background() }
