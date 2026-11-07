package media

// Segment discovery: turning ffmpeg's own segment list into manifest rows.
//
// ffmpeg writes `-segment_list_type csv` as one line per **closed** segment:
//
//	<slug>-<generation>-<n>.mp4,0.000000,5.012000
//
// Those two numbers are offsets from the start of *this* ffmpeg run, in seconds.
// The recorder turns them into absolute times with its own start time, which is
// what makes the plan's clock rule true in code: the segment's `start_ts` is
// `when this supervisor started + what ffmpeg says the segment started at`, and
// nothing is read from a camera clock, an NTP server, or a file name.
//
// The tail tracks a byte offset into the CSV and consumes only complete lines, so
// a half-written line is read on the next tick and a segment is never ingested
// twice or skipped. Each spawn starts a fresh tail at offset 0, which is the
// "CSV offset resets on restart" rule — and the recorder makes that offset true
// by emptying the list before the child starts (`resetSegmentList`): ffmpeg
// rewrites the list too, but not until its muxer opens, and a tail that reads in
// that gap would re-ingest the dead run's lines as this run's.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"nvr/server/models"
)

// segmentEntry is one line of ffmpeg's CSV segment list.
type segmentEntry struct {
	Name  string
	Start float64
	End   float64
}

// segmentCSV is a tail over one camera's segment list.
type segmentCSV struct {
	path string
	// offset is how far this tail has read. It is the state that makes re-ingest
	// impossible: the manifest insert happens once per line, and a line the tail
	// has consumed is never consumed again.
	offset int64
	// partial is the trailing text with no newline: a segment ffmpeg is still
	// writing its line for.
	partial string
	// skipped counts lines that did not parse, so a tail that is silently
	// swallowing rows shows up in a log line.
	skipped int
}

// newSegmentCSV starts a tail at the beginning of a list. Each spawn gets its
// own: the offset belongs to a run, not to a file.
func newSegmentCSV(path string) *segmentCSV {
	return &segmentCSV{path: path}
}

