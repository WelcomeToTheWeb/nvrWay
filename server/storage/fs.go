package storage

// The `fs` driver: a directory, whether a local disk, an NFS or SMB mount, or a
// compose volume.
//
// It is the simplest of the four and the one every deployment can actually
// test, which is why `scripts/verify.sh` step 7 seeds it (`local-tier`). It is
// also the driver that makes the tier's contract visible: an object store gives
// you atomic replacement for free, a filesystem does not, so this file is where
// "no partial objects" gets written down as a `.part` file and a rename.
//
// Three rules:
//
//   - **Keys are relative paths, and they cannot escape the root.** A key is
//     `nvr/<slug>/<YYYY>/<MM>/<DD>/<basename>.mp4`, slash-separated, never
//     absolute. `..` is refused rather than cleaned, because a key that walks
//     out of the root is a row that has been edited, and the honest answer to
//     that is a refusal.
//   - **A write is a `.part` file and a rename.** `os.Rename` within one
//     directory is atomic on every filesystem this driver is meant to run on,
//     so a crash mid-write leaves `<key>.part` and no `<key>` — the sweeper and
//     the playback proxy never see half an mp4.
//   - **A missing directory is `bucket missing`, a read-only one is `bad
//     credentials`, a stale mount is `discovery_unreachable`.** Those are the
//     names the API reports (S7-10), and for a mount they are the three
//     different things an operator can actually do about it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"nvr/server/models"
)

// fsDirPerm/fsFilePerm match the media tree: the recorder writes 0o750
// directories and 0o640 files, and a tier directory that a restore process
// reads is the same kind of thing. Group-readable, world-invisible.
const (
	fsDirPerm  = 0o750
	fsFilePerm = 0o640

	// fsPartSuffix is the in-progress name. It is a suffix rather than a temp
	// pattern so a `.part` left by a crash is recognisable from a directory
	// listing, and so the sweeper can garbage-collect it.
	fsPartSuffix = ".part"
)

// fsBackend is a RootPath directory addressed by key.
type fsBackend struct {
	root string
}

func init() {
	registerDriver(models.StorageKindFS, openFS)
}

// openFS builds the driver from a row's RootPath.
//
// The directory is not probed here: `Open` is called on every upload tick and
// every playback request, and a mount that is briefly away should fail the
// operation with a named error rather than fail the open. `Ping` is the route
// that checks the mount, because that is the one an operator asked.
func openFS(ctx context.Context, a *openArgs) (Backend, error) {
	root := strings.TrimSpace(a.Row.RootPath)
	if root == "" {
		// The container the settings name does not exist, which is the same
		// answer a missing bucket is.
		return nil, fmt.Errorf("root_path is empty: %w", ErrBucketMissing)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving root_path %q: %w", root, err)
	}
	return &fsBackend{root: abs}, nil
}

// Root is the directory this backend writes into. Exported for the boot log and
// the Settings page, which show an operator where their footage is.
func (b *fsBackend) Root() string { return b.root }

// path turns a key into the file that holds it, refusing anything that would
// leave the root.
//
// A leading slash is tolerated — `Open` hands drivers keys, and a caller that
// passes `/nvr/x.mp4` means the same object — but `..` is not: cleaning it away
// would silently retarget a row that someone edited by hand.
func (b *fsBackend) path(key string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(key), "/")
	if trimmed == "" {
		return "", errors.New("storage: fs: an empty key names no object")
	}
	if strings.ContainsRune(trimmed, 0) {
		return "", errors.New("storage: fs: a key may not contain a NUL")
	}
	// filepath.FromSlash turns the key's separators into this platform's, and
	// SplitList-style segment inspection is what catches `..` wherever it sits.
	cleaned := filepath.ToSlash(filepath.Clean(trimmed))
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return "", fmt.Errorf("storage: fs: key %q escapes %s", key, b.root)
		}
	}
	// A key that cleans away to nothing names the tier root itself, and the root
	// is a directory, not an object. Refusing it here is what stops `Get` from
	// handing back a directory handle that fails on the first read.
	if cleaned == "." {
		return "", fmt.Errorf("storage: fs: key %q names the tier root %s, not an object", key, b.root)
	}

	full := filepath.Join(b.root, filepath.FromSlash(cleaned))
	// The belt to the braces: Join cleans, and a key that is a symlink target
	// outside the root is still a key under it. What this check catches is a
	// root that was itself written by hand with a trailing `..`.
	if !strings.HasPrefix(full, b.root+string(filepath.Separator)) {
		return "", fmt.Errorf("storage: fs: key %q escapes %s", key, b.root)
	}
	return full, nil
}

