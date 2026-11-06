package media

// Card S4-06 — `recorder.status`: one broadcast per completed segment, carrying
// the four fields the console's status line reads.
//
// "One per segment, not per frame" is a real question about this recorder, not a
// tautology: the tail drains on a ticker, so the two ways to get it wrong are a
// status per tick (an announcement for a segment that did not close) and a status
// per frame (a feed that spends itself on one number). Both are counted here
// against the fake child that plays ffmpeg's side, exactly as the segment suites
// do — the recorder's own call site in `tailSegments`'s `drain` is what is under
// test.

import (
	"testing"
	"time"

	"nvr/server/models"
)

// TestStatusBroadcastIsOnePerSegment is the card's first criterion, counted: three
// closed segments, three broadcasts, and the payload says which camera, in which
// mode, at what segment length, and when the last one closed.
func TestStatusBroadcastIsOnePerSegment(t *testing.T) {
	rec, hub, spawns, _ := newTestRecorder(t)
	cam := addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	spawns.afterStart = writeThreeSegments(t, rec)
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "three segment rows", func() bool {
		return len(segmentsFor(t, rec.St, cam.ID)) == 3
	})
	waitFor(t, 5*time.Second, "three recorder.status broadcasts", func() bool {
		return len(hub.recorderStatuses()) == 3
	})

	// The count is the criterion, so it is asserted as a maximum too. The tail
	// keeps ticking after the last segment closes and the fake child stays alive,
	// so a per-tick announcement would add a fourth, fifth, sixth broadcast to a
	// run that closed three segments.
	time.Sleep(200 * time.Millisecond)
	statuses := hub.recorderStatuses()
	if len(statuses) != 3 {
		t.Fatalf("%d recorder.status broadcasts for 3 closed segments, want 3 (one per segment, not per tick and not per frame)", len(statuses))
	}

	rows := segmentsFor(t, rec.St, cam.ID)
	for i, s := range statuses {
		if s["camera_id"] != cam.ID {
			t.Fatalf("broadcast %d: camera_id is %v, want %s", i, s["camera_id"], cam.ID)
		}
		if s["mode"] != models.RecordModeContinuous {
			t.Fatalf("broadcast %d: mode is %v, want %q", i, s["mode"], models.RecordModeContinuous)
		}
		// The configured length, not the measured one: the line says what the
		// recorder is set to cut, and a segment that ran long (ffmpeg closes on
		// the nearest keyframe) does not change what the installation is doing.
		seconds, ok := s["segment_seconds"].(int)
		if !ok || seconds != rec.Cfg.SegmentSeconds {
			t.Fatalf("broadcast %d: segment_seconds is %v, want the configured %d", i, s["segment_seconds"], rec.Cfg.SegmentSeconds)
		}
		// The row's end on the server clock, in the format every other timestamp
		// in this product travels in. The console compares this with an event's
		// start, so a per-broadcast `time.Now()` would put two clocks on screen.
		want := rows[i].EndTS.UTC().Format(time.RFC3339)
		if s["last_segment_at"] != want {
			t.Fatalf("broadcast %d: last_segment_at is %v, want %s (segment %d's end_ts)", i, s["last_segment_at"], want, i)
		}
	}
}

// TestStatusBroadcastNamesNoCameraThatIsNotRecording: the payload is keyed by
// `camera_id` — an id, not a slug, because a slug can be reused — so the wall can
// put the line on the right tile. A camera that has closed no segment gets no line
// at all, and `off` (which never spawns) is the camera that proves the broadcast
// follows footage rather than following the row.
func TestStatusBroadcastNamesNoCameraThatIsNotRecording(t *testing.T) {
	rec, hub, spawns, _ := newTestRecorder(t)
	recording := addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	quiet := addCamera(t, rec.St, "gate", models.RecordModeOff)

	spawns.afterStart = func(n int, argv []string) {
		list := flagValue(argv, "-segment_list")
		name := segmentName(argv, 0)
		writeSegmentFile(t, rec.Cfg.SegmentsDir, name, 1024)
		writeCSVLine(t, list, name, 0, 1)
	}
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "the recording camera's broadcast", func() bool {
		return len(hub.recorderStatuses()) == 1
	})
	time.Sleep(200 * time.Millisecond)

	statuses := hub.recorderStatuses()
	if len(statuses) != 1 {
		t.Fatalf("%d broadcasts, want 1: one camera records, one is off", len(statuses))
	}
	if statuses[0]["camera_id"] != recording.ID {
		t.Fatalf("the broadcast names %v, want the recording camera %s", statuses[0]["camera_id"], recording.ID)
	}
	for _, s := range statuses {
		if s["camera_id"] == quiet.ID {
			t.Fatalf("an `off` camera got a recorder.status: %v", s)
		}
	}
}

// TestStatusBroadcastFollowsARowNotACSVLine: the announcement is made after the
// manifest insert succeeds, so a list line that never became a row produces no
// status. The two lines land in **different ticks** on purpose — the bad one
// alone in one, the good one in the next — so the good segment's broadcast is
// counted on its own and cannot be the bad line's, mislabelled. The console's
// line claims a segment closed; a line that did not parse, and so wrote no
// footage and no row, must not be allowed to make that claim.
func TestStatusBroadcastFollowsARowNotACSVLine(t *testing.T) {
	rec, hub, spawns, _ := newTestRecorder(t)
	cam := addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	logs := captureLogs(t, rec)
	spawns.afterStart = func(n int, argv []string) {
		appendRaw(t, flagValue(argv, "-segment_list"), "this is not a segment line\n")
	}
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "the skipped-line log line", func() bool {
		return len(logs.containing("did not parse")) > 0
	})
	if got := len(hub.recorderStatuses()); got != 0 {
		t.Fatalf("%d broadcasts, want 0: a list line that wrote no row announced nothing", got)
	}

	// The good line, in a later tick, from the same run.
	argv := spawns.lastArgv()
	list := flagValue(argv, "-segment_list")
	name := segmentName(argv, 0)
	writeSegmentFile(t, rec.Cfg.SegmentsDir, name, 512)
	writeCSVLine(t, list, name, 0, 1)

	waitFor(t, 5*time.Second, "the one good segment's broadcast", func() bool {
		return len(hub.recorderStatuses()) == 1
	})
	time.Sleep(200 * time.Millisecond)
	if got := len(hub.recorderStatuses()); got != 1 {
		t.Fatalf("%d broadcasts, want 1: the bad line still gets nothing", got)
	}
	if rows := segmentsFor(t, rec.St, cam.ID); len(rows) != 1 {
		t.Fatalf("%d segment rows, want 1", len(rows))
	}
}
