// The tier's one interface, and the dispatch that picks a driver for it.
//
// Everything above this file — the uploader, the sweeper, the playback proxy,
// the Settings page — asks six questions of a destination and does not care
// whether it is a bucket, a WebDAV share, an SFTP server or a directory on an
// NFS mount. That is the whole reason the four drivers exist as four files
// rather than as branches in the uploader: the queue's behaviour (claim, put,
// mark uploaded, retry with backoff) is one algorithm, and a tier that needs a
// special case in it is a tier that has leaked into the wrong layer.
//
// Two rules are decided here:
//
//   - **A failure answers in a name.** `Ping` failures map to the four the API
//     reports — `discovery_unreachable`, `bad credentials`, `sftp host key
//     changed`, `bucket missing` — rather than to a driver's own sentence.
//     SettingsPage draws what to fix from that name, `scripts/verify.sh` greps
//     for it, and a MinIO version that changes its error text cannot break the
//     console.
//   - **Credentials are unsealed at Open and nowhere else.** The row holds
//     sealed text (S7-02); `Open` is the one place that turns it into
//     plaintext, and it hands the plaintext to a driver, never back to a
//     handler that could serialise it.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
	"nvr/server/store"
)

// ObjectInfo is what a tier says about one object.
//
// ContentType is the type the tier was *given* at Put, not one guessed on the
// way out: the playback proxy answers it, and a bucket that returns
// `application/octet-stream` for a clip would make the browser download the
// file instead of playing it.
type ObjectInfo struct {
	Key         string
	Size        int64
	ModTime     time.Time
	ContentType string
}

// Backend is one destination for recorded media.
//
// Keys are the layout in `keys.go` — `nvr/<slug>/<YYYY>/<MM>/<DD>/<basename>.mp4`
// and `nvr/events/<event_id>/{clip,snapshot}.<ext>` — always slash-separated and
// never absolute, so a key means the same thing on every driver and a restore
// lands the file back in `SEGMENTS_DIR`.
//
// Get returns a reader the caller must close. It reads from byte 0: v1 does not
// seek into a remote object, which is why the clip route streams a remote copy
// whole and says so (S6-05, and `docs/architecture.md`'s "Playback and the
// remote seek gap").
type Backend interface {
	// Put stores the object at key, replacing whatever was there.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get opens the object for reading from byte 0.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Stat reports the object without reading it.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// Delete removes the object. A missing object is ErrNotFound.
	Delete(ctx context.Context, key string) error
	// List returns the keys under a prefix, sorted. An empty prefix lists the
	// whole tier; the Settings page's "verify this path" uses it to show that a
	// prefix the operator typed is really there.
	List(ctx context.Context, prefix string) ([]string, error)
	// Ping answers whether this destination is reachable and usable *now*, with
	// the credentials stored for it. Its failures are the four named errors
	// below, wrapped in whatever detail the log wants.
	Ping(ctx context.Context) error
}

// The four names the API reports for a failed connection test, verbatim
// (`POST /api/storage/backends/:id/test` answers
// `{"ok":false,"error":"<one of these>"}`, S7-10).
//
// They are the sentinel messages themselves, not a table mapping an error to a
// string, so the name a driver wraps is the name the operator reads and there
// is no second place for it to drift. Drivers add their own context by
// wrapping: `fmt.Errorf("s3: ping bucket %s: %w", bucket, ErrUnreachable)`.
var (
	// ErrUnreachable is "nothing answered": a refused connection, a DNS name
	// that does not resolve, a mount that is not there.
	ErrUnreachable = errors.New("discovery_unreachable")
	// ErrBadCredentials is "something answered and turned us away": a wrong
	// access key or secret, a wrong password, a 403 from a bucket policy.
	ErrBadCredentials = errors.New("bad credentials")
	// ErrHostKeyChanged is SFTP's pin refusal: the server this address presents
	// is not the one whose key was pinned on first use.
	ErrHostKeyChanged = errors.New("sftp host key changed")
	// ErrBucketMissing is "reachable, authorised, and the container named in
	// the settings is not there": an S3 bucket, a WebDAV collection, an SFTP
	// root directory.
	ErrBucketMissing = errors.New("bucket missing")
)

