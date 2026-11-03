package media

import (
	"fmt"
	"os"
	"testing"
	"time"

	"nvr/server/models"
)

// Card S4-03 — segment discovery: ffmpeg's CSV segment list becomes manifest rows,
// on the server's clock, once per line, with the bytes the file on disk has.
//
// The suites play ffmpeg's side of the contract: the fake child writes the segment
// files and appends the `filename,start_time,end_time` lines ffmpeg writes when it
// closes a segment. The recorder's side — the tail, the offsets, the rows — is what
// is under test. Card S4-07 does the same against real ffmpeg.

// TestSegmentRowsComeFromTheServerClockAndTheCSV is the plan's clock rule made
// concrete: `start_ts` is the moment the supervisor started plus the offset ffmpeg
// reports. The file name carries no time, and nothing in the recorder reads one.
func TestSegmentRowsComeFromTheServerClockAndTheCSV(t *testing.T) {
	rec, _, spawns, clock := newTestRecorder(t)
	cam := addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	spawns.afterStart = writeThreeSegments(t, rec)
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "three segment rows", func() bool {
		return len(segmentsFor(t, rec.St, cam.ID)) == 3
	})

	rows := segmentsFor(t, rec.St, cam.ID)
	started := clock.Now()
	for i, row := range rows {
		wantStart := started.Add(time.Duration(float64(i) * 1.012 * float64(time.Second)))
		wantEnd := started.Add(time.Duration(float64(i+1) * 1.012 * float64(time.Second)))
		if !row.StartTS.Equal(wantStart) {
			t.Fatalf("segment %d: start_ts is %s, want %s (the supervisor's start time + ffmpeg's offset %f)",
				i, row.StartTS, wantStart, float64(i)*1.012)
		}
		if !row.EndTS.Equal(wantEnd) {
			t.Fatalf("segment %d: end_ts is %s, want %s", i, row.EndTS, wantEnd)
		}
		if d := row.Seconds - 1.012; d > 1e-6 || d < -1e-6 {
			t.Fatalf("segment %d: seconds is %f, want 1.012", i, row.Seconds)
		}
		if want := fmt.Sprintf("%s/%s-1-%d.mp4", rec.Cfg.SegmentsDir, cam.Slug, i); row.Path != want {
			t.Fatalf("segment %d: path is %q, want %q", i, row.Path, want)
		}
		if row.CameraID != cam.ID || row.Generation != 1 {
			t.Fatalf("segment %d: camera/generation is %s/%d", i, row.CameraID, row.Generation)
		}
		if row.Kind != models.SegmentKindContinuous {
			t.Fatalf("segment %d: kind is %q, want %q", i, row.Kind, models.SegmentKindContinuous)
		}
	}
	// Order: the manifest the scrubber draws is start_ts ASC.
	for i := 1; i < len(rows); i++ {
		if !rows[i-1].StartTS.Before(rows[i].StartTS) {
			t.Fatalf("segment %d does not start after segment %d", i, i-1)
		}
	}
}

// TestSegmentBytesComeFromTheFileOnDisk: the row reports the file, not a hope.
func TestSegmentBytesComeFromTheFileOnDisk(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	cam := addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	size := 4242
	spawns.afterStart = func(n int, argv []string) {
		list := flagValue(argv, "-segment_list")
		name := segmentName(argv, 0)
		writeSegmentFile(t, rec.Cfg.SegmentsDir, name, size)
		writeCSVLine(t, list, name, 0, 1)
	}
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "the segment row", func() bool {
		return len(segmentsFor(t, rec.St, cam.ID)) == 1
	})
	row := segmentsFor(t, rec.St, cam.ID)[0]
	if row.Bytes != int64(size) {
		t.Fatalf("bytes is %d, want %d (the file's own size)", row.Bytes, size)
	}
}

