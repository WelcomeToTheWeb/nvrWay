package media

// Clip assembly: turning the manifest rows an event covers into the one mp4 a
// player is handed (plan Step 6, card S6-01).
//
// The clip is not a recording. The recorder already wrote the footage; this is
// a *window* over it — `[started_at - PRE_ROLL_SECONDS, ended_at + POST_ROLL_SECONDS]`
// — cut out of whatever segments cover that span and concatenated into
// `<CLIPS_DIR>/<event_id>.mp4`. Three consequences shape the code:
//
//   - **The assembly waits for the window to end.** A clip cut the moment an
//     event closes stops at the close, and the seconds after the trigger are the
//     ones an operator wants to see. So the assembly is armed `POST_ROLL_SECONDS`
//     after `EndedAt`, which is also the instant the recorder stops an
//     `events`-mode camera: the post-roll is one number, used by both.
//   - **The manifest is the only source of footage.** Segments come from
//     `segments` rows (`ListRange`, overlapping, any `kind`), never from a
//     directory scan: a file with no row is not footage, it is garbage the
//     sweeper is coming for. A row whose local file is gone is skipped, because
//     concat would fail the whole clip over one aged-out segment.
//   - **"No clip" is an outcome, not a failure.** A camera in `record_mode=off`
//     has no segments in its window, and the plan says what happens then: the
//     event keeps its row and its snapshot, `clip_key` stays empty, and the log
//     says `clip skipped: no segments for window`.
//
// The timing is injected and the media commands are injected, exactly as they
// are in the recorder: the scheduling rules (armed when, cancelled on close) and
// the selection rules (which segments, what ClipSeconds) are policy, and policy
// is asserted in milliseconds. The real-ffmpeg half of the suite is in
// `clips_test.go` and skips loudly where ffmpeg is absent — a clip suite that
// reports green while cutting nothing is the green that lies.

import (
	"bytes"
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
	"time"

	"nvr/server/config"
	"nvr/server/models"
	"nvr/server/store"
)

const (
	// DefaultClipReconcileInterval is how often the builder re-reads the event
	// list. The signal path arms an assembly the moment a close lands, so the
	// reconcile pass exists for what the signal path cannot see: events that were
	// already open, or already closed-without-a-clip, when this process started.
	DefaultClipReconcileInterval = 15 * time.Second

	// MissedEventLookback bounds the boot/reconcile sweep for closed events that
	// never got a clip — the ones closed while this server was down. It is a
	// bound, and it is stated: an event older than this that is still missing its
	// clip is a disk the operator cleared, not an assembly this process owes.
	MissedEventLookback = 24 * time.Hour

	// MinClipSeconds is the floor a clip has to clear to be worth playing. Under
	// it, a player is shown a file so short the scrubber cannot land on anything;
	// the plan's fix is to re-run the concat with the last segment repeated once,
	// which lengthens the file without inventing footage that was never recorded.
	MinClipSeconds = 2.0
)

// clipCommand runs one short-lived media command to completion and returns its
// stdout. ffmpeg (concat) and ffprobe (duration) are both short-lived, unlike
// the recorder's children, so this seam is a `Run` rather than a process handle.
//
// It is injectable for the same reason the recorder's factory is: the exit
// criteria here are about *selection and arithmetic* — which segments, what
// ClipSeconds, when the assembly fires — and a suite that needs a real encoder
// to assert arithmetic is a suite people skip. The real encoder is exercised by
// the ffprobe-measured criterion in clips_test.go, in the image that carries it.
type clipCommand func(ctx context.Context, argv []string) ([]byte, error)

// clipTimer is an armed assembly.
type clipTimer interface {
	// Stop disarms it, answering false when it had already fired.
	Stop() bool
}

// armFunc is the scheduler: `arm(d, f)` runs f about d from now, on its own
// goroutine. The production arm is `time.AfterFunc` (the plan's mechanism, one
// per open event); a suite swaps in a recorder it can fire and count by hand.
type armFunc func(d time.Duration, f func()) clipTimer

