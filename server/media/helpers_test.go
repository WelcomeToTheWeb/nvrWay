package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nvr/server/config"
	"nvr/server/models"
	"nvr/server/store"
)

// Shared fixtures for the recorder suites.
//
// The suites run on the sqlite database in memory like the rest of the server's
// suites (Postgres is the deployment, sqlite is the test target), against a
// config whose media tree is a temp dir, with an injectable clock, sleep, log and
// process factory.
//
// The injectability is the point of the shape: S4-01's and S4-04's criteria are
// about process ownership, restart policy and windows — "every goroutine exits
// when the lifecycle ctx is cancelled", "backoff caps at 60 s", "a record_mode
// change takes effect within one poll". Those are policy, and policy is asserted
// in milliseconds with a fake child process. The real-ffmpeg suite is card S4-07
// and it uses the real factory, in the image that carries ffmpeg.

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New(:memory:): %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := st.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return st
}

// newTestConfig is a config with a temp media tree, a one-second segment, and the
// plan's recording windows, so a suite sets one knob per test instead of one per
// field.
func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	t.Setenv("SEGMENTS_DIR", filepath.Join(dir, "segments"))
	t.Setenv("CLIPS_DIR", filepath.Join(dir, "clips"))
	t.Setenv("SNAPSHOTS_DIR", filepath.Join(dir, "snapshots"))
	t.Setenv("SEGMENT_SECONDS", "1")
	t.Setenv("PRE_ROLL_SECONDS", "8")
	t.Setenv("POST_ROLL_SECONDS", "15")
	t.Setenv("EVENT_MAX_SECONDS", "300")
	t.Setenv("GO2RTC_RTSP_URL", "rtsp://go2rtc:8554")
	t.Setenv("JWT_SECRET", "media-suite-secret-not-a-deployment-one")

	cfg := config.Load()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	return cfg
}

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock. The recorder's sleeps go through it, so a 60 s backoff
// costs a test nothing and a window's post-roll expires on schedule.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeHub records what the recorder announced. `recorder.status` and
// `camera.status` are the console's inputs, so "one broadcast per segment" and
// "producers reflect the truth" are read off this list, not inferred from a log.
type fakeHub struct {
	mu       sync.Mutex
	recorder []map[string]any
	camera   []map[string]any
}

func (h *fakeHub) RecorderStatus(payload any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recorder = append(h.recorder, asMap(payload))
	return nil
}

func (h *fakeHub) CameraStatus(payload any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.camera = append(h.camera, asMap(payload))
	return nil
}

func (h *fakeHub) recorderStatuses() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any{}, h.recorder...)
}

func (h *fakeHub) cameraStatuses() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any{}, h.camera...)
}

// cameraStatusLog is every `camera.status` announcement for one camera, oldest
// first — append-only, so an index found in it stays valid as the log grows.
//
// Suites that assert a *transition* read the log, not its newest entry. With an
// injectable sleep the offline state lasts microseconds of wall-clock time (the
// restart backoff is a fake-clock advance, not a real wait), so "the newest
// announcement is offline" is a race with the suite's poll rather than a property
// of the recorder. "An offline announcement followed the recording one" is.
func (h *fakeHub) cameraStatusLog(cameraID string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]map[string]any, 0, len(h.camera))
	for _, s := range h.camera {
		if s["camera_id"] == cameraID {
			out = append(out, s)
		}
	}
	return out
}

func asMap(payload any) map[string]any {
	if m, ok := payload.(map[string]any); ok {
		return m
	}
	return map[string]any{"payload": payload}
}

// fakeProcess is the child the suites spawn instead of ffmpeg: it is running
// until something Stops it or the test exits it, and it records how it was ended.
type fakeProcess struct {
	mu     sync.Mutex
	done   chan struct{}
	err    error
	closed bool
	stops  int
	onStop func()
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{done: make(chan struct{})}
}

func (p *fakeProcess) Done() <-chan struct{} { return p.done }

func (p *fakeProcess) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *fakeProcess) Stop() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.stops++
	p.err = fmt.Errorf("terminated by the recorder")
	fn := p.onStop
	p.mu.Unlock()
	close(p.done)
	if fn != nil {
		fn()
	}
}

// exit is the suite playing ffmpeg: the process died on its own.
func (p *fakeProcess) exit(err error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.err = err
	fn := p.onStop
	p.mu.Unlock()
	close(p.done)
	if fn != nil {
		fn()
	}
}

