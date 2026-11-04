package storage

// The key layout is the part of the tier an operator touches directly: it is
// what a restore reads, what a retention sweep matches, and what shows up in a
// bucket browser. So the layout is asserted as literal strings, and then again
// through all four drivers, because a key that round-trips through a driver's
// own path handling but lands somewhere else is still a broken layout.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvr/server/models"
)

// keyTestWhen is a Wednesday morning in November, deliberately not at a
// midnight or a month boundary so a date-formatting bug shows up as a wrong
// string rather than a plausible one.
var keyTestWhen = time.Date(2026, 11, 4, 6, 30, 0, 0, time.UTC)

// keyTestEventID is a UUID, because that is what an event id is.
const keyTestEventID = "3f2b1c00-0000-4000-8000-000000000001"

func TestKeyLayoutIsThePlansExactly(t *testing.T) {
	cases := []struct {
		name string
		ref  ObjectRef
		want string
	}{
		{
			"segment",
			ObjectRef{
				Kind: models.JobKindSegment, Slug: "front-door",
				When: keyTestWhen, LocalPath: "/var/lib/nvr/segments/front-door-0-7.mp4",
			},
			"nvr/front-door/2026/11/04/front-door-0-7.mp4",
		},
		{
			// The basename is the local file name verbatim, which is what lets a
			// restore be a copy into SEGMENTS_DIR.
			"segment with a name that has dots in it",
			ObjectRef{
				Kind: models.JobKindSegment, Slug: "cam-2",
				When: keyTestWhen, LocalPath: "/srv/nvr/cam-2-11-3.mp4",
			},
			"nvr/cam-2/2026/11/04/cam-2-11-3.mp4",
		},
		{
			"clip",
			ObjectRef{Kind: models.JobKindClip, EventID: keyTestEventID, LocalPath: "/tmp/build-9912.mp4"},
			"nvr/events/" + keyTestEventID + "/clip.mp4",
		},
		{
			"snapshot",
			ObjectRef{Kind: models.JobKindSnapshot, EventID: keyTestEventID, LocalPath: "/tmp/build-9912.jpg"},
			"nvr/events/" + keyTestEventID + "/snapshot.jpg",
		},
		{
			// A single-digit month and day are zero-padded, or the bucket browser
			// sorts August after December.
			"January is 01",
			ObjectRef{
				Kind: models.JobKindSegment, Slug: "gate",
				When: time.Date(2027, 1, 9, 0, 0, 0, 0, time.UTC), LocalPath: "gate-0-0.mp4",
			},
			"nvr/gate/2027/01/09/gate-0-0.mp4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := KeyFor(tc.ref)
			if err != nil {
				t.Fatalf("KeyFor: %v", err)
			}
			if got != tc.want {
				t.Fatalf("KeyFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyLayoutDatesInUTC(t *testing.T) {
	// A server whose zone is behind UTC records a segment at, say, 23:30Z on the
	// 4th; the key must say the 4th, not the local date the wall would show.
	local := time.FixedZone("UTC-5", -5*60*60)
	ref := ObjectRef{
		Kind: models.JobKindSegment, Slug: "front-door",
		When: keyTestWhen.In(local), LocalPath: "front-door-0-7.mp4",
	}
	got, err := KeyFor(ref)
	if err != nil {
		t.Fatal(err)
	}
	if want := "nvr/front-door/2026/11/04/front-door-0-7.mp4"; got != want {
		t.Fatalf("a key built from a non-UTC time is %q, want %q", got, want)
	}
}

func TestKeyLayoutRejectsWhatCannotBeAKey(t *testing.T) {
	cases := []struct {
		name string
		ref  ObjectRef
		want string
	}{
		{"an unknown kind", ObjectRef{Kind: "footage"}, "not a storage job kind"},
		{"an empty kind", ObjectRef{}, "not a storage job kind"},
		{"a slug with a slash", ObjectRef{Kind: models.JobKindSegment, Slug: "a/b",
			When: keyTestWhen, LocalPath: "x.mp4"}, "slug"},
		{"an empty slug", ObjectRef{Kind: models.JobKindSegment, Slug: "",
			When: keyTestWhen, LocalPath: "x.mp4"}, "slug"},
		{"a slug that is dot-dot", ObjectRef{Kind: models.JobKindSegment, Slug: "..",
			When: keyTestWhen, LocalPath: "x.mp4"}, "slug"},
		{"no start time", ObjectRef{Kind: models.JobKindSegment, Slug: "cam-1",
			LocalPath: "x.mp4"}, "start time"},
		{"a local path that is empty", ObjectRef{Kind: models.JobKindSegment, Slug: "cam-1",
			When: keyTestWhen, LocalPath: ""}, "no file name"},
		{
			// `filepath.Base` of a directory is the directory's own name, so a
			// path that names no file is caught by the extension check rather
			// than the empty-name one. Both refuse it; this pins which message
			// an operator reads.
			"a local path that is a directory", ObjectRef{Kind: models.JobKindSegment, Slug: "cam-1",
				When: keyTestWhen, LocalPath: "/var/lib/nvr/segments/"}, "not an mp4",
		},
		{"a segment that is not an mp4", ObjectRef{Kind: models.JobKindSegment, Slug: "cam-1",
			When: keyTestWhen, LocalPath: "/var/lib/nvr/segments/x.tmp"}, "not an mp4"},
		{"no event id", ObjectRef{Kind: models.JobKindClip}, "needs the event id"},
		{"an event id with a slash", ObjectRef{Kind: models.JobKindClip, EventID: "a/b"}, "event id"},
		{"an event id that is dot-dot", ObjectRef{Kind: models.JobKindSnapshot, EventID: ".."}, "event id"},
		{"an event id that is too long",
			ObjectRef{Kind: models.JobKindClip, EventID: strings.Repeat("a", maxIDLen+1)}, "event id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := KeyFor(tc.ref)
			if err == nil {
				t.Fatalf("KeyFor(%+v) produced %q, want an error naming %q", tc.ref, key, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the failure is %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestKeyLayoutOnEveryDriver is the card's criterion: the layout asserted on all
// four drivers. A key is only the layout if each driver puts the object where
// the layout says — so each case checks the *destination*, not a round trip
// through the driver's own idea of a path: the filesystem's own directory for fs
// and sftp, the request line the tier actually saw for s3 and WebDAV.
func TestKeyLayoutOnEveryDriver(t *testing.T) {
	refs := []ObjectRef{
		{Kind: models.JobKindSegment, Slug: "front-door", When: keyTestWhen,
			LocalPath: "/var/lib/nvr/segments/front-door-0-7.mp4"},
		{Kind: models.JobKindClip, EventID: keyTestEventID, LocalPath: "/tmp/build.mp4"},
		{Kind: models.JobKindSnapshot, EventID: keyTestEventID, LocalPath: "/tmp/build.jpg"},
	}

	drivers := []struct {
		kind string
		// open returns a backend and a function that answers whether an object
		// with the given key reached the place the layout says it should.
		open func(t *testing.T) (Backend, func(key string) bool)
	}{
		{
			models.StorageKindFS,
			func(t *testing.T) (Backend, func(string) bool) {
				b, root := newFSBackend(t)
				return b, onDisk(t, root)
			},
		},
		{
			models.StorageKindS3,
			func(t *testing.T) (Backend, func(string) bool) {
				s := newS3TestServer(t, s3TestAccess)
				b := newS3Backend(t, s, s3TestAccess, s3TestSecret, "")
				return b, func(key string) bool {
					want := "/" + s3TestBucket + "/" + key
					for _, got := range s.recorded() {
						if got == want {
							return true
						}
					}
					t.Errorf("the tier never saw a request for %s; it saw %v", want, s.recorded())
					return false
				}
			},
		},
		{
			models.StorageKindWebDAV,
			func(t *testing.T) (Backend, func(string) bool) {
				s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
				s.mkdir("/media")
				b := newWebdavBackend(t, s, "media", "")
				return b, func(key string) bool {
					if !s.saw("PUT", "/media/"+key) {
						t.Errorf("the share never saw a PUT for /media/%s; it saw %v", key, s.allRequests())
						return false
					}
					return true
				}
			},
		},
		{
			models.StorageKindSFTP,
			func(t *testing.T) (Backend, func(string) bool) {
				s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
				st := newStorageTestStore(t)
				root := s.mkdir("media")
				b, _, _ := newSFTPBackend(t, s, st, root, "", "")
				return b, onDisk(t, root)
			},
		},
	}

	for _, d := range drivers {
		t.Run(d.kind, func(t *testing.T) {
			b, landed := d.open(t)
			ctx := context.Background()

			for _, ref := range refs {
				key, err := KeyFor(ref)
				if err != nil {
					t.Fatalf("KeyFor(%s): %v", ref.Kind, err)
				}
				if err := b.Put(ctx, key, strings.NewReader("body"), 4, "video/mp4"); err != nil {
					t.Fatalf("Put(%s): %v", key, err)
				}
				if !landed(key) {
					t.Errorf("%s: %q did not reach the place the layout says", d.kind, key)
				}
				rc, err := b.Get(ctx, key)
				if err != nil {
					t.Fatalf("Get(%s): %v", key, err)
				}
				body, readErr := io.ReadAll(rc)
				_ = rc.Close()
				if readErr != nil || string(body) != "body" {
					t.Errorf("Get(%s) read %q, %v", key, body, readErr)
				}
				keys, err := b.List(ctx, "")
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				if !containsKey(keys, key) {
					t.Errorf("List() = %v, want it to contain %q", keys, key)
				}
				if err := b.Delete(ctx, key); err != nil {
					t.Fatalf("Delete(%s): %v", key, err)
				}
			}
		})
	}
}

// onDisk is the checker for the two drivers whose tier is a real filesystem: the
// object must be at root + key, with the key's slashes as directories.
func onDisk(t *testing.T, root string) func(string) bool {
	t.Helper()
	return func(key string) bool {
		full := filepath.Join(root, filepath.FromSlash(key))
		if _, err := os.Stat(full); err != nil {
			t.Errorf("%s is not at %s: %v", key, full, err)
			return false
		}
		return true
	}
}

// TestKeyLayoutIsStableAcrossRestarts is the property a restore depends on: the
// same facts produce the same key forever, so a bucket written before a restart
// is readable after one, and an operator's `cp --recursive` needs no table.
func TestKeyLayoutIsStableAcrossRestarts(t *testing.T) {
	ref := ObjectRef{
		Kind: models.JobKindSegment, Slug: "front-door", When: keyTestWhen,
		LocalPath: "/var/lib/nvr/segments/front-door-0-7.mp4",
	}
	first, err := KeyFor(ref)
	if err != nil {
		t.Fatal(err)
	}
	// A different absolute path with the same base name is the same object: the
	// tier names it after the file, not after the directory it happens to sit
	// in on this host. SEGMENTS_DIR may move; the key does not.
	other := ref
	other.LocalPath = "/srv/elsewhere/front-door-0-7.mp4"
	second, err := KeyFor(other)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the same file under two roots produced %q and %q", first, second)
	}
	// And the key is its own inverse in the only way that matters: the base name
	// of the key is the base name of the local file, which is what the segment
	// row already points at.
	if filepath.Base(first) != filepath.Base(ref.LocalPath) {
		t.Fatalf("a restore would land %q, but the row names %q",
			filepath.Base(first), filepath.Base(ref.LocalPath))
	}
}

// TestKeyLayoutIsSortableAndShallow states the two properties an operator
// browsing a bucket depends on.
func TestKeyLayoutIsSortableAndShallow(t *testing.T) {
	var keys []string
	for _, when := range []time.Time{
		time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC),
	} {
		key, err := KeyFor(ObjectRef{
			Kind: models.JobKindSegment, Slug: "cam-1", When: when, LocalPath: "cam-1-0-0.mp4",
		})
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
		if len(key) > 200 {
			t.Errorf("%q is %d bytes, which starts to crowd a tier's key limit", key, len(key))
		}
		if depth := strings.Count(key, "/"); depth != 5 {
			t.Errorf("%q is %d levels deep, want 5", key, depth)
		}
	}
	sorted := append([]string(nil), keys...)
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1] > sorted[i] {
			t.Fatalf("keys do not sort in time order: %v", keys)
		}
	}
}
