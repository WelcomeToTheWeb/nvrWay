package media

// Clip assembly suite (card S6-01).
//
// Two halves, same split as the recorder's:
//
//   - The policy half runs with a fake media command and a fake scheduler, so
//     "which segments are in the concat", "what ClipSeconds says", "when the
//     assembly is armed and what cancels it" are asserted in milliseconds. A
//     fake accepts any argv it is handed, which is exactly why it is good at
//     asserting argv and useless at asserting durations.
//   - The real half runs the real ffmpeg and ffprobe, because the criterion that
//     matters — "the clip is as long as the window it was cut for" — is a
//     property of the encoder, not of this package. It skips loudly where
//     ffmpeg is absent, naming the image that carries it.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nvr/server/config"
	"nvr/server/models"
	"nvr/server/store"
)

// realClipTolerance is how far a real clip's measured duration may sit from the
// window it was cut for. The concat copies whole segments, so the honest margin
// is the encoder's own granularity — and it stays tight enough to catch the way
// this goes wrong in practice: a window computed from the wrong roll constants,
// or a selection that dropped the segment the event actually happened in.
const realClipTolerance = 1.5

// clipFixture is a clip builder on the fast settings every suite wants: an
// in-memory store, a temp media tree, a clock the test moves, a media command
// that records what it was asked to run, and a scheduler that fires when the
// test says so.
type clipFixture struct {
	St    *store.Store
	Cfg   *config.Config
	B     *ClipBuilder
	Media *fakeMedia
	ARM   *fakeTimers
	Clock *fakeClock
	Log   *logCapture
}

func newClipFixture(t *testing.T) *clipFixture {
	t.Helper()

	st := newTestStore(t)
	cfg := newTestConfig(t)
	clock := newClipClock()

	b := NewClipBuilder(cfg, st)
	b.ReconcileInterval = 20 * time.Millisecond
	b.Logf = func(format string, args ...any) { t.Logf("clips log: "+format, args...) }
	b.SetClock(clock.Now)

	fx := &clipFixture{St: st, Cfg: cfg, B: b, Media: &fakeMedia{}, ARM: &fakeTimers{}, Clock: clock}
	fx.Log = captureClipLogs(t, b)
	b.SetCommandFactory(fx.Media.command())
	b.SetTimerFactory(fx.ARM.arm)

	t.Cleanup(b.Close)
	return fx
}

// newClipClock anchors the suite just before the real clock. The event store
// clamps a future `ts` to now, so a suite whose events sit in the real future
// silently gets `now` instead of the window it asked for.
func newClipClock() *fakeClock {
	return &fakeClock{t: time.Now().UTC().Add(-2 * time.Hour)}
}

// useRealMedia hands the builder back its own command runner and the binaries on
// PATH: the half of the suite that measures real footage.
func (f *clipFixture) useRealMedia(t *testing.T) {
	t.Helper()
	ffmpeg, ffprobe := requireMediaTools(t)
	f.B.run = runClipCommand
	f.B.FfmpegBin = ffmpeg
	f.B.FfprobeBin = ffprobe
}

// captureClipLogs wraps the builder's Logf so a criterion about a log line —
// "clip skipped: no segments for window" — is read from the log, not hunted for
// on stdout.
func captureClipLogs(t *testing.T, b *ClipBuilder) *logCapture {
	t.Helper()
	cap := &logCapture{}
	inner := b.Logf
	b.Logf = func(format string, args ...any) {
		cap.add(fmt.Sprintf(format, args...))
		if inner != nil {
			inner(format, args...)
		}
	}
	return cap
}

// camera is a camera row with footage: `continuous`, so its segments are the
// always-on kind the clip builder selects alongside `event` ones.
func (f *clipFixture) camera(t *testing.T, slug string) *models.Camera {
	t.Helper()
	return addCamera(t, f.St, slug, models.RecordModeContinuous)
}

// event files an event the way the signal path does — open, extend, and close
// when the event is not left open — and returns the row as stored.
func (f *clipFixture) event(t *testing.T, cameraID, key string, started, ended time.Time, open bool) *models.Event {
	t.Helper()
	ctx := context.Background()
	ev, _, err := f.St.Events.UpsertByEventKey(ctx, store.EventInput{
		EventKey: key, CameraID: cameraID, Action: models.SignalActionOpen,
		Rule: "intrusion", ZoneID: "zone-1", Severity: models.SeverityHigh,
		Classes: []string{"person"}, TS: started,
	})
	if err != nil {
		t.Fatalf("open signal for %s: %v", key, err)
	}
	if ended.After(started) {
		ev, _, err = f.St.Events.UpsertByEventKey(ctx, store.EventInput{
			EventKey: key, CameraID: cameraID, Action: models.SignalActionExtend,
			Rule: "intrusion", ZoneID: "zone-1", Severity: models.SeverityHigh,
			Classes: []string{"person"}, TS: ended,
		})
		if err != nil {
			t.Fatalf("extend signal for %s: %v", key, err)
		}
	}
	if !open {
		ev, _, err = f.St.Events.UpsertByEventKey(ctx, store.EventInput{
			EventKey: key, CameraID: cameraID, Action: models.SignalActionClose,
			Rule: "intrusion", ZoneID: "zone-1", Severity: models.SeverityHigh,
			TS: ended,
		})
		if err != nil {
			t.Fatalf("close signal for %s: %v", key, err)
		}
	}
	return ev
}

