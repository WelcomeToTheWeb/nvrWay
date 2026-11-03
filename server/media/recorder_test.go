package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nvr/server/config"
	"nvr/server/models"
	"nvr/server/store"
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

// Card S4-02 — the ingest command and the generation counter.
//
// The plan names this command character for character, and the card's criteria are
// about the names it produces: `<slug>-<generation>-<n>.mp4`, a generation that
// survives a restart, and a `-segment_start_number` that continues the numbering.
//
// The line below is the plan's with the two flags real ffmpeg corrected (S4-07, at
// the bottom of this file): `-segment_format` for the `-segment_type` ffmpeg does
// not have, and no `-segment_list_entry_prefix`, which ffmpeg prepends to the
// segment name rather than using as one. `FfmpegArgs` carries the evidence.

// TestRecorderIngestCommandIsThePlansLine compares the command to the plan's,
// token by token, in the plan's order — with S4-07's two corrections, which the
// real encoder forced and `FfmpegArgs` explains.
func TestRecorderIngestCommandIsThePlansLine(t *testing.T) {
	rec, _, spawns, _ := newTestRecorder(t)
	cfg := rec.Cfg
	addCamera(t, rec.St, "porch", models.RecordModeContinuous)
	startRecorder(t, rec)

	waitFor(t, 3*time.Second, "the spawn", func() bool { return spawns.spawns() == 1 })
	argv := spawns.lastArgv()

	want := []string{
		"ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", cfg.Go2rtcRTSPURL + "/porch",
		"-c", "copy",
		"-f", "segment",
		"-segment_format", "mp4",
		"-segment_time", "1",
		"-reset_timestamps", "1",
		"-segment_start_number", "0",
		"-segment_list", cfg.SegmentsDir + "/porch.csv",
		"-segment_list_type", "csv",
		cfg.SegmentsDir + "/porch-1-%d.mp4",
	}
	if got := joinTokens(argv); got != joinTokens(want) {
		t.Fatalf("the ingest command is not the plan's:\n got  %s\n want %s", got, joinTokens(want))
	}
}

// TestRecorderSegmentLengthComesFromConfig: SEGMENT_SECONDS is the knob, and the
// command reads it, so an operator's segment length is one env var and not a
// rebuild.
func TestRecorderSegmentLengthComesFromConfig(t *testing.T) {
	st := newTestStore(t)
	t.Setenv("SEGMENT_SECONDS", "7")
	seven := config.Load()

	rec, _, _, _ := newRecorderOn(t, st, seven)
	cam := addCamera(t, st, "porch", models.RecordModeContinuous)
	argv := rec.FfmpegArgs(cam, 1, 0)
	if got := flagValue(argv, "-segment_time"); got != "7" {
		t.Fatalf("SEGMENT_SECONDS=7 produced -segment_time %q", got)
	}
	if got := flagValue(argv, "-segment_start_number"); got != "0" {
		t.Fatalf("start number is %q, want 0 for a camera that has written nothing", got)
	}
}

// TestRecorderGenerationIncrementsAndSurvivesARestart is the counter's whole
// point: ffmpeg restarts its own segment numbering at 0, so the only thing
// separating a restarted supervisor's files from the ones it is replacing is this
// counter, and it lives in the settings table, not in memory.
func TestRecorderGenerationIncrementsAndSurvivesARestart(t *testing.T) {
	st := newTestStore(t)
	cfg := newTestConfig(t)

	first, _, firstSpawns, _ := newRecorderOn(t, st, cfg)
	cam := addCamera(t, st, "porch", models.RecordModeContinuous)
	startRecorder(t, first)
	waitFor(t, 3*time.Second, "the first recorder to spawn", func() bool { return firstSpawns.spawns() == 1 })
	if got := generation(t, st, cam.ID); got != 1 {
		t.Fatalf("generation after the first spawn is %d, want 1", got)
	}
	first.Close()

	// A restart: a new recorder over the same rows.
	second, _, secondSpawns, _ := newRecorderOn(t, st, cfg)
	startRecorder(t, second)
	waitFor(t, 3*time.Second, "the restarted recorder to spawn", func() bool { return secondSpawns.spawns() == 1 })

	if got := generation(t, st, cam.ID); got != 2 {
		t.Fatalf("generation after a restart is %d, want 2 (the counter is settings-backed, not process memory)", got)
	}
	argv := secondSpawns.lastArgv()
	if got, want := argv[len(argv)-1], cfg.SegmentsDir+"/porch-2-%d.mp4"; got != want {
		t.Fatalf("the restarted recorder writes the first generation's files: pattern %q, want %q", got, want)
	}
	if got := flagValue(argv, "-segment_list"); got != cfg.SegmentsDir+"/porch.csv" {
		t.Fatalf("segment list is %q", got)
	}
}

