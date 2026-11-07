// Package media owns the recording pipeline: the ffmpeg processes that write the
// mp4 segments, the rows that make those segments findable, and the announcements
// that say the recording is alive.
//
// Four rules the package is built on (plan Step 4):
//
//   - **The server clock owns event time.** A segment's `start_ts` is the moment
//     its ffmpeg process started plus the offset ffmpeg reports in its own segment
//     list. No camera clock is read and no time is parsed out of a file name, so
//     the timeline an operator scrubs is the timeline this process recorded.
//   - **One process per camera, owned here.** ffmpeg children are terminated on
//     Close(), so a restart resumes recording instead of leaving a writer holding
//     a half-open file the new process will overwrite.
//   - **The row is the manifest.** A segment on disk with no row is invisible
//     footage that ages out as an orphan file, so the CSV ffmpeg writes is tailed
//     into rows as soon as each segment closes.
//   - **Numbering never reuses a file.** The per-camera `Generation` counter lives
//     in the settings table (`recorder_gen_<camera_id>`) and the segment number
//     continues from the last one this camera wrote, because ffmpeg restarts its
//     own counter at 0 and would otherwise overwrite the files it just wrote.
package media

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"nvr/server/config"
	"nvr/server/models"
	"nvr/server/store"
)

// The plan's timings for this step, as named constants: the poll that notices a
// camera row moved, the read of ffmpeg's segment list, and the crash-restart
// window. A recorder whose restart policy is a magic number in a loop body is a
// recorder nobody can tune.
const (
	// DefaultPollInterval is the camera-row poll (plan: "a 10 s config poll on
	// cameras.updated_at"). A record_mode change lands within one poll.
	DefaultPollInterval = 10 * time.Second

	// DefaultTailInterval is how often the segment list is read while a process
	// runs. One second is well below the shortest sane segment length, so a
	// segment is in the manifest within a tick of closing.
	DefaultTailInterval = time.Second

	// DefaultBackoffStart and DefaultBackoffMax are the crash-restart window
	// (plan: restart after 10 s, max backoff 60 s). The backoff resets on a
	// healthy segment tick, so a camera that recovers recovers at once.
	DefaultBackoffStart = 10 * time.Second
	DefaultBackoffMax   = 60 * time.Second
)

// Broadcaster is the live feed the recorder announces to. `*ws.Hub` implements
// it; nil means no feed, which is a stated state and not a crash: the recorder
// records and says nothing.
type Broadcaster interface {
	RecorderStatus(payload any) error
	CameraStatus(payload any) error
}

// noopBroadcaster is what a recorder with no hub talks to, so the recorder never
// nil-checks its own announcements at every call site.
type noopBroadcaster struct{}

func (noopBroadcaster) RecorderStatus(payload any) error { return nil }
func (noopBroadcaster) CameraStatus(payload any) error   { return nil }

// process is one running ffmpeg, as the recorder needs to see it: a handle it can
// terminate, and a signal that says it has exited.
type process interface {
	// Done is closed when the process has exited.
	Done() <-chan struct{}
	// Err is the exit error, valid after Done is closed.
	Err() error
	// Stop terminates the process. Safe to call twice, safe after exit.
	Stop()
}

// processFactory starts one ffmpeg from an argv (argv[0] is the binary). The
// recorder owns the returned handle.
//
// It is injectable because S4-01's and S4-04's exit criteria are about process
// ownership and restart policy — "every goroutine exits when the lifecycle ctx is
// cancelled", "a killed process restarts and numbering continues" — and a suite
// that needs a real encoder to assert *policy* is a suite people skip. The real
// ffmpeg suite is card S4-07, and it uses the real factory.
type processFactory func(ctx context.Context, argv []string) (process, error)