// readNew returns the lines completed since the last call.
//
// A missing file is not an error: ffmpeg creates the list when it closes its
// first segment, so a camera that has not produced a segment yet has no list,
// and "no footage yet" is a state, not a failure.
func (c *segmentCSV) readNew() ([]segmentEntry, error) {
	f, err := os.Open(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking the segment list %s: %w", c.path, err)
	}

	var acc strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		acc.Write(buf[:n])
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("reading the segment list %s: %w", c.path, err)
		}
		if n == 0 {
			break
		}
	}

	data := c.partial + acc.String()
	lines := strings.Split(data, "\n")
	c.partial = lines[len(lines)-1]
	lines = lines[:len(lines)-1]
	// The offset moves past everything this read touched, **including the
	// unfinished tail**: that tail now lives in `partial`, so reading from disk at
	// the new offset sees only what is genuinely new. An offset that stayed behind
	// the partial re-reads it from disk and joins it to its own copy, which is the
	// line that "did not parse" — and, in a suite, a segment that never arrives.
	c.offset += int64(acc.Len())

	entries := make([]segmentEntry, 0, len(lines))
	for _, line := range lines {
		entry, err := parseSegmentLine(line)
		if err != nil {
			c.skipped++
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// skippedLines reports how many lines did not parse since the last report, and
// clearSkipped resets the count. A tail that swallows rows is the failure mode of
// every file tail, so the count is reported, not accumulated in silence.
func (c *segmentCSV) skippedLines() int { return c.skipped }
func (c *segmentCSV) clearSkipped()     { c.skipped = 0 }

// parseSegmentLine reads one `filename,start_time,end_time` line.
func parseSegmentLine(line string) (segmentEntry, error) {
	fields := strings.Split(strings.TrimSpace(line), ",")
	if len(fields) != 3 {
		return segmentEntry{}, fmt.Errorf("segment line %q is not filename,start,end", line)
	}
	start, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
	if err != nil {
		return segmentEntry{}, fmt.Errorf("segment start time %q: %w", fields[1], err)
	}
	end, err := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
	if err != nil {
		return segmentEntry{}, fmt.Errorf("segment end time %q: %w", fields[2], err)
	}
	name := strings.TrimSpace(fields[0])
	if name == "" {
		return segmentEntry{}, fmt.Errorf("segment line %q has no file name", line)
	}
	return segmentEntry{Name: name, Start: start, End: end}, nil
}

// segmentKindFor names what kind of footage a mode produces.
//
// `continuous` is the always-on recorder; a segment written inside an event
// window is `event`. The kind is recorded and never used to exclude footage: the
// clip builder selects overlapping segments of any kind, because an event segment
// is footage exactly like a continuous one.
func segmentKindFor(recordMode string) string {
	if recordMode == models.RecordModeEvents {
		return models.SegmentKindEvent
	}
	return models.SegmentKindContinuous
}

// recordSegment turns one closed segment into its manifest row.
//
// The times are the supervisor's start time plus ffmpeg's offsets; the byte count
// is the file on disk, stat'd now. A row whose bytes are 0 is a segment the
// recorder wrote and cannot read, and the row says so rather than inventing a
// size.
func (r *Recorder) recordSegment(cam models.Camera, generation int, started time.Time, entry segmentEntry) (*models.Segment, error) {
	// The CSV lists the bare file name — `<slug>-<generation>-<n>.mp4`, which is
	// what ffmpeg writes when no `-segment_list_entry_prefix` is passed (S4-07).
	// Base keeps the row's path inside SEGMENTS_DIR whatever ffmpeg puts there.
	path := filepath.Join(r.Cfg.SegmentsDir, filepath.Base(entry.Name))
	seg := &models.Segment{
		ID:         uuid.NewString(),
		CameraID:   cam.ID,
		Generation: generation,
		Path:       path,
		StartTS:    absoluteTime(started, entry.Start),
		EndTS:      absoluteTime(started, entry.End),
		Seconds:    entry.End - entry.Start,
		Kind:       segmentKindFor(cam.RecordMode),
	}
	if seg.Seconds < 0 {
		seg.Seconds = 0
	}
	if fi, err := os.Stat(path); err == nil {
		seg.Bytes = fi.Size()
	}
	if err := r.St.Segments.Create(seg); err != nil {
		return nil, fmt.Errorf("recorder: saving the segment row for %s: %w", path, err)
	}
	// The object is on disk and named on its row, so its obligation to the tier
	// starts now. A queue that only the uploader knows about is how S7-08's
	// green suite hid an empty queue from verify.sh's gate 7.
	if _, err := r.St.EnqueueUpload(models.JobKindSegment, seg.ID, seg.Path); err != nil {
		r.Logf("recorder: segment %s could not be queued for upload: %v", seg.ID, err)
	}
	return seg, nil
}

// absoluteTime is the plan's clock rule in one line: the server's own start time
// plus ffmpeg's offset.
func absoluteTime(started time.Time, offsetSeconds float64) time.Time {
	return started.Add(time.Duration(offsetSeconds * float64(time.Second))).UTC()
}

// tailSegments watches one running ffmpeg: it reads the segment list as it grows,
// writes the manifest rows, and announces each completed segment. It returns when
// the process exits (with the exit error), when the context ends, or when an
// event window closes.
func (r *Recorder) tailSegments(ctx context.Context, run *run, cam models.Camera, generation int, started time.Time, proc process, argv []string) error {
	csv := newSegmentCSV(r.SegmentListPath(cam.Slug))

	// The tail's own liveness clock: the last moment this child proved it was
	// moving footage. The stall watchdog below reads it.
	lastSegment := started.UTC()

	drain := func() {
		entries, err := csv.readNew()
		if err != nil {
			r.Logf("recorder: %s: %v", cam.Slug, err)
			return
		}
		if n := csv.skippedLines(); n > 0 {
			r.Logf("recorder: %s: %d segment list line(s) did not parse", cam.Slug, n)
			csv.clearSkipped()
		}
		for _, entry := range entries {
			seg, err := r.recordSegment(cam, generation, started, entry)
			if err != nil {
				r.Logf("recorder: %v", err)
				continue
			}
			lastSegment = r.now().UTC()
			// Footage on disk is the only proof that matters: it resets the
			// restart backoff, it puts the camera's tile up, and it ticks the
			// recorder's status line. One per segment, never per frame.
			run.markHealthy()
			r.cameraRecording(cam, argv)
			r.recorderStatus(cam, seg)
		}
	}

	ticker := time.NewTicker(r.TailInterval)
	defer ticker.Stop()

	for {
		drain()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-proc.Done():
			// ffmpeg closes a segment, writes its CSV line, then exits. The line
			// and the exit land together, so drain once more before reporting the
			// exit: the last segment of a run must not be lost to a race.
			drain()
			return proc.Err()
		case <-ticker.C:
			// The window check runs on the tail's own clock, so the post-roll
			// ends recording seconds after the event does, not at the next poll.
			if cam.RecordMode == models.RecordModeEvents && !r.windowOpen(cam.ID) {
				proc.Stop()
				drain()
				return errWindowClosed
			}
			// The stall watchdog: the child claims to be running and the window
			// is open, but no segment has landed for three segment-lengths plus
			// slack. A half-open RTSP connection — go2rtc's producer died
			// mid-stream, which the dev stack did — leaves ffmpeg blocked on
			// read forever, and no input flag cuts that stall (verified
			// against the image's ffmpeg 6.1.1 by killing the producer under a
			// connected consumer). The silence is the only evidence there is,
			// and ending the child is what turns it into a logged restart.
			if idle := r.now().UTC().Sub(lastSegment); idle > r.stallLimit() {
				r.Logf("recorder: %s: no segment for %s with the child still running — the input is stalled", cam.Slug, idle.Truncate(time.Second))
				proc.Stop()
				drain()
				return errStalledInput
			}
		}
	}
}

// errWindowClosed is the tail's name for "the event is over and the post-roll is
// spent". It is not a crash: the supervisor stops without an offline broadcast
// and without a restart, and the camera is recording again on the next signal.
var errWindowClosed = errors.New("event window closed")

// errStalledInput is the tail's name for "the child is alive and the footage
// stopped". Unlike errWindowClosed it is a crash: the supervisor says so, takes
// the camera's tile down, and restarts with backoff.
var errStalledInput = errors.New("input stalled: no segment")

// stallLimit is how long a running child may go without landing a segment
// before the tail calls its input dead: three segment-lengths — the muxer's
// own cadence, ffmpeg's startup probe, and one lost segment of slack — plus
// ten seconds.
func (r *Recorder) stallLimit() time.Duration {
	return time.Duration(3*r.Cfg.SegmentSeconds+10) * time.Second
}

// recorderStatus is the plan's `recorder.status`: one broadcast per completed
// segment, carrying what the console's status line reads.
//
// Not per frame. A status per frame is a feed that spends itself on a message the
// console shows as one number, and the number it shows is "the last segment
// landed at…", which changes once per segment.
func (r *Recorder) recorderStatus(cam models.Camera, seg *models.Segment) {
	_ = r.Hub.RecorderStatus(map[string]any{
		"camera_id":       cam.ID,
		"mode":            cam.RecordMode,
		"segment_seconds": r.Cfg.SegmentSeconds,
		"last_segment_at": seg.EndTS.UTC().Format(time.RFC3339),
	})
}