// segment writes one footage file and its manifest row, and returns the row.
// The file is real bytes on disk because `selectReadable` stats it: a row whose
// file is missing is a row the concat must skip.
func (f *clipFixture) segment(t *testing.T, cameraID, slug string, start, end time.Time, kind string) *models.Segment {
	t.Helper()
	segmentSeq++
	name := fmt.Sprintf("%s-%d.mp4", slug, segmentSeq)
	path := writeSegmentFile(t, f.Cfg.SegmentsDir, name, 4096)
	seg := &models.Segment{
		CameraID:   cameraID,
		Generation: 1,
		Path:       path,
		StartTS:    start.UTC(),
		EndTS:      end.UTC(),
		Seconds:    end.Sub(start).Seconds(),
		Bytes:      4096,
		Kind:       kind,
	}
	if err := f.St.Segments.Create(seg); err != nil {
		t.Fatalf("creating the segment row: %v", err)
	}
	return seg
}

// segmentSeq names the fixture's segment files uniquely: two segments of one
// test can share a start time, and a shared file name would make one test's
// "the file is gone" erase another's footage.
var segmentSeq int

// closeSignal applies the close the event has not been given yet.
func (f *clipFixture) closeSignal(t *testing.T, cameraID, key string, at time.Time) *models.Event {
	t.Helper()
	ev, _, err := f.St.Events.UpsertByEventKey(context.Background(), store.EventInput{
		EventKey: key, CameraID: cameraID, Action: models.SignalActionClose,
		Rule: "intrusion", ZoneID: "zone-1", Severity: models.SeverityHigh, TS: at,
	})
	if err != nil {
		t.Fatalf("close signal for %s: %v", key, err)
	}
	return ev
}

// reload reads an event row back from the database, so an assertion is about
// what was written, not about a struct the test still holds.
func (f *clipFixture) reload(t *testing.T, id string) *models.Event {
	t.Helper()
	ev, err := f.St.Events.GetByID(id)
	if err != nil {
		t.Fatalf("reading event %s back: %v", id, err)
	}
	return ev
}

// fakeMedia is the builder's media command under test: it records every argv and
// the concat list each ffmpeg run was handed, writes an output file where ffmpeg
// would have written one, and answers ffprobe with scripted durations.
type fakeMedia struct {
	mu   sync.Mutex
	runs []fakeMediaRun
	// durations is the scripted ffprobe answer per probe, in order. The last
	// value repeats, so a test scripts only what it cares about.
	durations  []float64
	probeFails bool
	next       int
}

type fakeMediaRun struct {
	Argv []string
	// List is the concat list the run was handed, so "which segments, in what
	// order, repeated or not" is asserted from the file ffmpeg would have read.
	List string
	// Out is the output path the run was given.
	Out string
}

func (f *fakeMedia) command() clipCommand {
	return func(_ context.Context, argv []string) ([]byte, error) {
		run := fakeMediaRun{Argv: append([]string{}, argv...)}
		if len(argv) > 0 {
			run.Out = argv[len(argv)-1]
			if i := argvIndex(argv, "-i"); i >= 0 && i+1 < len(argv) {
				if raw, err := os.ReadFile(argv[i+1]); err == nil {
					run.List = string(raw)
				}
			}
		}

		f.mu.Lock()
		f.runs = append(f.runs, run)
		f.mu.Unlock()

		if len(argv) > 0 && strings.Contains(filepath.Base(argv[0]), "ffprobe") {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.probeFails {
				return []byte(""), fmt.Errorf("ffprobe: no such file (fake)")
			}
			seconds := 10.0
			if len(f.durations) > 0 {
				i := f.next
				if i >= len(f.durations) {
					i = len(f.durations) - 1
				}
				seconds = f.durations[i]
				f.next++
			}
			return []byte(strconv.FormatFloat(seconds, 'f', 6, 64) + "\n"), nil
		}

		// The ffmpeg half: a file where the clip will be.
		if run.Out != "" {
			if err := os.WriteFile(run.Out, []byte("fake mp4 bytes"), 0o640); err != nil {
				return nil, fmt.Errorf("fake ffmpeg could not write %s: %w", run.Out, err)
			}
		}
		return nil, nil
	}
}

func (f *fakeMedia) allRuns() []fakeMediaRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeMediaRun{}, f.runs...)
}

func (f *fakeMedia) ffmpegRuns() []fakeMediaRun {
	out := []fakeMediaRun{}
	for _, r := range f.allRuns() {
		if len(r.Argv) > 0 && strings.Contains(filepath.Base(r.Argv[0]), "ffprobe") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// listPaths is the segment list one ffmpeg run was asked to concat, in order.
func listPaths(list string) []string {
	paths := []string{}
	for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "file '") || !strings.HasSuffix(line, "'") {
			continue
		}
		paths = append(paths, line[len("file '"):len(line)-1])
	}
	return paths
}

func argvIndex(argv []string, flag string) int {
	for i, a := range argv {
		if a == flag {
			return i
		}
	}
	return -1
}

// fakeTimers is the scheduler under test: it records every armed assembly with
// its delay, counts the ones stopped, and fires on demand — which is what makes
// "armed at the post-roll, cancelled on close" a checked fact rather than a
// fifteen-second wait.
type fakeTimers struct {
	mu      sync.Mutex
	live    []*fakeTimer
	fired   []*fakeTimer
	stopped []*fakeTimer
}

type fakeTimer struct {
	Delay   time.Duration
	arm     *fakeTimers
	f       func()
	stopped bool
	fired   bool
}

func (s *fakeTimers) arm(d time.Duration, f func()) clipTimer {
	tm := &fakeTimer{Delay: d, arm: s, f: f}
	s.mu.Lock()
	s.live = append(s.live, tm)
	s.mu.Unlock()
	return tm
}

func (t *fakeTimer) Stop() bool {
	t.arm.mu.Lock()
	defer t.arm.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	for i, live := range t.arm.live {
		if live == t {
			t.arm.live = append(t.arm.live[:i], t.arm.live[i+1:]...)
			break
		}
	}
	t.arm.stopped = append(t.arm.stopped, t)
	return true
}

func (s *fakeTimers) liveTimers() []*fakeTimer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fakeTimer{}, s.live...)
}

func (s *fakeTimers) countStopped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stopped)
}