// Recorder supervises one ffmpeg per camera.
type Recorder struct {
	Cfg *config.Config
	St  *store.Store
	// Hub is the live feed: `recorder.status` and `camera.status` go out here.
	Hub Broadcaster

	// FfmpegBin is the binary. `ffmpeg` in the container image.
	FfmpegBin string

	// PollInterval, TailInterval, BackoffStart and BackoffMax are the timings
	// above; exported so a suite runs them in milliseconds instead of minutes.
	PollInterval time.Duration
	TailInterval time.Duration
	BackoffStart time.Duration
	BackoffMax   time.Duration

	// Logf is the log. Assignable for the same reason the clock is: "ffmpeg exit
	// logs" is an exit criterion, and a criterion about a log line is asserted by
	// reading the log, not by watching the host's stdout.
	Logf func(format string, args ...any)

	// SourceCommand is the source override hook (card S4-07): the input and codec
	// arguments for one camera. The default is the plan's RTSP ingest of
	// GO2RTC_RTSP_URL/<slug>; a suite replaces it with a synthetic source. It does
	// not touch the segment arguments, the naming, or the generation: an override
	// changes what ffmpeg reads, never what the recorder writes.
	SourceCommand func(cam *models.Camera) []string

	// newProcess starts the child. See processFactory.
	newProcess processFactory

	// now and sleep are the recorder's clock: every timestamp it writes comes
	// from `now`, and every wait goes through `sleep`, so a suite moves time
	// instead of sleeping through it.
	now   func() time.Time
	sleep func(d time.Duration)

	mu       sync.Mutex
	runs     map[string]*run
	windows  map[string]*window
	closed   bool
	baseCtx  context.Context // set by Run; OpenWindow reconciles through it
	trigger  chan struct{}   // one nudge: start recording now, not at the next poll
	wg       sync.WaitGroup
	startSem sync.Mutex // serialises reconcile passes
}

// run is one camera's supervisor: the row it was started from, the handle that
// ends the goroutine, and the ffmpeg it currently owns.
type run struct {
	camera models.Camera
	cancel context.CancelFunc
	done   chan struct{} // closed when the supervisor goroutine has exited

	mu      sync.Mutex
	proc    process
	healthy bool // a segment landed since the last spawn; resets the backoff
}

// setProcess publishes (or clears) the current child.
func (r *run) setProcess(p process) {
	r.mu.Lock()
	r.proc = p
	r.mu.Unlock()
}

// current returns the child this run owns right now, or nil.
func (r *run) current() process {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.proc
}

// markHealthy records that a segment landed.
func (r *run) markHealthy() {
	r.mu.Lock()
	r.healthy = true
	r.mu.Unlock()
}

// takeHealthy consumes the flag, answering whether any segment landed since this
// spawn. Consuming it is the point: the backoff resets once, on the first healthy
// tick, and starts over if the next spawn crashes again.
func (r *run) takeHealthy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.healthy
	r.healthy = false
	return was
}

// stop ends this run: the goroutine first, then the child it owns. Terminating
// ffmpeg is what makes "a restart resumes recording" true — a child the recorder
// forgets keeps writing, and the next process in the same generation overwrites
// the file it has open.
func (r *run) stop() {
	r.cancel()
	if p := r.current(); p != nil {
		p.Stop()
	}
}

// window is one camera's open recording window in `record_mode=events`.
//
// A window opens on the first signal for that camera and is refreshed by every
// signal after it, so the window is "recording while the detection is hot, and
// POST_ROLL_SECONDS after the last thing that happened". hardEnd is the ceiling:
// a worker that dies mid-event must not leave a camera recording forever.
type window struct {
	cameraID string
	openedAt time.Time
	endsAt   time.Time
	hardEnd  time.Time
}

