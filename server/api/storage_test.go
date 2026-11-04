package api

// S7-10 — the storage surface as the Settings page drives it.
//
// The four exit criteria are all about what a response *says*:
//
//   - a Ping failure answers one of the four names, verbatim
//   - a secret has no path out: `secret_set: true` and no secret field
//   - every backend write is a ledger row
//   - the queue paginates
//
// The tier is real where it can be: an fs backend over a temp directory, and an
// s3 backend pointed at an in-test HTTP server that answers the way an S3
// endpoint answers when it does not like you. The fourth name,
// `sftp host key changed`, is produced by the sftp driver and asserted verbatim
// in `storage/sftp_test.go`; what this file proves is that the route prints
// whatever name the storage package produced, unchanged.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nvr/server/models"
	"nvr/server/storage"
	"nvr/server/store"
)

// storageFixture is the admin, the router, and one route table.
type storageFixture struct {
	h      *Handlers
	router interface {
		ServeHTTP(http.ResponseWriter, *http.Request)
	}
	token string
}

func newStorageFixture(t *testing.T) *storageFixture {
	t.Helper()
	st := newStore(t)
	h := newHandlers(t, st)
	token, _ := usersToken(t, h)
	return &storageFixture{h: h, router: routerFor(t, h), token: token}
}

func (f *storageFixture) call(t *testing.T, r request) (int, map[string]any, string) {
	t.Helper()
	r.bearer = f.token
	return do(t, f.router, r)
}

// createFS adds an fs backend over a temp directory that is really there.
func (f *storageFixture) createFS(t *testing.T, name string) map[string]any {
	t.Helper()
	status, body, raw := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends",
		body: map[string]any{"name": name, "kind": models.StorageKindFS, "root_path": t.TempDir()},
	})
	if status != http.StatusCreated {
		t.Fatalf("creating the backend: got %d %s, want 201", status, raw)
	}
	return body
}

// s3Stub is an S3 endpoint that answers the way S3 answers when the request is
// refused, so the four connection names can be driven through the route without
// a bucket in the room.
func s3Stub(t *testing.T, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusOK {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		fmt.Fprintf(w, `<Error><Code>%s</Code><Message>stub</Message><BucketName>nvr</BucketName></Error>`,
			stubCodeFor(status))
	}))
	t.Cleanup(server.Close)
	return server
}

func stubCodeFor(status int) string {
	switch status {
	case http.StatusForbidden:
		return "AccessDenied"
	case http.StatusNotFound:
		return "NoSuchBucket"
	default:
		return "InternalError"
	}
}

// endpointOf strips the scheme: the s3 driver takes a host:port, and the row
// stores what the operator typed.
func endpointOf(server *httptest.Server) string {
	return strings.TrimPrefix(strings.TrimPrefix(server.URL, "http://"), "https://")
}

func TestStorageGateIsAdminOnly(t *testing.T) {
	f := newStorageFixture(t)

	watcher := newUser(t, f.h.St, "watcher-storage", models.RoleViewer)
	operator := newUser(t, f.h.St, "operator-storage", models.RoleOperator)

	writes := []request{
		{method: http.MethodGet, path: "/api/storage/backends"},
		{method: http.MethodPost, path: "/api/storage/backends", body: map[string]any{"name": "x", "kind": "fs", "root_path": "/tmp"}},
		{method: http.MethodGet, path: "/api/storage/queue"},
	}
	for _, u := range []*models.User{watcher, operator} {
		for _, r := range writes {
			status, body, _ := do(t, f.router, request{method: r.method, path: r.path, bearer: accessToken(t, f.h.St, f.h.JWT, u), body: r.body})
			if status != http.StatusForbidden {
				t.Fatalf("%s on %s %s: got %d %v, want 403", u.Username, r.method, r.path, status, body)
			}
		}
	}

	for _, r := range writes {
		status, body, _ := f.call(t, r)
		if status == http.StatusForbidden {
			t.Fatalf("the admin was refused on %s %s: %v", r.method, r.path, body)
		}
	}
}

