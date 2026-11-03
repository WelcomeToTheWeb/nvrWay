package media

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nvr/server/models"
)

// Card S4-04 — crash handling. ffmpeg dying is the ordinary case, not the edge: a
// camera on a PoE switch that renegotiates, a source that stops, an encoder that
// runs out of buffer. The recorder's job is to say so, restart, and keep the
// numbering intact.

var errFakeExit = errors.New("exit status 1")

// TestCrashRestartsAndNumberingContinues: the process died, the supervisor starts
// another one, and the new one's names continue where the dead one's stopped.
// ffmpeg restarts its own segment counter at 0, so without both halves of this the
// restarted recorder overwrites the files it just wrote.
func TestCrashRestartsAndNumberingContinues(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	cam := addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	logs := captureLogs(t, rec)

	spawns.afterStart = writeSegments(t, rec, 2, 1.0)
	// Die after the segments have had time to land in the manifest.
	spawns.exitWith = func(n int) (error, time.Duration) { return errFakeExit, 150 * time.Millisecond }
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "the first generation's two segments", func() bool {
		return len(segmentsFor(t, rec.St, cam.ID)) >= 2
	})
	waitFor(t, 5*time.Second, "the crash to restart the recorder", func() bool {
		return spawns.spawns() >= 2
	})

	if got := generation(t, rec.St, cam.ID); got != 2 {
		t.Fatalf("generation after a crash is %d, want 2", got)
	}
	argv := spawns.allArgv()[1]
	if got, want := argv[len(argv)-1], filepath.Join(rec.Cfg.SegmentsDir, "porch-2-%d.mp4"); got != want {
		t.Fatalf("the restarted recorder writes generation 1's files: pattern %q, want %q (the generation is in the file name)", got, want)
	}
	if got := flagValue(argv, "-segment_start_number"); got != "2" {
		t.Fatalf("-segment_start_number is %q, want 2 (generation 1 wrote 0 and 1; ffmpeg would restart at 0)", got)
	}
	if len(logs.containing("ffmpeg exited")) == 0 {
		t.Fatalf("a crash that restarted left no log line: %v", logs.all())
	}
}

// TestCrashBackoffCapsAtSixtySeconds: the plan's restart policy — 10 s, doubling,
// 60 s ceiling. A recorder that hammers a dead source every second is a recorder
// that turns one broken camera into a machine-wide ffmpeg storm.
func TestCrashBackoffCapsAtSixtySeconds(t *testing.T) {
	rec, _, spawns, clock := newTestRecorder(t)
	addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	rec.BackoffStart = 10 * time.Second
	rec.BackoffMax = 60 * time.Second

	spawns.exitWith = func(n int) (error, time.Duration) { return errFakeExit, 0 }
	slept := newSleepRecorder(clock)
	slept.SetOn(rec)
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "five restart waits", func() bool {
		return len(slept.durations()) >= 5
	})

	want := []time.Duration{
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
		60 * time.Second,
	}
	got := slept.durations()[:len(want)]
	for i, d := range want {
		if got[i] != d {
			t.Fatalf("restart wait %d is %s, want %s (the whole sequence: %v)", i, got[i], d, slept.durations()[:len(want)])
		}
	}
}

// TestCrashBackoffResetsOnAHealthySegmentTick: the camera that recovered restarts
// at the first step, not at the ceiling it climbed to while it was down. The
// segment is the proof, not the process starting.
func TestCrashBackoffResetsOnAHealthySegmentTick(t *testing.T) {
	rec, _, spawns, clock := newTestRecorder(t)
	cam := addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	rec.BackoffStart = 10 * time.Second
	rec.BackoffMax = 60 * time.Second

	// Spawns 0, 1 and 2 die at once, so the backoff climbs 10 → 20 → 40. Spawn 3
	// writes a segment and then dies: healthy, so the next wait is 10 s again.
	spawns.afterStart = func(n int, argv []string) {
		if n == 3 {
			writeSegments(t, rec, 1, 1.0)(n, argv)
		}
	}
	spawns.exitWith = func(n int) (error, time.Duration) {
		if n == 3 {
			return errFakeExit, 200 * time.Millisecond
		}
		return errFakeExit, 0
	}
	slept := newSleepRecorder(clock)
	slept.SetOn(rec)
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "the healthy spawn's segment", func() bool {
		return len(segmentsFor(t, rec.St, cam.ID)) >= 1
	})
	waitFor(t, 5*time.Second, "the restart waits after it", func() bool {
		d := slept.durations()
		return len(d) >= 5 && d[3] == 10*time.Second
	})

	d := slept.durations()
	want := []time.Duration{
		10 * time.Second, // spawn 0 died
		20 * time.Second, // spawn 1 died
		40 * time.Second, // spawn 2 died
		10 * time.Second, // spawn 3 produced a segment: the window starts over
	}
	for i, w := range want {
		if d[i] != w {
			t.Fatalf("restart wait %d is %s, want %s (full sequence: %v)", i, d[i], w, d[:len(want)])
		}
	}
}