// NewRecorder builds the recorder. A nil hub means no live feed: the recorder
// records and announces nothing, which is the state a server with no console
// attached is in, and not a reason to refuse to record.
func NewRecorder(cfg *config.Config, st *store.Store, hub Broadcaster) *Recorder {
	if hub == nil {
		hub = noopBroadcaster{}
	}
	r := &Recorder{
		Cfg: cfg,
		St:  st,
		Hub: hub,

		FfmpegBin:    "ffmpeg",
		PollInterval: DefaultPollInterval,
		TailInterval: DefaultTailInterval,
		BackoffStart: DefaultBackoffStart,
		BackoffMax:   DefaultBackoffMax,

		Logf: log.Printf,

		now:   func() time.Time { return time.Now().UTC() },
		sleep: func(d time.Duration) { time.Sleep(d) },

		runs:    map[string]*run{},
		windows: map[string]*window{},
		trigger: make(chan struct{}, 1),
	}
	r.newProcess = r.execProcess
	r.SourceCommand = r.rtspSource
	return r
}

// SetClock points every timestamp this recorder writes at one clock, and its
// waits at a function, so a suite moves time instead of sleeping through it.
func (r *Recorder) SetClock(now func() time.Time, sleep func(d time.Duration)) {
	if now != nil {
		r.now = now
	}
	if sleep != nil {
		r.sleep = sleep
	}
}

// SetProcessFactory replaces the thing that starts ffmpeg. Suites use it to
// assert the spawn policy (argv, generation, restart, backoff) without an encoder.
func (r *Recorder) SetProcessFactory(f processFactory) {
	if f != nil {
		r.newProcess = f
	}
}

// SetSourceCommand replaces the ffmpeg input arguments (the source override hook,
// card S4-07).
func (r *Recorder) SetSourceCommand(f func(cam *models.Camera) []string) {
	if f != nil {
		r.SourceCommand = f
	}
}

// rtspSource is the plan's ingest: TCP RTSP from the media sidecar, stream
// copied, not re-encoded. `-c copy` is here rather than in the fixed part of the
// command because it is a property of the source, and the source is the part a
// suite overrides.
func (r *Recorder) rtspSource(cam *models.Camera) []string {
	// `-timeout` (µs) is the socket I/O timeout: a stalled input — go2rtc's
	// producer frozen mid-stream, which the dev stack did — makes ffmpeg
	// answer "Operation timed out" and exit within ten seconds, which is an
	// exit the supervisor's restart exists to handle (verified by SIGSTOP-ing
	// the producer under a connected recorder). The tail's stall watchdog is
	// the second layer, for a child that hangs without ever exiting.
	return []string{
		"-rtsp_transport", "tcp",
		"-timeout", "10000000",
		"-i", strings.TrimSuffix(r.Cfg.Go2rtcRTSPURL, "/") + "/" + cam.Slug,
		"-c", "copy",
	}
}

// GenerationKey is the settings key holding one camera's spawn counter.
func GenerationKey(cameraID string) string { return "recorder_gen_" + cameraID }

// SegmentListPath is the CSV ffmpeg writes for one camera's segments.
func (r *Recorder) SegmentListPath(slug string) string {
	return filepath.Join(r.Cfg.SegmentsDir, slug+".csv")
}

// SegmentPattern is ffmpeg's output pattern for one generation of one camera.
// The name carries the slug, the generation and the segment number, so a file
// says which camera and which spawn wrote it, and two spawns cannot collide.
// ffmpeg writes this pattern's basename, with `%d` filled in, into the segment
// list — that bare name is what the manifest row resolves against SEGMENTS_DIR.
func (r *Recorder) SegmentPattern(slug string, generation int) string {
	return filepath.Join(r.Cfg.SegmentsDir, fmt.Sprintf("%s-%d-%%d.mp4", slug, generation))
}

// SegmentListPrefix was the plan's `-segment_list_entry_prefix`, and it is gone.
// Real ffmpeg (7.1.5 and 8.1.3, found by card S4-07) *prepends* that value to the
// segment name it writes into the list, so `<slug>-<generation>-` made the list
// name `porch-1-porch-1-0.mp4`: a file that does not exist, a row with no bytes,
// and a restart that cannot read its own last segment number back out of the
// manifest. With no prefix the list holds the bare file name, which is exactly
// what `recordSegment` resolves against SEGMENTS_DIR.