func TestStorageSecretsAreNeverEchoedByAnyRoute(t *testing.T) {
	f := newStorageFixture(t)

	const (
		access = "AKIAIOSFODNN7EXAMPLE"
		secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	)
	stub := s3Stub(t, http.StatusOK)

	status, created, raw := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends",
		body: map[string]any{
			"name": "minio", "kind": models.StorageKindS3,
			"endpoint": endpointOf(stub), "bucket": "nvr", "region": "us-east-1",
			"access_key": access, "secret_key": secret, "insecure_tls": true,
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("creating the s3 backend: got %d %s", status, raw)
	}

	// The create response itself.
	assertNoSecret(t, "POST /api/storage/backends", raw, secret)
	if created["secret_set"] != true {
		t.Errorf("the create response says secret_set=%v, want true", created["secret_set"])
	}
	if created["access_key_set"] != true || created["secret_key_set"] != true {
		t.Errorf("the create response says %+v, want both credential flags true", created)
	}
	for _, field := range []string{"secret_key", "access_key", "password", "private_key_path"} {
		if _, present := created[field]; present {
			t.Errorf("the create response carries the field %q at all: %v", field, created[field])
		}
	}

	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("the create response carries no id")
	}

	for _, r := range []request{
		{method: http.MethodGet, path: "/api/storage/backends"},
		{method: http.MethodPut, path: "/api/storage/backends/" + id, body: map[string]any{"region": "eu-west-1"}},
		{method: http.MethodPost, path: "/api/storage/backends/" + id + "/test"},
		{method: http.MethodPost, path: "/api/storage/backends/" + id + "/prefixes", query: "prefix=nvr/"},
		{method: http.MethodGet, path: "/api/storage/queue"},
	} {
		code, _, body := f.call(t, r)
		if code >= 500 {
			t.Errorf("%s %s answered %d: %s", r.method, r.path, code, body)
		}
		assertNoSecret(t, r.method+" "+r.path, body, secret)
	}

	// And the column holds sealed text, not the plaintext the operator typed: a
	// database backup is readable and a credential leak still needs the seal key.
	row, err := f.h.St.StorageBackends.GetByID(id)
	if err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if row.SecretKey == secret {
		t.Error("the secret_key column holds plaintext")
	}
	key, keyErr := f.h.Cfg.SealKey()
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if opened, openErr := storage.Unseal(row.SecretKey, key); openErr != nil || opened != secret {
		t.Errorf("the stored secret does not open with this installation's key: %q, %v", opened, openErr)
	}
}

// assertNoSecret is the criterion in its strongest form: not "the view has no
// secret field" but "the bytes of the response do not contain the secret", which
// also catches a handler that puts one in an error message.
func assertNoSecret(t *testing.T, where, body, secret string) {
	t.Helper()
	if strings.Contains(body, secret) {
		t.Errorf("%s echoed the secret: %s", where, body)
	}
}

func TestEveryStorageBackendWriteIsALedgerRow(t *testing.T) {
	f := newStorageFixture(t)
	created := f.createFS(t, "audited")
	id, _ := created["id"].(string)

	calls := []struct {
		action string
		req    request
	}{
		{models.AuditStorageRead, request{method: http.MethodGet, path: "/api/storage/backends"}},
		{models.AuditStorageCreate, request{method: http.MethodPost, path: "/api/storage/backends",
			body: map[string]any{"name": "audited-two", "kind": models.StorageKindFS, "root_path": t.TempDir()}}},
		{models.AuditStorageUpdate, request{method: http.MethodPut, path: "/api/storage/backends/" + id,
			body: map[string]any{"prefix": "nvr"}}},
		{models.AuditStorageTest, request{method: http.MethodPost, path: "/api/storage/backends/" + id + "/test"}},
		{models.AuditStoragePrefix, request{method: http.MethodPost, path: "/api/storage/backends/" + id + "/prefixes", query: "prefix="}},
		{models.AuditStorageDefault, request{method: http.MethodPut, path: "/api/storage/backends/" + id + "/default"}},
		{models.AuditStorageEnable, request{method: http.MethodPut, path: "/api/storage/backends/" + id + "/enabled",
			body: map[string]any{"enabled": false}}},
		{models.AuditStorageRequeue, request{method: http.MethodPost, path: "/api/storage/backends/" + id + "/requeue",
			query: "kind=segment"}},
		{models.AuditStorageDelete, request{method: http.MethodDelete, path: "/api/storage/backends/" + id}},
	}

	for _, tc := range calls {
		status, body, raw := f.call(t, tc.req)
		if status >= 400 {
			t.Fatalf("%s %s answered %d %s, want a success that gets audited", tc.req.method, tc.req.path, status, raw)
		}
		_ = body
		rows := auditRows(t, f.h.St, tc.action)
		if len(rows) == 0 {
			t.Errorf("%s %s wrote no %s row", tc.req.method, tc.req.path, tc.action)
			continue
		}
		if strings.Contains(rows[0].Details, "wJalrXUtnFEMI") {
			t.Errorf("the %s ledger row carries a secret: %s", tc.action, rows[0].Details)
		}
	}
}