// TestCrashCameraStatusNamesTheTruth is the machine surface's rule: a camera's
// state is what the server is doing about it, not what the configuration says it
// should be doing. A dead recorder keeping the tile green — and keeping the
// configured source in `producers` — is exactly the failure the plan names.
func TestCrashCameraStatusNamesTheTruth(t *testing.T) {
	rec, hub, spawns, _ := newTestRecorder(t)
	cam := addCamera(t, rec.St, "porch", models.RecordModeContinuous)

	spawns.afterStart = writeSegments(t, rec, 1, 1.0)
	spawns.exitWith = func(n int) (error, time.Duration) { return errFakeExit, 200 * time.Millisecond }
	startRecorder(t, rec)

	// Recording: the announcement names the source this process is reading — the
	// sidecar's stream, not the camera's own RTSP URL from the row.
	//
	// The assertions walk the announcement log rather than reading its newest
	// entry. The recorder restarts as fast as the fake clock allows — the 10 s
	// backoff is a clock advance, not a wait — so at any instant the suite polls,
	// the newest announcement is close to a coin flip between the two truths. What
	// is never a coin flip is their order: the offline one comes after the
	// recording one, and only because the process died.
	recIdx := -1
	waitFor(t, 5*time.Second, "the recording announcement", func() bool {
		statuses := hub.cameraStatusLog(cam.ID)
		for i, status := range statuses {
			if status["online"] == true {
				recIdx = i
				return true
			}
		}
		return false
	})
	status := hub.cameraStatusLog(cam.ID)[recIdx]
	producers, _ := status["producers"].([]string)
	if len(producers) != 1 || producers[0] != rec.Cfg.Go2rtcRTSPURL+"/porch" {
		t.Fatalf("while recording, producers is %v, want [%s] (the input ffmpeg is actually reading)",
			producers, rec.Cfg.Go2rtcRTSPURL+"/porch")
	}
	if producers[0] == cam.SourceURI {
		t.Fatalf("producers repeats the camera row's intent (%q), not the source being read", producers[0])
	}

	// Dead: the announcement that follows says offline, with no producers. An
	// empty list, not the configured source and not a null the console has to
	// defend against.
	offIdx := -1
	waitFor(t, 5*time.Second, "an offline announcement after the recording one", func() bool {
		statuses := hub.cameraStatusLog(cam.ID)
		for i := recIdx + 1; i < len(statuses); i++ {
			if statuses[i]["online"] == false {
				offIdx = i
				return true
			}
		}
		return false
	})
	status = hub.cameraStatusLog(cam.ID)[offIdx]
	if status["slug"] != cam.Slug {
		t.Fatalf("the offline announcement names %v, want the slug", status["slug"])
	}
	if producers, ok := status["producers"].([]string); !ok || len(producers) != 0 {
		t.Fatalf("an offline camera announces producers %v, want an empty list", status["producers"])
	}
}

// TestCrashDuringShutdownDoesNotRestart: the last thing a shutting-down recorder
// does is announce a camera as offline, and the second-to-last is start ffmpeg
// again. Both are wrong, so neither happens.
func TestCrashDuringShutdownDoesNotRestart(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	spawns.exitWith = func(n int) (error, time.Duration) { return errFakeExit, 0 }
	startRecorder(t, rec)

	waitFor(t, 5*time.Second, "the first crash", func() bool { return spawns.spawns() >= 1 })
	rec.Close()
	before := spawns.spawns()

	time.Sleep(300 * time.Millisecond)
	if after := spawns.spawns(); after != before {
		t.Fatalf("a closed recorder restarted ffmpeg after Close (%d → %d)", before, after)
	}
}

// writeSegments is ffmpeg's side of the contract at N one-second segments: the
// files, and the list that names them. The list is **rewritten**, not appended
// to — ffmpeg opens `-segment_list` fresh when it starts, so a restarted run's
// list holds only that run's segments, which is what lets the tail start each
// spawn at offset 0 without re-ingesting the last run's rows.
func writeSegments(t *testing.T, rec *Recorder, n int, seconds float64) func(int, []string) {
	return func(spawn int, argv []string) {
		list := flagValue(argv, "-segment_list")
		if err := os.Truncate(list, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("truncating the segment list %s: %v", list, err)
			return
		}
		for i := 0; i < n; i++ {
			name := segmentName(argv, i)
			writeSegmentFile(t, rec.Cfg.SegmentsDir, name, 1024+i)
			writeCSVLine(t, list, name, float64(i)*seconds, float64(i+1)*seconds)
		}
	}
}
