package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

func segment(cameraID string, generation, number int, start time.Time, seconds float64, kind string) *models.Segment {
	return &models.Segment{
		CameraID:   cameraID,
		Generation: generation,
		Path:       filepath.Join("/var/lib/nvr/segments", "cam-"+itoa(generation)+"-"+itoa(number)+".mp4"),
		StartTS:    start,
		EndTS:      start.Add(time.Duration(seconds * float64(time.Second))),
		Seconds:    seconds,
		Bytes:      1024 * int64(number+1),
		Kind:       kind,
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func TestSegmentCreateAndRangeManifest(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	base := minutesAgo(30)
	for i := 0; i < 6; i++ {
		if err := st.Segments.Create(segment(camera.ID, 1, i, base.Add(time.Duration(i*5)*time.Minute), 5, models.SegmentKindContinuous)); err != nil {
			t.Fatalf("create segment %d: %v", i, err)
		}
	}

	// A window that starts mid-segment needs that segment: the clip builder
	// selects overlapping segments, ordered by start.
	cover, err := st.Segments.ListRange(camera.ID, base.Add(2*time.Minute+30*time.Second), base.Add(16*time.Minute), 0)
	if err != nil {
		t.Fatalf("list range: %v", err)
	}
	if len(cover) != 3 {
		t.Fatalf("expected 3 overlapping segments, got %d", len(cover))
	}
	if !cover[0].StartTS.Before(cover[1].StartTS) || !cover[1].StartTS.Before(cover[2].StartTS) {
		t.Fatal("segments must come back in start order — the concat list is positional")
	}

	// A window ending exactly at a segment's end_ts does not include the next
	// one: segments are half-open on the far edge.
	tail, err := st.Segments.ListRange(camera.ID, base.Add(25*time.Minute), base.Add(30*time.Minute), 0)
	if err != nil {
		t.Fatalf("list range: %v", err)
	}
	if len(tail) != 1 {
		t.Fatalf("expected the one segment ending at the window edge, got %d", len(tail))
	}
}

func TestSegmentRangeSpansAnyKind(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	base := minutesAgo(20)

	if err := st.Segments.Create(segment(camera.ID, 1, 0, base, 5, models.SegmentKindContinuous)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Segments.Create(segment(camera.ID, 1, 1, base.Add(5*time.Minute), 5, models.SegmentKindEvent)); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.Segments.ListRange(camera.ID, base, base.Add(11*time.Minute), 0)
	if err != nil {
		t.Fatalf("list range: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("a clip window must be built from every kind of footage, got %d segments", len(got))
	}
}

func TestSegmentRangeIsScopedToOneCamera(t *testing.T) {
	st := newTestStore(t)
	one := seedCamera(t, st, "front-door")
	two := seedCamera(t, st, "side-gate")
	base := minutesAgo(10)

	for _, c := range []*models.Camera{one, two} {
		if err := st.Segments.Create(segment(c.ID, 1, 0, base, 5, models.SegmentKindContinuous)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	got, err := st.Segments.ListRange(one.ID, base.Add(-time.Minute), base.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("list range: %v", err)
	}
	if len(got) != 1 || got[0].CameraID != one.ID {
		t.Fatalf("the timeline manifest must not leak another camera's segments, got %d", len(got))
	}
}

func TestSegmentMaxSegmentNumberContinuesAfterACrash(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	base := minutesAgo(20)

	for i := 0; i < 4; i++ {
		if err := st.Segments.Create(segment(camera.ID, 3, i, base.Add(time.Duration(i*5)*time.Minute), 5, models.SegmentKindContinuous)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	// A previous generation wrote a higher number: the restart must continue
	// this generation's numbering, not the old one's.
	if err := st.Segments.Create(segment(camera.ID, 2, 99, base.Add(-time.Hour), 5, models.SegmentKindContinuous)); err != nil {
		t.Fatalf("create: %v", err)
	}

	last, err := st.Segments.MaxSegmentNumber(camera.ID, 3)
	if err != nil {
		t.Fatalf("max segment number: %v", err)
	}
	if last != 3 {
		t.Fatalf("expected generation 3's last number 3, got %d", last)
	}

	fresh, err := st.Segments.MaxSegmentNumber(camera.ID, 7)
	if err != nil {
		t.Fatalf("max segment number: %v", err)
	}
	if fresh != -1 {
		t.Fatalf("an unseen generation has no last segment, got %d", fresh)
	}

	if _, err := st.Segments.MaxSegmentNumber("", 1); err == nil {
		t.Fatal("expected an error for an unscoped query")
	}
}

func TestSegmentUploadAndLocalDeleteStamps(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	seg := segment(camera.ID, 1, 0, minutesAgo(10), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := st.Segments.MarkUploaded(seg.ID, "nvr/front-door/2026/11/01/cam-1-0.mp4"); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}
	loaded, err := st.Segments.GetByID(seg.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !loaded.Uploaded() || loaded.RemoteKey == "" {
		t.Fatalf("expected the upload stamp, got %+v", loaded)
	}
	if !loaded.HasLocalFile() {
		t.Fatal("upload does not delete the local file: that is the sweeper's job")
	}

	if err := st.Segments.MarkLocalDeleted(seg.ID); err != nil {
		t.Fatalf("mark local deleted: %v", err)
	}
	loaded, err = st.Segments.GetByID(seg.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.HasLocalFile() {
		t.Fatal("expected the local file to be recorded as gone, with the row kept")
	}
	if loaded.UploadedAt == nil {
		t.Fatal("the upload stamp must survive the local delete")
	}

	if err := st.Segments.MarkUploaded("missing", "k"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found for an unknown segment, got %v", err)
	}
	if err := st.Segments.MarkLocalDeleted("missing"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found for an unknown segment, got %v", err)
	}
}

func TestSweeperCandidatesSplitOnUploaded(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	// 20 days old, uploaded: a local-delete candidate.
	upped := segment(camera.ID, 1, 0, daysAgo(20), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(upped); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Segments.MarkUploaded(upped.ID, "nvr/front-door/upped.mp4"); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}

	// 20 days old, NOT uploaded: the storage tier has been down. Deleting this
	// destroys the only copy, so it is not a candidate at the local window.
	stranded := segment(camera.ID, 1, 1, daysAgo(20), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(stranded); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 2 days old, uploaded: still inside the window.
	recent := segment(camera.ID, 1, 2, daysAgo(2), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(recent); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Segments.MarkUploaded(recent.ID, "nvr/front-door/recent.mp4"); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}

	cutoff := daysAgo(14)
	deletable, err := st.Segments.ListLocalOlderThan(cutoff, 100)
	if err != nil {
		t.Fatalf("list local older than: %v", err)
	}
	if len(deletable) != 1 || deletable[0].ID != upped.ID {
		t.Fatalf("expected only the uploaded old segment, got %+v", deletable)
	}

	protected, err := st.Segments.ListUnuploadedOlderThan(cutoff, 100)
	if err != nil {
		t.Fatalf("list unuploaded older than: %v", err)
	}
	if len(protected) != 1 || protected[0].ID != stranded.ID {
		t.Fatalf("the un-uploaded segment belongs to the protected window, got %+v", protected)
	}
}

func TestSegmentDeleteByIDs(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	seg := segment(camera.ID, 1, 0, daysAgo(20), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create: %v", err)
	}

	deleted, err := st.Segments.DeleteByIDs([]string{seg.ID, "not-a-uuid"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected exactly one row deleted, got %d", deleted)
	}

	none, err := st.Segments.DeleteByIDs(nil)
	if err != nil || none != 0 {
		t.Fatalf("an empty sweep deletes nothing: %d rows, %v", none, err)
	}
}

func TestSegmentCountsAreTheLiveRecordingCheck(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	other := seedCamera(t, st, "side-gate")

	if err := st.Segments.Create(segment(camera.ID, 1, 0, minutesAgo(3), 5, models.SegmentKindContinuous)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Segments.Create(segment(camera.ID, 1, 1, minutesAgo(2), 5, models.SegmentKindContinuous)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Segments.Create(segment(other.ID, 1, 0, minutesAgo(1), 5, models.SegmentKindContinuous)); err != nil {
		t.Fatalf("create: %v", err)
	}

	total, unuploaded, err := st.Segments.CountByCamera(camera.ID)
	if err != nil {
		t.Fatalf("count by camera: %v", err)
	}
	if total != 2 || unuploaded != 2 {
		t.Fatalf("expected 2 segments / 2 unuploaded for the camera, got %d/%d", total, unuploaded)
	}

	all, unupAll, err := st.Segments.CountByCamera("")
	if err != nil {
		t.Fatalf("count all: %v", err)
	}
	if all != 3 || unupAll != 3 {
		t.Fatalf("expected 3 segments installation-wide, got %d/%d", all, unupAll)
	}

	// "is recording live" is a count of rows *written* recently, measured on the
	// server's clock — which is what scripts/verify.sh watches increase over a
	// 15 s window.
	since, err := st.Segments.CountSince(time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("count since: %v", err)
	}
	if since != 3 {
		t.Fatalf("expected all three rows (all just written), got %d", since)
	}
	older, err := st.Segments.CountSince(time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatalf("count since: %v", err)
	}
	if older != 0 {
		t.Fatalf("nothing is written in the future, got %d", older)
	}
}

func TestSegmentKindDefaultsToContinuous(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	// A recorder that forgot to name the kind still produces readable footage:
	// the clip builder selects any kind, and the sweeper's local sweep is
	// defined on continuous segments.
	seg := &models.Segment{
		CameraID: camera.ID, Generation: 1, Path: "/var/lib/nvr/segments/cam-1-0.mp4",
		StartTS: minutesAgo(5), EndTS: minutesAgo(4), Seconds: 5, Bytes: 1,
	}
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err := st.Segments.GetByID(seg.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Kind != models.SegmentKindContinuous {
		t.Fatalf("expected the default kind, got %q", loaded.Kind)
	}
}