func TestStorageConnectionTestAnswersTheNamedFailures(t *testing.T) {
	f := newStorageFixture(t)

	// The healthy case, against a directory that is really there.
	healthy := f.createFS(t, "healthy")
	status, body, raw := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends/" + healthy["id"].(string) + "/test",
	})
	if status != http.StatusOK {
		t.Fatalf("the test route answered %d %s, want 200", status, raw)
	}
	if body["ok"] != true {
		t.Fatalf("a reachable tier answered %v, want ok:true", body)
	}

	cases := []struct {
		name   string
		body   map[string]any
		want   string
		reason string
	}{
		{
			name:   "a directory that is not there",
			body:   map[string]any{"name": "gone", "kind": models.StorageKindFS, "root_path": filepath.Join(t.TempDir(), "nope")},
			want:   "bucket missing",
			reason: "is not there",
		},
		{
			name: "an endpoint that refuses the connection",
			body: map[string]any{"name": "dead", "kind": models.StorageKindS3, "endpoint": "127.0.0.1:1",
				"bucket": "nvr", "insecure_tls": true},
			want: "discovery_unreachable",
		},
		{
			name: "a tier that does not recognise the credentials",
			body: map[string]any{"name": "denied", "kind": models.StorageKindS3,
				"endpoint": endpointOf(s3Stub(t, http.StatusForbidden)), "bucket": "nvr",
				"access_key": "AKIAIOSFODNN7EXAMPLE", "secret_key": "nope", "insecure_tls": true},
			want: "bad credentials",
		},
		{
			name: "a bucket that is not there",
			body: map[string]any{"name": "nobucket", "kind": models.StorageKindS3,
				"endpoint": endpointOf(s3Stub(t, http.StatusNotFound)), "bucket": "nvr",
				"access_key": "AKIAIOSFODNN7EXAMPLE", "secret_key": "nope", "insecure_tls": true},
			want: "bucket missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, created, raw := f.call(t, request{method: http.MethodPost, path: "/api/storage/backends", body: tc.body})
			if code != http.StatusCreated {
				t.Fatalf("creating the backend: got %d %s", code, raw)
			}
			_, body, raw := f.call(t, request{
				method: http.MethodPost, path: "/api/storage/backends/" + created["id"].(string) + "/test",
			})
			if body["ok"] != false {
				t.Fatalf("the test route answered %s, want ok:false", raw)
			}
			// Verbatim, not paraphrased: the console draws a state from this
			// string, and the four states are the four fixes.
			if body["error"] != tc.want {
				t.Errorf("error is %v, want %q", body["error"], tc.want)
			}
			if reason, _ := body["reason"].(string); reason == "" {
				t.Error("the refusal carries no reason, and the operator has to fix something")
			} else if tc.reason != "" && !strings.Contains(reason, tc.reason) {
				t.Errorf("reason is %q, want it to say %q", reason, tc.reason)
			}
		})
	}
}

func TestStorageConnectionTestNamesWhatItCannotAsOneOfFour(t *testing.T) {
	// `ErrBadKey` is deliberately not one of the four connection names: the
	// server and the credential may both be fine, and only this process cannot
	// open the row. The route says so without inventing a fifth tier state.
	f := newStorageFixture(t)
	created := f.createFS(t, "sealed-elsewhere")
	id, _ := created["id"].(string)

	// An s3 row whose secret was sealed by a different installation.
	other := [32]byte{}
	other[0] = 0xff
	row, err := f.h.St.StorageBackends.GetByID(id)
	if err != nil {
		t.Fatal(err)
	}
	row.Kind = models.StorageKindS3
	row.Endpoint = "127.0.0.1:1"
	row.Bucket = "nvr"
	sealed, sealErr := storage.Seal("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", other)
	if sealErr != nil {
		t.Fatal(sealErr)
	}
	row.SecretKey = sealed
	if err := f.h.St.StorageBackends.Update(row, false, false, true, false); err != nil {
		t.Fatal(err)
	}

	_, body, raw := f.call(t, request{method: http.MethodPost, path: "/api/storage/backends/" + id + "/test"})
	if body["ok"] != false {
		t.Fatalf("a row this process cannot open answered %s, want ok:false", raw)
	}
	if body["error"] != "connection_failed" {
		t.Errorf("error is %v, want connection_failed: the four names describe the tier, not this process", body["error"])
	}
	if reason, _ := body["reason"].(string); !strings.Contains(reason, "seal") {
		t.Errorf("reason is %q, want it to name the seal key as the thing to fix", reason)
	}
}

