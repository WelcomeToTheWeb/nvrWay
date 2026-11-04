package storage

// S7-03 — the `fs` driver: a directory, an NFS mount, or a compose volume.
//
// The exit criteria are "put/get/stat/delete/list asserted against a temp dir",
// "a crash mid-write leaves no <key> file" and "path confinement: a key cannot
// escape RootPath". The first is the shared contract (contract_test.go) run
// against a temp directory; the second is asserted by a reader that looks at the
// directory *during* the copy and then fails; the third is asserted against
// every shape a `..` can arrive in.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"nvr/server/models"
)

// newFSBackend is this driver's fixture: a temp directory as the tier root, and
// a backend opened the way the process opens one — through Open, from a row.
func newFSBackend(t *testing.T) (Backend, string) {
	t.Helper()
	root := t.TempDir()
	row := &models.StorageBackend{
		ID: "fs-1", Name: "local-tier", Kind: models.StorageKindFS, RootPath: root,
	}
	b, err := Open(context.Background(), row, sealKey(t))
	if err != nil {
		t.Fatalf("Open(fs): %v", err)
	}
	return b, root
}

func TestFSContractAgainstATempDir(t *testing.T) {
	b, _ := newFSBackend(t)
	assertBackendContract(t, b, "nvr/")
}

func TestFSOpenDispatchesForTheFSKind(t *testing.T) {
	// The criterion is "Open returns the driver named by the row's Kind", and
	// this is that assertion for a real kind: the row says `fs`, and what comes
	// back is the directory driver, rooted where the row says.
	root := t.TempDir()
	row := &models.StorageBackend{ID: "fs-2", Name: "local", Kind: models.StorageKindFS, RootPath: root}
	b, err := Open(context.Background(), row, sealKey(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fsb, ok := b.(*fsBackend)
	if !ok {
		t.Fatalf("kind fs produced a %T, want *fsBackend", b)
	}
	if abs, _ := filepath.Abs(root); fsb.Root() != abs {
		t.Fatalf("Root() = %q, want %q", fsb.Root(), abs)
	}

	// A relative root is made absolute at open, so a key cannot be resolved
	// against a working directory that changes between the boot and the upload.
	t.Chdir(root)
	relative := &models.StorageBackend{ID: "fs-3", Name: "relative", Kind: models.StorageKindFS, RootPath: "tier"}
	b2, err := Open(context.Background(), relative, sealKey(t))
	if err != nil {
		t.Fatalf("Open(relative root): %v", err)
	}
	if got := b2.(*fsBackend).Root(); got != filepath.Join(root, "tier") {
		t.Fatalf("a relative root resolved to %q, want %q", got, filepath.Join(root, "tier"))
	}

	// An empty root_path is the container the settings name not existing.
	if _, err := Open(context.Background(), &models.StorageBackend{
		ID: "fs-4", Name: "no-root", Kind: models.StorageKindFS,
	}, sealKey(t)); !errors.Is(err, ErrBucketMissing) {
		t.Fatal("an fs row with no root_path opened; the tier would be the working directory")
	}

	// And `fs` needs no credential, so a row with none opens.
	if _, err := Open(context.Background(), &models.StorageBackend{
		ID: "fs-5", Name: "creds", Kind: models.StorageKindFS, RootPath: root, Password: "junk-not-sealed",
	}, sealKey(t)); err != nil {
		t.Fatalf("Open(fs row carrying a stray password): %v", err)
	}
}

func TestFSCrashMidWriteLeavesNoKeyFile(t *testing.T) {
	b, root := newFSBackend(t)
	ctx := context.Background()

	key := "nvr/front-door/2026/11/04/segment-7.mp4"
	full := filepath.Join(root, filepath.FromSlash(key))
	part := full + fsPartSuffix

	// Big enough that the copy takes more than one chunk, so the probe below
	// lands while the `.part` is open and the rename has not happened.
	body := strings.Repeat("x", 64*1024+10)

	var sawPart, sawFinal bool
	reader := &probeReader{
		body: strings.NewReader(body),
		at:   64 * 1024,
		probe: func() {
			_, partErr := os.Stat(part)
			sawPart = partErr == nil
			_, finalErr := os.Stat(full)
			sawFinal = finalErr == nil
		},
	}

	err := b.Put(ctx, key, reader, int64(len(body)), "video/mp4")
	if err == nil {
		t.Fatal("a reader that failed mid-copy produced no error")
	}
	if !sawPart {
		t.Fatal("the in-progress write was not at <key>.part while it was running")
	}
	if sawFinal {
		t.Fatalf("the object appeared at %s before the copy finished, which is the partial object the plan forbids", key)
	}

	// After the failure: neither name. The `.part` is removed on the way out, so
	// a directory of leftovers stays the signal for a crash rather than the
	// normal state of a tier.
	if _, err := os.Stat(full); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed write left the object behind: %v", err)
	}
	if _, err := os.Stat(part); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed write left %s behind: %v", part, err)
	}
	if _, err := b.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after a failed write: got %v, want ErrNotFound", err)
	}

	// And the retry writes cleanly, because it does not race its own leftovers.
	if err := b.Put(ctx, key, strings.NewReader(body), int64(len(body)), "video/mp4"); err != nil {
		t.Fatalf("Put after the failed attempt: %v", err)
	}
	info, err := b.Stat(ctx, key)
	if err != nil || info.Size != int64(len(body)) {
		t.Fatalf("Stat after the retry = %+v, %v", info, err)
	}
}

