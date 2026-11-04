package storage

// The `webdav` driver's tests run against an httptest server that speaks the
// WebDAV subset gowebdav uses — OPTIONS, PROPFIND at Depth 0 and 1, MKCOL, PUT,
// GET, DELETE — behind Basic auth, because that is what a NAS, a Nextcloud and
// a router with a USB disk all speak. It is a protocol-level stand-in, not a
// mock of the driver: the driver's own HTTP client, its own XML parser and its
// own auth negotiation all run.
//
// The server is written to the RFC where the driver depends on the letter:
// MKCOL on a collection that already exists answers 405 (RFC 4918), and PUT
// into a collection that does not exist answers 409. gowebdav answers the first
// by *rewriting it to 201* in `mkcol`, which is what makes its create-parents-
// then-PUT order safe and lets `Put` stream instead of buffering — a fact worth
// a test of its own, because the RFC says otherwise and the next reader will
// trust the RFC.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"nvr/server/models"

	"github.com/studio-b12/gowebdav"
)

const (
	webdavTestUser = "nvr"
	webdavTestPass = "a-share-secret"
	webdavTestBad  = "not-the-password"
)

// webdavNode is one resource on the in-test share. Paths are kept with no
// trailing slash, except the root, which is "/".
type webdavNode struct {
	data        []byte
	modTime     time.Time
	contentType string
	dir         bool
}

// statusRecorder remembers the status a handler wrote, so a test can see the
// tier's answers in order next to the requests that produced them.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func mustPathUnescape(p string) string {
	out, err := url.PathUnescape(p)
	if err != nil {
		return p
	}
	return out
}

type webdavTestServer struct {
	srv  *httptest.Server
	user string
	pass string

	mu    sync.Mutex
	nodes map[string]*webdavNode

	requests []string
	methods  []string
	statuses []string
}