// Put writes the object, atomically.
//
// The bytes go to `<key>.part` in the destination directory, are flushed to the
// device, and only then become the object by rename. Two consequences worth
// stating: a crash leaves a `.part` and never a half-written `<key>`, and the
// fsync is not decoration — on an NFS mount a rename the client has not
// flushed can survive the client but not the power cut.
func (b *fsBackend) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	full, err := b.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, fsDirPerm); err != nil {
		return b.mapIO("creating the directory for", key, err)
	}

	part := full + fsPartSuffix
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fsFilePerm)
	if err != nil {
		return b.mapIO("opening", key, err)
	}

	written, copyErr := copyWhileChecking(ctx, f, r)
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr != nil {
		// The failed attempt is removed, so a caller that retries does not race
		// its own leftovers, and a directory of abandoned `.part` files stays
		// the signal for a crash rather than the normal state.
		_ = os.Remove(part)
		return b.mapIO("writing", key, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(part)
		return b.mapIO("closing", key, closeErr)
	}

	// `size` is what the caller said it was. A mismatch is a caller bug worth a
	// named failure: the uploader passes the length it read from disk, and an
	// object that arrived short is an object the row describes wrongly forever.
	// Checked before the rename, so a refused write leaves no object behind —
	// the same rule as a failed copy, and the reason the check is not after.
	if size >= 0 && written != size {
		_ = os.Remove(part)
		return fmt.Errorf("storage: fs: %s was written %d bytes, the caller said %d", key, written, size)
	}

	if err := os.Rename(part, full); err != nil {
		_ = os.Remove(part)
		return b.mapIO("renaming", key, err)
	}
	return nil
}

// copyWhileChecking copies with a context check per chunk. A tier write against
// a hung mount is otherwise an uncancellable read, and the uploader's shutdown
// would wait for it.
func copyWhileChecking(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 64*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			w, writeErr := dst.Write(buf[:n])
			total += int64(w)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, readErr
		}
	}
}

// Get opens the object for reading from byte 0.
func (b *fsBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full, err := b.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("storage: fs: %s: %w", key, ErrNotFound)
		}
		return nil, b.mapIO("opening", key, err)
	}
	return f, nil
}

// Stat reports the object.
//
// ContentType comes from the key's extension: a filesystem has no object
// metadata, and this layout always ends in `.mp4` or `.jpg`, so the extension
// is the type. That is a property of the key layout (S7-07), not of the driver,
// and it is why the uploader passes a content type on Put for the tiers that do
// store one.
func (b *fsBackend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	full, err := b.path(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ObjectInfo{}, fmt.Errorf("storage: fs: %s: %w", key, ErrNotFound)
		}
		return ObjectInfo{}, b.mapIO("statting", key, err)
	}
	if info.IsDir() {
		return ObjectInfo{}, fmt.Errorf("storage: fs: %s is a directory, not an object", key)
	}
	return ObjectInfo{
		Key:         strings.TrimPrefix(filepath.ToSlash(full), filepath.ToSlash(b.root)+"/"),
		Size:        info.Size(),
		ModTime:     info.ModTime().UTC(),
		ContentType: contentTypeFor(key),
	}, nil
}

// Delete removes the object.
//
// Empty parent directories are left behind on purpose: the sweeper and the
// playback proxy walk rows and keys, never directories, so pruning empty ones
// is a cosmetic pass that would cost a second syscall per delete.
func (b *fsBackend) Delete(ctx context.Context, key string) error {
	full, err := b.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("storage: fs: %s: %w", key, ErrNotFound)
		}
		return b.mapIO("removing", key, err)
	}
	return nil
}