func TestFSCancelledContextStopsAWrite(t *testing.T) {
	// A tier on a hung mount must not be an uncancellable write: the uploader's
	// shutdown waits for it otherwise.
	b, root := newFSBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	key := "nvr/front-door/2026/11/04/segment-8.mp4"
	err := b.Put(ctx, key, strings.NewReader(strings.Repeat("y", 200*1024)), 200*1024, "video/mp4")
	if err == nil {
		t.Fatal("a cancelled context did not stop the write")
	}
	if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(key))); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a cancelled write left the object behind: %v", statErr)
	}
}

func TestFSKeyCannotEscapeTheRoot(t *testing.T) {
	b, root := newFSBackend(t)
	ctx := context.Background()

	// Every shape a `..` can arrive in. The row is the thing being defended
	// against here: a key is built by this server from a slug and a date, so a
	// key with `..` in it is a row someone edited, and the answer is a refusal
	// rather than a write into the parent directory.
	escaping := []string{
		"../outside.mp4",
		"nvr/../../outside.mp4",
		"nvr/front-door/../../../../etc/passwd",
		"/../outside.mp4",
		"..",
	}
	for _, key := range escaping {
		if _, err := b.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) accepted a key that escapes the root", key)
		} else if !strings.Contains(err.Error(), "escapes") {
			t.Errorf("Get(%q) failed with %v, want a refusal that says it escapes", key, err)
		}
		if err := b.Put(ctx, key, strings.NewReader("x"), 1, "video/mp4"); err == nil {
			t.Errorf("Put(%q) accepted a key that escapes the root", key)
		} else if !strings.Contains(err.Error(), "escapes") {
			t.Errorf("Put(%q) failed with %v, want a refusal that says it escapes", key, err)
		}
		if _, err := b.Stat(ctx, key); err == nil {
			t.Errorf("Stat(%q) accepted a key that escapes the root", key)
		}
		if err := b.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) accepted a key that escapes the root", key)
		}
		if _, err := b.List(ctx, key); err == nil {
			t.Errorf("List(%q) accepted a prefix that escapes the root", key)
		}
	}

	// Nothing was written outside the root, and the root holds nothing.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", root, err)
	}
	if len(entries) != 0 {
		t.Fatalf("the escaping writes left %v in %s", entries, root)
	}

	// A key that cleans away to the tier root names a directory, not an object.
	// `Get` would otherwise hand back a directory handle that fails on the first
	// read, and `Put` would try to rename a file onto a directory.
	for _, key := range []string{"nvr/..", "."} {
		if _, err := b.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) accepted a key that names the root", key)
		} else if !strings.Contains(err.Error(), "not an object") {
			t.Errorf("Get(%q) failed with %v, want a refusal that says it names no object", key, err)
		}
		if err := b.Put(ctx, key, strings.NewReader("x"), 1, "video/mp4"); err == nil {
			t.Errorf("Put(%q) accepted a key that names the root", key)
		}
		// Listing the root is legitimate: it is the whole tier, and the Settings
		// page's "verify this path" asks for it with an empty prefix.
		if _, err := b.List(ctx, key); err != nil {
			t.Errorf("List(%q) = %v, want the root listed", key, err)
		}
	}

	// An empty key names no object, and a NUL in one is a refusal rather than a
	// syscall that fails in a way nobody can read.
	for _, key := range []string{"", "   ", "/"} {
		if _, err := b.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) accepted an empty key", key)
		}
	}
	if _, err := b.Get(ctx, "nvr/x\x00y.mp4"); err == nil {
		t.Error("Get accepted a key containing a NUL")
	}

	// A leading slash is tolerated: the caller means the same object, and the
	// key layout (S7-07) never produces one, so this is a caller slip rather
	// than a different name.
	if err := b.Put(ctx, "/nvr/front-door/a.mp4", strings.NewReader("bytes"), 5, "video/mp4"); err != nil {
		t.Fatalf("Put with a leading slash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "nvr/front-door/a.mp4")); err != nil {
		t.Fatalf("a leading-slash key landed somewhere else: %v", err)
	}
}

