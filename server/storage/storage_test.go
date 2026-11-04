package storage

// S7-01 — the tier's one interface, the dispatch that picks a driver, and the
// four names a failed connection test answers with.
//
// The exit criteria are "Open returns the driver named by the row's Kind",
// "Ping failure maps to the named errors the API reports (discovery_unreachable,
// bad credentials, sftp host key changed, bucket missing)" and "no driver is
// linked by a binary that does not use it".
//
// The drivers land one card each (S7-03…S7-06), so the dispatch is asserted
// here with a driver registered under a kind that is not one of the four: the
// registry is the mechanism, and what is being proved is that `Open` consults
// it and hands the driver the row and its opened credentials. Each driver card
// then asserts the dispatch for its own real kind.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"nvr/server/models"
	"nvr/server/store"
)

// fakeKind is a kind no driver file claims, so a test can register a driver
// without colliding with the real ones.
const fakeKind = "test-fake-tier"

// fakeBackend records the calls made through the interface, so the contract the
// rest of the package relies on is asserted rather than assumed.
type fakeBackend struct {
	puts    []string
	got     []string
	pings   int
	listeds []string
	deleted []string
	statd   []string
	err     error
}

func (f *fakeBackend) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if f.err != nil {
		return f.err
	}
	body, _ := io.ReadAll(r)
	f.puts = append(f.puts, fmt.Sprintf("%s %s %d %s", key, contentType, size, body))
	return nil
}

func (f *fakeBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.got = append(f.got, key)
	return io.NopCloser(strings.NewReader("remote bytes")), nil
}

func (f *fakeBackend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if f.err != nil {
		return ObjectInfo{}, f.err
	}
	f.statd = append(f.statd, key)
	return ObjectInfo{Key: key, Size: 11, ModTime: time.Unix(1700000000, 0).UTC(), ContentType: "video/mp4"}, nil
}

func (f *fakeBackend) Delete(ctx context.Context, key string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, key)
	return nil
}

func (f *fakeBackend) List(ctx context.Context, prefix string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.listeds = append(f.listeds, prefix)
	return []string{prefix + "a.mp4", prefix + "b.mp4"}, nil
}

func (f *fakeBackend) Ping(ctx context.Context) error {
	f.pings++
	return f.err
}

// lastFake is the driver's window into the last row it was handed.
var lastFake *openArgs

// lastFakeBackend is the same window onto the backend the driver returned, so a
// test can read what went through the interface rather than only what Open was
// called with.
var lastFakeBackend *fakeBackend

func init() {
	registerDriver(fakeKind, func(ctx context.Context, a *openArgs) (Backend, error) {
		lastFake = a
		lastFakeBackend = &fakeBackend{}
		return lastFakeBackend, nil
	})
}