// FfmpegArgs is the plan's ingest command for one spawn, with the two flags real
// ffmpeg corrected (S4-07 ran the plan's line against the encoder; the plan was
// written against a fake that accepts any argv):
//
//   - `-segment_format mp4`, not the plan's `-segment_type mp4`. ffmpeg 7.1 and
//     8.1 both answer "Unrecognized option 'segment_type'" and never start; the
//     segment muxer's name for that option is `-segment_format`.
//   - no `-segment_list_entry_prefix` at all, for the reason above.
//
// It is exported because "the ingest command is exactly the plan's" is a fact a
// test should assert token by token, not one a reader infers from a builder.
func (r *Recorder) FfmpegArgs(cam *models.Camera, generation, startNumber int) []string {
	argv := []string{r.FfmpegBin, "-hide_banner", "-loglevel", "error"}
	argv = append(argv, r.SourceCommand(cam)...)
	argv = append(argv,
		"-f", "segment",
		"-segment_format", "mp4",
		"-segment_time", strconv.Itoa(r.Cfg.SegmentSeconds),
		"-reset_timestamps", "1",
		"-segment_start_number", strconv.Itoa(startNumber),
		"-segment_list", r.SegmentListPath(cam.Slug),
		"-segment_list_type", "csv",
		r.SegmentPattern(cam.Slug, generation),
	)
	return argv
}

// Run is the supervisor loop: poll the camera rows, keep one supervisor per
// camera that wants one, and stop everything when the lifecycle context is
// cancelled. It blocks until that cancellation; main starts it on a goroutine.
func (r *Recorder) Run(ctx context.Context) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.baseCtx = ctx
	r.mu.Unlock()

	// The first pass runs immediately: recording should start at boot, not one
	// poll after it.
	r.reconcile(ctx)

	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Shutdown: terminate the children, then return. Every supervisor
			// goroutine is in r.wg, so Run does not return while one of them is
			// still running.
			r.stopAll()
			r.wg.Wait()
			return
		case <-r.trigger:
			r.reconcile(ctx)
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

// Close ends recording: every supervisor stops, every ffmpeg this recorder owns
// is terminated, and Close returns once they are all gone. A child left running
// past the process that owns it is the failure the plan names — a restart that
// does not resume recording, because the old writer still has the file open.
func (r *Recorder) Close() {
	r.stopAll()
	r.wg.Wait()
}

// OpenWindow opens (or refreshes) one camera's event-recording window, and
// answers whether this call is what opened it. Card S6-03 calls it on signal
// ingest, which is why it is exported and safe to call from a request handler.
//
// Refreshing matters: an `extend` signal is the window staying hot, and a window
// that stopped extending at its first signal would stop recording mid-event.
func (r *Recorder) OpenWindow(cameraID string) bool {
	if cameraID == "" {
		return false
	}
	now := r.now().UTC()
	post := time.Duration(r.Cfg.PostRollSeconds) * time.Second
	max := time.Duration(r.Cfg.EventMaxSeconds) * time.Second

	r.mu.Lock()
	w := r.windows[cameraID]
	opened := false
	if w == nil {
		w = &window{cameraID: cameraID, openedAt: now, hardEnd: now.Add(max + post)}
		w.endsAt = minTime(now.Add(post), w.hardEnd)
		r.windows[cameraID] = w
		opened = true
	} else {
		w.endsAt = minTime(w.endsAt.Add(post), w.hardEnd)
		if !now.Before(w.endsAt) {
			// The window had expired; this signal opens it again.
			w.openedAt = now
			w.hardEnd = now.Add(max + post)
			w.endsAt = minTime(now.Add(post), w.hardEnd)
			opened = true
		}
	}
	r.mu.Unlock()

	if opened {
		r.nudge()
	}
	return opened
}