func TestFSListSkipsPartFilesAndStartsUnderThePrefix(t *testing.T) {
	b, root := newFSBackend(t)
	ctx := context.Background()

	for _, key := range []string{
		"nvr/front-door/2026/11/03/segment-1.mp4",
		"nvr/front-door/2026/11/04/segment-1.mp4",
		"nvr/front-door/2026/11/04/segment-2.mp4",
		"nvr/garage/2026/11/04/segment-1.mp4",
		"nvr/events/evt-1/clip.mp4",
		"nvr/events/evt-1/snapshot.jpg",
	} {
		if err := b.Put(ctx, key, strings.NewReader("body"), 4, "video/mp4"); err != nil {
			t.Fatalf("Put(%s): %v", key, err)
		}
	}

	// A `.part` left by a crash is not an object, and the playback proxy must
	// never be offered one.
	if err := os.WriteFile(filepath.Join(root, "nvr/front-door/2026/11/04/segment-9.mp4.part"),
		[]byte("half"), 0o640); err != nil {
		t.Fatalf("writing the leftover .part: %v", err)
	}

	all, err := b.List(ctx, "")
	if err != nil {
		t.Fatalf("List(\"\"): %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("List(\"\") = %v, want the six objects and not the .part", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1] > all[i] {
			t.Fatalf("List is not sorted: %v", all)
		}
	}

	day, err := b.List(ctx, "nvr/front-door/2026/11/04/")
	if err != nil {
		t.Fatalf("List by day: %v", err)
	}
	if len(day) != 2 || day[0] != "nvr/front-door/2026/11/04/segment-1.mp4" {
		t.Fatalf("List by day = %v", day)
	}

	// The walk starts at the deepest directory the prefix names, so a prefix
	// that is a whole camera costs one directory read and not one tier scan.
	// Asserted by result here; the cost is the reason the code is written that
	// way, and the result is what a wrong optimisation would show.
	garage, err := b.List(ctx, "nvr/garage/")
	if err != nil || len(garage) != 1 {
		t.Fatalf("List(nvr/garage/) = %v, %v", garage, err)
	}

	// A prefix naming an object rather than a directory lists that object.
	one, err := b.List(ctx, "nvr/events/evt-1/clip.mp4")
	if err != nil || len(one) != 1 || one[0] != "nvr/events/evt-1/clip.mp4" {
		t.Fatalf("List of an object key = %v, %v", one, err)
	}

	// A prefix that is not there yet is an empty answer, not an error: the
	// Settings page asks before the first recording lands.
	absent, err := b.List(ctx, "nvr/front-door/2030/01/01/")
	if err != nil || len(absent) != 0 {
		t.Fatalf("List of an absent day = %v, %v", absent, err)
	}
}

func TestFSStatContentTypeComesFromTheKey(t *testing.T) {
	b, _ := newFSBackend(t)
	ctx := context.Background()

	// A filesystem has no object metadata, so the type a Stat reports is the
	// key's extension. That is a property of the key layout (S7-07), which is
	// why the layout ends every object in `.mp4` or `.jpg`.
	cases := map[string]string{
		"nvr/front-door/2026/11/04/segment-1.mp4": "video/mp4",
		"nvr/events/evt-1/clip.mp4":               "video/mp4",
		"nvr/events/evt-1/snapshot.jpg":           "image/jpeg",
		"nvr/events/evt-2/snapshot.jpeg":          "image/jpeg",
		"nvr/events/evt-3/mystery":                "application/octet-stream",
	}
	for key, want := range cases {
		if err := b.Put(ctx, key, strings.NewReader("body"), 4, "video/mp4"); err != nil {
			t.Fatalf("Put(%s): %v", key, err)
		}
		info, err := b.Stat(ctx, key)
		if err != nil {
			t.Fatalf("Stat(%s): %v", key, err)
		}
		if info.ContentType != want {
			t.Errorf("Stat(%s).ContentType = %q, want %q", key, info.ContentType, want)
		}
		if info.Key != key {
			t.Errorf("Stat(%s).Key = %q, want the key back", key, info.Key)
		}
	}
}

func TestFSPutRefusesASizeMismatch(t *testing.T) {
	// The uploader passes the length it read from disk. An object that arrived
	// short is an object the row describes wrongly forever, so the driver says
	// so rather than letting the row go on believing the lie.
	b, _ := newFSBackend(t)
	ctx := context.Background()
	key := "nvr/front-door/2026/11/04/segment-3.mp4"

	err := b.Put(ctx, key, strings.NewReader("five"), 500, "video/mp4")
	if err == nil {
		t.Fatal("a 4-byte write claimed to be 500 and no one objected")
	}
	if !strings.Contains(err.Error(), "4 bytes") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the mismatch error does not name both numbers: %v", err)
	}
	if _, statErr := b.Stat(ctx, key); !errors.Is(statErr, ErrNotFound) {
		t.Fatalf("a refused write left an object behind: %v", statErr)
	}

	// A caller that does not know the size says -1 and is not refused.
	if err := b.Put(ctx, key, strings.NewReader("five"), -1, "video/mp4"); err != nil {
		t.Fatalf("Put with an unknown size: %v", err)
	}
}