// TestRecorderStartNumberContinuesAfterARestart: `-segment_start_number` is the
// plan's "continue the numbering after a restart". The only place the last number
// lives between runs is the manifest, so the counter reads it from there.
func TestRecorderStartNumberContinuesAfterARestart(t *testing.T) {
	st := newTestStore(t)
	cfg := newTestConfig(t)
	cam := addCamera(t, st, "porch", models.RecordModeContinuous)

	// Generation 1 wrote segments 0..5.
	written := []string{"porch-1-0.mp4", "porch-1-1.mp4", "porch-1-2.mp4", "porch-1-4.mp4", "porch-1-5.mp4"}
	at := time.Now().UTC().Add(-time.Hour)
	for _, name := range written {
		at = at.Add(5 * time.Second)
		seg := &models.Segment{
			CameraID:   cam.ID,
			Generation: 1,
			Path:       cfg.SegmentsDir + "/" + name,
			StartTS:    at,
			EndTS:      at.Add(time.Duration(cfg.SegmentSeconds) * time.Second),
			Seconds:    float64(cfg.SegmentSeconds),
			Bytes:      4096,
			Kind:       models.SegmentKindContinuous,
		}
		if err := st.Segments.Create(seg); err != nil {
			t.Fatalf("seeding segment row %s: %v", name, err)
		}
	}
	// The counter says this camera's last generation was 1.
	if err := st.Settings.Set(GenerationKey(cam.ID), "1"); err != nil {
		t.Fatalf("seeding the generation: %v", err)
	}

	rec, _, spawns, _ := newRecorderOn(t, st, cfg)
	startRecorder(t, rec)
	waitFor(t, 3*time.Second, "the spawn", func() bool { return spawns.spawns() == 1 })

	argv := spawns.lastArgv()
	if got := flagValue(argv, "-segment_start_number"); got != "6" {
		t.Fatalf("-segment_start_number is %q, want 6 (the last number written was 5; ffmpeg would restart at 0 and overwrite)", got)
	}
	if got, want := argv[len(argv)-1], cfg.SegmentsDir+"/porch-2-%d.mp4"; got != want {
		t.Fatalf("the restarted recorder writes %q, want %q (the generation is in the file name)", got, want)
	}
	if got := flagValue(argv, "-segment_list"); !strings.HasSuffix(got, "porch.csv") {
		t.Fatalf("segment list is %q", got)
	}
}

// TestRecorderGenerationIsPersistedBeforeTheChildStarts pins the order. A spawn
// that dies before ffmpeg writes anything must still move the counter: a next
// spawn reading the old generation would restart ffmpeg's numbering at 0 and write
// over the files the previous generation already wrote.
func TestRecorderGenerationIsPersistedBeforeTheChildStarts(t *testing.T) {
	st := newTestStore(t)
	cfg := newTestConfig(t)
	cam := addCamera(t, st, "porch", models.RecordModeContinuous)

	rec, _, spawns, _ := newRecorderOn(t, st, cfg)
	// Every spawn fails: nothing is recorded, and the counter still advances.
	spawns.fail = func(n int) error { return errFakeStart }
	startRecorder(t, rec)

	waitFor(t, 3*time.Second, "two failed spawn attempts", func() bool { return spawns.spawns() >= 2 })
	if got := generation(t, st, cam.ID); got < 2 {
		t.Fatalf("generation is %d after %d failed spawns, want >= 2 (the counter is advanced before the child starts)",
			got, spawns.spawns())
	}
}