// fireAll runs every armed assembly, on the calling goroutine, and clears the
// live set. The callback is invoked without the scheduler's lock held, because
// the callback is the builder and the builder locks its own state.
func (s *fakeTimers) fireAll() {
	s.mu.Lock()
	pending := s.live
	s.live = nil
	for _, tm := range pending {
		tm.fired = true
	}
	s.fired = append(s.fired, pending...)
	s.mu.Unlock()

	for _, tm := range pending {
		tm.f()
	}
}

func (s *fakeTimers) firedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.fired)
}

// ---------------------------------------------------------------------------
// The policy half
// ---------------------------------------------------------------------------

// TestClipConcatCommandIsThePlansLine pins the assembly command. The plan names
// every token, and `+faststart` is the one that decides whether the console can
// start playing before the file is fully downloaded.
func TestClipConcatCommandIsThePlansLine(t *testing.T) {
	fx := newClipFixture(t)
	cam := fx.camera(t, "porch")
	start := fx.Clock.Now().Add(-30 * time.Minute)
	end := start.Add(20 * time.Second)
	fx.segment(t, cam.ID, cam.Slug, start, start.Add(5*time.Minute), models.SegmentKindContinuous)
	ev := fx.event(t, cam.ID, "k-cmd", start, end, false)

	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble: %v", err)
	}

	runs := fx.Media.ffmpegRuns()
	if len(runs) != 1 {
		t.Fatalf("%d ffmpeg run(s), want 1", len(runs))
	}
	argv := runs[0].Argv
	want := []string{"-y", "-f", "concat", "-safe", "0"}
	for i, flag := range want {
		if argv[i+1] != flag {
			t.Fatalf("concat argv is %q, want %s at position %d", joinTokens(argv), flag, i+1)
		}
	}
	for _, pair := range [][2]string{{"-c", "copy"}, {"-movflags", "+faststart"}} {
		if flagValue(argv, pair[0]) != pair[1] {
			t.Fatalf("%s is %q, want %q (argv %q)", pair[0], flagValue(argv, pair[0]), pair[1], joinTokens(argv))
		}
	}
	wantOut := filepath.Join(fx.Cfg.ClipsDir, ev.ID+".mp4")
	if argv[len(argv)-1] != wantOut {
		t.Fatalf("clip output is %q, want %q", argv[len(argv)-1], wantOut)
	}
	if _, err := os.Stat(wantOut); err != nil {
		t.Fatalf("the clip was not filed at %s: %v", wantOut, err)
	}
}

// TestClipWindowSelectsOverlappingSegmentsOfAnyKind is the selection rule: the
// window is pre-roll to post-roll, a segment counts if it *overlaps* (an event
// that happened mid-segment needs that segment), and `event`-kind footage is
// footage exactly like `continuous` footage.
func TestClipWindowSelectsOverlappingSegmentsOfAnyKind(t *testing.T) {
	fx := newClipFixture(t)
	fx.Cfg.PreRollSeconds = 8
	fx.Cfg.PostRollSeconds = 15

	cam := fx.camera(t, "drive")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	started := t0.Add(60 * time.Second)
	ended := t0.Add(70 * time.Second)

	// Window: [t0+52s, t0+85s].
	far := fx.segment(t, cam.ID, cam.Slug, t0, t0.Add(50*time.Second), models.SegmentKindContinuous)
	edge := fx.segment(t, cam.ID, cam.Slug, t0.Add(45*time.Second), t0.Add(55*time.Second), models.SegmentKindContinuous)
	middle := fx.segment(t, cam.ID, cam.Slug, t0.Add(55*time.Second), t0.Add(70*time.Second), models.SegmentKindEvent)
	tail := fx.segment(t, cam.ID, cam.Slug, t0.Add(70*time.Second), t0.Add(90*time.Second), models.SegmentKindContinuous)
	after := fx.segment(t, cam.ID, cam.Slug, t0.Add(90*time.Second), t0.Add(100*time.Second), models.SegmentKindContinuous)

	// A second camera's footage in the same window must not join the clip.
	other := fx.camera(t, "attic")
	fx.segment(t, other.ID, other.Slug, t0.Add(60*time.Second), t0.Add(65*time.Second), models.SegmentKindContinuous)

	ev := fx.event(t, cam.ID, "k-window", started, ended, false)
	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble: %v", err)
	}

	runs := fx.Media.ffmpegRuns()
	if len(runs) != 1 {
		t.Fatalf("%d ffmpeg run(s), want 1", len(runs))
	}
	got := listPaths(runs[0].List)
	want := []string{edge.Path, middle.Path, tail.Path}
	if len(got) != len(want) {
		t.Fatalf("concat list is %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("concat list position %d is %q, want %q", i, got[i], want[i])
		}
	}
	for _, excluded := range []string{far.Path, after.Path} {
		for _, g := range got {
			if g == excluded {
				t.Fatalf("segment %s is outside the window but made the concat list", excluded)
			}
		}
	}
}