func TestStorageKindPresenceIsValidatedPerKind(t *testing.T) {
	f := newStorageFixture(t)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"s3 without a bucket", map[string]any{"name": "a", "kind": "s3", "endpoint": "minio:9000"}, "bucket"},
		{"s3 without an endpoint", map[string]any{"name": "b", "kind": "s3", "bucket": "nvr"}, "endpoint"},
		{"webdav without an endpoint", map[string]any{"name": "c", "kind": "webdav"}, "endpoint"},
		{"sftp without a root", map[string]any{"name": "d", "kind": "sftp", "endpoint": "host:22"}, "root_path"},
		{"fs without a root", map[string]any{"name": "e", "kind": "fs"}, "root_path"},
		{"an unknown kind", map[string]any{"name": "f", "kind": "ftp", "root_path": "/tmp"}, "kind"},
		{"no name", map[string]any{"kind": "fs", "root_path": "/tmp"}, "name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body, _ := f.call(t, request{method: http.MethodPost, path: "/api/storage/backends", body: tc.body})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("got %d %v, want 422", status, body)
			}
			if err, _ := body["error"].(string); !strings.Contains(err, tc.want) {
				t.Errorf("the refusal is %q, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestStorageUpdateKeepsAnOmittedSecretAndClearsAnEmptyOne(t *testing.T) {
	f := newStorageFixture(t)
	stub := s3Stub(t, http.StatusOK)

	_, created, raw := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends",
		body: map[string]any{
			"name": "roundtrip", "kind": models.StorageKindS3, "endpoint": endpointOf(stub),
			"bucket": "nvr", "access_key": "AKIAIOSFODNN7EXAMPLE",
			"secret_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "insecure_tls": true,
		},
	})
	if created["secret_key_set"] != true {
		t.Fatalf("the row was created without a secret key: %s", raw)
	}
	id, _ := created["id"].(string)

	// A PUT that never mentions the secret keeps it: this is what lets the
	// console edit a row whose secrets it cannot read back.
	_, after, _ := f.call(t, request{
		method: http.MethodPut, path: "/api/storage/backends/" + id,
		body: map[string]any{"region": "eu-west-1"},
	})
	if after["secret_key_set"] != true {
		t.Error("a PUT that omitted the secret key lost it")
	}
	if after["region"] != "eu-west-1" {
		t.Errorf("the PUT did not apply the field it sent: %v", after)
	}

	// A PUT that sends it empty clears it. Reading the flag off the sealed value
	// would have kept the old secret through the operator's delete.
	_, cleared, _ := f.call(t, request{
		method: http.MethodPut, path: "/api/storage/backends/" + id,
		body: map[string]any{"secret_key": ""},
	})
	if cleared["secret_key_set"] != false {
		t.Errorf("sending an empty secret_key did not clear it: %v", cleared)
	}
	row, err := f.h.St.StorageBackends.GetByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if row.SecretKey != "" {
		t.Error("the column still holds a secret after the clear")
	}
}