func (p *fakeProcess) stopCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stops
}

// spawnLog is the recorder's process factory under test: it logs every argv,
// hands back a fake child, and can be told to fail the nth spawn.
type spawnLog struct {
	mu    sync.Mutex
	argvs [][]string
	procs []*fakeProcess
	fail  func(n int) error
	// afterStart is the suite playing ffmpeg: it runs on a goroutine once the
	// child has been handed out, with the argv the recorder built. The segment
	// suites use it to write what ffmpeg writes — the files and the CSV lines —
	// without an encoder.
	afterStart func(n int, argv []string)
	// exitWith is the suite playing a crash: the child exits with this error, this
	// long after it started. The crash suite needs a process that dies, and needs
	// to say when.
	exitWith func(n int) (error, time.Duration)
}

func (l *spawnLog) factory() processFactory {
	return func(ctx context.Context, argv []string) (process, error) {
		l.mu.Lock()
		n := len(l.argvs)
		l.argvs = append(l.argvs, append([]string{}, argv...))
		l.mu.Unlock()

		if l.fail != nil {
			if err := l.fail(n); err != nil {
				return nil, err
			}
		}
		p := newFakeProcess()
		l.mu.Lock()
		l.procs = append(l.procs, p)
		after := l.afterStart
		exitWith := l.exitWith
		l.mu.Unlock()
		if after != nil {
			go after(n, argv)
		}
		if exitWith != nil {
			err, d := exitWith(n)
			go func() {
				if d > 0 {
					time.Sleep(d)
				}
				p.exit(err)
			}()
		}
		return p, nil
	}
}

func (l *spawnLog) spawns() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.argvs)
}

func (l *spawnLog) lastArgv() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.argvs) == 0 {
		return nil
	}
	return append([]string{}, l.argvs[len(l.argvs)-1]...)
}

func (l *spawnLog) allArgv() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]string, 0, len(l.argvs))
	for _, a := range l.argvs {
		out = append(out, append([]string{}, a...))
	}
	return out
}

// processes lists the children handed out so far, in spawn order.
func (l *spawnLog) processes() []*fakeProcess {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*fakeProcess{}, l.procs...)
}

func (l *spawnLog) liveProcs() []*fakeProcess {
	out := []*fakeProcess{}
	for _, p := range l.processes() {
		select {
		case <-p.Done():
		default:
			out = append(out, p)
		}
	}
	return out
}

// newTestRecorder is a recorder on the fast settings every suite wants: a 20 ms
// poll, a 10 ms tail, a 20 ms restart delay capped at 40 ms, a clock the test
// moves, and a fake child process.
func newTestRecorder(t *testing.T) (*Recorder, *fakeHub, *spawnLog, *fakeClock) {
	t.Helper()
	return newRecorderOn(t, newTestStore(t), newTestConfig(t))
}

// newRecorderOn builds the same fast recorder on a store and config the caller
// already has. Suites that measure a restart need it: "the generation survives a
// restart" is a second recorder built on the same rows, which is what a process
// restart is.
func newRecorderOn(t *testing.T, st *store.Store, cfg *config.Config) (*Recorder, *fakeHub, *spawnLog, *fakeClock) {
	t.Helper()
	hub := &fakeHub{}
	spawns := &spawnLog{}

	rec := NewRecorder(cfg, st, hub)
	rec.PollInterval = 20 * time.Millisecond
	rec.TailInterval = 10 * time.Millisecond
	rec.BackoffStart = 20 * time.Millisecond
	rec.BackoffMax = 40 * time.Millisecond
	rec.Logf = func(format string, args ...any) {
		t.Logf("recorder log: "+format, args...)
	}
	clock := newFakeClock()
	rec.SetClock(clock.Now, clock.Advance)
	rec.SetProcessFactory(spawns.factory())

	t.Cleanup(rec.Close)
	return rec, hub, spawns, clock
}

// logCapture collects what the recorder logged, so a criterion about a log line —
// "ffmpeg exit logs", "a swallowed segment list line is reported" — is asserted by
// reading the log, not by watching the host's stdout.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) add(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func (c *logCapture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.lines...)
}

// containing returns the captured lines naming the substring.
func (c *logCapture) containing(sub string) []string {
	out := []string{}
	for _, line := range c.all() {
		if strings.Contains(line, sub) {
			out = append(out, line)
		}
	}
	return out
}