// afterTimer is the production clipTimer: a `time.AfterFunc` handle.
type afterTimer struct{ t *time.Timer }

func (a *afterTimer) Stop() bool { return a.t.Stop() }

// afterFuncArm is the production scheduler.
func afterFuncArm(d time.Duration, f func()) clipTimer {
	return &afterTimer{t: time.AfterFunc(d, f)}
}

// ClipBuilder assembles event clips: one ffmpeg concat per event, armed to fire
// after the event's post-roll.
type ClipBuilder struct {
	Cfg *config.Config
	St  *store.Store

	// FfmpegBin and FfprobeBin are the binaries. Both live in the server image
	// (S9-01 installs ffmpeg); a suite points them at anything it likes, and the
	// real-ffmpeg suite uses whatever is on PATH.
	FfmpegBin  string
	FfprobeBin string

	// ReconcileInterval is the event-list poll described above.
	ReconcileInterval time.Duration

	// Logf is the log. "clip skipped: no segments for window" is an exit
	// criterion, so it is asserted by reading the log, not by watching stdout.
	Logf func(format string, args ...any)

	// OnAssembled runs after an assembly has landed on the event row, with the row
	// as it now reads — including the empty-window case, where what landed is the
	// knowledge that no clip is coming.
	//
	// It is a callback rather than a hub reference because this package must not own
	// the message shapes: what the console receives is the event view, and the view
	// belongs to the API. main.go wires the two together, and a builder with no
	// callback is a builder that records clips without announcing them — which is
	// what every build before S6-08 was.
	OnAssembled func(ev *models.Event)

	// run, arm and now are the injected seams: the media commands, the
	// scheduler, and the clock every delay is measured against.
	run  clipCommand
	arm  armFunc
	now  func() time.Time
	base context.Context

	mu      sync.Mutex
	timers  map[string]clipTimer
	attempt map[string]bool
	closed  bool
}

// NewClipBuilder builds the builder with the production command runner and the
// production scheduler.
func NewClipBuilder(cfg *config.Config, st *store.Store) *ClipBuilder {
	b := &ClipBuilder{
		Cfg:               cfg,
		St:                st,
		FfmpegBin:         "ffmpeg",
		FfprobeBin:        "ffprobe",
		ReconcileInterval: DefaultClipReconcileInterval,
		Logf:              func(format string, args ...any) { log.Printf(format, args...) },
		run:               runClipCommand,
		arm:               afterFuncArm,
		now:               func() time.Time { return time.Now().UTC() },
		base:              context.Background(),
		timers:            map[string]clipTimer{},
		attempt:           map[string]bool{},
	}
	return b
}

// SetClock points every delay this builder computes at one clock, so a suite
// sets an event's window and reads back the delay it produces instead of
// waiting through a post-roll.
func (b *ClipBuilder) SetClock(now func() time.Time) {
	if now != nil {
		b.now = now
	}
}

// SetCommandFactory replaces the thing that runs ffmpeg and ffprobe.
func (b *ClipBuilder) SetCommandFactory(run clipCommand) {
	if run != nil {
		b.run = run
	}
}

// SetTimerFactory replaces the scheduler. Suites use it to assert "one timer per
// open event, cancelled on close" without waiting for either deadline.
func (b *ClipBuilder) SetTimerFactory(arm armFunc) {
	if arm != nil {
		b.arm = arm
	}
}

// Run reconciles the event list until the lifecycle context ends or Close is
// called. Assemblies it armed keep running on their own timers; shutdown stops
// them through the context it handed them.
func (b *ClipBuilder) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.mu.Lock()
	b.base = ctx
	b.mu.Unlock()

	interval := b.ReconcileInterval
	if interval <= 0 {
		interval = DefaultClipReconcileInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	b.reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.reconcile()
		}
	}
}

// Close disarms every pending assembly. An ffmpeg already running is cancelled by
// the lifecycle context, not here.
func (b *ClipBuilder) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for id, t := range b.timers {
		t.Stop()
		delete(b.timers, id)
	}
}