// CloseWindow ends one camera's window POST_ROLL_SECONDS from now: the event is
// over, and the last seconds of it are the point of a post-roll.
func (r *Recorder) CloseWindow(cameraID string) {
	if cameraID == "" {
		return
	}
	now := r.now().UTC()
	post := time.Duration(r.Cfg.PostRollSeconds) * time.Second

	r.mu.Lock()
	if w := r.windows[cameraID]; w != nil {
		w.endsAt = minTime(now.Add(post), w.hardEnd)
	}
	r.mu.Unlock()
}

// WindowEnd reports when one camera's window stops, and whether it is open now.
func (r *Recorder) WindowEnd(cameraID string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.windows[cameraID]
	if w == nil || !r.now().UTC().Before(w.endsAt) {
		return time.Time{}, false
	}
	return w.endsAt, true
}

// windowOpen reports whether a camera's event window is open, dropping it when
// the clock has passed its end.
func (r *Recorder) windowOpen(cameraID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.windows[cameraID]
	if w == nil {
		return false
	}
	if !r.now().UTC().Before(w.endsAt) {
		delete(r.windows, cameraID)
		return false
	}
	return true
}

// nudge asks the supervisor loop to reconcile now: "an event just opened on this
// camera" is not information worth holding until the next poll, because the
// first seconds of an event are the seconds a clip needs.
func (r *Recorder) nudge() {
	r.mu.Lock()
	ctx := r.baseCtx
	r.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return
	}
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// stopAll ends every run and marks the recorder closed, so no supervisor restarts
// a child on the way out.
func (r *Recorder) stopAll() {
	r.mu.Lock()
	r.closed = true
	runs := make([]*run, 0, len(r.runs))
	for _, run := range r.runs {
		runs = append(runs, run)
	}
	r.runs = map[string]*run{}
	r.mu.Unlock()

	for _, run := range runs {
		run.stop()
	}
}

// reconcile makes the running set match the camera rows: start a supervisor for
// every camera that wants one, end the ones whose row moved, whose row is gone,
// or whose window has closed.
//
// The rows are read, not remembered, and the poll is the mechanism: "a
// record_mode change takes effect within one poll" is a property of a loop that
// reads the table, and it survives a missed write, a restarted server, and an
// operator editing the row from a console that is not talking to this process.
func (r *Recorder) reconcile(ctx context.Context) {
	// One pass at a time: the poll tick and an event signal can both ask for a
	// reconcile, and two passes racing the same map start two ffmpeg for one
	// camera.
	r.startSem.Lock()
	defer r.startSem.Unlock()

	cameras, err := r.St.Cameras.ListAll()
	if err != nil {
		r.Logf("recorder: reading camera rows failed: %v (keeping the recording as it is)", err)
		return
	}

	r.mu.Lock()
	if r.closed || ctx.Err() != nil {
		r.mu.Unlock()
		return
	}

	want := make(map[string]models.Camera, len(cameras))
	for _, cam := range cameras {
		if !cam.Active {
			continue
		}
		switch cam.RecordMode {
		case models.RecordModeContinuous:
			want[cam.ID] = cam
		case models.RecordModeEvents:
			// `events` records only inside a window. The window opens on the
			// first signal (OpenWindow, from S6-03), so an events camera with no
			// events is not recording, and that is the mode working, not failing.
			if r.windowOpenLocked(cam.ID) {
				want[cam.ID] = cam
			}
		case models.RecordModeOff:
			// `off` never spawns: snapshot-only. Stated behaviour, not a failure.
		}
	}

	for id, run := range r.runs {
		cam, ok := want[id]
		if !ok {
			delete(r.runs, id)
			go run.stop()
			continue
		}
		if rowChanged(run.camera, cam) {
			// The row moved: the slug (the stream name), the mode, the active
			// flag, or any write at all. The supervisor is re-created from the
			// new row, which is what "re-created on camera row change" means.
			delete(r.runs, id)
			go run.stop()
			continue
		}
	}

	for id, cam := range want {
		if _, running := r.runs[id]; running {
			continue
		}
		r.startRunLocked(ctx, cam)
	}
	r.mu.Unlock()
}