func newWebdavTestServer(t *testing.T, user, pass string) *webdavTestServer {
	t.Helper()
	s := &webdavTestServer{user: user, pass: pass, nodes: map[string]*webdavNode{
		"/": {dir: true, modTime: time.Now().UTC()},
	}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		s.handle(rec, r)
		s.mu.Lock()
		s.statuses = append(s.statuses, fmt.Sprintf("%s %s -> %d", r.Method, webdavClean(mustPathUnescape(r.URL.Path)), rec.code))
		s.mu.Unlock()
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// webdavClean normalises a request path: no doubled slashes, no trailing slash
// except on the root.
func webdavClean(p string) string {
	if p == "" {
		return "/"
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if p != "/" {
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		return "/"
	}
	return p
}

func webdavParent(p string) string {
	if p == "/" {
		return ""
	}
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func (s *webdavTestServer) handle(w http.ResponseWriter, r *http.Request) {
	p, err := url.PathUnescape(r.URL.Path)
	if err != nil {
		p = r.URL.Path
	}
	p = webdavClean(p)

	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+p)
	s.methods = append(s.methods, r.Method)
	s.mu.Unlock()

	// The share is behind Basic auth, and gowebdav learns the scheme from the
	// challenge: its first request carries nothing, and the server names Basic.
	user, password, ok := r.BasicAuth()
	if !ok || user != s.user || password != s.pass {
		w.Header().Set("WWW-Authenticate", `Basic realm="nvr-test"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("DAV", "1,2")
		w.Header().Set("Allow", "OPTIONS, GET, PUT, DELETE, PROPFIND, MKCOL")
		w.WriteHeader(http.StatusOK)
	case "MKCOL":
		s.mkcol(w, p)
	case http.MethodPut:
		s.put(w, r, p)
	case http.MethodGet:
		s.get(w, p)
	case http.MethodDelete:
		s.del(w, p)
	case "PROPFIND":
		s.propfind(w, r, p)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *webdavTestServer) mkcol(w http.ResponseWriter, p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[p]; ok {
		// RFC 4918: MKCOL on an existing resource is 405, not success. The
		// client turns this into 201; see TestWebdavMKCOL405IsNotAFailure.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parent, ok := s.nodes[webdavParent(p)]
	if !ok || !parent.dir {
		// A collection may not be created under a parent that is not there.
		w.WriteHeader(http.StatusConflict)
		return
	}
	s.nodes[p] = &webdavNode{dir: true, modTime: time.Now().UTC()}
	w.WriteHeader(http.StatusCreated)
}

func (s *webdavTestServer) put(w http.ResponseWriter, r *http.Request, p string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, ok := s.nodes[webdavParent(p)]
	if !ok || !parent.dir {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if existing, ok := s.nodes[p]; ok && existing.dir {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ct := contentTypeFor(p)
	s.nodes[p] = &webdavNode{data: body, modTime: time.Now().UTC(), contentType: ct}
	w.WriteHeader(http.StatusCreated)
}

func (s *webdavTestServer) get(w http.ResponseWriter, p string) {
	s.mu.Lock()
	n, ok := s.nodes[p]
	s.mu.Unlock()
	if !ok || n.dir {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", n.contentType)
	w.WriteHeader(http.StatusOK)
	w.Write(n.data)
}

func (s *webdavTestServer) del(w http.ResponseWriter, p string) {
	s.mu.Lock()
	_, ok := s.nodes[p]
	if ok {
		delete(s.nodes, p)
	}
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *webdavTestServer) propfind(w http.ResponseWriter, r *http.Request, p string) {
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "0"
	}
	s.mu.Lock()
	n, ok := s.nodes[p]
	if !ok {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	paths := []string{p}
	if depth == "1" && n.dir {
		for k := range s.nodes {
			if k != "/" && webdavParent(k) == p {
				paths = append(paths, k)
			}
		}
	}
	body := webdavMultistatus(paths, s.nodes)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	w.Write([]byte(body))
}

// webdavMultistatus renders the Multi-Status document gowebdav parses. The
// namespace prefixes are the ones its struct tags name, and the collection
// itself comes first because ReadDir requires the first response to be it.
func webdavMultistatus(paths []string, nodes map[string]*webdavNode) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	b.WriteString(`<d:multistatus xmlns:d="DAV:">`)
	for _, p := range paths {
		n := nodes[p]
		name := p[strings.LastIndex(p, "/")+1:]
		href := p
		if n.dir && href != "/" {
			href += "/"
		}
		b.WriteString(`<d:response><d:href>`)
		b.WriteString((&url.URL{Path: href}).EscapedPath())
		b.WriteString(`</d:href><d:propstat><d:prop>`)
		fmt.Fprintf(&b, "<d:displayname>%s</d:displayname>", name)
		if n.dir {
			b.WriteString(`<d:resourcetype><d:collection xmlns:d="DAV:"/></d:resourcetype>`)
		} else {
			b.WriteString(`<d:resourcetype/>`)
			fmt.Fprintf(&b, "<d:getcontentlength>%d</d:getcontentlength>", len(n.data))
			fmt.Fprintf(&b, "<d:getcontenttype>%s</d:getcontenttype>", n.contentType)
		}
		fmt.Fprintf(&b, "<d:getlastmodified>%s</d:getlastmodified>", n.modTime.UTC().Format(time.RFC1123))
		b.WriteString(`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
	}
	b.WriteString(`</d:multistatus>`)
	return b.String()
}

// mkdir creates a collection and its parents, the way a share would already
// have it before the recorder ever connects.
func (s *webdavTestServer) mkdir(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p = webdavClean(p)
	for p != "/" && p != "" {
		if _, ok := s.nodes[p]; !ok {
			s.nodes[p] = &webdavNode{dir: true, modTime: time.Now().UTC()}
		}
		p = webdavParent(p)
	}
}

func (s *webdavTestServer) saw(method, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := method + " " + webdavClean(path)
	for _, got := range s.requests {
		if got == want {
			return true
		}
	}
	return false
}

func (s *webdavTestServer) sawMethod(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, got := range s.methods {
		if got == method {
			return true
		}
	}
	return false
}

func (s *webdavTestServer) allRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// webdavRow is a backend row for the in-test share. The password is sealed with
// `key`, because that is what a row in the database holds.
func webdavRow(t *testing.T, endpoint, prefix, user, password string, key [32]byte, insecure bool) *models.StorageBackend {
	t.Helper()
	sealed := ""
	if password != "" {
		sealed = mustSeal(t, password, key)
	}
	return &models.StorageBackend{
		Name:        "share",
		Kind:        models.StorageKindWebDAV,
		Endpoint:    endpoint,
		Prefix:      prefix,
		Username:    user,
		Password:    sealed,
		InsecureTLS: insecure,
		Enabled:     true,
	}
}

// webdavOtherKey is a seal key this process does not have, built the way the
// package's other tests build one.
func webdavOtherKey() [32]byte {
	other := [32]byte{}
	other[31] = 0x7f
	return other
}

// newWebdavBackend opens a backend against the in-test share. The password
// defaults to the one the share accepts.
func newWebdavBackend(t *testing.T, s *webdavTestServer, prefix, password string) Backend {
	t.Helper()
	key := sealKey(t)
	if password == "" {
		password = webdavTestPass
	}
	b, err := Open(context.Background(), webdavRow(t, s.srv.URL, prefix, webdavTestUser, password, key, true), key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return b
}

// -- the shared contract --------------------------------------------------

func TestWebDavContractAgainstAnHTTPServer(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "")

	assertBackendContract(t, b, "nvr/")

	// The contract covers the five object operations; Ping is the sixth, and it
	// is asserted here so a driver cannot pass the contract with a probe that
	// never left the process.
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping against a healthy share: %v", err)
	}

	for _, want := range []string{"PUT", "GET", "DELETE", "PROPFIND", "OPTIONS"} {
		if !s.sawMethod(want) {
			t.Errorf("the share never saw a %s, so that operation is not WebDAV here", want)
		}
	}
	// Every operation the contract performed must have stayed at or under the
	// row's prefix. This is asserted structurally rather than against the
	// contract's own key spelling, because the driver's job is to keep requests
	// inside the prefix, not to reproduce a particular date format. `/media`
	// itself is legitimate: it is the prefix, and Ping checks that the
	// collection is there.
	for _, got := range s.allRequests() {
		if strings.HasPrefix(got, "OPTIONS ") {
			continue // Ping probes the share root, which is above the prefix
		}
		path := got[strings.Index(got, " ")+1:]
		if path != "/media" && !strings.HasPrefix(path, "/media/") {
			t.Errorf("the share saw %q, which is outside the row's prefix", got)
		}
	}
	if !s.sawMethod("PUT") {
		t.Error("the contract never reached the share with a PUT")
	}
}

// -- the MKCOL trap -------------------------------------------------------

// TestWebdavMKCOL405IsNotAFailure records the library behaviour that lets Put
// stream. RFC 4918 says MKCOL on a collection that already exists is 405, and
// gowebdav's `mkcol` maps that answer to 201, so creating the parents of an
// object in a directory that is already there is not a failure.
//
// This matters because the alternative reading of the RFC — "streaming writes
// are unsafe, buffer instead" — would have put every uploaded object in memory.
// The test asserts the server really did answer 405 and that the upload still
// succeeded, so the behaviour is pinned from both sides.
func TestWebDavMKCOL405IsNotAFailure(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "")

	// First upload: the day's collection does not exist yet, so it is created.
	if err := b.Put(context.Background(), "2026/11/04/first.mp4", strings.NewReader("body"), 4, "video/mp4"); err != nil {
		t.Fatalf("the upload that made the collection: %v", err)
	}

	// Second upload into the same collection. The client MKCOLs it again, the
	// server answers 405 as the RFC requires, and the upload still lands.
	s.mu.Lock()
	before := len(s.statuses)
	s.mu.Unlock()
	if err := b.Put(context.Background(), "2026/11/04/second.mp4", strings.NewReader("body"), 4, "video/mp4"); err != nil {
		t.Fatalf("an upload into a collection whose MKCOL answers 405: %v", err)
	}

	s.mu.Lock()
	saw405 := false
	for _, line := range s.statuses[before:] {
		if strings.HasPrefix(line, "MKCOL /media/2026/11/04 ") && strings.HasSuffix(line, "-> 405") {
			saw405 = true
		}
	}
	s.mu.Unlock()
	if !saw405 {
		t.Fatal("the server never answered 405 for the collection that already existed, so this test " +
			"proved nothing about that answer; the statuses were: " + fmt.Sprint(s.statuses))
	}
}

// TestWebdavSecondUploadIntoACollection is the same fact from the uploader's
// side: the second segment of a day lands as easily as the first, and a new
// day's directory is created on demand.
func TestWebDavSecondUploadIntoACollection(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "")
	ctx := context.Background()

	keys := []string{"2026/11/04/cam-a-000001.mp4", "2026/11/04/cam-a-000002.mp4", "2026/11/04/cam-a-000003.mp4"}
	for i, key := range keys {
		if err := b.Put(ctx, key, strings.NewReader("body"), 4, "video/mp4"); err != nil {
			t.Fatalf("upload %d of %s: %v", i+1, key, err)
		}
	}
	keys = append(keys, "2026/11/05/cam-a-000001.mp4")
	if err := b.Put(ctx, keys[len(keys)-1], strings.NewReader("body"), 4, "video/mp4"); err != nil {
		t.Fatalf("the first upload into a *new* day: %v", err)
	}

	s.mu.Lock()
	_, made := s.nodes["/media/2026/11/04"]
	s.mu.Unlock()
	if !made {
		t.Fatal("the day's collection was never created")
	}
}

// -- the four names -------------------------------------------------------

func TestWebDavBadCredentialsIsTheNamedErrorNotAPanic(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")

	t.Run("the wrong password", func(t *testing.T) {
		err := newWebdavBackend(t, s, "media", webdavTestBad).Ping(context.Background())
		if err == nil {
			t.Fatal("a wrong password was accepted")
		}
		if ErrorCode(err) != ErrBadCredentials.Error() {
			t.Fatalf("a wrong password maps to %q (%v), want %q", ErrorCode(err), err, ErrBadCredentials.Error())
		}
		if strings.Contains(err.Error(), webdavTestBad) {
			t.Fatalf("the failure names the password: %v", err)
		}
	})

	t.Run("the wrong user", func(t *testing.T) {
		key := sealKey(t)
		row := webdavRow(t, s.srv.URL, "media", "someoneelse", webdavTestPass, key, true)
		b, err := Open(context.Background(), row, key)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := b.Ping(context.Background()); ErrorCode(err) != ErrBadCredentials.Error() {
			t.Fatalf("a wrong user maps to %q, want %q", ErrorCode(err), ErrBadCredentials.Error())
		}
	})

	t.Run("a sealed password this process cannot open is not bad credentials", func(t *testing.T) {
		row := webdavRow(t, s.srv.URL, "media", webdavTestUser, webdavTestPass, sealKey(t), true)
		_, err := Open(context.Background(), row, webdavOtherKey())
		if !errors.Is(err, ErrBadKey) {
			t.Fatalf("an unopenable credential reported as %v, want %v", err, ErrBadKey)
		}
	})
}

func TestWebDavMissingCollectionIsBucketMissing(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	// The share answers; the directory the operator named is not on it.
	err := newWebdavBackend(t, s, "media", "").Ping(context.Background())
	if ErrorCode(err) != ErrBucketMissing.Error() {
		t.Fatalf("a missing collection maps to %q (%v), want %q", ErrorCode(err), err, ErrBucketMissing.Error())
	}
	if !strings.Contains(err.Error(), "media") {
		t.Fatalf("the failure does not name the collection: %v", err)
	}
}

func TestWebDavNothingAnsweringIsDiscoveryUnreachable(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.srv.Close() // the share is off, and the URL still resolves

	err := newWebdavBackend(t, s, "media", "").Ping(context.Background())
	if ErrorCode(err) != ErrUnreachable.Error() {
		t.Fatalf("a dead share maps to %q (%v), want %q", ErrorCode(err), err, ErrUnreachable.Error())
	}
}

func TestWebDavUnverifiableCertificateIsUnreachableAndSaysItWasTLS(t *testing.T) {
	// The same share over TLS with a certificate this process does not trust,
	// and `insecure_tls` unticked.
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.srv.Close()
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	s.mkdir("/media")

	key := sealKey(t)
	row := webdavRow(t, s.srv.URL, "media", webdavTestUser, webdavTestPass, key, false)
	b, err := Open(context.Background(), row, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	err = b.Ping(context.Background())
	if err == nil {
		t.Fatal("a certificate this process does not trust was accepted")
	}
	if !strings.Contains(err.Error(), "x509") && !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("the failure does not read as TLS: %v", err)
	}
	if ErrorCode(err) != ErrUnreachable.Error() {
		t.Fatalf("an unverifiable certificate maps to %q, want %q", ErrorCode(err), ErrUnreachable.Error())
	}

	// And with the box ticked, the same share works — which is the whole point
	// of `insecure_tls` on a NAS with a self-signed certificate.
	if err := newWebdavBackend(t, s, "media", "").Ping(context.Background()); err != nil {
		t.Fatalf("Ping over TLS with insecure_tls set: %v", err)
	}
}

// -- the shape of the tier ------------------------------------------------

func TestWebDavPrefixLayout(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "")

	key := "nvr/front-door/2026/11/04/cam-a-000001.mp4"
	if err := b.Put(context.Background(), key, strings.NewReader("v"), 1, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if !s.saw("PUT", "/media/"+key) {
		t.Fatalf("the object did not land under the prefix; the share saw %v", s.allRequests())
	}
}

func TestWebDavEndpointPathIsPartOfEveryRequest(t *testing.T) {
	// A share mounted under a path — Nextcloud serves at /remote.php/dav — is
	// the common case, and gowebdav appends object paths to the endpoint, so
	// the endpoint's own path must survive into the request.
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.srv.Close()
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/remote.php/dav") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/remote.php/dav")
		s.handle(w, r)
	}))
	t.Cleanup(s.srv.Close)

	key := sealKey(t)
	row := webdavRow(t, s.srv.URL+"/remote.php/dav", "media", webdavTestUser, webdavTestPass, key, true)
	b, err := Open(context.Background(), row, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Put(context.Background(), "x.mp4", strings.NewReader("v"), 1, "video/mp4"); err != nil {
		t.Fatalf("Put through a share mounted under a path: %v", err)
	}
}