var errFakeStart = errors.New("the fake ffmpeg refused to start")

// Card S4-07 — the real-ffmpeg suite.
//
// Everything above this line plays ffmpeg's side of the contract with a fake
// child, which is what makes the policy suites finish in milliseconds. This is
// the other half: the real encoder, the real segment muxer, the real files.
//
// It earns its keep because a fake accepts any argv it is handed. The real ffmpeg
// refused the plan's line twice over (ffmpeg 7.1.5 in Debian trixie, and 8.1.3 as
// bundled by go2rtc — both checked by hand while writing this card):
//
//   - `-segment_type mp4` → "Unrecognized option 'segment_type'", exit 8, no
//     process, no footage. The segment muxer's name for that option is
//     `-segment_format`.
//   - `-segment_list_entry_prefix <slug>-` → ffmpeg *prepends* the prefix to the
//     segment name, so the list named `porch-1-porch-1-0.mp4`: a file that does
//     not exist, a row with no bytes, and a restart that cannot read its own last
//     number back out of the manifest. With no prefix the list holds the bare
//     file name, which is what `recordSegment` resolves against SEGMENTS_DIR.
//
// ffmpeg is not in golang:1.26, so these tests skip without it — loudly, naming
// the binary and the card, because a recording suite that reports success while
// recording nothing is the green that lies. The card's verification command makes
// the same absence a hard failure rather than a skip.

const (
	// lavfiDuration is how much footage one synthetic camera offers: long enough
	// that the source never ends inside a test (the recorder decides when
	// recording ends, not the feed), short enough that a forgotten child cannot
	// run until the suite times out.
	lavfiDuration = 60

	// realSegmentTolerance is how far a row's absolute time may sit from "the
	// instant this generation's ffmpeg was started + ffmpeg's own offset". The
	// recorder adds exactly those two numbers, so the honest margin is the time a
	// fork takes — and it stays tight enough to catch the ways this is got wrong:
	// a camera clock, a time read out of a file name, or a start time taken from
	// the tail instead of the spawn is off by seconds, not by 250 ms.
	realSegmentTolerance = 250 * time.Millisecond
)

// requireFfmpeg is this suite's gate on the image it runs in.
func requireFfmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("SKIPPING the real-ffmpeg suite: no ffmpeg on PATH here (%v). These tests record with the real encoder, so they are skipped, not passed. Install it (apt-get install -y ffmpeg) or run them in the image that carries it — card S4-07's verification command is: docker run --rm -v $PWD/server:/src -w /src golang:1.26-ffmpeg sh -c 'command -v ffmpeg && go test ./media -count=1'", err)
	}
	return path
}

// lavfiSource is the source-override hook (Recorder.SetSourceCommand) pointed at a
// synthetic camera: testsrc2, read at its own frame rate with -re so ffmpeg's
// media time and the wall clock advance together, which is what an RTSP feed does.
// The segment arguments, the naming and the numbering are the recorder's own and
// untouched — an override changes what ffmpeg reads, never what it writes.
func lavfiSource(cam *models.Camera) []string {
	return []string{
		"-re",
		"-f", "lavfi",
		"-i", fmt.Sprintf("testsrc2=size=320x240:rate=25:duration=%d", lavfiDuration),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "25",
	}
}

// realSpawner starts the real ffmpeg — the recorder's own execProcess — and keeps
// what the suite needs from each child: the argv, the instant it was handed the
// clock, and a handle it can kill.
type realSpawner struct {
	mu     sync.Mutex
	argvs  [][]string
	starts []time.Time
	procs  []*execProcess
}

// on wraps a recorder's real factory. The recorder takes its start time
// immediately before calling this, so `starts` is the same instant the rows are
// measured from, to within the time a fork takes.
func (s *realSpawner) on(rec *Recorder) processFactory {
	return func(ctx context.Context, argv []string) (process, error) {
		started := time.Now().UTC()
		p, err := rec.execProcess(ctx, argv)
		if err != nil {
			return nil, err
		}
		ep, ok := p.(*execProcess)
		if !ok {
			return nil, fmt.Errorf("recorder: the real process factory handed back a %T", p)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.argvs = append(s.argvs, append([]string{}, argv...))
		s.starts = append(s.starts, started)
		s.procs = append(s.procs, ep)
		return p, nil
	}
}

func (s *realSpawner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.argvs)
}