// reconcile arms what the signal path could not have armed:
//
//   - every **open** event, so a server restart does not leave a live event with
//     no scheduled clip (the plan's reason `ListOpen` exists);
//   - every **closed** event inside MissedEventLookback that has no clip at all,
//     which is the set closed while this process was down.
//
// Both are bounded reads (`ListOpen` is the open set; the closed sweep is the
// event list's own 200-row page, newest first), and neither re-assembles an
// event this process already tried, so a poll tick is a query, not a fan-out.
func (b *ClipBuilder) reconcile() {
	if b.isClosed() {
		return
	}
	open, err := b.St.Events.ListOpen()
	if err != nil {
		b.Logf("clips: reading open events: %v", err)
	} else {
		for i := range open {
			b.Schedule(&open[i])
		}
	}

	from := b.now().UTC().Add(-MissedEventLookback)
	closed, err := b.St.Events.List(store.EventFilter{From: &from, Limit: store.EventListMaxLimit}, 0)
	if err != nil {
		b.Logf("clips: reading recent events for missed clips: %v", err)
		return
	}
	for i := range closed {
		ev := closed[i]
		if ev.Open || eventHasClip(&ev) || b.wasAttempted(ev.ID) {
			continue
		}
		// The post-roll of an event closed an hour ago is long spent: the delay
		// clamps to zero and the assembly runs on the next timer tick.
		b.Schedule(&ev)
	}
}

// eventHasClip answers whether an event's clip is accounted for: on disk, or
// uploaded (the local file may already be swept). An event with neither is one
// this process still owes an assembly for.
func eventHasClip(ev *models.Event) bool {
	return ev != nil && (ev.LocalClipPath != "" || ev.ClipUploadedAt != nil)
}

// Schedule arms (or re-arms) one event's assembly. The signal path calls it on
// every applied signal, which is what makes the plan's two rules true in one
// place:
//
//   - an **open** event gets exactly one armed timer, at the ceiling
//     `started_at + EVENT_MAX_SECONDS + POST_ROLL_SECONDS`. Extends do not move
//     it: an open event cannot outlive `EVENT_MAX_SECONDS`, so if the worker dies
//     without ever sending a close, that deadline is this event's only clip.
//   - a **close** cancels whatever was armed and re-arms at
//     `ended_at + POST_ROLL_SECONDS`, which is the moment the footage the clip
//     needs is all on disk.
//
// A delay that is already past clamps to zero: an event closed while the server
// was down is assembled now, not at a deadline the clock already passed.
func (b *ClipBuilder) Schedule(ev *models.Event) {
	if ev == nil {
		return
	}
	delay, deadline := b.assemblyDelay(ev)

	b.mu.Lock()
	if b.closed || b.attempt[ev.ID] {
		b.mu.Unlock()
		return
	}
	if prev, armed := b.timers[ev.ID]; armed {
		if ev.Open {
			// Already armed for the open case, and the open case's deadline does
			// not depend on this signal. One timer per open event, not one per
			// extend.
			b.mu.Unlock()
			return
		}
		prev.Stop()
		delete(b.timers, ev.ID)
	}
	ctx := b.base
	b.timers[ev.ID] = b.arm(delay, func() { b.assembleScheduled(ctx, ev.ID) })
	b.mu.Unlock()

	b.Logf("clips: event %s: assembly armed for %s (%s from now, %s)",
		ev.ID, deadline.Format(time.RFC3339), delay, scheduleKind(ev))
}

// scheduleKind names which of the two rules armed a timer, for the log line.
func scheduleKind(ev *models.Event) string {
	if ev.Open {
		return "open ceiling"
	}
	return "post-roll"
}