// List returns the keys under a prefix, sorted.
//
// The walk starts at the deepest directory the prefix names, so listing
// `nvr/front-door/2026/11/` does not read the whole tier first — on an NFS
// mount of a year of footage that is the difference between a Settings page and
// a spinner.
func (b *fsBackend) List(ctx context.Context, prefix string) ([]string, error) {
	cleanedPrefix := strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	if cleanedPrefix != "" {
		for _, segment := range strings.Split(filepath.ToSlash(filepath.Clean(cleanedPrefix)), "/") {
			if segment == ".." {
				return nil, fmt.Errorf("storage: fs: prefix %q escapes %s", prefix, b.root)
			}
		}
		cleanedPrefix = filepath.ToSlash(filepath.Clean(cleanedPrefix))
		if cleanedPrefix == "." {
			// `List("nvr/..")` is `List("")`: the prefix names the root, and the
			// root is where the whole tier lives.
			cleanedPrefix = ""
		}
	}

	// The root itself is a different question from the prefix. A prefix with
	// nothing under it is an empty listing — "this path has no footage yet" is
	// the answer a Settings page needs before the first recording lands — but a
	// root that is not there means the mount is not there, and an empty listing
	// is exactly what an unmounted tier looks like. `Ping` says so, `s3` says so
	// (NoSuchBucket), and a listing that answered "nothing here" would send the
	// operator to look for footage that was never reachable.
	if info, err := os.Stat(b.root); err != nil {
		return nil, b.mapIO("reaching", b.root, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("storage: fs: %s is not a directory: %w", b.root, ErrBucketMissing)
	}

	walkFrom := b.root
	if cleanedPrefix != "" {
		// The prefix may name an object rather than a directory
		// (`List("nvr/x/2026/11/04/a.mp4")`), so the walk starts at its parent
		// and the filter below does the rest.
		candidate := filepath.Join(b.root, filepath.FromSlash(filepath.Dir(cleanedPrefix)))
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			walkFrom = candidate
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, b.mapIO("listing", prefix, err)
		}
		// A missing directory under the prefix is an empty listing, not an
		// error: "this prefix has nothing yet" is the answer a Settings page
		// needs before the first recording lands.
	}

	rootSlash := filepath.ToSlash(b.root) + "/"
	var keys []string
	walkErr := filepath.WalkDir(walkFrom, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished mid-walk is a tier being swept while we
			// read it; the rest of the walk is still the truth.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		name := filepath.ToSlash(path)
		if !strings.HasPrefix(name, rootSlash) {
			return nil
		}
		key := strings.TrimPrefix(name, rootSlash)
		if cleanedPrefix != "" && !strings.HasPrefix(key, cleanedPrefix) {
			return nil
		}
		// A `.part` left by a crash is not an object, and listing it as one
		// would put a half-written file in front of the playback proxy.
		if strings.HasSuffix(key, fsPartSuffix) {
			return nil
		}
		keys = append(keys, key)
		return nil
	})
	if walkErr != nil {
		return nil, b.mapIO("listing", prefix, walkErr)
	}
	sort.Strings(keys)
	return keys, nil
}

// Ping answers whether the directory is there and will take a write.
//
// The three answers are the three things an operator can do about a mount, and
// they are deliberately not one answer: "the directory is not there" (mount it,
// or fix root_path), "it is there and read-only" (export it read-write, or fix
// the ownership), and "the handle went stale" (remount it) look identical from
// `ls` and different from here.
func (b *fsBackend) Ping(ctx context.Context) error {
	info, err := os.Stat(b.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("the directory %s is not there: %w", b.root, ErrBucketMissing)
		}
		return b.mapIO("reaching", b.root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory: %w", b.root, ErrBucketMissing)
	}

	// Writable, proved rather than inferred from the mode bits: an NFS export
	// can be read-only with 0777 on it, and root_squash turns an owner into a
	// nobody with no warning.
	probe, err := os.CreateTemp(b.root, ".nvr-ping-*")
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("%s is not writable by this process: %w", b.root, ErrBadCredentials)
		}
		return b.mapIO("writing a probe file in", b.root, err)
	}
	name := probe.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := probe.Write([]byte("nvr")); err != nil {
		_ = probe.Close()
		return b.mapIO("writing a probe file in", b.root, err)
	}
	if err := probe.Close(); err != nil {
		return b.mapIO("closing a probe file in", b.root, err)
	}
	if err := os.Remove(name); err != nil {
		return b.mapIO("removing a probe file from", b.root, err)
	}
	return nil
}

// mapIO turns a filesystem error into one of the four names.
//
// The mapping is by the errno, not by the message, because the message is the
// C library's and changes with the locale. `ESTALE` is the one worth naming
// out loud: it is what an NFS client says after the server restarted under it,
// and it means "remount", not "your settings are wrong".
func (b *fsBackend) mapIO(verb, subject string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("storage: fs: %s %s: %w", verb, subject, ErrBucketMissing)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("storage: fs: %s %s: %w", verb, subject, ErrBadCredentials)
	case errors.Is(err, syscall.ESTALE), errors.Is(err, syscall.EIO),
		errors.Is(err, syscall.ENOTCONN), errors.Is(err, syscall.EROFS):
		return fmt.Errorf("storage: fs: %s %s: %w", verb, subject, ErrUnreachable)
	default:
		return fmt.Errorf("storage: fs: %s %s: %w", verb, subject, err)
	}
}

// contentTypeFor is the type a filesystem can report for a key. The two
// extensions are the whole of this layout (S7-07); anything else is reported as
// an octet stream, which is what a browser does with a file it cannot name.
func contentTypeFor(key string) string {
	switch strings.ToLower(filepath.Ext(key)) {
	case ".mp4":
		return "video/mp4"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	default:
		return "application/octet-stream"
	}
}