// windowOpenLocked is windowOpen for a caller already holding r.mu.
func (r *Recorder) windowOpenLocked(cameraID string) bool {
	w := r.windows[cameraID]
	if w == nil {
		return false
	}
	if !r.now().UTC().Before(w.endsAt) {
		delete(r.windows, cameraID)
		return false
	}
	return true
}

// rowChanged answers whether a supervisor started from `old` should be re-created
// for the row `now`.
//
// `UpdatedAt` is the trigger the plan names, and it is deliberately broad: a
// write that changes nothing the recorder reads still costs one restart, and a
// restart is a segment boundary, not a hole in the recording.
func rowChanged(old, now models.Camera) bool {
	return old.Slug != now.Slug ||
		old.RecordMode != now.RecordMode ||
		old.Active != now.Active ||
		!old.UpdatedAt.Equal(now.UpdatedAt)
}

// startRunLocked launches one camera's supervisor. Callers hold r.mu.
func (r *Recorder) startRunLocked(ctx context.Context, cam models.Camera) {
	runCtx, cancel := context.WithCancel(ctx)
	run := &run{camera: cam, cancel: cancel, done: make(chan struct{})}
	r.runs[cam.ID] = run

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(run.done)
		defer cancel()
		r.cameraLoop(runCtx, run)
		// The map entry means "a supervisor owns this camera", and a loop that
		// has returned owns nothing. A stale entry strands the camera for good:
		// reconcile skips cameras that already have a run, so a loop that exits
		// while its window stays open — the silent window-close exit over a hung
		// ffmpeg, with event signals still extending the window — is never
		// re-spawned, and the camera records nothing while its tile stays green.
		// The identity check keeps this from deleting a successor's entry when
		// reconcile replaced this run before the loop unwound.
		r.mu.Lock()
		if r.runs[cam.ID] == run {
			delete(r.runs, cam.ID)
		}
		r.mu.Unlock()
	}()
}

// cameraLoop is one camera's supervisor: spawn, watch, tail, and on an
// unexpected exit restart with backoff, until the context ends, the recorder
// closes, or the camera stops wanting a recorder.
func (r *Recorder) cameraLoop(ctx context.Context, run *run) {
	cam := run.camera
	backoff := r.BackoffStart

	for {
		if ctx.Err() != nil {
			return
		}

		generation, startNumber, err := r.prepareSpawn(cam)
		if err != nil {
			// The counter is unreadable. Recording does not start on a guess: a
			// recorder that invents a generation overwrites the files the last
			// generation wrote, which is the one thing this counter exists to
			// prevent.
			r.Logf("recorder: %s: %v (not starting; the segment counter is repaired by fixing the setting)", cam.Slug, err)
			if !r.sleepCtx(ctx, backoff) {
				return
			}
			backoff = r.nextBackoff(backoff)
			continue
		}

		argv := r.FfmpegArgs(&cam, generation, startNumber)
		// The list belongs to the run that is about to start, and this supervisor's
		// tail reads it from offset 0, so it has to be empty **before** the child
		// exists. ffmpeg rewrites it too — but only when it opens the muxer, a beat
		// after the spawn — and a tail that reads inside that window turns the dead
		// generation's lines into this generation's rows (S4-07's real-ffmpeg suite
		// caught it: generation 2 came back naming `porch-1-0.mp4`).
		if err := r.resetSegmentList(cam.Slug); err != nil {
			r.Logf("recorder: %s: %v (recording anyway; the list is emptied again at the next spawn)", cam.Slug, err)
		}
		started := r.now().UTC()
		proc, err := r.newProcess(ctx, argv)
		if err != nil {
			r.Logf("recorder: %s: starting ffmpeg: %v (retrying in %s)", cam.Slug, err, backoff)
			r.cameraOffline(cam)
			if !r.sleepCtx(ctx, backoff) {
				return
			}
			backoff = r.nextBackoff(backoff)
			continue
		}

		run.setProcess(proc)
		r.Logf("recorder: %s: recording gen=%d start_number=%d segment=%ds (pid %s)",
			cam.Slug, generation, startNumber, r.Cfg.SegmentSeconds, procID(proc))

		exit := r.tailSegments(ctx, run, cam, generation, started, proc, argv)

		// The child handle stays on the run. shutdown terminates children through
		// the run, and a supervisor that cleared its handle on the way out would
		// leave the shutdown path holding nothing to terminate. Stop is safe on an
		// exited child, so keeping the handle costs nothing.

		if ctx.Err() != nil || r.isClosed() {
			// Shutdown, a row change, or a mode that no longer wants a recorder.
			// Not a crash: no offline announcement, no restart.
			return
		}
		if !r.wantsRecording(cam) {
			return
		}

		if run.takeHealthy() {
			// This spawn produced footage, so the crash window starts over.
			backoff = r.BackoffStart
		}

		// S4-04: the process exited on its own. Say so, with what ffmpeg said,
		// and take the camera's tile down: a camera whose tile is green while its
		// recorder is dead is the failure the machine surface exists to prevent.
		r.Logf("recorder: %s: ffmpeg exited (%v); last output: %s (restarting in %s)",
			cam.Slug, exit, procOutput(proc), backoff)
		r.cameraOffline(cam)

		if !r.sleepCtx(ctx, backoff) {
			return
		}
		backoff = r.nextBackoff(backoff)
	}
}