// assemblyDelay answers "how long from now" and "at what absolute instant".
func (b *ClipBuilder) assemblyDelay(ev *models.Event) (time.Duration, time.Time) {
	post := time.Duration(b.Cfg.PostRollSeconds) * time.Second
	deadline := ev.EndedAt.UTC().Add(post)
	if ev.Open {
		ceiling := ev.StartedAt.UTC().Add(time.Duration(b.Cfg.EventMaxSeconds) * time.Second)
		if ceiling.After(deadline) {
			deadline = ceiling.Add(post)
		}
	}
	delay := deadline.Sub(b.now().UTC())
	if delay < 0 {
		delay = 0
	}
	return delay, deadline
}

// Cancel disarms a pending assembly without assembling it. The delete path
// (S6-04) uses it: an event an operator deleted has no clip to file.
func (b *ClipBuilder) Cancel(eventID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.timers[eventID]; ok {
		t.Stop()
		delete(b.timers, eventID)
	}
}

// pendingCount is how many assemblies are armed: the number a suite asserts
// "one timer per open event" against, and the number a health surface would show.
func (b *ClipBuilder) pendingCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.timers)
}

func (b *ClipBuilder) wasAttempted(eventID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempt[eventID]
}

func (b *ClipBuilder) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// assembleScheduled is what a timer fires. It skips an event this process has
// already tried, which is what keeps a reconcile tick from re-running a failed
// ffmpeg every fifteen seconds forever.
func (b *ClipBuilder) assembleScheduled(ctx context.Context, eventID string) {
	b.mu.Lock()
	delete(b.timers, eventID)
	if b.attempt[eventID] {
		b.mu.Unlock()
		return
	}
	b.attempt[eventID] = true
	b.mu.Unlock()

	if err := b.Assemble(ctx, eventID); err != nil {
		b.Logf("clips: event %s: %v", eventID, err)
	}
}