// TestClipAbsentWindowLeavesTheEventWithItsSnapshot is the plan's empty-window
// rule, which is what makes `record_mode=off` a supported mode rather than a
// broken one: no clip, no seconds, the stated log line, and an event that is
// still an event.
func TestClipAbsentWindowLeavesTheEventWithItsSnapshot(t *testing.T) {
	fx := newClipFixture(t)
	cam := fx.camera(t, "attic") // record_mode=off would have no segments at all
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	ev := fx.event(t, cam.ID, "k-absent", t0, t0.Add(10*time.Second), false)

	snapPath := filepath.Join(fx.Cfg.SnapshotsDir, ev.ID+".jpg")
	if err := os.WriteFile(snapPath, []byte("jpeg"), 0o640); err != nil {
		t.Fatalf("writing the snapshot: %v", err)
	}
	if err := fx.St.Events.SetLocalSnapshot(ev.ID, snapPath); err != nil {
		t.Fatalf("recording the snapshot: %v", err)
	}

	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble over an empty window: %v", err)
	}

	if runs := fx.Media.ffmpegRuns(); len(runs) != 0 {
		t.Fatalf("ffmpeg ran for a window with no segments: %q", runs[0].Argv)
	}
	stored := fx.reload(t, ev.ID)
	if stored.LocalClipPath != "" || stored.ClipKey != "" || stored.ClipSeconds != 0 {
		t.Fatalf("absent clip filed as path=%q key=%q seconds=%v, want all empty/zero",
			stored.LocalClipPath, stored.ClipKey, stored.ClipSeconds)
	}
	if stored.LocalSnapshotPath != snapPath {
		t.Fatalf("the snapshot was lost: %q, want %q", stored.LocalSnapshotPath, snapPath)
	}
	lines := fx.Log.containing("clip skipped: no segments for window")
	if len(lines) == 0 {
		t.Fatalf("no 'clip skipped' log line; the log said: %q", fx.Log.all())
	}
}

// TestClipSecondsIsTheWindowClampedByTheFootage is the plan's arithmetic in both
// directions: plenty of footage means the window's length, and a window with
// less footage than it asked for says the shorter number.
func TestClipSecondsIsTheWindowClampedByTheFootage(t *testing.T) {
	t.Run("footage longer than the window", func(t *testing.T) {
		fx := newClipFixture(t)
		fx.Cfg.PreRollSeconds = 0
		fx.Cfg.PostRollSeconds = 0
		cam := fx.camera(t, "long")
		t0 := fx.Clock.Now().Add(-1 * time.Hour)
		for i := 0; i < 3; i++ {
			fx.segment(t, cam.ID, cam.Slug, t0.Add(time.Duration(i*5)*time.Second), t0.Add(time.Duration(i*5+5)*time.Second), models.SegmentKindContinuous)
		}
		ev := fx.event(t, cam.ID, "k-long", t0, t0.Add(10*time.Second), false)
		if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
			t.Fatalf("assemble: %v", err)
		}
		stored := fx.reload(t, ev.ID)
		if stored.ClipSeconds != 10 {
			t.Fatalf("clip_seconds is %v, want 10 (the window; the footage is 15 s)", stored.ClipSeconds)
		}
	})

	t.Run("footage shorter than the window", func(t *testing.T) {
		fx := newClipFixture(t)
		fx.Cfg.PreRollSeconds = 0
		fx.Cfg.PostRollSeconds = 0
		cam := fx.camera(t, "short")
		t0 := fx.Clock.Now().Add(-1 * time.Hour)
		fx.segment(t, cam.ID, cam.Slug, t0, t0.Add(3*time.Second), models.SegmentKindContinuous)
		ev := fx.event(t, cam.ID, "k-short", t0, t0.Add(30*time.Second), false)
		if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
			t.Fatalf("assemble: %v", err)
		}
		stored := fx.reload(t, ev.ID)
		if stored.ClipSeconds != 3 {
			t.Fatalf("clip_seconds is %v, want 3 (the footage; the window is 30 s)", stored.ClipSeconds)
		}
	})
}

// TestClipUnderTwoSecondsIsPaddedByRepeatingTheLastSegment is the padding rule:
// the trigger is the *measured* duration, the fix is one more copy of the last
// segment, and the number filed on the row is still the window's, because the
// repeat lengthens the file and not the event.
func TestClipUnderTwoSecondsIsPaddedByRepeatingTheLastSegment(t *testing.T) {
	fx := newClipFixture(t)
	fx.Cfg.PreRollSeconds = 0
	fx.Cfg.PostRollSeconds = 0
	fx.Media.durations = []float64{1.0, 2.4}

	cam := fx.camera(t, "tiny")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	seg := fx.segment(t, cam.ID, cam.Slug, t0, t0.Add(1*time.Second), models.SegmentKindContinuous)
	ev := fx.event(t, cam.ID, "k-tiny", t0, t0.Add(1*time.Second), false)

	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble: %v", err)
	}

	runs := fx.Media.ffmpegRuns()
	if len(runs) != 2 {
		t.Fatalf("%d ffmpeg run(s), want 2 (the concat, then the padded concat)", len(runs))
	}
	first := listPaths(runs[0].List)
	second := listPaths(runs[1].List)
	if len(second) != len(first)+1 {
		t.Fatalf("padded list has %d entries, want %d: %q", len(second), len(first)+1, second)
	}
	if second[len(second)-1] != first[len(first)-1] {
		t.Fatalf("the padding repeated %q, want the last segment %q", second[len(second)-1], first[len(first)-1])
	}
	if lines := fx.Log.containing("padded by repeating the last segment"); len(lines) == 0 {
		t.Fatalf("the padding was not logged; the log said: %q", fx.Log.all())
	}
	stored := fx.reload(t, ev.ID)
	if stored.ClipSeconds != 1 {
		t.Fatalf("clip_seconds is %v, want 1 (the padded repeat does not lengthen the event)", stored.ClipSeconds)
	}
	if stored.LocalClipPath == "" {
		t.Fatalf("the padded clip was not filed on the row")
	}
	_ = seg
}