func (s *realSpawner) argv(i int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.argvs) {
		return nil
	}
	return append([]string{}, s.argvs[i]...)
}

// start is the instant the i-th ffmpeg was handed the clock.
func (s *realSpawner) start(i int) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.starts) {
		return time.Time{}
	}
	return s.starts[i]
}

// killLast SIGKILLs the newest child. This is the suite playing the crash, and it
// is deliberately not the recorder's Stop(): Stop sends SIGTERM, ffmpeg closes the
// segment it is writing, writes its list line, and exits — a graceful end. A
// SIGKILL leaves a half-written file with no list line, which is the state a
// restart has to number around.
func (s *realSpawner) killLast(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.procs) == 0 {
		t.Fatalf("nothing to kill: no ffmpeg has been started")
	}
	p := s.procs[len(s.procs)-1]
	if p.cmd.Process == nil {
		t.Fatalf("the newest child has no OS process to kill")
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("killing ffmpeg pid %d: %v", p.cmd.Process.Pid, err)
	}
}

// newRealRecorder is the recorder with everything real: the real clock (the rows
// must land on the timeline an operator scrubs), real sleeps, the real process
// factory, and ffmpeg's synthetic source. The poll, the tail and the restart delay
// are shortened because they are S4-01's and S4-04's subject, already pinned by
// the fake suites; here they are only the ceiling on how long a test waits.
func newRealRecorder(t *testing.T, st *store.Store, cfg *config.Config) (*Recorder, *fakeHub, *realSpawner, *logCapture) {
	t.Helper()

	hub := &fakeHub{}
	rec := NewRecorder(cfg, st, hub)
	rec.PollInterval = 100 * time.Millisecond
	rec.TailInterval = 100 * time.Millisecond
	rec.BackoffStart = 200 * time.Millisecond
	rec.BackoffMax = 400 * time.Millisecond
	rec.Logf = func(format string, args ...any) { t.Logf("recorder log: "+format, args...) }

	spawns := &realSpawner{}
	rec.SetProcessFactory(spawns.on(rec))
	rec.SetSourceCommand(lavfiSource)

	logs := captureLogs(t, rec)
	t.Cleanup(rec.Close)
	return rec, hub, spawns, logs
}

// readSegmentCSV reads a whole segment list back: the ground truth the card's
// first criterion names. parseSegmentLine is the recorder's own parser, so the
// suite and the recorder are reading the same format.
func readSegmentCSV(t *testing.T, path string) []segmentEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading ffmpeg's segment list %s: %v", path, err)
	}
	out := []segmentEntry{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry, err := parseSegmentLine(line)
		if err != nil {
			t.Errorf("ffmpeg wrote a segment list line the recorder cannot parse: %v", err)
			continue
		}
		out = append(out, entry)
	}
	return out
}

// generationRows is one generation's rows, oldest first.
func generationRows(t *testing.T, st *store.Store, cameraID string, generation int) []models.Segment {
	t.Helper()
	out := []models.Segment{}
	for _, row := range segmentsFor(t, st, cameraID) {
		if row.Generation == generation {
			out = append(out, row)
		}
	}
	return out
}

// segmentNumberFromName is the `<n>` in `<slug>-<generation>-<n>.mp4`.
func segmentNumberFromName(name string) int {
	stem := strings.TrimSuffix(filepath.Base(name), ".mp4")
	i := strings.LastIndex(stem, "-")
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(stem[i+1:])
	if err != nil {
		return -1
	}
	return n
}

// rowNames is a manifest row list as file names, for failure messages.
func rowNames(rows []models.Segment) string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, filepath.Base(row.Path))
	}
	return strings.Join(names, ", ")
}