// wantsRecording answers whether this camera row still wants a recorder right
// now. An events camera whose window closed stops at the window's end, which is
// the post-roll: the seconds after the event are part of the clip.
func (r *Recorder) wantsRecording(cam models.Camera) bool {
	if r.isClosed() {
		return false
	}
	switch cam.RecordMode {
	case models.RecordModeContinuous:
		return true
	case models.RecordModeEvents:
		return r.windowOpen(cam.ID)
	default:
		return false
	}
}

// nextBackoff doubles the restart delay and caps it at the plan's 60 s.
func (r *Recorder) nextBackoff(d time.Duration) time.Duration {
	next := d * 2
	if next > r.BackoffMax {
		return r.BackoffMax
	}
	return next
}

func (r *Recorder) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// sleepCtx waits, answering false when the context ended during the wait — which
// is how a shutdown lands in the middle of a restart backoff.
func (r *Recorder) sleepCtx(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return ctx.Err() == nil
	}
	if r.sleep != nil {
		r.sleep(d)
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// prepareSpawn picks the generation and the first segment number for a spawn, and
// persists the generation **before** ffmpeg starts.
//
// Persisting first is deliberate: a spawn that crashes after writing three
// segments must not have the next spawn read the same generation, because ffmpeg
// restarts its own counter at 0 and the files would collide.
func (r *Recorder) prepareSpawn(cam models.Camera) (generation, startNumber int, err error) {
	key := GenerationKey(cam.ID)
	raw, err := r.St.Settings.GetOr(key, "0")
	if err != nil {
		return 0, 0, fmt.Errorf("%s is unreadable: %w", key, err)
	}
	last, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, 0, fmt.Errorf("%s is %q, not a number", key, raw)
	}

	// The last number this camera's last generation wrote, so the new spawn
	// continues the sequence instead of starting a second sequence over the top
	// of it. `-reset_timestamps 1` makes each segment self-contained; the numbers
	// stay monotonic so a scrubber can read them in order.
	lastNumber, err := r.St.Segments.MaxSegmentNumber(cam.ID, last)
	if err != nil {
		return 0, 0, fmt.Errorf("last segment number for generation %d is unreadable: %w", last, err)
	}

	generation = last + 1
	if err := r.St.Settings.Set(key, strconv.Itoa(generation)); err != nil {
		return 0, 0, fmt.Errorf("saving %s=%d: %w", key, generation, err)
	}
	return generation, lastNumber + 1, nil
}

