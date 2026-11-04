package storage

// The tier's key layout, in one function.
//
// Everything else in this package asks a destination to put bytes at a key;
// this file decides what that key is. It matters more than it looks: the key is
// the only place the layout exists, so the uploader, the sweeper, the playback
// proxy and an operator restoring footage with `aws s3 cp --recursive` all agree
// on where a file is without any of them coordinating.
//
// The layout is the plan's, exact:
//
//	segment    nvr/<camera_slug>/<YYYY>/<MM>/<DD>/<basename>.mp4
//	clip       nvr/events/<event_id>/clip.mp4
//	snapshot   nvr/events/<event_id>/snapshot.jpg
//
// `<basename>` is **the local file name**, and that single choice is what makes
// a restore a copy rather than a translation: the segment rows already point at
// that name, so downloading the tree back into `SEGMENTS_DIR` gives the manifest
// the files it already names. It also means the name is unique across cameras
// without a counter — the recorder's own name is `<slug>-<generation>-<n>.mp4`,
// which starts with the slug.
//
// The layout is deliberately shallow and sortable. A day of one camera's
// footage is one `ls`, the date path sorts in order in any browser, and no
// implementation's key-length limit (1024 bytes on S3) is in reach.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"nvr/server/models"
)

// keyRoot is the top-level directory every object of every installation lives
// under. It is *not* the row's `prefix`: the prefix is where on the tier this
// backend keeps its things (a bucket shared with another service), and the root
// is what this installation calls its own. Both are applied, in that order, so
// a bucket with no prefix holds `nvr/…` at its top and a shared one holds
// `media/nvr/…`.
const keyRoot = "nvr"

// eventRoot is where an event's media lives, one directory per event, so
// deleting an event's footage is one prefix and not a search.
const eventRoot = keyRoot + "/events"

// clipName and snapshotName are fixed, not derived from a local path: an event's
// clip is always `clip.mp4` whoever built it and whatever temp file the builder
// renamed. That is what lets the playback proxy answer
// `/api/events/:id/clip` without consulting the tier for a name.
const (
	clipName     = "clip.mp4"
	snapshotName = "snapshot.jpg"
)

// maxIDLen bounds an event id used as a path segment. A UUID is 36, and the
// bound is there to keep a hand-edited row from producing an absurd key.
const maxIDLen = 64

// ObjectRef is the local fact set a key is derived from.
//
// It is a struct rather than four arguments because the three kinds need
// different subsets, and a caller that fills the wrong ones should fail loudly
// in `KeyFor` rather than pass a zero time into a date path.
type ObjectRef struct {
	// Kind is a storage job kind: segment, clip or snapshot.
	Kind string

	// LocalPath is the file the bytes are in. For a segment its base name
	// becomes the object's basename; for an event object it is ignored.
	LocalPath string

	// Slug is the camera's slug, validated by models.ValidSlug — the same rule
	// that makes it a go2rtc stream name and a file prefix.
	Slug string

	// When is the segment's start. The date path is its UTC date; see
	// segmentKey for why UTC and not the server's zone.
	When time.Time

	// EventID is the event's id, for a clip or a snapshot.
	EventID string
}

// KeyFor is the layout. One function, one call site per job, and the only place
// a key is ever built.
func KeyFor(ref ObjectRef) (string, error) {
	var (
		key string
		err error
	)
	switch ref.Kind {
	case models.JobKindSegment:
		key, err = segmentKey(ref)
	case models.JobKindClip:
		key, err = eventKey(ref, clipName)
	case models.JobKindSnapshot:
		key, err = eventKey(ref, snapshotName)
	default:
		return "", fmt.Errorf("storage: %q is not a storage job kind, so it has no key", ref.Kind)
	}
	if err != nil {
		return "", err
	}
	// The belt. Every driver refuses a `..` in a key, and a layout that
	// produced one would be a bug in this file rather than an attack; checking
	// here means the bug is named where it is made.
	if !strings.HasPrefix(key, keyRoot+"/") || strings.Contains(key, "..") {
		return "", fmt.Errorf("storage: the layout produced %q, which is not under %s/", key, keyRoot)
	}
	return key, nil
}

// segmentKey is `nvr/<slug>/<YYYY>/<MM>/<DD>/<basename>.mp4`.
//
// The date is UTC. A server in a zone with a DST change would otherwise put the
// repeated hour of a fall-back into two different directories, and an
// installation that changes its TZ would split one day of footage across two
// paths — both of which read as lost footage to an operator browsing the bucket.
// The row's `start_ts` is already UTC, so this is the same number the timeline
// shows, just formatted for a path.
func segmentKey(ref ObjectRef) (string, error) {
	if !models.ValidSlug(ref.Slug) {
		return "", fmt.Errorf("storage: the camera slug %q is not usable as a key segment", ref.Slug)
	}
	if ref.When.IsZero() {
		return "", fmt.Errorf("storage: a segment key needs the segment's start time")
	}
	name := filepath.Base(strings.TrimSpace(ref.LocalPath))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "", fmt.Errorf("storage: the local path %q has no file name", ref.LocalPath)
	}
	// The layout says `.mp4`, and the recorder only ever writes mp4, so a
	// segment whose name says otherwise means the manifest and the file
	// disagree — which is worth naming here rather than uploading an object the
	// playback proxy will answer with the wrong type.
	if ext := strings.ToLower(filepath.Ext(name)); ext != ".mp4" {
		return "", fmt.Errorf("storage: a segment is uploaded as %q, which is not an mp4", name)
	}
	if !isSafeSegment(strings.TrimSuffix(name, filepath.Ext(name))) {
		return "", fmt.Errorf("storage: the file name %q is not usable as a key segment", name)
	}

	when := ref.When.UTC()
	return fmt.Sprintf("%s/%s/%04d/%02d/%02d/%s",
		keyRoot, ref.Slug, when.Year(), int(when.Month()), when.Day(), name), nil
}

// eventKey is `nvr/events/<event_id>/<name>`.
func eventKey(ref ObjectRef, name string) (string, error) {
	id := strings.TrimSpace(ref.EventID)
	if id == "" {
		return "", fmt.Errorf("storage: an event %s key needs the event id", name)
	}
	if !isSafeSegment(id) {
		return "", fmt.Errorf("storage: the event id %q is not usable as a key segment", ref.EventID)
	}
	return eventRoot + "/" + id + "/" + name, nil
}

// isSafeSegment reports whether a value can be dropped into a key verbatim.
//
// Ids are UUIDs in practice and this does not require one: the check that has to
// survive is the one against a row someone edited by hand, and "no separators,
// no dot-dot, no NUL, not absurdly long" is what a key needs regardless of what
// the id is spelled with.
func isSafeSegment(s string) bool {
	if s == "" || len(s) > maxIDLen {
		return false
	}
	if strings.ContainsAny(s, "/\\") || strings.ContainsRune(s, 0) {
		return false
	}
	for _, segment := range strings.Split(s, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