func TestWebDavEndpointIsRefusedWhenItIsNotAURL(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		prefix   string
		want     string
	}{
		{"no scheme", "nas.local:5005", "media", "scheme"},
		{"empty", "", "media", "empty"},
		{"not http", "ftp://nas.local/dav", "media", "not http"},
		{"a user in the URL", "http://nvr@nas.local:5005", "media", "username"},
		{"a prefix that repeats the endpoint's path", "http://nas.local:5005/dav", "dav/media", "repeats"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := sealKey(t)
			_, err := Open(context.Background(), webdavRow(t, tc.endpoint, tc.prefix, webdavTestUser, webdavTestPass, key, true), key)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("endpoint %q opened as %v, want a message naming %q", tc.endpoint, err, tc.want)
			}
		})
	}
}

func TestWebDavStatReportsTheServersTypeAndTheExtensionIsTheFallback(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "")
	ctx := context.Background()

	if err := b.Put(ctx, "typed.mp4", strings.NewReader("v"), 1, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	info, err := b.Stat(ctx, "typed.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 1 {
		t.Fatalf("Stat reported size %d, want 1", info.Size)
	}
	// The in-test share derives the type from the name, as Apache, nginx and
	// Nextcloud all do, because gowebdav sends no Content-Type on a PUT.
	if info.ContentType != "video/mp4" {
		t.Fatalf("Stat content type = %q, want the type the share derived", info.ContentType)
	}

	// A share that labels a file something this process would never guess wins:
	// the tier's answer beats a local guess about what the bytes are.
	s.mu.Lock()
	s.nodes["/media/typed.mp4"].contentType = "text/plain"
	s.mu.Unlock()
	info, err = b.Stat(ctx, "typed.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.ContentType != "text/plain" {
		t.Fatalf("the share's type should win, got %q", info.ContentType)
	}

	// And a share that reports no type at all falls back to the name, which is
	// the rule the fs backend follows for the same reason.
	s.mu.Lock()
	s.nodes["/media/typed.mp4"].contentType = ""
	s.mu.Unlock()
	info, err = b.Stat(ctx, "typed.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.ContentType != "video/mp4" {
		t.Fatalf("with no type from the share the extension should decide, got %q", info.ContentType)
	}
}