func TestFSPingNamesWhatIsWrongWithTheMount(t *testing.T) {
	ctx := context.Background()

	t.Run("a directory that is there answers ok", func(t *testing.T) {
		b, _ := newFSBackend(t)
		if err := b.Ping(ctx); err != nil {
			t.Fatalf("Ping on a good directory: %v", err)
		}
	})

	t.Run("a directory that is not there is bucket missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "not-mounted")
		b, err := Open(ctx, &models.StorageBackend{
			ID: "fs-miss", Name: "nfs-tier", Kind: models.StorageKindFS, RootPath: missing,
		}, sealKey(t))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := b.Ping(ctx); !errors.Is(err, ErrBucketMissing) {
			t.Fatalf("Ping on a missing directory: got %v, want bucket missing", err)
		}
	})

	t.Run("a root_path that names a file is bucket missing", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "not-a-directory")
		if err := os.WriteFile(file, []byte("x"), 0o640); err != nil {
			t.Fatalf("writing the file: %v", err)
		}
		b, err := Open(ctx, &models.StorageBackend{
			ID: "fs-file", Name: "wrong-root", Kind: models.StorageKindFS, RootPath: file,
		}, sealKey(t))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := b.Ping(ctx); !errors.Is(err, ErrBucketMissing) {
			t.Fatalf("Ping on a file root: got %v, want bucket missing", err)
		}
	})

	t.Run("the ping leaves no file behind", func(t *testing.T) {
		b, root := newFSBackend(t)
		if err := b.Ping(ctx); err != nil {
			t.Fatalf("Ping: %v", err)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("Ping left %v in the tier root", entries)
		}
	})
}