func TestStorageKindCannotChangeOnAnExistingBackend(t *testing.T) {
	f := newStorageFixture(t)
	created := f.createFS(t, "immutable-kind")

	status, body, _ := f.call(t, request{
		method: http.MethodPut, path: "/api/storage/backends/" + created["id"].(string),
		body: map[string]any{"kind": models.StorageKindS3},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("got %d %v, want 422", status, body)
	}
}

func TestStorageDefaultMovesTheFlagAndRefusesADisabledBackend(t *testing.T) {
	f := newStorageFixture(t)
	first := f.createFS(t, "default-one")
	second := f.createFS(t, "default-two")

	// No backend claims to be the default until an operator says so — the plan's
	// example body sends `is_default: true` explicitly, and auto-flagging the
	// first row would leave it flagged after a second one arrives. What the
	// installation still resolves is the store's fallback: with one enabled
	// backend and no flag set, that backend *is* the default.
	if first["is_default"] != false {
		t.Errorf("creating the first backend set is_default=%v, want false: the flag is the operator's to set", first["is_default"])
	}

	_, moved, _ := f.call(t, request{method: http.MethodPut, path: "/api/storage/backends/" + second["id"].(string) + "/default"})
	if moved["default"] != "default-two" {
		t.Fatalf("the default move answered %v", moved)
	}
	if effective, err := f.h.St.StorageBackends.GetDefault(); err != nil || effective.ID != second["id"] {
		t.Errorf("the queue resolves the default to %+v, want the backend that was just made default", effective)
	}

	_, list, _ := f.call(t, request{method: http.MethodGet, path: "/api/storage/backends"})
	backends, _ := list["backends"].([]any)
	defaults := 0
	for _, raw := range backends {
		if row, ok := raw.(map[string]any); ok && row["is_default"] == true {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("%d backends claim to be the default, want exactly 1", defaults)
	}

	// A disabled default is a queue with nowhere to send.
	_, off, _ := f.call(t, request{
		method: http.MethodPut, path: "/api/storage/backends/" + second["id"].(string) + "/enabled",
		body: map[string]any{"enabled": false},
	})
	if off["enabled"] != false {
		t.Fatalf("the enable route did not switch it off: %v", off)
	}
	status, body, _ := f.call(t, request{method: http.MethodPut, path: "/api/storage/backends/" + second["id"].(string) + "/default"})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a disabled backend became the default with %d %v, want 422", status, body)
	}
}

func TestStorageDisableStopsNewClaims(t *testing.T) {
	// The plan's verify step 7: `PUT .../enabled {"enabled":false}` → new jobs
	// stay pending. The claim is the gate, so the queue page shows the pile.
	f := newStorageFixture(t)
	created := f.createFS(t, "pausable")
	id, _ := created["id"].(string)

	segment := &models.Segment{
		CameraID: "00000000-0000-4000-8000-000000000001", Generation: 1,
		Path: filepath.Join(t.TempDir(), "front-door-1-1.mp4"), Kind: models.SegmentKindContinuous,
	}
	if err := f.h.St.Segments.Create(segment); err != nil {
		t.Fatal(err)
	}
	if err := f.h.St.StorageJobs.Enqueue(store.JobInput{
		ID: segment.ID, Kind: models.JobKindSegment, Target: segment.Path,
		BackendID: id, Action: models.JobActionUpload,
	}); err != nil {
		t.Fatal(err)
	}

	f.call(t, request{
		method: http.MethodPut, path: "/api/storage/backends/" + id + "/enabled",
		body: map[string]any{"enabled": false},
	})
	if _, err := f.h.St.StorageJobs.ClaimNext(t.Context(), "worker"); err == nil {
		t.Fatal("a disabled backend handed out a claim")
	}

	f.call(t, request{
		method: http.MethodPut, path: "/api/storage/backends/" + id + "/enabled",
		body: map[string]any{"enabled": true},
	})
	claimed, err := f.h.St.StorageJobs.ClaimNext(t.Context(), "worker")
	if err != nil {
		t.Fatalf("re-enabling did not resume the queue: %v", err)
	}
	if claimed.ID != segment.ID {
		t.Errorf("the claim went to %s, want the queued segment", claimed.ID)
	}
}

func TestStoragePrefixesVerifyAPath(t *testing.T) {
	f := newStorageFixture(t)
	root := t.TempDir()

	_, created, raw := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends",
		body: map[string]any{"name": "verifiable", "kind": models.StorageKindFS, "root_path": root},
	})
	if created["id"] == nil {
		t.Fatalf("no id: %d %s", http.StatusCreated, raw)
	}
	id, _ := created["id"].(string)

	for _, key := range []string{
		"nvr/front-door/2026/11/04/front-door-0-7.mp4",
		"nvr/front-door/2026/11/05/front-door-0-8.mp4",
		"nvr/events/e1/clip.mp4",
	} {
		full := filepath.Join(root, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, listed, _ := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends/" + id + "/prefixes",
		query: "prefix=nvr/front-door/",
	})
	keys, _ := listed["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("prefix=nvr/front-door/ listed %v, want the two segments", listed["keys"])
	}
	if listed["prefix"] != "nvr/front-door/" {
		t.Errorf("the response does not echo the prefix that was asked for: %v", listed["prefix"])
	}

	_, everything, _ := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends/" + id + "/prefixes",
	})
	if count, _ := everything["count"].(float64); int(count) != 3 {
		t.Errorf("an empty prefix listed %v keys, want the whole tier (3)", everything["count"])
	}

	// A tier that is not there answers with the named failure, not an empty list:
	// an empty list is exactly what a wrong prefix looks like, and the operator
	// has to be able to tell "nothing here" from "nothing there".
	_, gone, _ := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends",
		body: map[string]any{"name": "unverifiable", "kind": models.StorageKindFS,
			"root_path": filepath.Join(root, "not-a-directory")},
	})
	_, answer, _ := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends/" + gone["id"].(string) + "/prefixes",
		query: "prefix=nvr/",
	})
	if answer["ok"] != false || answer["error"] != "bucket missing" {
		t.Errorf("an unreachable tier answered %v, want ok:false bucket missing", answer)
	}
}