func TestWebDavListDescendsCollections(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "")
	ctx := context.Background()

	keys := []string{
		"events/7/clip.mp4",
		"events/7/snapshot.jpg",
		"events/9/clip.mp4",
		"2026/11/04/cam-a-000001.mp4",
	}
	for _, k := range keys {
		if err := b.Put(ctx, k, strings.NewReader("x"), 1, "video/mp4"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := b.List(ctx, "events/")
	if err != nil {
		t.Fatalf("List(events/): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List(events/) returned %v, want the three event objects", got)
	}
	for _, want := range []string{"events/7/clip.mp4", "events/7/snapshot.jpg", "events/9/clip.mp4"} {
		if !contains(got, want) {
			t.Errorf("List(events/) is missing %q, got %v", want, got)
		}
	}

	// A prefix naming one directory, and the whole tier.
	got, err = b.List(ctx, "2026/11/04")
	if err != nil {
		t.Fatalf("List(day): %v", err)
	}
	if len(got) != 1 || got[0] != "2026/11/04/cam-a-000001.mp4" {
		t.Fatalf("List(2026/11/04) returned %v", got)
	}
	all, err := b.List(ctx, "")
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(all) != len(keys) {
		t.Fatalf("List() returned %d keys, want %d: %v", len(all), len(keys), all)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1] > all[i] {
			t.Fatalf("List() is not sorted: %v", all)
		}
	}
}