// The tier's own failures, which are not connection-test answers and so are not
// among the four names.
var (
	// ErrNotFound is a missing object. Get, Stat and Delete answer it; Put
	// never does.
	ErrNotFound = errors.New("storage: object not found")
	// ErrNoBackend is an installation with no storage destination configured.
	// It is a state, not a failure: a local-only deployment records and serves
	// and uploads nothing, and the routes say so in those words.
	ErrNoBackend = errors.New("no storage backend is configured")
	// ErrKindUnsupported is a row whose Kind this build has no driver for — a
	// database restored from a newer installation, or a kind added without a
	// driver.
	ErrKindUnsupported = errors.New("storage: this build has no driver for that kind")
	// ErrBadKey is a row whose stored credential cannot be opened with this
	// process's seal key. It is deliberately not ErrBadCredentials: the
	// credential may be perfectly correct, and telling an operator to retype a
	// password when the fix is NVR_SECRET_KEY wastes their morning.
	ErrBadKey = errors.New("the stored credential cannot be opened with this seal key")
)

// ErrorCode maps any storage error to the name the API reports, or "" when the
// error is not one of the four. The API answers the name and logs the wrapped
// error, which is the difference between a console that can say what to fix and
// a console that shows a MinIO stack trace.
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrHostKeyChanged):
		// Before ErrBadCredentials: an SFTP host-key change is an
		// authentication-shaped failure, and a driver that wrapped both wants
		// the more specific answer.
		return ErrHostKeyChanged.Error()
	case errors.Is(err, ErrBadCredentials):
		return ErrBadCredentials.Error()
	case errors.Is(err, ErrBucketMissing):
		return ErrBucketMissing.Error()
	case errors.Is(err, ErrUnreachable):
		return ErrUnreachable.Error()
	default:
		return ""
	}
}

// Credentials is one row's secrets, opened.
//
// Username is not sealed and is carried as stored: a username is not a secret
// in the sense the seal is about (it is in the URL, and the console shows it),
// and sealing it would make "which account does this backend use" unanswerable
// from a backup.
type Credentials struct {
	Username       string
	Password       string
	AccessKey      string
	SecretKey      string
	PrivateKeyPath string
}

// openArgs is what a driver is handed: the row, its opened credentials, and the
// settings store for the one piece of state a driver keeps outside its row (the
// SFTP host key, pinned per backend, S7-06).
type openArgs struct {
	Row    *models.StorageBackend
	Secret Credentials
	// Settings may be nil. A driver that needs it and does not have it fails
	// with a named error rather than panicking — the API's connection test runs
	// with the real store, and a test elsewhere runs without one.
	Settings *store.SettingStore
}

// driverFunc builds one backend.
type driverFunc func(ctx context.Context, a *openArgs) (Backend, error)

// drivers is the kind → driver table.
//
// A table rather than a switch in `Open`, for one reason that is a criterion on
// its card: a driver file adds itself in `init()`, so the package compiles and
// the dispatch is testable before every driver exists, and a binary that does
// not compile a driver file does not link that driver's SDK. The four driver
// files in this package all register, so the server build links exactly the
// four the plan commits to and no others.
var drivers = map[string]driverFunc{}

// registerDriver is called from a driver file's init().
//
// A duplicate registration is a programming error, and it panics: two files
// claiming one kind is a build in which one driver silently never runs, which
// is the sort of bug that survives to a deployment.
func registerDriver(kind string, fn driverFunc) {
	if _, taken := drivers[kind]; taken {
		panic("storage: driver registered twice for kind " + kind)
	}
	drivers[kind] = fn
}

// RegisteredKinds lists the kinds this build has a driver for, sorted by the
// order of models.StorageKinds so the answer is stable.
func RegisteredKinds() []string {
	var out []string
	for _, kind := range models.StorageKinds {
		if _, ok := drivers[kind]; ok {
			out = append(out, kind)
		}
	}
	return out
}

// Open builds the driver named by a row's Kind, with its credentials unsealed
// by the given key.
func Open(ctx context.Context, row *models.StorageBackend, key [32]byte) (Backend, error) {
	return OpenWithSettings(ctx, row, key, nil)
}