// Assemble cuts one event's clip now: select the segments the window covers,
// concat them, measure the result, pad it if it is too short to play, and file
// the outcome on the row.
//
// Every outcome except a command failure leaves the row stating something: a clip
// with its seconds, or an explicit "no clip". A failure leaves the row untouched,
// which is the truth — the event is still there, its media is still missing, and
// the log says why.
func (b *ClipBuilder) Assemble(ctx context.Context, eventID string) error {
	if ctx == nil {
		ctx = b.base
	}
	ev, err := b.St.Events.GetByID(eventID)
	if err != nil {
		return fmt.Errorf("clips: event %s could not be read: %w", eventID, err)
	}

	from, to := b.window(ev)
	rows, err := b.St.Segments.ListRange(ev.CameraID, from, to, 0)
	if err != nil {
		return fmt.Errorf("clips: event %s: segments could not be read: %w", eventID, err)
	}
	segments, missing := b.selectReadable(rows)

	if len(segments) == 0 {
		// The plan's empty-window rule. The event stays visible with its
		// snapshot; only the clip is absent.
		if err := b.St.Events.SetClipAbsent(ev.ID); err != nil {
			return fmt.Errorf("clips: event %s: the absent clip could not be recorded: %w", ev.ID, err)
		}
		b.Logf("clip skipped: no segments for window (event %s, %s → %s, %d row(s) unreadable)",
			ev.ID, from.Format(time.RFC3339), to.Format(time.RFC3339), missing)
		// The console's player has been waiting on this event; "there will be no
		// clip" is the answer it needs as much as the clip itself.
		b.notifyAssembled(ev)
		return nil
	}

	if err := b.Cfg.EnsureDirs(); err != nil {
		return fmt.Errorf("clips: %w", err)
	}
	outPath := b.ClipPath(ev.ID)
	listPath, err := b.writeConcatList(ev.ID, segments)
	if err != nil {
		return fmt.Errorf("clips: event %s: %w", ev.ID, err)
	}
	defer func() {
		// The list is a temp file holding absolute paths. Leaving it behind is
		// how a data dir fills with one text file per event.
		if err := os.Remove(listPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.Logf("clips: event %s: concat list %s could not be removed: %v", ev.ID, listPath, err)
		}
	}()

	if _, err := b.run(ctx, b.concatArgv(listPath, outPath)); err != nil {
		return fmt.Errorf("clips: event %s: concat failed: %w", ev.ID, err)
	}
	if _, err := os.Stat(outPath); err != nil {
		return fmt.Errorf("clips: event %s: ffmpeg exited without writing %s: %w", ev.ID, outPath, err)
	}

	measured, probeErr := b.probeDuration(ctx, outPath)
	if probeErr != nil {
		b.Logf("clips: event %s: %v (the clip is filed unmeasured)", ev.ID, probeErr)
	}

	// The plan's padding rule: a clip under two seconds is a file a scrubber
	// cannot land on, so the concat is re-run with the last segment repeated
	// once. The repeat lengthens the file; it does not lengthen the event, which
	// is why ClipSeconds below is computed from the selected set, not the padded one.
	if probeErr == nil && measured < MinClipSeconds {
		padded := append(append([]models.Segment{}, segments...), segments[len(segments)-1])
		if _, err := b.writeConcatList(ev.ID, padded); err != nil {
			b.Logf("clips: event %s: padding list could not be written: %v (filing the short clip)", ev.ID, err)
		} else if _, err := b.run(ctx, b.concatArgv(listPath, outPath)); err != nil {
			b.Logf("clips: event %s: padding concat failed: %v (filing the short clip)", ev.ID, err)
		} else if longer, err := b.probeDuration(ctx, outPath); err == nil {
			b.Logf("clips: event %s: clip measured %.2fs, padded by repeating the last segment: %.2fs",
				ev.ID, measured, longer)
			measured = longer
		}
	}

	windowSeconds := to.Sub(from).Seconds()
	sumSeconds := 0.0
	for i := range segments {
		sumSeconds += segments[i].Seconds
	}
	// The plan's arithmetic: the clip is as long as the window asked for, and no
	// longer than the footage that exists. Overlapping segments (a recorder
	// restart that overlapped its predecessor by a second) make the sum exceed the
	// window, and the window wins.
	clipSeconds := windowSeconds
	if sumSeconds < clipSeconds {
		clipSeconds = sumSeconds
	}

	if err := b.St.Events.SetLocalClip(ev.ID, outPath, clipSeconds); err != nil {
		return fmt.Errorf("clips: event %s: the clip could not be recorded on the row: %w", ev.ID, err)
	}

	// The row now reads differently and the consoles watching do not poll. The
	// in-memory copy is brought in line with the write first, so the payload the
	// callback builds carries the clip rather than the state from before it.
	ev.LocalClipPath = outPath
	ev.ClipSeconds = clipSeconds
	b.notifyAssembled(ev)

	b.Logf("clips: event %s: %d segment(s) → %s (window %.2fs, footage %.2fs, clip %.2fs, ffprobe %s)",
		ev.ID, len(segments), outPath, windowSeconds, sumSeconds, clipSeconds, formatSeconds(measured, probeErr))
	return nil
}

// notifyAssembled hands the finished row to the live feed. A builder with no
// callback is the common case in tests and in a build with no hub, so this is the
// only place the nil check lives.
func (b *ClipBuilder) notifyAssembled(ev *models.Event) {
	if b.OnAssembled == nil || ev == nil {
		return
	}
	b.OnAssembled(ev)
}

// window is the clip's span: the pre-roll before the trigger and the post-roll
// after the last hit, both from config, both the same numbers the recorder uses
// to decide when to start and stop an `events`-mode camera.
func (b *ClipBuilder) window(ev *models.Event) (time.Time, time.Time) {
	from := ev.StartedAt.UTC().Add(-time.Duration(b.Cfg.PreRollSeconds) * time.Second)
	to := ev.EndedAt.UTC().Add(time.Duration(b.Cfg.PostRollSeconds) * time.Second)
	return from, to
}

// ClipPath is the plan's name for one event's clip: the id, so a clip cannot
// collide with another event's, and so the delete path can find the file from the
// row without storing a second name.
func (b *ClipBuilder) ClipPath(eventID string) string {
	return filepath.Join(b.Cfg.ClipsDir, eventID+".mp4")
}

