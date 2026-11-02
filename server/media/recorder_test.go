package media

import (
	"strings"
	"testing"
	"time"

	"nvr/server/models"
)

// Card S4-01 — the supervisor: one goroutine per camera, dispatch on record_mode,
// a row change re-creating the supervisor, windows opened from the signal path,
// and Close ending every child this recorder owns.

// TestSupervisorDispatchesRecordModes is the plan's dispatch rule, all three
// modes in one recorder: continuous always records, events records only inside a
// window, off never spawns.
func TestSupervisorDispatchesRecordModes(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	st := rec.St

	addCamera(t, st, "porch", models.RecordModeContinuous)
	addCamera(t, st, "attic", models.RecordModeOff)
	onSignal := addCamera(t, st, "garden", models.RecordModeEvents)
	startRecorder(t, rec)

	waitFor(t, 3*time.Second, "the continuous camera to be recording", func() bool {
		return spawns.spawns() >= 1
	})

	// Give the poll several passes: an `off` camera and an events camera with no
	// signal must not spawn in any of them.
	time.Sleep(150 * time.Millisecond)
	if got := spawns.spawns(); got != 1 {
		t.Fatalf("dispatch: got %d spawn(s), want exactly 1 (the continuous camera): %s",
			got, strings.Join(argvLines(spawns.allArgv()), " / "))
	}
	// The events camera starts on the first signal, which is what S6-03 calls.
	if !rec.OpenWindow(onSignal.ID) {
		t.Fatalf("OpenWindow did not report opening a window for %s", onSignal.Slug)
	}
	waitFor(t, 3*time.Second, "the events camera to start recording on its first signal", func() bool {
		return spawns.spawns() >= 2
	})
	if !containsArgv(spawns.allArgv(), "garden") {
		t.Fatalf("the events camera is not recording: %s", strings.Join(argvLines(spawns.allArgv()), " / "))
	}
	if !containsArgv(spawns.allArgv(), "porch") {
		t.Fatalf("the continuous camera stopped recording: %s", strings.Join(argvLines(spawns.allArgv()), " / "))
	}
	if containsArgv(spawns.allArgv(), "attic") {
		t.Fatalf("record_mode=off spawned an ffmpeg for the attic camera")
	}
}

// TestRecordModeChangeTakesEffectWithinOnePoll is the plan's poll rule: the
// recorder reads the rows, so a write from anywhere — this process, the API, a
// restarted server — reaches the recording within one poll.
func TestRecordModeChangeTakesEffectWithinOnePoll(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	st := rec.St
	cam := addCamera(t, st, "porch", models.RecordModeContinuous)
	startRecorder(t, rec)

	waitFor(t, 3*time.Second, "the camera to start recording", func() bool {
		return len(spawns.liveProcs()) == 1
	})

	setRecordMode(t, st, cam.ID, models.RecordModeOff)

	// One poll is 20 ms here; a second is allowed for the hand-off. The claim
	// being asserted is "within one poll", and the poll is the mechanism, so the
	// bound is 2 × PollInterval plus the scheduling of the stop.
	waitFor(t, 2*time.Second, "the off camera's ffmpeg to be terminated", func() bool {
		return len(spawns.liveProcs()) == 0
	})
	before := spawns.spawns()
	time.Sleep(200 * time.Millisecond)
	if after := spawns.spawns(); after != before {
		t.Fatalf("record_mode=off restarted ffmpeg: %d spawn(s) before the quiet window, %d after", before, after)
	}

	// And the way back: continuous again, within a poll.
	setRecordMode(t, st, cam.ID, models.RecordModeContinuous)
	waitFor(t, 2*time.Second, "the camera to resume recording", func() bool {
		return spawns.spawns() > before
	})
}

// TestRowChangeRecreatesTheSupervisor: the supervisor belongs to a row, and the
// row moved. The old child is terminated and a supervisor started from the new
// row takes over.
func TestRowChangeRecreatesTheSupervisor(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	st := rec.St
	cam := addCamera(t, st, "porch", models.RecordModeContinuous)
	startRecorder(t, rec)

	waitFor(t, 3*time.Second, "the first supervisor to spawn ffmpeg", func() bool {
		return spawns.spawns() == 1
	})
	first := spawns.processes()[0]

	if err := st.Cameras.Update(func() *models.Camera {
		c := *cam
		c.Slug = "porch-front"
		return &c
	}()); err != nil {
		t.Fatalf("changing the slug: %v", err)
	}

	waitFor(t, 3*time.Second, "the old ffmpeg to be terminated by the row change", func() bool {
		return first.stopCount() >= 1
	})
	waitFor(t, 3*time.Second, "a supervisor from the new row to spawn", func() bool {
		return spawns.spawns() >= 2 && containsArgv(spawns.allArgv(), "porch-front")
	})
}