func TestWebDavGetOfAnAbsentObjectNamesTheAbsence(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "")

	rc, err := b.Get(context.Background(), "never-uploaded.mp4")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of an absent object: %v, want %v", err, ErrNotFound)
	}
	if rc != nil {
		t.Fatalf("Get returned a reader alongside its error: %v", rc)
	}
	if ErrorCode(err) == ErrBadCredentials.Error() {
		t.Fatal("an absent object was reported as a credential problem")
	}
	if _, err := b.Stat(context.Background(), "never-uploaded.mp4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat of an absent object: %v, want %v", err, ErrNotFound)
	}
}

func TestWebDavSecondDeleteIsSilent(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "")
	ctx := context.Background()

	if err := b.Put(ctx, "gone.mp4", strings.NewReader("x"), 1, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, "gone.mp4"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// gowebdav answers success for a DELETE that found nothing, as S3 does. The
	// sweeper clears remote_key either way, so this is recorded, not relied on.
	if err := b.Delete(ctx, "gone.mp4"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestWebDavKeyCannotEscapeThePrefix(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "")

	for _, key := range []string{"../secret.txt", "nvr/../../secret.txt", ""} {
		if _, err := b.Get(context.Background(), key); err == nil {
			t.Errorf("Get(%q) was accepted", key)
		} else if ErrorCode(err) != "" {
			t.Errorf("Get(%q) reported %q, want a settings-shaped error rather than a named one", key, ErrorCode(err))
		}
		if err := b.Put(context.Background(), key, strings.NewReader("x"), 1, ""); err == nil {
			t.Errorf("Put(%q) was accepted", key)
		}
	}
	if s.saw("PUT", "/secret.txt") {
		t.Fatal("a key with .. wrote outside the prefix")
	}
}

// -- the mapping, and what it refuses to invent ---------------------------

func TestWebDavMapErrKeepsAnUnrecognisedStatus(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "").(*webdavBackend)

	// 507 Insufficient Storage is a full NAS. It is neither a credential
	// problem nor an unreachable one, so it keeps its number: inventing a name
	// for it sends the operator to the wrong settings field.
	err := b.mapErr("put", "k", gowebdav.NewPathError("Write", "/media/k", 507))
	if ErrorCode(err) != "" {
		t.Fatalf("a full share maps to %q, want no name", ErrorCode(err))
	}
	if !strings.Contains(err.Error(), "507") {
		t.Fatalf("a full share lost its status: %v", err)
	}
}