// OpenWithSettings is Open for the kinds that keep state outside the row.
//
// `settings` is the SFTP host-key pin (S7-06); every other kind ignores it, and
// nil is legitimate for a caller that has no store to give.
func OpenWithSettings(ctx context.Context, row *models.StorageBackend, key [32]byte, settings *store.SettingStore) (Backend, error) {
	if row == nil {
		return nil, ErrNoBackend
	}
	fn, ok := drivers[row.Kind]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrKindUnsupported, row.Kind)
	}

	secret, err := unseal(row, key)
	if err != nil {
		return nil, err
	}

	backend, err := fn(ctx, &openArgs{Row: row, Secret: secret, Settings: settings})
	if err != nil {
		// The driver's own error is returned wrapped with the row's name, and
		// never with a credential in it: `s3: backend "minio-prod": …` is a
		// log line an operator reads, and the access key is not in it.
		return nil, fmt.Errorf("storage: backend %q (%s): %w", row.Name, row.Kind, err)
	}
	return backend, nil
}

// OpenDefault opens the installation's default destination — the row marked
// `is_default`, or the only enabled row when there is exactly one (the store's
// rule, S1-11).
//
// It answers ErrNoBackend when the installation has none, which is the answer
// the delete route already gives in words ("no storage tier is configured:
// nothing remote to purge") and the answer the uploader logs once per tick
// rather than pretending the queue is idle.
func OpenDefault(ctx context.Context, st *store.Store, key [32]byte) (Backend, error) {
	if st == nil {
		return nil, ErrNoBackend
	}
	row, err := st.StorageBackends.GetDefault()
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNoBackend
		}
		return nil, fmt.Errorf("storage: reading the default backend: %w", err)
	}
	return OpenWithSettings(ctx, row, key, st.Settings)
}

// unseal opens a row's secret columns.
//
// Which columns are secrets is a property of the kind, not of the row: an `fs`
// backend with a password in it is a row someone edited by hand, and unsealing
// it would fail on a backend that needs no credential at all. So each kind
// unseals exactly what its driver reads.
func unseal(row *models.StorageBackend, key [32]byte) (Credentials, error) {
	var out Credentials
	var err error

	// Every column is opened with the same rule and the same named failure, and
	// the column is named in the error while its value never is.
	open := func(column, sealed string) (string, error) {
		plain, err := Unseal(sealed, key)
		if err != nil {
			return "", fmt.Errorf("%w: %s (%s) is sealed with a key this process does not have: %v",
				ErrBadKey, row.Name, column, err)
		}
		return plain, nil
	}

	switch row.Kind {
	case models.StorageKindS3:
		if out.AccessKey, err = open("access_key", row.AccessKey); err != nil {
			return out, err
		}
		if out.SecretKey, err = open("secret_key", row.SecretKey); err != nil {
			return out, err
		}
	case models.StorageKindWebDAV:
		out.Username = row.Username
		if out.Password, err = open("password", row.Password); err != nil {
			return out, err
		}
	case models.StorageKindSFTP:
		out.Username = row.Username
		if out.Password, err = open("password", row.Password); err != nil {
			return out, err
		}
		// The private key *path* is sealed too: which key file a backend uses
		// is information about the host, and the path is how an attacker who
		// has the database but not the seal key learns what to read.
		if out.PrivateKeyPath, err = open("private_key_path", row.PrivateKeyPath); err != nil {
			return out, err
		}
	case models.StorageKindFS:
		// Nothing to open. A local directory has no credential, and a row that
		// carries one is ignored rather than rejected — the operator's `fs`
		// backend is not broken by a field the form left behind.
	default:
		// Two different situations wear the same shape, and only one of them is
		// a bug. A kind `Open` let through because a driver registered for it
		// but which this table has no rule for is a real storage kind that
		// arrived without declaring its secrets — that is a build error, and
		// saying so beats handing a driver an empty credential and letting it
		// fail at the tier with "bad credentials". A kind that is not a storage
		// kind at all is a test double: it has no secrets, and it gets none.
		if models.ValidStorageKind(row.Kind) {
			return out, fmt.Errorf("%w: %s has a driver but no credential rule", ErrKindUnsupported, row.Kind)
		}
		return out, nil
	}
	return out, nil
}