// resetSegmentList empties one camera's segment list before its ffmpeg starts.
//
// The recorder owns that emptiness rather than assuming it: ffmpeg truncates the
// list itself, but not until it has probed the input and opened the muxer, which
// is after the spawn. The tail starts at offset 0 for every spawn, so a list that
// still holds the previous generation's lines becomes rows attributed to this one
// — wrong names, wrong absolute times, and a manifest that double-counts.
func (r *Recorder) resetSegmentList(slug string) error {
	path := r.SegmentListPath(slug)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("emptying the segment list %s: %w", path, err)
	}
	return f.Close()
}

// cameraStatus is the plan's `camera.status` payload, as the camera API answers it
// too: the console switches on these names.
func (r *Recorder) cameraStatus(cam models.Camera, online bool, producers []string) {
	if producers == nil {
		producers = []string{}
	}
	_ = r.Hub.CameraStatus(map[string]any{
		"camera_id": cam.ID,
		"slug":      cam.Slug,
		"online":    online,
		"producers": producers,
	})
}

// cameraOffline says the recorder's process is gone. The producers list is empty
// because the truth is empty: this camera has no reader right now, and a status
// that repeats the configured source would report a camera as live while it is
// writing nothing.
func (r *Recorder) cameraOffline(cam models.Camera) {
	r.cameraStatus(cam, false, []string{})
}

// cameraRecording announces the source this recorder is actually reading, from
// the argv of the process that produced the segment.
func (r *Recorder) cameraRecording(cam models.Camera, argv []string) {
	r.cameraStatus(cam, true, []string{inputOf(argv)})
}

// inputOf picks the `-i` argument out of an ffmpeg argv: the source this process
// reads, which is the honest content of `producers`.
func inputOf(argv []string) string {
	for i, arg := range argv {
		if arg == "-i" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// execProcess is the real processFactory: the plan's exec.CommandContext, with
// the child terminated by SIGTERM on shutdown and its last lines of stderr kept
// for the log line that explains an exit.
func (r *Recorder) execProcess(ctx context.Context, argv []string) (process, error) {
	if len(argv) == 0 {
		return nil, errors.New("recorder: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// SIGTERM, not SIGKILL: ffmpeg closes the segment it is writing on a
	// SIGTERM, so the last segment is a playable file and its CSV line is
	// written. A SIGKILL leaves a truncated mp4 the clip builder cannot read.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	stderr := &tailBuffer{max: 8}
	cmd.Stderr = stderr
	cmd.Stdout = discardWriter{}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProcess{cmd: cmd, stderr: stderr, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// execProcess is one running ffmpeg.
type execProcess struct {
	cmd    *exec.Cmd
	stderr *tailBuffer
	done   chan struct{}
	err    error
	stop   sync.Once
}

func (p *execProcess) Done() <-chan struct{} { return p.done }
func (p *execProcess) Err() error            { return p.err }
func (p *execProcess) Stop() {
	p.stop.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
		}
	})
}

// tailBuffer keeps the last few lines written to it: ffmpeg's `-loglevel error`
// output is short and it is the reason, and the whole of a long run's stderr is
// not something to keep in memory for one log line.
type tailBuffer struct {
	mu  sync.Mutex
	buf []string
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		t.buf = append(t.buf, line)
		if len(t.buf) > t.max {
			t.buf = t.buf[len(t.buf)-t.max:]
		}
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buf) == 0 {
		return "(no output)"
	}
	return strings.Join(t.buf, " | ")
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// procID names the child for the boot log; an empty string for a fake process
// that has no pid.
func procID(p process) string {
	if ep, ok := p.(*execProcess); ok && ep.cmd.Process != nil {
		return strconv.Itoa(ep.cmd.Process.Pid)
	}
	return "fake"
}

// procOutput is what the child said on its way out.
func procOutput(p process) string {
	if ep, ok := p.(*execProcess); ok {
		return ep.stderr.String()
	}
	return "(no output)"
}

// minTime is the older of two times.
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
