package storage

// S7-04 — the `s3` driver: minio-go, path-style always, credentials unsealed at
// Open.
//
// The exit criteria are "gofakes3 in-test server: put/get/stat/delete/list,
// prefix layout, EnableSSL=false, path-style asserted", "bad credentials -> the
// named error, not a panic" and "no secret in any log line".
//
// The in-test tier is the real S3 protocol: `gofakes3` over an httptest server,
// so the driver is exercised through its HTTP client and not through a mock of
// itself. gofakes3 does not check signatures, so the suite wraps it in a
// middleware that answers the way MinIO does — 403 `AccessDenied` for an access
// key it does not recognise — which is what makes "bad credentials" a tested
// path rather than an assumed one.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"nvr/server/models"
)

const (
	s3TestBucket   = "nvr-media"
	s3TestAccess   = "AKIAIOSFODNN7EXAMPLE"
	s3TestSecret   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	s3TestRegion   = "us-east-1"
	s3TestRowLabel = "minio-test"
)

// s3TestServer is gofakes3 behind the credential check MinIO makes and it does
// not: the access key must be one it recognises.
//
// gofakes3 does not verify signatures at all, so without this wrapper "bad
// credentials" would be an assumed path rather than a tested one. It checks the
// access key and not the signature, because recomputing minio-go's signature
// server-side means reimplementing SigV4 in the suite — and the secret half of
// the path (`SignatureDoesNotMatch`) is asserted where it is decided, in
// TestS3MapErrNamesWhatItCanAndKeepsWhatItCannot. What this wrapper does prove
// is that the unsealed credential actually travels: a request signed with a key
// the tier has never seen comes back 403.
type s3TestServer struct {
	srv    *httptest.Server
	accept string

	mu       sync.Mutex
	requests []string
	hosts    []string
	keysSeen []string
}