// TestSegmentRowReportsAFileTheRecorderCannotRead: a segment ffmpeg listed and the
// filesystem will not show is a row with zero bytes, stated. The manifest is the
// recorder's own record, and inventing a size for a missing file is how footage
// quietly disappears.
func TestSegmentRowReportsAFileTheRecorderCannotRead(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	cam := addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	spawns.afterStart = func(n int, argv []string) {
		list := flagValue(argv, "-segment_list")
		writeCSVLine(t, list, segmentName(argv, 0), 0, 1)
	}
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "the segment row", func() bool {
		return len(segmentsFor(t, rec.St, cam.ID)) == 1
	})
	row := segmentsFor(t, rec.St, cam.ID)[0]
	if row.Bytes != 0 {
		t.Fatalf("bytes is %d for a file that does not exist, want 0", row.Bytes)
	}
}

// TestSegmentTailIngestsEachLineOnce is the offset's whole purpose: the manifest
// insert happens once per line. A tail that re-reads writes duplicate rows, and a
// tail that reads a half-written line writes a row for a segment ffmpeg is still
// writing.
func TestSegmentTailIngestsEachLineOnce(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	cam := addCamera(t, rec.St, "porch", models.RecordModeContinuous)

	var list string
	spawns.afterStart = func(n int, argv []string) {
		list = flagValue(argv, "-segment_list")
		for i := 0; i < 2; i++ {
			name := segmentName(argv, i)
			writeSegmentFile(t, rec.Cfg.SegmentsDir, name, 1024+i)
			writeCSVLine(t, list, name, float64(i), float64(i)+1)
		}
		// A line ffmpeg has not finished: no newline yet. ffmpeg writes the line
		// when the segment closes, so this is the shape of a segment in progress.
		appendRaw(t, list, fmt.Sprintf("%s,2.000000,3.000000", segmentName(argv, 2)))
	}
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "the two completed segments", func() bool {
		return len(segmentsFor(t, rec.St, cam.ID)) == 2
	})
	// The half line is not a row.
	time.Sleep(150 * time.Millisecond)
	if rows := segmentsFor(t, rec.St, cam.ID); len(rows) != 2 {
		t.Fatalf("the tail ingested a line ffmpeg has not finished: %d rows", len(rows))
	}

	// Complete it: the tail picks it up from its offset, not from a re-read.
	appendRaw(t, list, "\n")
	waitFor(t, 5*time.Second, "the third segment", func() bool {
		return len(segmentsFor(t, rec.St, cam.ID)) == 3
	})
	time.Sleep(150 * time.Millisecond)
	rows := segmentsFor(t, rec.St, cam.ID)
	if len(rows) != 3 {
		t.Fatalf("the tail re-ingested lines: %d rows, want 3", len(rows))
	}
	seen := map[string]int{}
	for _, row := range rows {
		seen[row.Path]++
	}
	for path, n := range seen {
		if n != 1 {
			t.Fatalf("segment %s has %d rows; the tail wrote it more than once", path, n)
		}
	}
}

// TestSegmentTailResetsOnRestart: each spawn tails its own run. ffmpeg rewrites the
// list when it opens it, so a restarted recorder reads the new list from offset 0 —
// no skipping the first line of the new run, no re-inserting the old run's rows.
func TestSegmentTailResetsOnRestart(t *testing.T) {
	st := newTestStore(t)
	cfg := newTestConfig(t)
	cam := addCamera(t, st, "porch", models.RecordModeContinuous)

	// Run one writes two segments.
	first, _, firstSpawns, _ := newRecorderOn(t, st, cfg)
	firstSpawns.afterStart = func(n int, argv []string) {
		list := flagValue(argv, "-segment_list")
		for i := 0; i < 2; i++ {
			name := segmentName(argv, i)
			writeSegmentFile(t, cfg.SegmentsDir, name, 1024)
			writeCSVLine(t, list, name, float64(i), float64(i)+1)
		}
	}
	startRecorder(t, first)
	waitFor(t, 5*time.Second, "the first run's two rows", func() bool {
		return len(segmentsFor(t, st, cam.ID)) == 2
	})
	first.Close()

	// Run two: generation 2, and ffmpeg truncating the list at startup, as it does.
	second, _, secondSpawns, _ := newRecorderOn(t, st, cfg)
	secondSpawns.afterStart = func(n int, argv []string) {
		list := flagValue(argv, "-segment_list")
		if err := os.Truncate(list, 0); err != nil {
			t.Errorf("truncating the segment list: %v", err)
			return
		}
		for i := 0; i < 2; i++ {
			name := segmentName(argv, i)
			writeSegmentFile(t, cfg.SegmentsDir, name, 2048)
			writeCSVLine(t, list, name, float64(i), float64(i)+1)
		}
	}
	startRecorder(t, second)

	waitFor(t, 5*time.Second, "the restarted run's two rows", func() bool {
		return len(segmentsFor(t, st, cam.ID)) == 4
	})
	time.Sleep(150 * time.Millisecond)
	rows := segmentsFor(t, st, cam.ID)
	if len(rows) != 4 {
		t.Fatalf("after a restart the manifest has %d rows, want 4 (2 from each run, none re-ingested)", len(rows))
	}
	byGen := map[int]int{}
	for _, row := range rows {
		byGen[row.Generation]++
	}
	if byGen[1] != 2 || byGen[2] != 2 {
		t.Fatalf("rows by generation: %v, want 2 in generation 1 and 2 in generation 2", byGen)
	}
}