// TestClipSkipsRowsWhoseLocalFileIsGone: the sweeper deletes a segment file and
// keeps the row. Passing a missing file to ffmpeg's concat demuxer fails the
// whole clip, so the row is skipped and the event still gets a clip from the
// footage that is here.
func TestClipSkipsRowsWhoseLocalFileIsGone(t *testing.T) {
	fx := newClipFixture(t)
	fx.Cfg.PreRollSeconds = 0
	fx.Cfg.PostRollSeconds = 0
	cam := fx.camera(t, "aged")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)

	gone := fx.segment(t, cam.ID, cam.Slug, t0, t0.Add(5*time.Second), models.SegmentKindContinuous)
	if err := fx.St.Segments.MarkLocalDeleted(gone.ID); err != nil {
		t.Fatalf("marking the segment deleted: %v", err)
	}
	if err := os.Remove(gone.Path); err != nil {
		t.Fatalf("removing the segment file: %v", err)
	}
	// A row that says the file is here and is wrong about it: the same state one
	// tick later, and the concat must survive it too.
	vanished := fx.segment(t, cam.ID, cam.Slug, t0.Add(5*time.Second), t0.Add(10*time.Second), models.SegmentKindContinuous)
	if err := os.Remove(vanished.Path); err != nil {
		t.Fatalf("removing the vanished segment file: %v", err)
	}
	here := fx.segment(t, cam.ID, cam.Slug, t0.Add(10*time.Second), t0.Add(15*time.Second), models.SegmentKindContinuous)

	ev := fx.event(t, cam.ID, "k-aged", t0, t0.Add(12*time.Second), false)
	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble: %v", err)
	}

	runs := fx.Media.ffmpegRuns()
	if len(runs) != 1 {
		t.Fatalf("%d ffmpeg run(s), want 1", len(runs))
	}
	got := listPaths(runs[0].List)
	if len(got) != 1 || got[0] != here.Path {
		t.Fatalf("concat list is %q, want only %q", got, here.Path)
	}
	if stored := fx.reload(t, ev.ID); stored.ClipSeconds != 5 {
		t.Fatalf("clip_seconds is %v, want 5 (only the readable segment counts)", stored.ClipSeconds)
	}
}

// TestClipAssemblyIsArmedAtPostRollAndCancelledOnClose is the plan's scheduling
// rule, which is the whole reason the clip has the post-roll in it: an open
// event carries one armed timer (the ceiling, so a worker that dies without a
// close still gets a clip), and the close cancels it and re-arms at
// `ended_at + POST_ROLL_SECONDS`.
func TestClipAssemblyIsArmedAtPostRollAndCancelledOnClose(t *testing.T) {
	fx := newClipFixture(t)
	fx.Cfg.PreRollSeconds = 8
	fx.Cfg.PostRollSeconds = 15
	fx.Cfg.EventMaxSeconds = 300

	cam := fx.camera(t, "gate")
	t0 := fx.Clock.Now().Add(-30 * time.Second)
	ev := fx.event(t, cam.ID, "k-arm", t0, t0.Add(20*time.Second), true)

	fx.B.Schedule(ev)
	live := fx.ARM.liveTimers()
	if len(live) != 1 {
		t.Fatalf("%d timer(s) armed for an open event, want 1", len(live))
	}
	// The open ceiling: started_at + EVENT_MAX_SECONDS + POST_ROLL_SECONDS, and
	// the event started 30 s ago, so the deadline is 285 s from now.
	wantOpen := 300*time.Second + 15*time.Second - 30*time.Second
	if live[0].Delay != wantOpen {
		t.Fatalf("open-event timer is %s, want %s (event max + post-roll, from the start)", live[0].Delay, wantOpen)
	}

	// The close cancels it. A timer left armed would assemble the clip at the
	// ceiling with the footage the close already made final.
	closed := fx.closeSignal(t, cam.ID, "k-arm", t0.Add(20*time.Second))
	fx.B.Schedule(closed)
	if fx.ARM.countStopped() != 1 {
		t.Fatalf("%d timer(s) stopped on close, want 1 (the open-case timer must be cancelled)", fx.ARM.countStopped())
	}
	live = fx.ARM.liveTimers()
	if len(live) != 1 {
		t.Fatalf("%d timer(s) armed after close, want 1", len(live))
	}
	// ended_at is 20 s after t0, which is 10 s ago; the post-roll is 15 s, so the
	// assembly is armed 5 s from now.
	if live[0].Delay != 5*time.Second {
		t.Fatalf("close timer is %s, want 5s (ended_at + post-roll - now)", live[0].Delay)
	}

	// A post-roll already spent clamps to zero: an event closed while this server
	// was down is assembled now, not at a deadline the clock walked past.
	old := fx.event(t, cam.ID, "k-old", t0.Add(-2*time.Hour), t0.Add(-2*time.Hour+5*time.Second), false)
	fx.B.Schedule(old)
	armed := fx.ARM.liveTimers()
	if len(armed) != 2 {
		t.Fatalf("%d timer(s) armed, want 2", len(armed))
	}
	if armed[1].Delay != 0 {
		t.Fatalf("a spent post-roll armed %s, want 0 (assemble now)", armed[1].Delay)
	}
}

// TestClipOpenEventArmsOneTimerNotOnePerExtend: an open event is extended every
// few seconds by a live worker. One timer per open event is the plan's wording,
// and it is also the difference between a builder with a timer table and one
// with a timer table that grows by the second.
func TestClipOpenEventArmsOneTimerNotOnePerExtend(t *testing.T) {
	fx := newClipFixture(t)
	cam := fx.camera(t, "busy")
	t0 := fx.Clock.Now().Add(-1 * time.Minute)
	ev := fx.event(t, cam.ID, "k-extend", t0, t0.Add(10*time.Second), true)

	for i := 0; i < 5; i++ {
		fx.B.Schedule(ev)
	}
	if n := len(fx.ARM.liveTimers()); n != 1 {
		t.Fatalf("%d timers armed for one open event after 5 signals, want 1", n)
	}
	if n := fx.B.pendingCount(); n != 1 {
		t.Fatalf("the builder holds %d pending assemblies, want 1", n)
	}
}