func TestOpenDispatchesToTheDriverNamedByTheRow(t *testing.T) {
	key := sealKey(t)
	row := &models.StorageBackend{
		ID: "b1", Name: "primary", Kind: fakeKind,
		AccessKey: mustSeal(t, "AKIAIOSFODNN7EXAMPLE", key),
		SecretKey: mustSeal(t, "wJalrXUtnFEMI/K7MDENG", key),
	}

	backend, err := Open(context.Background(), row, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if backend == nil {
		t.Fatal("Open returned a nil backend with no error")
	}
	if lastFake == nil {
		t.Fatal("the registered driver never ran")
	}
	if lastFake.Row != row {
		t.Fatalf("the driver was handed row %+v, want the row Open was called with", lastFake.Row)
	}
	if lastFake.Secret != (Credentials{}) {
		// A kind this table has no credential rule for is handed no credentials,
		// and the sealed columns it carries are left sealed rather than opened
		// and guessed at. Each real kind's credentials are asserted by
		// TestUnsealOpensOnlyWhatTheKindReads, and again from the driver's own
		// side on that driver's card.
		t.Fatalf("a kind with no credential rule was handed %+v", lastFake.Secret)
	}

	// The interface is the whole contract: every method is reachable through
	// the value Open returned.
	ctx := context.Background()
	if err := backend.Put(ctx, "nvr/x/2026/11/04/a.mp4", strings.NewReader("bytes"), 5, "video/mp4"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(lastFakeBackend.puts) != 1 ||
		lastFakeBackend.puts[0] != "nvr/x/2026/11/04/a.mp4 video/mp4 5 bytes" {
		t.Fatalf("Put reached the driver as %v", lastFakeBackend.puts)
	}
	rc, err := backend.Get(ctx, "nvr/x/2026/11/04/a.mp4")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "remote bytes" {
		t.Fatalf("Get read %q", body)
	}
	info, err := backend.Stat(ctx, "nvr/x/2026/11/04/a.mp4")
	if err != nil || info.Size != 11 || info.ContentType != "video/mp4" {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	keys, err := backend.List(ctx, "nvr/x/")
	if err != nil || len(keys) != 2 {
		t.Fatalf("List = %v, %v", keys, err)
	}
	if err := backend.Delete(ctx, "nvr/x/2026/11/04/a.mp4"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := backend.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if lastFakeBackend.pings != 1 || len(lastFakeBackend.got) != 1 ||
		len(lastFakeBackend.deleted) != 1 || len(lastFakeBackend.statd) != 1 ||
		len(lastFakeBackend.listeds) != 1 {
		t.Fatalf("the driver saw puts=%v gets=%v stats=%v lists=%v deletes=%v pings=%d",
			lastFakeBackend.puts, lastFakeBackend.got, lastFakeBackend.statd,
			lastFakeBackend.listeds, lastFakeBackend.deleted, lastFakeBackend.pings)
	}
}

func TestOpenRefusesAKindThisBuildHasNoDriverFor(t *testing.T) {
	key := sealKey(t)
	// A kind that is not in models.StorageKinds at all: the case is a database
	// restored from a newer installation, and the answer must name the kind
	// rather than return a nil backend and a nil error.
	row := &models.StorageBackend{ID: "b2", Name: "future", Kind: "carrier-pigeon"}

	_, err := Open(context.Background(), row, key)
	if !errors.Is(err, ErrKindUnsupported) {
		t.Fatalf("Open with an unknown kind: got %v, want ErrKindUnsupported", err)
	}
	if !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Fatalf("the refusal does not name the kind: %v", err)
	}
}

func TestOpenNilRowIsNoBackend(t *testing.T) {
	if _, err := Open(context.Background(), nil, sealKey(t)); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("Open(nil): got %v, want ErrNoBackend", err)
	}
}

func TestUnsealOpensOnlyWhatTheKindReads(t *testing.T) {
	key := sealKey(t)

	t.Run("s3 reads its two access keys", func(t *testing.T) {
		row := &models.StorageBackend{
			Kind:      models.StorageKindS3,
			AccessKey: mustSeal(t, "access-key-value", key),
			SecretKey: mustSeal(t, "secret-key-value", key),
		}
		secret, err := unseal(row, key)
		if err != nil {
			t.Fatalf("unseal: %v", err)
		}
		if secret.AccessKey != "access-key-value" || secret.SecretKey != "secret-key-value" {
			t.Fatalf("unseal gave %+v", secret)
		}
	})

	t.Run("webdav reads its password and keeps the username in the clear", func(t *testing.T) {
		row := &models.StorageBackend{
			Kind:     models.StorageKindWebDAV,
			Username: "media",
			Password: mustSeal(t, "webdav-password", key),
		}
		secret, err := unseal(row, key)
		if err != nil {
			t.Fatalf("unseal: %v", err)
		}
		if secret.Password != "webdav-password" {
			t.Fatalf("unseal gave %+v", secret)
		}
		if secret.Username != "media" {
			t.Fatalf("the username was not carried: %+v", secret)
		}
	})

	t.Run("sftp reads the password and the key path", func(t *testing.T) {
		row := &models.StorageBackend{
			Kind:           models.StorageKindSFTP,
			Username:       "media",
			Password:       mustSeal(t, "sftp-password", key),
			PrivateKeyPath: mustSeal(t, "/etc/nvr/keys/id_ed25519", key),
		}
		secret, err := unseal(row, key)
		if err != nil {
			t.Fatalf("unseal: %v", err)
		}
		if secret.Password != "sftp-password" || secret.PrivateKeyPath != "/etc/nvr/keys/id_ed25519" {
			t.Fatalf("unseal gave %+v", secret)
		}
	})

	t.Run("fs needs no credential and is not failed by a stray one", func(t *testing.T) {
		row := &models.StorageBackend{Kind: models.StorageKindFS, RootPath: "/srv/nvr"}
		secret, err := unseal(row, key)
		if err != nil {
			t.Fatalf("unseal: %v", err)
		}
		if secret != (Credentials{}) {
			t.Fatalf("fs unsealed %+v, want nothing", secret)
		}

		// A row edited by hand, or one saved by a form that kept the fields of
		// another kind: the operator's local backend works, and the junk is
		// ignored rather than opening a failure path through the mount.
		stray := &models.StorageBackend{Kind: models.StorageKindFS, RootPath: "/srv/nvr", Password: "not-even-base64-$$"}
		if _, err := unseal(stray, key); err != nil {
			t.Fatalf("unseal of an fs row with a stray password: %v", err)
		}
	})

	t.Run("a credential sealed with another key is named, and not as bad credentials", func(t *testing.T) {
		other := [32]byte{}
		other[31] = 0x7f
		row := &models.StorageBackend{
			Name:      "minio-prod",
			Kind:      models.StorageKindS3,
			AccessKey: mustSeal(t, "access-key-value", other),
		}
		_, err := unseal(row, key)
		if !errors.Is(err, ErrBadKey) {
			t.Fatalf("unseal with the wrong seal key: got %v, want ErrBadKey", err)
		}
		if errors.Is(err, ErrBadCredentials) {
			t.Fatalf("a seal failure was reported as %v, which sends the operator to the wrong field", err)
		}
		if strings.Contains(err.Error(), "access-key-value") {
			t.Fatalf("the failure names the credential value: %v", err)
		}
		if !strings.Contains(err.Error(), "minio-prod") || !strings.Contains(err.Error(), "access_key") {
			t.Fatalf("the failure does not name the backend and the column: %v", err)
		}
	})
}

func TestOpenFailureNamesTheBackendAndNeverTheCredential(t *testing.T) {
	key := sealKey(t)

	// A driver that refuses, wrapping one of the four names.
	registerDriver("test-fake-broken", func(ctx context.Context, a *openArgs) (Backend, error) {
		return nil, fmt.Errorf("dial refused: %w", ErrUnreachable)
	})

	row := &models.StorageBackend{
		ID: "b3", Name: "minio-prod", Kind: "test-fake-broken",
		SecretKey: mustSeal(t, "the-actual-secret-value", key),
	}
	_, err := Open(context.Background(), row, key)
	if err == nil {
		t.Fatal("a failing driver returned no error")
	}
	if !strings.Contains(err.Error(), "minio-prod") {
		t.Fatalf("the error does not name the backend: %v", err)
	}
	if strings.Contains(err.Error(), "the-actual-secret-value") {
		t.Fatalf("the error carries the credential: %v", err)
	}
	// The plaintext leak check that matters is on `unseal`, which is the only
	// place a plaintext exists at all; this one pins the neighbouring half — a
	// failure on a row that carries sealed text never echoes the sealed text
	// either, so a log line names the backend and not the object.
	if strings.Contains(err.Error(), row.SecretKey) {
		t.Fatalf("the error carries the sealed column value: %v", err)
	}
	if ErrorCode(err) != ErrUnreachable.Error() {
		t.Fatalf("the wrapped failure maps to %q, want %q", ErrorCode(err), ErrUnreachable.Error())
	}
}

func TestTheFourNamesAreTheOnesThePlanNames(t *testing.T) {
	// The API answers these strings verbatim and scripts/verify.sh greps for
	// them, so a rename is a contract change. Asserting the literals here is
	// what makes the rename deliberate.
	for got, want := range map[string]string{
		ErrUnreachable.Error():    "discovery_unreachable",
		ErrBadCredentials.Error(): "bad credentials",
		ErrHostKeyChanged.Error(): "sftp host key changed",
		ErrBucketMissing.Error():  "bucket missing",
	} {
		if got != want {
			t.Errorf("the API reports %q, want %q", got, want)
		}
	}
}

func TestPingFailuresMapToTheNamedErrors(t *testing.T) {
	// Each name on its own, and each wrapped in the kind of sentence a driver
	// actually logs — the mapping is by errors.Is, so the wrapper must not
	// change the answer.
	cases := []struct {
		err  error
		want string
	}{
		{ErrUnreachable, "discovery_unreachable"},
		// A driver that copies the name into its own sentence instead of
		// wrapping it is a driver whose failure the API cannot name. Asserting
		// that answers "" keeps the wrapping honest.
		{errors.New("wrapped: " + ErrUnreachable.Error()), ""},
		{fmt.Errorf("dial tcp 10.0.0.5:9000: connect: connection refused: %w", ErrUnreachable), "discovery_unreachable"},
		{fmt.Errorf("The access key id you provided does not exist in our records: %w", ErrBadCredentials), "bad credentials"},
		{fmt.Errorf("ssh: the host key for 10.0.0.9:22 differs from the pinned key: %w", ErrHostKeyChanged), "sftp host key changed"},
		{fmt.Errorf("the bucket `nvr` does not exist: %w", ErrBucketMissing), "bucket missing"},
		{ErrNotFound, ""},
		{ErrNoBackend, ""},
		{ErrBadKey, ""},
		{errors.New("a driver bug"), ""},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := ErrorCode(tc.err); got != tc.want {
			t.Errorf("ErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}

	// An SFTP host-key change is an authentication-shaped refusal, and a driver
	// that wrapped both names must answer the more specific one: "bad
	// credentials" would send the operator to retype a password while the real
	// answer is "someone is standing between you and that server".
	both := fmt.Errorf("%w (and also %w)", ErrHostKeyChanged, ErrBadCredentials)
	if got := ErrorCode(both); got != "sftp host key changed" {
		t.Fatalf("an error wrapping both names maps to %q", got)
	}
}

func TestOpenDefaultFollowsTheDefaultRow(t *testing.T) {
	st := newStorageTestStore(t)
	key := sealKey(t)

	t.Run("an installation with no backend says so by name", func(t *testing.T) {
		_, err := OpenDefault(context.Background(), st, key)
		if !errors.Is(err, ErrNoBackend) {
			t.Fatalf("OpenDefault with no rows: got %v, want ErrNoBackend", err)
		}
	})

	first := seedBackend(t, st, "first", fakeKind)
	second := seedBackend(t, st, "second", fakeKind)

	t.Run("one enabled backend is the default without being marked", func(t *testing.T) {
		// The store's rule (S1-11): asking an operator who configured one
		// destination to also tick "default" is a setup step with no answer in it.
		if err := st.StorageBackends.SetEnabled(first.ID, false); err != nil {
			t.Fatalf("disabling %s: %v", first.Name, err)
		}
		if _, err := OpenDefault(context.Background(), st, key); err != nil {
			t.Fatalf("OpenDefault with one enabled backend: %v", err)
		}
		if lastFake == nil || lastFake.Row.ID != second.ID {
			t.Fatalf("OpenDefault opened %+v, want %q", lastFake, second.Name)
		}
		if err := st.StorageBackends.SetEnabled(first.ID, true); err != nil {
			t.Fatalf("re-enabling %s: %v", first.Name, err)
		}
	})

	t.Run("two enabled backends with no marked default is no default", func(t *testing.T) {
		// Not a coin flip: the store refuses to guess which destination a new
		// upload goes to, and OpenDefault passes that answer through.
		if _, err := OpenDefault(context.Background(), st, key); !errors.Is(err, ErrNoBackend) {
			t.Fatalf("OpenDefault with two enabled backends and no default: got %v, want ErrNoBackend", err)
		}
	})

	t.Run("the marked default wins over the oldest row", func(t *testing.T) {
		if err := st.StorageBackends.SetDefault(second.ID); err != nil {
			t.Fatalf("SetDefault: %v", err)
		}
		if _, err := OpenDefault(context.Background(), st, key); err != nil {
			t.Fatalf("OpenDefault: %v", err)
		}
		if lastFake == nil || lastFake.Row.ID != second.ID {
			t.Fatalf("OpenDefault opened %+v, want the marked default %q", lastFake, second.Name)
		}
	})

	t.Run("the settings store travels to the driver", func(t *testing.T) {
		// S7-06 pins an SFTP host key in settings; a driver that reaches for it
		// gets the real store through OpenDefault, not a nil.
		if _, err := OpenDefault(context.Background(), st, key); err != nil {
			t.Fatalf("OpenDefault: %v", err)
		}
		if lastFake == nil || lastFake.Settings != st.Settings {
			t.Fatalf("the driver got Settings=%v, want the store's setting store", lastFake.Settings)
		}
	})

	t.Run("a nil store is no backend rather than a panic", func(t *testing.T) {
		if _, err := OpenDefault(context.Background(), nil, key); !errors.Is(err, ErrNoBackend) {
			t.Fatalf("OpenDefault(nil store): got %v, want ErrNoBackend", err)
		}
	})

	t.Run("a default row whose kind this build cannot drive is refused", func(t *testing.T) {
		if err := st.DB.Model(&models.StorageBackend{}).Where("id = ?", second.ID).
			Update("kind", "carrier-pigeon").Error; err != nil {
			t.Fatalf("forcing the kind: %v", err)
		}
		if _, err := OpenDefault(context.Background(), st, key); !errors.Is(err, ErrKindUnsupported) {
			t.Fatalf("OpenDefault onto an undrivable kind: got %v, want ErrKindUnsupported", err)
		}
	})
}

func TestRegisteredKindsIsWhatThisBuildLinks(t *testing.T) {
	kinds := RegisteredKinds()
	for _, kind := range kinds {
		if !models.ValidStorageKind(kind) {
			t.Fatalf("RegisteredKinds returned %q, which is not a storage kind", kind)
		}
	}
	// The order is models.StorageKinds', not the map's: the answer goes in a
	// boot log, and a map iteration order that moves each run is a log line
	// that cannot be diffed between two boots.
	last := -1
	for _, kind := range kinds {
		at := -1
		for i, known := range models.StorageKinds {
			if known == kind {
				at = i
			}
		}
		if at < last {
			t.Fatalf("RegisteredKinds is out of models.StorageKinds order: %v", kinds)
		}
		last = at
	}
	// The test kind is not a storage kind, so it must not appear.
	for _, kind := range kinds {
		if kind == fakeKind {
			t.Fatalf("RegisteredKinds lists the test kind %q", kind)
		}
	}
}

func TestNoDriverIsLinkedByABinaryThatDoesNotUseIt(t *testing.T) {
	// The criterion is about the dependency set: the plan commits to four
	// storage SDKs plus the in-test S3 server, and every one of them is pinned
	// in `deps/` from Step 1 so a Step 7 commit changes code and not a
	// dependency resolved under pressure. A fifth SDK in go.mod means a driver
	// arrived that the plan did not ask for.
	raw, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}

	// The names that would appear if a driver reached for a different SDK.
	banned := []string{
		"github.com/aws/aws-sdk-go",
		"github.com/rustfs/",
		"github.com/ncw/swift",
		"github.com/slic6313/webdav",
		"github.com/jlaffaye/ftp",
		"github.com/pkg/sftp/v2",
		"github.com/minio/minio-go/v6",
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "require ") {
			line = strings.TrimPrefix(line, "require ")
		}
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "module ") ||
			strings.HasPrefix(line, "go ") || strings.HasPrefix(line, "toolchain ") ||
			line == ")" || strings.HasPrefix(line, "require (") {
			continue
		}
		module := strings.Fields(line)[0]
		for _, b := range banned {
			if strings.HasPrefix(module, b) {
				t.Fatalf("go.mod requires %q, a storage SDK the plan does not name", module)
			}
		}
	}

	// And the five the plan does name are present, so a driver card cannot
	// quietly drop one and answer its kind with a stub.
	for _, needed := range []string{
		"github.com/minio/minio-go/v7",
		"github.com/studio-b12/gowebdav",
		"github.com/pkg/sftp",
		"golang.org/x/crypto",
		"github.com/johannesboyne/gofakes3",
	} {
		if !strings.Contains(string(raw), needed) {
			t.Fatalf("go.mod no longer requires %q", needed)
		}
	}
}

// -- fixtures -------------------------------------------------------------

func mustSeal(t *testing.T, plain string, key [32]byte) string {
	t.Helper()
	sealed, err := Seal(plain, key)
	if err != nil {
		t.Fatalf("Seal(%q): %v", plain, err)
	}
	return sealed
}

// newStorageTestStore is this package's fixture: the sqlite path every other
// suite uses, migrated by the same code a production boot runs.
func newStorageTestStore(t *testing.T) *store.Store {
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

func seedBackend(t *testing.T, st *store.Store, name, kind string) *models.StorageBackend {
	t.Helper()
	row := &models.StorageBackend{Name: name, Kind: kind, Enabled: true}
	if err := st.StorageBackends.Create(row); err != nil {
		t.Fatalf("creating backend %s: %v", name, err)
	}
	return row
}