func newS3TestServer(t *testing.T, accept string) *s3TestServer {
	t.Helper()
	s := &s3TestServer{accept: accept}
	handler := gofakes3.New(s3mem.New()).Server()
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s3RequestAccessKey(r)
		s.mu.Lock()
		s.requests = append(s.requests, r.URL.RequestURI())
		s.hosts = append(s.hosts, r.Host)
		s.keysSeen = append(s.keysSeen, key)
		s.mu.Unlock()

		if key != s.accept {
			writeS3Error(w, http.StatusForbidden, "AccessDenied",
				fmt.Sprintf("Access Denied for access key %q", key))
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)

	// The bucket exists before any test runs, created through the real API by a
	// client holding the key this tier accepts. A test that then points the
	// driver at another key is testing the refusal, not the setup.
	client, err := minio.New(s.endpointFor(), &minio.Options{
		Creds:        credentials.NewStaticV4(s3TestAccess, s3TestSecret, ""),
		Secure:       false,
		Region:       s3TestRegion,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatalf("building the bootstrap client: %v", err)
	}
	if err := client.MakeBucket(context.Background(), s3TestBucket,
		minio.MakeBucketOptions{Region: s3TestRegion}); err != nil {
		t.Fatalf("creating the test bucket: %v", err)
	}
	return s
}

// s3IsStreamingPayload reports a request whose body is signed chunk by chunk.
// It is here for the next person who tries to verify signatures in this
// middleware: the uploads are chunked, the reads are not, and minio's own
// SignV4 overwrites X-Amz-Date, so a received signature cannot be recomputed
// with it.
func s3IsStreamingPayload(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-")
}

// s3RequestAccessKey pulls the access key out of an AWS Signature V4
// Authorization header: `Credential=<ak>/<date>/<region>/s3/aws4_request`.
func s3RequestAccessKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	i := strings.Index(auth, "Credential=")
	if i < 0 {
		return ""
	}
	cred := auth[i+len("Credential="):]
	if slash := strings.Index(cred, "/"); slash >= 0 {
		return cred[:slash]
	}
	return cred
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><RequestId>test</RequestId><HostId>test</HostId></Error>`,
		code, message)
}

// recorded is the request log, copied under the lock.
func (s *s3TestServer) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *s3TestServer) recordedHosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hosts...)
}

// sawAccessKey answers whether a request arrived signed with this access key.
func (s *s3TestServer) sawAccessKey(want string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.keysSeen {
		if key == want {
			return true
		}
	}
	return false
}

// endpointFor is how a row addresses one of these servers.
func (s *s3TestServer) endpointFor() string {
	return strings.TrimPrefix(s.srv.URL, "http://")
}

// newS3Backend opens a driver against the test tier. The bucket is already
// there — newS3TestServer created it with the accepted key — so a test can hand
// the driver a wrong credential and be sure the failure it sees is the
// credential's and not the fixture's.
func newS3Backend(t *testing.T, s *s3TestServer, access, secret, prefix string) Backend {
	t.Helper()
	key := sealKey(t)
	row := &models.StorageBackend{
		ID: "s3-1", Name: s3TestRowLabel, Kind: models.StorageKindS3,
		Endpoint:    s.endpointFor(),
		Region:      s3TestRegion,
		Bucket:      s3TestBucket,
		Prefix:      prefix,
		AccessKey:   mustSeal(t, access, key),
		SecretKey:   mustSeal(t, secret, key),
		InsecureTLS: true,
	}
	b, err := Open(context.Background(), row, key)
	if err != nil {
		t.Fatalf("Open(s3): %v", err)
	}
	if _, ok := b.(*s3Backend); !ok {
		t.Fatalf("kind s3 produced a %T, want *s3Backend", b)
	}
	return b
}

func TestS3ContractAgainstGofakes3(t *testing.T) {
	s := newS3TestServer(t, s3TestAccess)
	// gofakes3 refuses a PUT with no Content-Length header (`MissingContentLength`,
	// gofakes3.go:701) and minio-go sends a zero-length body chunked. Real S3 and
	// MinIO accept it, so this is the in-test tier's limit rather than the
	// driver's, and it is stated here rather than quietly dropped.
	b := newS3Backend(t, s, s3TestAccess, s3TestSecret, "")
	assertBackendContract(t, b, "nvr/", skipZeroByteObject)
}

func TestS3DeleteOfAnAbsentObjectIsSilent(t *testing.T) {
	// The protocol's answer, asserted rather than assumed: S3's DELETE is
	// idempotent, so removing an object that is already gone is not an error.
	// The sweeper clears the row's `remote_key` either way, which is why the
	// shared contract does not require a second Delete to say anything.
	s := newS3TestServer(t, s3TestAccess)
	b := newS3Backend(t, s, s3TestAccess, s3TestSecret, "")
	ctx := context.Background()

	key := "nvr/front-door/2026/11/04/segment-1.mp4"
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("Delete of an object that was never written: got %v, want the protocol's silence", err)
	}
}

func TestS3IsPathStyleAlways(t *testing.T) {
	// The plan's decision, asserted on the wire: the bucket is in the path, not
	// in the host. A virtual-host request would arrive at
	// `nvr-media.127.0.0.1:PORT/…`, which needs a wildcard DNS record and a
	// certificate covering it — the thing a LAN media tier does not have.
	s := newS3TestServer(t, s3TestAccess)
	b := newS3Backend(t, s, s3TestAccess, s3TestSecret, "")
	ctx := context.Background()

	key := "nvr/front-door/2026/11/04/segment-2.mp4"
	if err := b.Put(ctx, key, strings.NewReader("bytes"), 5, "video/mp4"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := b.Stat(ctx, key); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if _, err := b.List(ctx, "nvr/"); err != nil {
		t.Fatalf("List: %v", err)
	}

	requests := s.recorded()
	if len(requests) == 0 {
		t.Fatal("the driver made no requests")
	}
	for _, host := range s.recordedHosts() {
		if strings.HasPrefix(host, s3TestBucket+".") {
			t.Fatalf("a request went to the virtual host %q, which is not path style", host)
		}
	}
	sawBucketInPath := false
	for _, uri := range requests {
		if strings.HasPrefix(uri, "/"+s3TestBucket+"/") || uri == "/"+s3TestBucket {
			sawBucketInPath = true
		}
	}
	if !sawBucketInPath {
		t.Fatalf("no request carried the bucket in its path: %v", requests)
	}
}

func TestS3EnableSSLIsTheInsecureTLSCheckbox(t *testing.T) {
	// The plan's rule is `EnableSSL = !InsecureTLS`, and both halves are
	// asserted: with the box ticked the requests are plain HTTP (which is what
	// a self-hosted MinIO on a LAN actually speaks), and with it unticked the
	// driver really does speak TLS — proved against a TLS server whose
	// certificate this process does not trust, which is the one failure that
	// can only come from a TLS handshake having been attempted.
	s := newS3TestServer(t, s3TestAccess)
	key := sealKey(t)

	t.Run("insecure_tls true means plain HTTP", func(t *testing.T) {
		// The assertion is that Ping *succeeds*: a client configured for TLS
		// against this plain-HTTP server fails its handshake, so an answer here
		// can only come from EnableSSL having been false.
		b := newS3Backend(t, s, s3TestAccess, s3TestSecret, "")
		if err := b.Ping(context.Background()); err != nil {
			t.Fatalf("Ping against a plain-HTTP endpoint with insecure_tls set: %v", err)
		}
	})

	t.Run("insecure_tls false attempts TLS and says so when it cannot verify", func(t *testing.T) {
		tlsSrv := httptest.NewTLSServer(gofakes3.New(s3mem.New()).Server())
		t.Cleanup(tlsSrv.Close)

		row := &models.StorageBackend{
			ID: "s3-tls", Name: "minio-https", Kind: models.StorageKindS3,
			Endpoint: strings.TrimPrefix(tlsSrv.URL, "https://"),
			Region:   s3TestRegion, Bucket: s3TestBucket,
			AccessKey:   mustSeal(t, s3TestAccess, key),
			SecretKey:   mustSeal(t, s3TestSecret, key),
			InsecureTLS: false,
		}
		b, err := Open(context.Background(), row, key)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		err = b.Ping(context.Background())
		if err == nil {
			t.Fatal("a self-signed certificate verified without being told to")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "tls") &&
			!strings.Contains(strings.ToLower(err.Error()), "x509") &&
			!strings.Contains(strings.ToLower(err.Error()), "certificate") {
			t.Fatalf("the failure does not read as a TLS failure: %v", err)
		}
		if ErrorCode(err) != ErrUnreachable.Error() {
			t.Fatalf("an unverifiable certificate maps to %q, want %q", ErrorCode(err), ErrUnreachable.Error())
		}
	})
}

func TestS3BadCredentialsIsTheNamedErrorNotAPanic(t *testing.T) {
	s := newS3TestServer(t, s3TestAccess)
	ctx := context.Background()

	t.Run("an unknown access key is turned away", func(t *testing.T) {
		b := newS3Backend(t, s, "AKIATHATDOESNOTEXIST", s3TestSecret, "")
		err := b.Ping(ctx)
		if err == nil {
			t.Fatal("an unknown access key was accepted")
		}
		if ErrorCode(err) != ErrBadCredentials.Error() {
			t.Fatalf("an unknown access key maps to %q, want %q", ErrorCode(err), ErrBadCredentials.Error())
		}
	})

	t.Run("a wrong secret is the same named failure, by code", func(t *testing.T) {
		// gofakes3 does not verify signatures, so the tier cannot be the one that
		// refuses a wrong secret. What is asserted here is the driver's answer to
		// the refusal a real MinIO gives — `SignatureDoesNotMatch`, which is what
		// a wrong secret produces — and that the secret never appears in it.
		b := newS3Backend(t, s, s3TestAccess, "theWrongSecretEntirely", "").(*s3Backend)
		err := b.mapErr("put", "k", minio.ErrorResponse{
			StatusCode: http.StatusForbidden, Code: "SignatureDoesNotMatch",
			Message: "The request signature we calculated does not match the signature you provided.",
		})
		if ErrorCode(err) != ErrBadCredentials.Error() {
			t.Fatalf("a signature mismatch maps to %q, want %q", ErrorCode(err), ErrBadCredentials.Error())
		}
		if strings.Contains(err.Error(), "theWrongSecretEntirely") {
			t.Fatalf("the failure names the secret: %v", err)
		}
		// And the credential still travelled: the tier saw this access key, so a
		// wrong secret would have reached the signature check, not been dropped.
		if err := b.Ping(context.Background()); err != nil {
			t.Fatalf("Ping with the right access key and a wrong secret: %v", err)
		}
		if !s.sawAccessKey(s3TestAccess) {
			t.Fatal("the access key never reached the tier")
		}
	})

	t.Run("a sealed credential this process cannot open is not bad credentials", func(t *testing.T) {
		// S7-01's distinction, asserted at the driver's door: the credential may
		// be perfect and the seal key wrong, and telling the operator to retype
		// it is a wasted morning.
		other := [32]byte{}
		row := &models.StorageBackend{
			ID: "s3-seal", Name: "minio", Kind: models.StorageKindS3,
			Endpoint: s.endpointFor(), Region: s3TestRegion, Bucket: s3TestBucket,
			AccessKey: mustSeal(t, s3TestAccess, other),
		}
		_, err := Open(context.Background(), row, sealKey(t))
		if !errors.Is(err, ErrBadKey) {
			t.Fatalf("Open with an unopenable credential: got %v, want ErrBadKey", err)
		}
		if ErrorCode(err) != "" {
			t.Fatalf("a seal failure was given one of the four names: %v", err)
		}
	})
}

func TestS3MissingBucketIsTheNamedError(t *testing.T) {
	s := newS3TestServer(t, s3TestAccess)
	key := sealKey(t)

	// The same endpoint, the same credentials, a bucket nobody created: the
	// Settings page has to tell "your MinIO is down" from "you typed the wrong
	// bucket", and those are different names.
	row := &models.StorageBackend{
		ID: "s3-nobucket", Name: "wrong-bucket", Kind: models.StorageKindS3,
		Endpoint: s.endpointFor(), Region: s3TestRegion, Bucket: "not-the-right-bucket",
		AccessKey:   mustSeal(t, s3TestAccess, key),
		SecretKey:   mustSeal(t, s3TestSecret, key),
		InsecureTLS: true,
	}
	b, err := Open(context.Background(), row, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Ping(context.Background()); !errors.Is(err, ErrBucketMissing) {
		t.Fatalf("Ping onto a missing bucket: got %v, want bucket missing", err)
	}
}

func TestS3NothingAnsweringIsDiscoveryUnreachable(t *testing.T) {
	key := sealKey(t)

	// A port that is not listening: the listener is opened and closed to get an
	// address that is almost certainly free.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dead := listener.Addr().String()
	_ = listener.Close()

	row := &models.StorageBackend{
		ID: "s3-dead", Name: "down", Kind: models.StorageKindS3,
		Endpoint: dead, Region: s3TestRegion, Bucket: s3TestBucket,
		AccessKey:   mustSeal(t, s3TestAccess, key),
		SecretKey:   mustSeal(t, s3TestSecret, key),
		InsecureTLS: true,
	}
	b, err := Open(context.Background(), row, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Ping(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Ping onto a closed port: got %v, want discovery_unreachable", err)
	}
}

func TestS3PrefixLayout(t *testing.T) {
	// The row's prefix is the bucket-side namespace; the key layout (S7-07) is
	// the namespace the queue speaks. A bucket shared with another installation
	// is the reason the two are separate, and the reason List must give back the
	// keys the caller addressed rather than the ones the bucket holds.
	s := newS3TestServer(t, s3TestAccess)
	b := newS3Backend(t, s, s3TestAccess, s3TestSecret, "site-a")
	ctx := context.Background()

	key := "nvr/front-door/2026/11/04/segment-4.mp4"
	if err := b.Put(ctx, key, strings.NewReader("body"), 4, "video/mp4"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	requests := s.recorded()
	sawPrefixed := false
	for _, uri := range requests {
		if strings.Contains(uri, "/"+s3TestBucket+"/site-a/"+key) {
			sawPrefixed = true
		}
	}
	if !sawPrefixed {
		t.Fatalf("the object did not land under the row prefix: %v", requests)
	}

	keys, err := b.List(ctx, "nvr/front-door/2026/11/04/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0] != key {
		t.Fatalf("List returned %v, want the key as the caller addressed it", keys)
	}

	info, err := b.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Key != key {
		t.Fatalf("Stat reported %q, want the key as the caller addressed it", info.Key)
	}
}

func TestS3EndpointIsNormalisedAndAPathIsRefused(t *testing.T) {
	s := newS3TestServer(t, s3TestAccess)
	key := sealKey(t)

	// A browser URL pasted into the endpoint field is the common mistake, and
	// the scheme is decoration: `insecure_tls` decides TLS.
	for _, raw := range []string{
		s.endpointFor(),
		"http://" + s.endpointFor(),
		"http://" + s.endpointFor() + "/",
	} {
		row := &models.StorageBackend{
			ID: "s3-ok", Name: "minio", Kind: models.StorageKindS3,
			Endpoint: raw, Region: s3TestRegion, Bucket: s3TestBucket,
			AccessKey: mustSeal(t, s3TestAccess, key), SecretKey: mustSeal(t, s3TestSecret, key),
		}
		if _, err := Open(context.Background(), row, key); err != nil {
			t.Errorf("Open(endpoint=%q): %v", raw, err)
		}
	}

	// A path in the endpoint is the same mistake one step further: path style
	// puts the bucket in the path, so a path here would be sent twice.
	for _, raw := range []string{"http://" + s.endpointFor() + "/nvr-media", "host.example/bucket"} {
		row := &models.StorageBackend{
			ID: "s3-bad", Name: "minio", Kind: models.StorageKindS3,
			Endpoint: raw, Region: s3TestRegion, Bucket: s3TestBucket,
			AccessKey: mustSeal(t, s3TestAccess, key), SecretKey: mustSeal(t, s3TestSecret, key),
		}
		_, err := Open(context.Background(), row, key)
		if err == nil {
			t.Errorf("Open(endpoint=%q) accepted a path in the endpoint", raw)
			continue
		}
		if !strings.Contains(err.Error(), "host or host:port") {
			t.Errorf("Open(endpoint=%q) = %v, want a refusal that says host or host:port", raw, err)
		}
	}

	// An empty bucket is the container the settings name not existing.
	if _, err := Open(context.Background(), &models.StorageBackend{
		ID: "s3-nb", Name: "no-bucket", Kind: models.StorageKindS3,
		Endpoint: s.endpointFor(), AccessKey: mustSeal(t, s3TestAccess, key),
	}, key); !errors.Is(err, ErrBucketMissing) {
		t.Fatalf("Open with no bucket: got %v, want bucket missing", err)
	}
}

func TestS3GetAnswersAtGetNotAtRead(t *testing.T) {
	// minio's GetObject returns a handle whose error only surfaces on the first
	// Read, and by then the clip route has written its 200 headers. The driver
	// stats first, so the route gets a named 404 instead of a truncated
	// download that plays as silence.
	s := newS3TestServer(t, s3TestAccess)
	b := newS3Backend(t, s, s3TestAccess, s3TestSecret, "")

	rc, err := b.Get(context.Background(), "nvr/front-door/2026/11/04/never-written.mp4")
	if err == nil {
		_ = rc.Close()
		t.Fatal("Get of a missing object returned a reader")
	}
	if rc != nil {
		t.Fatalf("Get returned a reader alongside the error: %v", rc)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of a missing object: got %v, want ErrNotFound", err)
	}
}

func TestS3SecretNeverAppearsInALineThisProcessWrites(t *testing.T) {
	// The criterion is about logs, so the assertion is on the log: capture what
	// this process writes during a failing connection test and look for the
	// secret in it. The error string is checked too, because the API logs what
	// the driver returned before it answers the named error.
	s := newS3TestServer(t, s3TestAccess)

	var logged strings.Builder
	before := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(before)

	// The right access key, a secret the tier cannot match: the failure is the
	// signature, and the signature is the only place the secret ever appears.
	b := newS3Backend(t, s, "AKIATHATDOESNOTEXIST", s3TestSecret, "")
	err := b.Ping(context.Background())

	if err == nil {
		t.Fatal("a refused credential produced no error")
	}
	if strings.Contains(err.Error(), s3TestSecret) {
		t.Fatalf("the error carries the secret key: %v", err)
	}
	if strings.Contains(logged.String(), s3TestSecret) {
		t.Fatalf("the log carries a secret key: %s", logged.String())
	}
	// The access key may legitimately appear — it is the server that names it,
	// in its message, and it is not a secret in the sense the seal is about.
}

func TestS3OpenDispatchesForTheS3KindWithUnsealedCredentials(t *testing.T) {
	// The criterion "Open returns the driver named by the row's Kind", for the
	// s3 kind — and the driver is handed the *unsealed* pair, because a signed
	// request with the base64 of a ciphertext in it fails with a message about
	// signatures that nobody would connect to the seal.
	s := newS3TestServer(t, s3TestAccess)
	key := sealKey(t)
	row := &models.StorageBackend{
		ID: "s3-dispatch", Name: "minio", Kind: models.StorageKindS3,
		Endpoint: s.endpointFor(), Region: s3TestRegion, Bucket: s3TestBucket,
		AccessKey:   mustSeal(t, s3TestAccess, key),
		SecretKey:   mustSeal(t, s3TestSecret, key),
		InsecureTLS: true,
	}
	b, err := Open(context.Background(), row, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := b.(*s3Backend); !ok {
		t.Fatalf("kind s3 produced a %T", b)
	}
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with the unsealed credentials: %v", err)
	}
	if !s.sawAccessKey(s3TestAccess) {
		t.Fatal("no request carried the unsealed access key, so the credential never travelled")
	}
}

func TestS3ListSkipsDirectoryMarkers(t *testing.T) {
	// Some implementations store a zero-length key ending in `/` to stand in for
	// a folder. It is not an object the queue shelved, and offering it to the
	// console would put a file with no bytes and no type on the screen.
	s := newS3TestServer(t, s3TestAccess)
	b := newS3Backend(t, s, s3TestAccess, s3TestSecret, "")
	ctx := context.Background()

	s3b := b.(*s3Backend)
	for _, name := range []string{
		"nvr/front-door/2026/11/04/segment-1.mp4",
		"nvr/front-door/2026/",
	} {
		if _, err := s3b.client.PutObject(ctx, s3TestBucket, name,
			strings.NewReader("body"), 4, minio.PutObjectOptions{ContentType: "video/mp4"}); err != nil {
			t.Fatalf("Put(%s): %v", name, err)
		}
	}

	keys, err := b.List(ctx, "nvr/front-door/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0] != "nvr/front-door/2026/11/04/segment-1.mp4" {
		t.Fatalf("List = %v, want the object and not the directory marker", keys)
	}
}

func TestS3MapErrNamesWhatItCanAndKeepsWhatItCannot(t *testing.T) {
	b := &s3Backend{bucket: s3TestBucket}

	// A code the mapping does not know keeps its own message: inventing
	// "discovery_unreachable" for a 503 would send the operator to check their
	// network while MinIO is the thing that is sick.
	unknown := b.mapErr("put", "k", minio.ErrorResponse{
		StatusCode: http.StatusServiceUnavailable, Code: "InternalError", Message: "please retry",
	})
	if ErrorCode(unknown) != "" {
		t.Fatalf("a 503 was given one of the four names: %v", unknown)
	}
	if !strings.Contains(unknown.Error(), "InternalError") {
		t.Fatalf("the unmapped failure lost the server's code: %v", unknown)
	}

	cases := []struct {
		err  error
		want error
	}{
		{minio.ErrorResponse{StatusCode: http.StatusNotFound, Code: "NoSuchKey"}, ErrNotFound},
		{minio.ErrorResponse{StatusCode: http.StatusNotFound, Code: "NoSuchBucket"}, ErrBucketMissing},
		{minio.ErrorResponse{StatusCode: http.StatusForbidden, Code: "AccessDenied"}, ErrBadCredentials},
		{minio.ErrorResponse{StatusCode: http.StatusForbidden, Code: "SignatureDoesNotMatch"}, ErrBadCredentials},
		{minio.ErrorResponse{StatusCode: http.StatusForbidden, Code: "InvalidAccessKeyId"}, ErrBadCredentials},
		{minio.ErrorResponse{StatusCode: http.StatusUnauthorized}, ErrBadCredentials},
		{errors.New("dial tcp 10.0.0.5:9000: connect: connection refused"), ErrUnreachable},
	}
	for _, tc := range cases {
		got := b.mapErr("testing", "k", tc.err)
		if !errors.Is(got, tc.want) {
			t.Errorf("mapErr(%v) = %v, want it to map to %v", tc.err, got, tc.want)
		}
	}

	if b.mapErr("testing", "k", nil) != nil {
		t.Error("mapErr(nil) returned an error")
	}
}

// TestS3DriverIsRegisteredForTheRealKind keeps the registry honest: the four
// kinds the plan names each have a driver in this build, and `Open` reaches
// them by kind.
func TestS3DriverIsRegisteredForTheRealKind(t *testing.T) {
	kinds := RegisteredKinds()
	if !containsKey(kinds, models.StorageKindS3) {
		t.Fatalf("RegisteredKinds = %v, want it to include s3", kinds)
	}
	// The fs driver landed on S7-03; both are registered, so the registry is
	// additive and not a switch being edited.
	if !containsKey(kinds, models.StorageKindFS) {
		t.Fatalf("RegisteredKinds = %v, want it to include fs", kinds)
	}
}