// TestClipTimerFiringAssemblesTheClip closes the loop: the thing that was armed
// is the thing that runs, and when it runs the row says what it produced.
func TestClipTimerFiringAssemblesTheClip(t *testing.T) {
	fx := newClipFixture(t)
	fx.Cfg.PreRollSeconds = 0
	fx.Cfg.PostRollSeconds = 0
	cam := fx.camera(t, "timer")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	fx.segment(t, cam.ID, cam.Slug, t0, t0.Add(10*time.Second), models.SegmentKindContinuous)
	ev := fx.event(t, cam.ID, "k-fire", t0, t0.Add(5*time.Second), false)

	fx.B.Schedule(fx.reload(t, ev.ID))
	if n := len(fx.Media.ffmpegRuns()); n != 0 {
		t.Fatalf("ffmpeg ran %d time(s) before the timer fired", n)
	}
	fx.ARM.fireAll()

	if n := len(fx.Media.ffmpegRuns()); n != 1 {
		t.Fatalf("%d ffmpeg run(s) after the timer fired, want 1", n)
	}
	stored := fx.reload(t, ev.ID)
	if stored.LocalClipPath == "" || stored.ClipSeconds != 5 {
		t.Fatalf("the timer fired but the row says path=%q seconds=%v", stored.LocalClipPath, stored.ClipSeconds)
	}
	if n := fx.B.pendingCount(); n != 0 {
		t.Fatalf("a fired assembly is still pending (%d)", n)
	}
}

// TestClipReconcileAfterARestartArmsOpenAndMissedEvents is the plan's reason
// `ListOpen` exists, plus the case it does not cover: a server that comes back
// up has to arm the events that were live when it went down, and the ones that
// closed while it was away with no clip to show for them.
func TestClipReconcileAfterARestartArmsOpenAndMissedEvents(t *testing.T) {
	fx := newClipFixture(t)
	cam := fx.camera(t, "reboot")
	t0 := fx.Clock.Now().Add(-10 * time.Minute)

	open := fx.event(t, cam.ID, "r-open", t0, t0.Add(30*time.Second), true)
	closedNoClip := fx.event(t, cam.ID, "r-closed", t0.Add(time.Minute), t0.Add(90*time.Second), false)
	done := fx.event(t, cam.ID, "r-done", t0.Add(2*time.Minute), t0.Add(3*time.Minute), false)
	if err := fx.St.Events.SetLocalClip(done.ID, filepath.Join(fx.Cfg.ClipsDir, done.ID+".mp4"), 60); err != nil {
		t.Fatalf("filing the finished clip: %v", err)
	}

	// A second builder on the same store is what a restart is.
	restarted := NewClipBuilder(fx.Cfg, fx.St)
	restarted.Logf = func(format string, args ...any) { t.Logf("restarted clips: "+format, args...) }
	restarted.SetClock(fx.Clock.Now)
	restarted.SetCommandFactory(fx.Media.command())
	arm := &fakeTimers{}
	restarted.SetTimerFactory(arm.arm)
	t.Cleanup(restarted.Close)

	restarted.reconcile()

	armed := arm.liveTimers()
	if len(armed) != 2 {
		t.Fatalf("%d assembly(ies) armed after a restart, want 2 (the open event and the closed one with no clip): %v",
			len(armed), []string{open.ID, closedNoClip.ID})
	}
	if restarted.pendingCount() != 2 {
		t.Fatalf("the restarted builder holds %d pending, want 2", restarted.pendingCount())
	}
}

// TestClipAbsentEventIsNotReArmedByEveryTick: the reconcile pass runs every
// fifteen seconds forever. An event whose window had no footage must be said
// once, not re-assembled on every tick, or the log fills with a sentence that is
// true and the data dir never sees a clip.
func TestClipAbsentEventIsNotReArmedByEveryTick(t *testing.T) {
	fx := newClipFixture(t)
	cam := fx.camera(t, "quiet")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	ev := fx.event(t, cam.ID, "k-quiet", t0, t0.Add(5*time.Second), false)

	fx.B.Schedule(fx.reload(t, ev.ID))
	fx.ARM.fireAll()
	if n := len(fx.Media.ffmpegRuns()); n != 0 {
		t.Fatalf("ffmpeg ran %d time(s) for an empty window, want 0", n)
	}
	if n := len(fx.Log.containing("clip skipped: no segments for window")); n != 1 {
		t.Fatalf("the absent-clip line was logged %d time(s), want 1", n)
	}

	fx.B.reconcile()
	fx.B.reconcile()
	if n := len(fx.ARM.liveTimers()); n != 0 {
		t.Fatalf("reconcile re-armed %d timer(s) for an event already assembled", n)
	}
	if n := len(fx.Log.containing("clip skipped: no segments for window")); n != 1 {
		t.Fatalf("the absent-clip line was logged %d time(s) after two reconcile ticks, want 1", n)
	}
}

// TestClipProbeFailureStillFilesTheClip: ffprobe is a second binary, and a
// second binary can be missing. The clip on disk is the deliverable; the number
// next to it is a report about it, so a failed probe costs the number and not
// the clip — and it does not trigger padding, which needs a measurement.
func TestClipProbeFailureStillFilesTheClip(t *testing.T) {
	fx := newClipFixture(t)
	fx.Cfg.PreRollSeconds = 0
	fx.Cfg.PostRollSeconds = 0
	fx.Media.probeFails = true

	cam := fx.camera(t, "unprobed")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	fx.segment(t, cam.ID, cam.Slug, t0, t0.Add(10*time.Second), models.SegmentKindContinuous)
	ev := fx.event(t, cam.ID, "k-noprobe", t0, t0.Add(5*time.Second), false)

	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble with a failing probe: %v", err)
	}
	if n := len(fx.Media.ffmpegRuns()); n != 1 {
		t.Fatalf("%d ffmpeg run(s), want 1 (an unmeasured clip is not padded)", n)
	}
	stored := fx.reload(t, ev.ID)
	if stored.LocalClipPath == "" || stored.ClipSeconds != 5 {
		t.Fatalf("clip filed as path=%q seconds=%v, want the clip and 5 s", stored.LocalClipPath, stored.ClipSeconds)
	}
	if lines := fx.Log.containing("unmeasured"); len(lines) == 0 {
		t.Fatalf("the missing measurement was not reported; the log said: %q", fx.Log.all())
	}
}