// captureLogs wraps a recorder's Logf so the suite sees every line it writes.
func captureLogs(t *testing.T, rec *Recorder) *logCapture {
	t.Helper()
	cap := &logCapture{}
	inner := rec.Logf
	rec.Logf = func(format string, args ...any) {
		cap.add(fmt.Sprintf(format, args...))
		if inner != nil {
			inner(format, args...)
		}
	}
	return cap
}

// startRecorder runs the supervisor on a goroutine and returns a stop function
// that cancels the lifecycle context and waits for Run to return.
func startRecorder(t *testing.T, rec *Recorder) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		rec.Run(ctx)
		close(returned)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Errorf("Run did not return after the lifecycle context was cancelled")
		}
	})
	return cancel
}

// addCamera writes a camera row and returns the row as stored.
func addCamera(t *testing.T, st *store.Store, slug, mode string) *models.Camera {
	t.Helper()
	cam := &models.Camera{
		Slug:       slug,
		Name:       strings.ToTitle(slug),
		SourceURI:  "rtsp://cam.example/" + slug,
		RecordMode: mode,
		Active:     true,
	}
	if err := st.Cameras.Create(cam); err != nil {
		t.Fatalf("creating camera %s: %v", slug, err)
	}
	stored, err := st.Cameras.GetByID(cam.ID)
	if err != nil {
		t.Fatalf("reading camera %s back: %v", slug, err)
	}
	return stored
}

// setRecordMode changes a camera's mode the way the API does: a write to the row.
func setRecordMode(t *testing.T, st *store.Store, cameraID, mode string) {
	t.Helper()
	cam, err := st.Cameras.GetByID(cameraID)
	if err != nil {
		t.Fatalf("reading camera %s: %v", cameraID, err)
	}
	cam.RecordMode = mode
	if err := st.Cameras.Update(cam); err != nil {
		t.Fatalf("setting record_mode on %s: %v", cameraID, err)
	}
}

// generation reads the persisted spawn counter.
func generation(t *testing.T, st *store.Store, cameraID string) int {
	t.Helper()
	raw, err := st.Settings.GetOr(GenerationKey(cameraID), "0")
	if err != nil {
		t.Fatalf("reading %s: %v", GenerationKey(cameraID), err)
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		t.Fatalf("%s is %q, not a number", GenerationKey(cameraID), raw)
	}
	return n
}

// waitFor polls a condition until it holds, so a suite waits for the fact it is
// asserting instead of sleeping for a duration it hopes is long enough.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// writeSegmentFile stands in for ffmpeg's output: a file of n bytes in the
// segments dir, so a manifest row can assert the byte count it read from disk.
func writeSegmentFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o640); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// writeCSVLine appends one line to a segment list the way ffmpeg writes it.
func writeCSVLine(t *testing.T, path, name string, start, end float64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatalf("opening the segment list %s: %v", path, err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s,%.6f,%.6f\n", name, start, end); err != nil {
		t.Fatalf("writing the segment list: %v", err)
	}
}

// segmentsFor reads a camera's manifest rows back, oldest first.
func segmentsFor(t *testing.T, st *store.Store, cameraID string) []models.Segment {
	t.Helper()
	rows, err := st.Segments.ListRange(cameraID, time.Now().Add(-48*time.Hour), time.Now().Add(48*time.Hour), 0)
	if err != nil {
		t.Fatalf("reading segments: %v", err)
	}
	return rows
}

// tokenOf is a small helper for the string joining used in assertions.
func joinTokens(argv []string) string { return strings.Join(argv, " ") }

// flagValue is the argument a flag takes, for assertions that name one token of
// the command instead of comparing a whole string a reader has to scan.
func flagValue(argv []string, flag string) string {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// sleepRecorder is a clock whose sleeps the suite can read: the restart backoff is
// a sequence of waits, and the only honest way to assert a sequence is to record
// the waits.
type sleepRecorder struct {
	mu    sync.Mutex
	slept []time.Duration
	clock *fakeClock
}

func newSleepRecorder(clock *fakeClock) *sleepRecorder {
	return &sleepRecorder{clock: clock}
}

// SetOn wires it to a recorder: the clock advances as the recorder asks, instantly,
// and every duration is recorded.
func (s *sleepRecorder) SetOn(rec *Recorder) {
	rec.SetClock(s.clock.Now, func(d time.Duration) {
		s.mu.Lock()
		s.slept = append(s.slept, d)
		s.mu.Unlock()
		s.clock.Advance(d)
	})
}

func (s *sleepRecorder) durations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration{}, s.slept...)
}