func TestWebDavSecretNeverAppearsInALineThisProcessWrites(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	// The right credential, and no /media on the share: the failure is the
	// missing collection, and the password belongs nowhere in it.
	b := newWebdavBackend(t, s, "media", "")

	var logged bytes.Buffer
	out := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(out)

	err := b.Ping(context.Background())
	if err == nil {
		t.Fatal("a missing collection produced no error")
	}
	if strings.Contains(err.Error(), webdavTestPass) {
		t.Fatalf("the error carries the password: %v", err)
	}
	if strings.Contains(logged.String(), webdavTestPass) {
		t.Fatalf("the log carries the password: %s", logged.String())
	}
}

// -- dispatch -------------------------------------------------------------

func TestWebDavOpenDispatchesForTheWebdavKindWithUnsealedCredentials(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	key := sealKey(t)

	row := webdavRow(t, s.srv.URL, "media", webdavTestUser, webdavTestPass, key, true)
	row.IsDefault = true
	b, err := Open(context.Background(), row, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping through the dispatched backend: %v", err)
	}
}

func TestWebDavDriverIsRegisteredForTheRealKind(t *testing.T) {
	kinds := RegisteredKinds()
	if !contains(kinds, models.StorageKindWebDAV) {
		t.Fatalf("registered kinds are %v, want %q among them", kinds, models.StorageKindWebDAV)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