func TestStorageRequeueReShelvesUploadedRows(t *testing.T) {
	f := newStorageFixture(t)
	from := f.createFS(t, "old-tier")
	to := f.createFS(t, "new-tier")

	if err := f.h.St.StorageJobs.Enqueue(store.JobInput{
		ID: "3f2b1c00-0000-4000-8000-000000000001", Kind: models.JobKindSegment,
		Target:    "/var/lib/nvr/segments/front-door-0-7.mp4",
		BackendID: from["id"].(string), Action: models.JobActionUpload,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.St.StorageJobs.MarkUploaded("3f2b1c00-0000-4000-8000-000000000001", "nvr/front-door/x.mp4", 10); err != nil {
		t.Fatal(err)
	}

	_, moved, _ := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends/" + to["id"].(string) + "/requeue",
		query: "kind=segment",
	})
	if moved["requeued"] != float64(1) {
		t.Fatalf("requeue answered %v, want one row moved", moved)
	}

	status, body, _ := f.call(t, request{
		method: http.MethodPost, path: "/api/storage/backends/" + to["id"].(string) + "/requeue",
		query: "kind=footage",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("an unknown kind answered %d %v, want 400", status, body)
	}
}

func TestStorageQueuePaginates(t *testing.T) {
	f := newStorageFixture(t)
	backend := f.createFS(t, "queued")
	id, _ := backend["id"].(string)

	for i := 0; i < 5; i++ {
		if err := f.h.St.StorageJobs.Enqueue(store.JobInput{
			ID:   fmt.Sprintf("3f2b1c00-0000-4000-8000-00000000000%d", i+1),
			Kind: models.JobKindSegment, Target: "/var/lib/nvr/segments/x.mp4",
			BackendID: id, Action: models.JobActionUpload,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, page, _ := f.call(t, request{method: http.MethodGet, path: "/api/storage/queue", query: "limit=2"})
	jobs, _ := page["jobs"].([]any)
	if len(jobs) != 2 {
		t.Fatalf("limit=2 answered %d rows, want 2", len(jobs))
	}
	if page["truncated"] != true {
		t.Error("a full page of a longer queue answered truncated:false")
	}
	if page["limit"] != float64(2) || page["offset"] != float64(0) {
		t.Errorf("the page did not echo its window: %v/%v", page["limit"], page["offset"])
	}

	// The totals are the whole queue, not the page: the queue page's headline
	// number is "what is stuck".
	counts, _ := page["counts"].(map[string]any)
	if counts == nil || counts["pending"] != float64(5) {
		t.Errorf("counts is %v, want five pending", page["counts"])
	}

	_, last, _ := f.call(t, request{method: http.MethodGet, path: "/api/storage/queue", query: "limit=2&offset=4"})
	lastJobs, _ := last["jobs"].([]any)
	if len(lastJobs) != 1 {
		t.Fatalf("offset=4 answered %d rows, want the fifth", len(lastJobs))
	}
	if last["truncated"] != false {
		t.Error("the end of the queue answered truncated:true")
	}

	// The pages must not overlap or drop a row.
	_, first, _ := f.call(t, request{method: http.MethodGet, path: "/api/storage/queue", query: "limit=2"})
	seen := map[string]bool{}
	for _, raw := range append(firstJobs(first), lastJobs...) {
		row, _ := raw.(map[string]any)
		id, _ := row["id"].(string)
		if seen[id] {
			t.Fatalf("the pages showed %s twice", id)
		}
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Errorf("the two pages covered %d distinct rows, want 3", len(seen))
	}

	// The backend is named, not just identified: a queue of uuids is a page no
	// operator reads.
	row, _ := firstJobs(first)[0].(map[string]any)
	if row["backend_name"] != "queued" {
		t.Errorf("the row names its backend %v, want %q", row["backend_name"], "queued")
	}
}

func firstJobs(page map[string]any) []any {
	jobs, _ := page["jobs"].([]any)
	return jobs
}

func TestStorageQueueRejectsAStateThatIsNotOneOfFour(t *testing.T) {
	f := newStorageFixture(t)

	status, body, _ := f.call(t, request{method: http.MethodGet, path: "/api/storage/queue", query: "state=stuck"})
	if status != http.StatusBadRequest {
		t.Fatalf("an unknown state answered %d %v, want 400", status, body)
	}

	for _, state := range []string{"pending", "uploading", "uploaded", "failed"} {
		code, page, _ := f.call(t, request{method: http.MethodGet, path: "/api/storage/queue", query: "state=" + state})
		if code != http.StatusOK {
			t.Fatalf("state=%s answered %d %v, want 200", state, code, page)
		}
		if page["state"] != state {
			t.Errorf("state=%s was not echoed: %v", state, page["state"])
		}
	}

	for _, query := range []string{"limit=0", "limit=-3", "limit=abc", "offset=-1"} {
		code, _, _ := f.call(t, request{method: http.MethodGet, path: "/api/storage/queue", query: query})
		if code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", query, code)
		}
	}
}

func TestStorageQueueShowsAFailedJobWithItsReason(t *testing.T) {
	// The queue page is where a failed upload is discovered, so the row has to
	// carry the reason and the state has to be reachable by the filter.
	f := newStorageFixture(t)
	backend := f.createFS(t, "failing")
	id, _ := backend["id"].(string)

	if err := f.h.St.StorageJobs.Enqueue(store.JobInput{
		ID: "3f2b1c00-0000-4000-8000-0000000000aa", Kind: models.JobKindSegment,
		Target: "/var/lib/nvr/segments/gone.mp4", BackendID: id, Action: models.JobActionUpload,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.St.StorageJobs.MarkFailed("3f2b1c00-0000-4000-8000-0000000000aa", "the local file is gone"); err != nil {
		t.Fatal(err)
	}

	_, page, _ := f.call(t, request{method: http.MethodGet, path: "/api/storage/queue", query: "state=failed"})
	jobs, _ := page["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("state=failed showed %d rows, want 1", len(jobs))
	}
	row, _ := jobs[0].(map[string]any)
	if row["last_error"] != "the local file is gone" {
		t.Errorf("the failed row carries %v, want the reason", row["last_error"])
	}
	if row["state"] != models.JobStateFailed {
		t.Errorf("the row says state=%v", row["state"])
	}
	counts, _ := page["counts"].(map[string]any)
	if counts["failed"] != float64(1) {
		t.Errorf("counts says %v, want one failed", counts)
	}
}

func TestDeleteBackendReportsTheQueueRowsItOrphans(t *testing.T) {
	f := newStorageFixture(t)
	created := f.createFS(t, "removed")
	id, _ := created["id"].(string)

	if err := f.h.St.StorageJobs.Enqueue(store.JobInput{
		ID: "3f2b1c00-0000-4000-8000-0000000000bb", Kind: models.JobKindSegment,
		Target: "/var/lib/nvr/segments/x.mp4", BackendID: id, Action: models.JobActionUpload,
	}); err != nil {
		t.Fatal(err)
	}

	_, body, _ := f.call(t, request{method: http.MethodDelete, path: "/api/storage/backends/" + id})
	if body["deleted"] != "removed" {
		t.Fatalf("the delete answered %v", body)
	}
	if body["orphaned_jobs"] != float64(1) {
		t.Errorf("the delete reported %v orphaned queue rows, want 1: the rows stay so the operator can see why nothing is moving", body["orphaned_jobs"])
	}

	_, gone, _ := f.call(t, request{method: http.MethodPost, path: "/api/storage/backends/" + id + "/test"})
	if _, ok := gone["ok"]; ok {
		t.Error("a deleted backend still answers the test route")
	}
}