// TestClipCommandFailureLeavesTheRowUntouched: ffmpeg failing is the one
// outcome that must not write a confident lie onto the row. The event keeps its
// window and its snapshot, the clip columns stay as they were, and the log names
// ffmpeg's own sentence.
func TestClipCommandFailureLeavesTheRowUntouched(t *testing.T) {
	fx := newClipFixture(t)
	fx.Cfg.PreRollSeconds = 0
	fx.Cfg.PostRollSeconds = 0
	cam := fx.camera(t, "broken")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	fx.segment(t, cam.ID, cam.Slug, t0, t0.Add(10*time.Second), models.SegmentKindContinuous)
	ev := fx.event(t, cam.ID, "k-fail", t0, t0.Add(5*time.Second), false)

	fx.B.SetCommandFactory(func(_ context.Context, argv []string) ([]byte, error) {
		return nil, fmt.Errorf("ffmpeg exited status 1 (concat: Invalid data found)")
	})
	if err := fx.B.Assemble(context.Background(), ev.ID); err == nil {
		t.Fatalf("assemble reported success with a failing ffmpeg")
	}
	stored := fx.reload(t, ev.ID)
	if stored.LocalClipPath != "" || stored.ClipSeconds != 0 {
		t.Fatalf("a failed assembly filed path=%q seconds=%v, want the row untouched",
			stored.LocalClipPath, stored.ClipSeconds)
	}
}

// TestClipCancelDisarmsAPendingAssembly is the delete path's need (S6-04): an
// event an operator deleted has no clip to file, and a timer that fires after
// the row is gone is a query that finds nothing.
func TestClipCancelDisarmsAPendingAssembly(t *testing.T) {
	fx := newClipFixture(t)
	cam := fx.camera(t, "doomed")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	ev := fx.event(t, cam.ID, "k-cancel", t0, t0.Add(5*time.Second), false)

	fx.B.Schedule(fx.reload(t, ev.ID))
	fx.B.Cancel(ev.ID)
	if n := fx.B.pendingCount(); n != 0 {
		t.Fatalf("%d assembly(ies) pending after Cancel, want 0", n)
	}
	fx.ARM.fireAll()
	if n := fx.ARM.firedCount(); n != 0 {
		t.Fatalf("a cancelled assembly still fired (%d)", n)
	}
}

// ---------------------------------------------------------------------------
// The real-ffmpeg half
// ---------------------------------------------------------------------------

// requireMediaTools is this half's gate on the image it runs in. Both binaries
// ship in the server image (S9-01); neither is in golang:1.26, so the suite
// skips — loudly, naming the image, because a clip suite that reports success
// while cutting nothing is the green that lies.
func requireMediaTools(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	var err error
	ffmpeg, err = exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("SKIPPING the real-ffmpeg clip suite: no ffmpeg on PATH here (%v). These tests cut real clips and measure them with ffprobe, so they are skipped, not passed. Card S6-01's real-media command is: docker run --rm -v $PWD/server:/src -w /src golang:1.26-ffmpeg sh -c 'command -v ffmpeg ffprobe && go test ./media -count=1 -run Clip'", err)
	}
	ffprobe, err = exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("SKIPPING the real-ffmpeg clip suite: no ffprobe on PATH here (%v) — the duration criterion needs it", err)
	}
	return ffmpeg, ffprobe
}