func TestFSMapIONamesTheErrnoNotTheMessage(t *testing.T) {
	// The mapping is by errno, because the message is the C library's and moves
	// with the locale. These are the three an operator meets on a mount, and
	// the three fixes are different, which is the whole reason they are three.
	b := &fsBackend{root: "/srv/nvr"}

	cases := []struct {
		err  error
		want error
	}{
		{fmt.Errorf("open: %w", fs.ErrNotExist), ErrBucketMissing},
		{fmt.Errorf("open: %w", fs.ErrPermission), ErrBadCredentials},
		{fmt.Errorf("rename: %w", syscall.ESTALE), ErrUnreachable},
		{fmt.Errorf("write: %w", syscall.EROFS), ErrUnreachable},
		{fmt.Errorf("read: %w", syscall.EIO), ErrUnreachable},
		{fmt.Errorf("dial: %w", syscall.ENOTCONN), ErrUnreachable},
	}
	for _, tc := range cases {
		got := b.mapIO("testing", "/srv/nvr", tc.err)
		if !errors.Is(got, tc.want) {
			t.Errorf("mapIO(%v) = %v, want it to map to %v", tc.err, got, tc.want)
		}
		if ErrorCode(got) == "" && tc.want != nil {
			t.Errorf("mapIO(%v) = %v, which the API cannot name", tc.err, got)
		}
	}

	// An error that is none of the above stays itself: inventing a name for a
	// driver bug would send the operator to check their mount.
	other := b.mapIO("testing", "/srv/nvr", errors.New("a driver bug"))
	if ErrorCode(other) != "" {
		t.Fatalf("an unmappable error was given a name: %v", other)
	}
}

func TestFSObjectsAreNotWorldReadable(t *testing.T) {
	// The media tree is 0o750 directories and 0o640 files, and a tier directory
	// is the same kind of thing: pictures of someone's property, group-readable,
	// world-invisible.
	b, root := newFSBackend(t)
	ctx := context.Background()
	key := "nvr/front-door/2026/11/04/segment-1.mp4"
	if err := b.Put(ctx, key, strings.NewReader("body"), 4, "video/mp4"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Fatalf("the object is world-readable: %O", perm)
	}
	dir, err := os.Stat(filepath.Dir(filepath.Join(root, filepath.FromSlash(key))))
	if err != nil {
		t.Fatalf("Stat the directory: %v", err)
	}
	if perm := dir.Mode().Perm(); perm&0o007 != 0 {
		t.Fatalf("the object's directory is world-accessible: %O", perm)
	}
}

// probeReader fails at a chosen byte offset, running `probe` first. It is how
// the test looks at the tier *during* a write, which is the only way to tell
// "atomic rename" from "write into the final name and hope".
type probeReader struct {
	body  io.Reader
	at    int
	seen  int
	probe func()
}

func (p *probeReader) Read(buf []byte) (int, error) {
	if p.probe != nil && p.seen >= p.at {
		p.probe()
		p.probe = nil
		return 0, io.ErrUnexpectedEOF
	}
	n, err := p.body.Read(buf)
	p.seen += n
	return n, err
}