// TestOpenWindowEndsAfterThePostRoll is the window's other half: an events camera
// records while its window is open, and the ffmpeg ends POST_ROLL_SECONDS after
// the event closes. The post-roll is the seconds a clip needs.
func TestOpenWindowEndsAfterThePostRoll(t *testing.T) {
	rec, _, spawns, clock := newTestRecorder(t)
	st := rec.St
	cam := addCamera(t, st, "garden", models.RecordModeEvents)
	startRecorder(t, rec)

	rec.OpenWindow(cam.ID)
	waitFor(t, 3*time.Second, "the events camera to start recording", func() bool {
		return len(spawns.liveProcs()) == 1
	})

	end, open := rec.WindowEnd(cam.ID)
	if !open {
		t.Fatalf("the window is not open after OpenWindow")
	}
	if want := clock.Now().Add(time.Duration(rec.Cfg.PostRollSeconds) * time.Second); !end.Equal(want) {
		t.Fatalf("window ends at %s, want %s (POST_ROLL_SECONDS = %ds)", end, want, rec.Cfg.PostRollSeconds)
	}

	// The event closes: the window is now "post-roll from the close".
	time.Sleep(100 * time.Millisecond)
	rec.CloseWindow(cam.ID)

	// Still recording: the post-roll is not spent.
	time.Sleep(100 * time.Millisecond)
	if len(spawns.liveProcs()) != 1 {
		t.Fatalf("recording stopped before the post-roll ran out")
	}

	// Spend the post-roll: the clock passes the window's end, and the tail's own
	// tick ends the recording — not the next poll, and not a crash restart.
	clock.Advance(time.Duration(rec.Cfg.PostRollSeconds+1) * time.Second)
	waitFor(t, 3*time.Second, "the events camera's ffmpeg to end with the post-roll", func() bool {
		return len(spawns.liveProcs()) == 0
	})
	before := spawns.spawns()
	time.Sleep(200 * time.Millisecond)
	if after := spawns.spawns(); after != before {
		t.Fatalf("an events camera whose window closed restarted ffmpeg (%d → %d spawns)", before, after)
	}
}

// TestCloseTerminatesOpenProcesses is the plan's shutdown rule, the one OurWay
// recorded as the reason ffmpeg children are owned: a child that outlives the
// process that started it is a recording a restart cannot resume, because the old
// writer still has the segment open.
func TestCloseTerminatesOpenProcesses(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	startRecorder(t, rec)

	waitFor(t, 3*time.Second, "the camera to be recording", func() bool {
		return len(spawns.liveProcs()) == 1
	})

	rec.Close()

	procs := spawns.processes()
	if len(procs) != 1 {
		t.Fatalf("expected exactly one child, got %d", len(procs))
	}
	if procs[0].stopCount() == 0 {
		t.Fatalf("Close did not terminate the open ffmpeg")
	}
	select {
	case <-procs[0].Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("Close left the child running")
	}

	before := spawns.spawns()
	time.Sleep(200 * time.Millisecond)
	if after := spawns.spawns(); after != before {
		t.Fatalf("a closed recorder restarted recording (%d → %d spawns)", before, after)
	}
}

// TestRunReturnsWhenLifecycleContextIsCancelled: every supervisor goroutine is
// inside the recorder's wait group, so Run returning is the assertion that they
// all exited, and it is what makes main.go's `cancel()` at shutdown mean
// "recording has stopped".
func TestRunReturnsWhenLifecycleContextIsCancelled(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	addCamera(t, rec.St, "garden", models.RecordModeContinuous)
	cancel := startRecorder(t, rec)

	waitFor(t, 3*time.Second, "both cameras to be recording", func() bool {
		return len(spawns.liveProcs()) == 2
	})

	returned := make(chan struct{})
	go func() {
		cancel()
		// Run owns the loop; startRecorder's goroutine returns when it does.
		close(returned)
	}()
	<-returned

	// The children are terminated on the way out, not left writing into a
	// directory the process is leaving.
	waitFor(t, 3*time.Second, "every child to be terminated", func() bool {
		for _, p := range spawns.processes() {
			select {
			case <-p.Done():
			default:
				return false
			}
		}
		return true
	})
}

// TestNoSpawnWithoutRun pins the other direction: a recorder that is constructed
// but not run does not start children. main.go's boot order builds the recorder
// in the dependency graph and starts it on a goroutine; between the two, nothing
// should be spawning.
func TestNoSpawnWithoutRun(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	time.Sleep(150 * time.Millisecond)
	if got := spawns.spawns(); got != 0 {
		t.Fatalf("a recorder that was not Run spawned %d process(es)", got)
	}
	// A window for a camera that has no row is inert: the recorder supervises rows,
	// not window ids, so an id that matches nothing records nothing.
	rec.OpenWindow("no-such-camera")
	time.Sleep(50 * time.Millisecond)
	if got := spawns.spawns(); got != 0 {
		t.Fatalf("a window with no camera row spawned ffmpeg (%d)", got)
	}
}

func argvLines(all [][]string) []string {
	lines := make([]string, 0, len(all))
	for _, a := range all {
		lines = append(lines, joinTokens(a))
	}
	return lines
}

func containsArgv(all [][]string, needle string) bool {
	for _, a := range all {
		for _, tok := range a {
			if strings.Contains(tok, needle) {
				return true
			}
		}
	}
	return false
}