// recordRealSegments writes real footage with the real encoder and files a
// manifest row per segment, measured with the real ffprobe rather than assumed
// from the segment length. That is what makes the duration criterion mean
// something: the rows the clip builder selects against carry the same numbers a
// player would read.
func recordRealSegments(t *testing.T, fx *clipFixture, cam *models.Camera, start time.Time, count int, segSeconds float64) []models.Segment {
	t.Helper()
	ffmpeg, ffprobe := requireMediaTools(t)
	ctx := context.Background()

	prefix := fmt.Sprintf("%s-real-", cam.Slug)
	out := filepath.Join(fx.Cfg.SegmentsDir, prefix+"%d.mp4")
	argv := []string{
		ffmpeg, "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x240:rate=25:duration=%s",
			strconv.FormatFloat(float64(count)*segSeconds, 'f', -1, 64)),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "25", "-an",
		"-f", "segment", "-segment_format", "mp4",
		"-segment_time", strconv.FormatFloat(segSeconds, 'f', -1, 64),
		"-reset_timestamps", "1",
		out,
	}
	if _, err := runClipCommand(ctx, argv); err != nil {
		t.Fatalf("recording real segments: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(fx.Cfg.SegmentsDir, prefix+"*.mp4"))
	if err != nil {
		t.Fatalf("finding the segments ffmpeg wrote: %v", err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		t.Fatalf("ffmpeg wrote no segments into %s", fx.Cfg.SegmentsDir)
	}

	probe := NewClipBuilder(fx.Cfg, fx.St)
	probe.FfmpegBin, probe.FfprobeBin = ffmpeg, ffprobe

	segments := make([]models.Segment, 0, len(matches))
	cursor := start.UTC()
	for _, path := range matches {
		seconds, err := probe.probeDuration(ctx, path)
		if err != nil {
			t.Fatalf("probing %s: %v", path, err)
		}
		if seconds < 0.2 {
			// ffmpeg's segment muxer can leave a stub file at the end of a run.
			// It is not footage, so it gets no row.
			continue
		}
		seg := &models.Segment{
			CameraID:   cam.ID,
			Generation: 1,
			Path:       path,
			StartTS:    cursor,
			EndTS:      cursor.Add(time.Duration(seconds * float64(time.Second))),
			Seconds:    seconds,
			Kind:       models.SegmentKindContinuous,
		}
		if fi, err := os.Stat(path); err == nil {
			seg.Bytes = fi.Size()
		}
		if err := fx.St.Segments.Create(seg); err != nil {
			t.Fatalf("creating the segment row for %s: %v", path, err)
		}
		segments = append(segments, *seg)
		cursor = seg.EndTS
	}
	if len(segments) == 0 {
		t.Fatalf("no usable segments were recorded")
	}
	return segments
}

// TestClipMeasuredDurationMatchesTheWindow is the criterion only the real
// encoder can answer: the file that lands in CLIPS_DIR is as long as the window
// it was cut for, to within the encoder's own granularity.
//
// The window here is aligned to the segment boundaries, which is the case the
// criterion is about. A window that starts mid-segment cannot be cut to the
// second with `-c copy` (the plan's command copies, it does not re-encode), so
// the next test states what that window does produce instead.
func TestClipMeasuredDurationMatchesTheWindow(t *testing.T) {
	fx := newClipFixture(t)
	fx.useRealMedia(t)
	fx.Cfg.PreRollSeconds = 0
	fx.Cfg.PostRollSeconds = 0

	cam := fx.camera(t, "aligned")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	segments := recordRealSegments(t, fx, cam, t0, 3, 5.0)

	var footage float64
	for _, s := range segments {
		footage += s.Seconds
	}
	started := segments[0].StartTS
	ended := segments[len(segments)-1].EndTS
	ev := fx.event(t, cam.ID, "real-aligned", started, ended, false)

	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble: %v", err)
	}

	stored := fx.reload(t, ev.ID)
	if stored.LocalClipPath == "" {
		t.Fatalf("no clip was filed")
	}
	measured, err := fx.B.probeDuration(context.Background(), stored.LocalClipPath)
	if err != nil {
		t.Fatalf("probing the clip: %v", err)
	}
	window := ended.Sub(started).Seconds()
	if diff := measured - window; diff > realClipTolerance || diff < -realClipTolerance {
		t.Fatalf("clip measures %.2fs, window is %.2fs (footage %.2fs): off by %.2fs, over the %.1fs tolerance",
			measured, window, footage, diff, realClipTolerance)
	}
	if stored.ClipSeconds < window-0.01 || stored.ClipSeconds > footage+0.01 {
		t.Fatalf("clip_seconds is %v, want min(window %.2f, footage %.2f)", stored.ClipSeconds, window, footage)
	}
}

// TestClipWindowStartingMidSegmentKeepsWholeSegments states what the plan's
// command really does at the edges: `-c copy` cannot cut mid-GOP, so the clip is
// the whole segments the window touches, and `ClipSeconds` is the number the
// console is told — the window, clamped by the footage. The extra seconds at the
// edges are context, not a lie: the row says what the event asked for.
func TestClipWindowStartingMidSegmentKeepsWholeSegments(t *testing.T) {
	fx := newClipFixture(t)
	fx.useRealMedia(t)
	fx.Cfg.PreRollSeconds = 0
	fx.Cfg.PostRollSeconds = 0

	cam := fx.camera(t, "midcut")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	segments := recordRealSegments(t, fx, cam, t0, 3, 5.0)

	started := segments[0].StartTS.Add(2 * time.Second)
	ended := segments[0].StartTS.Add(12 * time.Second)
	ev := fx.event(t, cam.ID, "real-midcut", started, ended, false)

	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	stored := fx.reload(t, ev.ID)
	if stored.LocalClipPath == "" {
		t.Fatalf("no clip was filed")
	}
	if stored.ClipSeconds != 10 {
		t.Fatalf("clip_seconds is %v, want 10 (the window; the concat is whole segments)", stored.ClipSeconds)
	}
	measured, err := fx.B.probeDuration(context.Background(), stored.LocalClipPath)
	if err != nil {
		t.Fatalf("probing the clip: %v", err)
	}
	if measured < 10 {
		t.Fatalf("a clip cut from whole segments measures %.2fs, shorter than the 10 s window", measured)
	}
	if measured > 15+realClipTolerance {
		t.Fatalf("clip measures %.2fs, more than the %.2fs of footage the window touches", measured, 15.0)
	}
}

// TestClipRealShortClipIsPaddedToPlayableLength is the padding rule against a
// real measurement: one second of footage produces a file a scrubber cannot land
// on, and the padded concat makes it two.
func TestClipRealShortClipIsPaddedToPlayableLength(t *testing.T) {
	fx := newClipFixture(t)
	fx.useRealMedia(t)
	fx.Cfg.PreRollSeconds = 0
	fx.Cfg.PostRollSeconds = 0

	cam := fx.camera(t, "blink")
	t0 := fx.Clock.Now().Add(-1 * time.Hour)
	segments := recordRealSegments(t, fx, cam, t0, 1, 1.0)
	seg := segments[0]
	if seg.Seconds > MinClipSeconds {
		t.Skipf("the encoder produced a %.2fs segment, which needs no padding", seg.Seconds)
	}

	ev := fx.event(t, cam.ID, "real-blink", seg.StartTS, seg.EndTS, false)
	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble: %v", err)
	}

	if n := len(fx.Media.ffmpegRuns()); n != 0 {
		t.Fatalf("the fake media command ran %d time(s) in the real suite", n)
	}
	if lines := fx.Log.containing("padded by repeating the last segment"); len(lines) == 0 {
		t.Fatalf("no padding was logged; the log said: %q", fx.Log.all())
	}
	stored := fx.reload(t, ev.ID)
	measured, err := fx.B.probeDuration(context.Background(), stored.LocalClipPath)
	if err != nil {
		t.Fatalf("probing the clip: %v", err)
	}
	if measured < 2*seg.Seconds-realClipTolerance {
		t.Fatalf("the padded clip measures %.2fs, want at least %.2fs (the segment repeated)",
			measured, 2*seg.Seconds-realClipTolerance)
	}
}