// selectReadable keeps the segments whose file is actually here. A row with
// `local_deleted_at` set is footage the sweeper already took; a row whose file
// vanished underneath us is the same state one tick late. Either one fails the
// whole concat if it is passed to ffmpeg, so the row is skipped, counted, and the
// count is logged — a clip that quietly lost its first ten seconds is the failure
// mode of every media assembler.
func (b *ClipBuilder) selectReadable(rows []models.Segment) ([]models.Segment, int) {
	usable := make([]models.Segment, 0, len(rows))
	missing := 0
	for _, seg := range rows {
		if !seg.HasLocalFile() {
			missing++
			continue
		}
		if _, err := os.Stat(seg.Path); err != nil {
			missing++
			continue
		}
		usable = append(usable, seg)
	}
	return usable, missing
}

// writeConcatList writes ffmpeg's concat input: one `file '<abs path>'` line per
// segment, in manifest order. The list is written to CLIPS_DIR rather than
// `/tmp` so the file is on the same filesystem as the clip, and it is named after
// the event so two concurrent assemblies never read each other's list.
func (b *ClipBuilder) writeConcatList(eventID string, segments []models.Segment) (string, error) {
	path := filepath.Join(b.Cfg.ClipsDir, eventID+"-concat.txt")
	var sb strings.Builder
	for _, seg := range segments {
		abs, err := filepath.Abs(seg.Path)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", seg.Path, err)
		}
		// ffmpeg's concat demuxer takes the path in single quotes; a file name
		// containing a single quote is escaped the way its parser asks ('\'').
		sb.WriteString("file '" + strings.ReplaceAll(abs, "'", `'\''`) + "'\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o640); err != nil {
		return "", fmt.Errorf("writing the concat list %s: %w", path, err)
	}
	return path, nil
}

// concatArgv is the plan's assembly command, verbatim.
func (b *ClipBuilder) concatArgv(listPath, outPath string) []string {
	return []string{
		b.FfmpegBin,
		"-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listPath,
		"-c", "copy",
		"-movflags", "+faststart",
		outPath,
	}
}

// probeArgv asks ffprobe for one number: the container's duration in seconds.
// `noprint_wrappers=1:nokey=1` is what makes the answer a bare float, so the
// parse does not depend on the section names a version might rename.
func (b *ClipBuilder) probeArgv(path string) []string {
	return []string{
		b.FfprobeBin,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	}
}

// probeDuration measures a file the way a player will see it.
func (b *ClipBuilder) probeDuration(ctx context.Context, path string) (float64, error) {
	out, err := b.run(ctx, b.probeArgv(path))
	if err != nil {
		return 0, fmt.Errorf("ffprobe %s failed: %w", filepath.Base(path), err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe reported %q for %s, which is not a duration", strings.TrimSpace(string(out)), path)
	}
	if seconds < 0 {
		return 0, fmt.Errorf("ffprobe reported %.3fs for %s", seconds, path)
	}
	return seconds, nil
}

// formatSeconds renders a measurement for the log, naming the failure instead of
// hiding it behind a zero.
func formatSeconds(seconds float64, err error) string {
	if err != nil {
		return "unmeasured"
	}
	return fmt.Sprintf("%.2fs", seconds)
}

// runClipCommand is the production clipCommand: run it, keep stdout, and put
// ffmpeg's last lines into the error, because "concat failed" without ffmpeg's
// own sentence is a log line that sends someone to a shell history.
func runClipCommand(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("clips: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out bytes.Buffer
	stderr := &tailBuffer{max: 8}
	cmd.Stdout = &out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			return out.Bytes(), fmt.Errorf("%s: %w", filepath.Base(argv[0]), err)
		}
		return out.Bytes(), fmt.Errorf("%s: %w (%s)", filepath.Base(argv[0]), err, reason)
	}
	return out.Bytes(), nil
}