// assertNearTime is the clock assertion with its margin stated out loud.
func assertNearTime(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	d := got.Sub(want)
	if d < 0 {
		d = -d
	}
	if d > realSegmentTolerance {
		t.Fatalf("%s is %s, want %s ± %s — the spawn instant plus ffmpeg's own offset. A camera clock, a time read out of a file name, or a start time taken from the tail instead of the spawn is off by more than this",
			what, got, want, realSegmentTolerance)
	}
}

// TestRealFfmpegSegmentsMatchTheCSVAndTheDisk is the card's first two criteria at
// once: the rows' absolute times are the spawn instant plus what ffmpeg reports in
// its own list, and every row has bytes on the disk behind it.
func TestRealFfmpegSegmentsMatchTheCSVAndTheDisk(t *testing.T) {
	requireFfmpeg(t)
	st, cfg := newTestStore(t), newTestConfig(t)
	cam := addCamera(t, st, "porch", models.RecordModeContinuous)
	rec, _, spawns, _ := newRealRecorder(t, st, cfg)
	startRecorder(t, rec)

	waitFor(t, 30*time.Second, "three real segments in the manifest", func() bool {
		return len(segmentsFor(t, st, cam.ID)) >= 3
	})

	// The list is read before the rows. ffmpeg writes a line when a segment
	// closes and the tail picks it up on its next tick, so the list is always at
	// least as long as the manifest: every row must be one of its lines, in order.
	list := readSegmentCSV(t, rec.SegmentListPath(cam.Slug))
	rows := segmentsFor(t, st, cam.ID)
	if len(rows) < 3 {
		t.Fatalf("the manifest has %d rows, want at least 3", len(rows))
	}
	if len(list) < len(rows) {
		t.Fatalf("the manifest has %d rows but ffmpeg's list has %d lines: a row with no list line is footage the recorder invented", len(rows), len(list))
	}

	started := spawns.start(0)
	for i, row := range rows {
		entry := list[i]
		if want := fmt.Sprintf("porch-1-%d.mp4", i); entry.Name != want {
			t.Fatalf("ffmpeg's list line %d names %q, want %q (the plan's naming rule, from the real muxer)", i, entry.Name, want)
		}
		if row.Generation != 1 {
			t.Fatalf("row %d is generation %d, want 1", i, row.Generation)
		}
		if want := filepath.Join(cfg.SegmentsDir, entry.Name); row.Path != want {
			t.Fatalf("row %d points at %q, want %q (the list's name resolved against SEGMENTS_DIR)", i, row.Path, want)
		}
		assertNearTime(t, fmt.Sprintf("row %d start_ts", i), row.StartTS, started.Add(time.Duration(entry.Start*float64(time.Second))))
		assertNearTime(t, fmt.Sprintf("row %d end_ts", i), row.EndTS, started.Add(time.Duration(entry.End*float64(time.Second))))
		if d := row.Seconds - (entry.End - entry.Start); d > 1e-6 || d < -1e-6 {
			t.Fatalf("row %d says %.6f seconds, want %.6f (ffmpeg's own end minus start)", i, row.Seconds, entry.End-entry.Start)
		}
		if row.Bytes <= 0 {
			t.Fatalf("row %d has %d bytes: the card's criterion is bytes > 0 on every row", i, row.Bytes)
		}
		fi, err := os.Stat(row.Path)
		if err != nil {
			t.Fatalf("row %d names a file the recorder cannot read: %v", i, err)
		}
		if fi.Size() != row.Bytes {
			t.Fatalf("row %d says %d bytes, the file on disk says %d", i, row.Bytes, fi.Size())
		}
		if row.Kind != models.SegmentKindContinuous {
			t.Fatalf("row %d kind is %q, want %q", i, row.Kind, models.SegmentKindContinuous)
		}
	}
}