// TestSegmentListLineThatDoesNotParseIsReported: a tail that swallows rows is the
// failure mode of every file tail, so the count is said out loud.
func TestSegmentListLineThatDoesNotParseIsReported(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	cam := addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	logs := captureLogs(t, rec)
	spawns.afterStart = func(n int, argv []string) {
		list := flagValue(argv, "-segment_list")
		appendRaw(t, list, "this is not a segment line\n")
		name := segmentName(argv, 0)
		writeSegmentFile(t, rec.Cfg.SegmentsDir, name, 512)
		writeCSVLine(t, list, name, 0, 1)
	}
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "the good segment row", func() bool {
		return len(segmentsFor(t, rec.St, cam.ID)) == 1
	})
	waitFor(t, 2*time.Second, "the skipped-line log line", func() bool {
		return len(logs.containing("did not parse")) > 0
	})
}

// TestSegmentKindFollowsTheRecordMode: the always-on recorder writes `continuous`;
// footage recorded inside an event window is `event`. The kind is recorded and never
// used to exclude footage — the clip builder takes any kind.
func TestSegmentKindFollowsTheRecordMode(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	always := addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	onSignal := addCamera(t, rec.St, "garden", models.RecordModeEvents)

	spawns.afterStart = func(n int, argv []string) {
		list := flagValue(argv, "-segment_list")
		name := segmentName(argv, 0)
		writeSegmentFile(t, rec.Cfg.SegmentsDir, name, 1024)
		writeCSVLine(t, list, name, 0, 1)
	}
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "the continuous camera's segment", func() bool {
		return len(segmentsFor(t, rec.St, always.ID)) == 1
	})

	rec.OpenWindow(onSignal.ID)
	waitFor(t, 5*time.Second, "the events camera's segment", func() bool {
		return len(segmentsFor(t, rec.St, onSignal.ID)) == 1
	})

	if kind := segmentsFor(t, rec.St, always.ID)[0].Kind; kind != models.SegmentKindContinuous {
		t.Fatalf("a continuous camera wrote kind %q, want %q", kind, models.SegmentKindContinuous)
	}
	if kind := segmentsFor(t, rec.St, onSignal.ID)[0].Kind; kind != models.SegmentKindEvent {
		t.Fatalf("an events camera wrote kind %q, want %q", kind, models.SegmentKindEvent)
	}
}

// writeThreeSegments is ffmpeg's side of the contract for the clock test: three
// closed segments, named as the recorder's own output pattern says, with the
// offsets ffmpeg reports.
func writeThreeSegments(t *testing.T, rec *Recorder) func(n int, argv []string) {
	return func(n int, argv []string) {
		list := flagValue(argv, "-segment_list")
		for i := 0; i < 3; i++ {
			name := segmentName(argv, i)
			writeSegmentFile(t, rec.Cfg.SegmentsDir, name, 2048+i)
			writeCSVLine(t, list, name, float64(i)*1.012, float64(i+1)*1.012)
		}
	}
}

// appendRaw writes to a file without the newline the CSV parser needs, so a suite
// can produce a half-written line.
func appendRaw(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