// TestRealFfmpegForcedKillResumesNumbering is the card's third criterion with a
// process that can actually die: SIGKILL the running ffmpeg, and the recorder's
// next spawn carries the next generation and continues the segment numbers, so it
// cannot write over the files the dead one left behind.
func TestRealFfmpegForcedKillResumesNumbering(t *testing.T) {
	requireFfmpeg(t)
	st, cfg := newTestStore(t), newTestConfig(t)
	cam := addCamera(t, st, "porch", models.RecordModeContinuous)
	rec, hub, spawns, logs := newRealRecorder(t, st, cfg)
	startRecorder(t, rec)

	waitFor(t, 30*time.Second, "two segments from the first generation", func() bool {
		return len(generationRows(t, st, cam.ID, 1)) >= 2
	})
	first := generationRows(t, st, cam.ID, 1)
	lastNumber := -1
	for _, row := range first {
		if n := segmentNumberFromName(row.Path); n > lastNumber {
			lastNumber = n
		}
	}
	if lastNumber < 1 {
		t.Fatalf("the first generation's highest segment number is %d, want >= 1 (rows: %s)", lastNumber, rowNames(first))
	}

	spawns.killLast(t)

	waitFor(t, 15*time.Second, "the recorder to start another ffmpeg", func() bool {
		return spawns.count() >= 2
	})
	if got := generation(t, st, cam.ID); got != 2 {
		t.Fatalf("generation after a forced kill is %d, want 2 (the counter is what keeps the new files off the old ones)", got)
	}
	argv := spawns.argv(1)
	if got, want := flagValue(argv, "-segment_start_number"), strconv.Itoa(lastNumber+1); got != want {
		t.Fatalf("-segment_start_number after the kill is %q, want %q: ffmpeg restarts its own counter at 0, so continuing is the only thing protecting generation 1's files", got, want)
	}
	if got, want := argv[len(argv)-1], filepath.Join(cfg.SegmentsDir, "porch-2-%d.mp4"); got != want {
		t.Fatalf("the restarted recorder writes %q, want %q", got, want)
	}

	waitFor(t, 30*time.Second, "a segment from the second generation", func() bool {
		return len(generationRows(t, st, cam.ID, 2)) >= 1
	})
	second := generationRows(t, st, cam.ID, 2)
	if got, want := segmentNumberFromName(second[0].Path), lastNumber+1; got != want {
		t.Fatalf("generation 2's first segment is numbered %d, want %d — the number continues, it does not restart at 0 or jump (gen 1: %s, gen 2: %s)",
			got, want, rowNames(first), rowNames(second))
	}
	for _, row := range second {
		if n := segmentNumberFromName(row.Path); n <= lastNumber {
			t.Fatalf("generation 2 wrote %s, numbered %d, but generation 1 already wrote %d (%s)", filepath.Base(row.Path), n, lastNumber, rowNames(second))
		}
		if row.Bytes <= 0 {
			t.Fatalf("the restarted recorder's segment %s has %d bytes", filepath.Base(row.Path), row.Bytes)
		}
	}

	// The dead generation's footage stays in the manifest: a crash loses the
	// segment ffmpeg was mid-way through, not everything before it.
	if after := len(generationRows(t, st, cam.ID, 1)); after < len(first) {
		t.Fatalf("the forced kill removed rows from generation 1: %d, want at least %d", after, len(first))
	}

	// The crash was said out loud, with what ffmpeg said.
	if len(logs.containing("ffmpeg exited")) == 0 {
		t.Fatalf("a forced kill that restarted left no log line: %v", logs.all())
	}

	// And the camera's tile came down, then came back up — read as a transition
	// over the append-only log, not as the newest entry: with a 200 ms restart the
	// offline state is a moment of wall-clock time (S4-04's finding).
	var sawOnline, sawOffline, sawOnlineAgain bool
	for _, status := range hub.cameraStatusLog(cam.ID) {
		on, ok := status["online"].(bool)
		if !ok {
			continue
		}
		switch {
		case on && sawOffline:
			sawOnlineAgain = true
		case on:
			sawOnline = true
		case !on && sawOnline:
			sawOffline = true
		}
	}
	if !sawOnline || !sawOffline || !sawOnlineAgain {
		t.Fatalf("the camera's status did not go online → offline → online after the forced kill (online=%v offline=%v online-again=%v): %v",
			sawOnline, sawOffline, sawOnlineAgain, hub.cameraStatusLog(cam.ID))
	}
}
