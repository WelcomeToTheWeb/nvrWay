package go2rtc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"nvr/server/config"
)

// The suite drives a fake go2rtc: an httptest server that records every request
// it received. The assertions that matter here are the wire details — the query
// pairs, the config body, the auth header, the attempt count — and a fake that
// records requests is the only way to assert them.

type recorded struct {
	method      string
	path        string
	query       url.Values
	contentType string
	body        string
	authUser    string
	authSet     bool
}

type fake struct {
	server *httptest.Server
	t      *testing.T

	mu       sync.Mutex
	requests []recorded
	// responder decides the answer for a request: method + path -> status + body.
	responder func(rec recorded, n int) (int, string, map[string]string)
}

func newFake(t *testing.T, responder func(rec recorded, n int) (int, string, map[string]string)) *fake {
	t.Helper()
	f := &fake{t: t, responder: responder}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		user, _, set := r.BasicAuth()
		rec := recorded{
			method:      r.Method,
			path:        r.URL.Path,
			query:       r.URL.Query(),
			contentType: r.Header.Get("Content-Type"),
			body:        string(body),
			authUser:    user,
			authSet:     set,
		}
		f.mu.Lock()
		f.requests = append(f.requests, rec)
		n := len(f.requests)
		f.mu.Unlock()

		status, text, headers := f.responder(rec, n)
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(text))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fake) url() string { return f.server.URL }

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fake) at(i int) recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.requests) {
		f.t.Fatalf("no such request: %d of %d recorded", i, len(f.requests))
	}
	return f.requests[i]
}

func (f *fake) all() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

// clientFor is the client under test: pointed at the fake, with the retry spacing
// recorded instead of slept, so the suite asserts the policy in milliseconds
// rather than spending a second per case.
func clientFor(f *fake) (*Client, *[]time.Duration) {
	c := NewClient(&config.Config{Go2rtcURL: f.url()})
	sleeps := &[]time.Duration{}
	c.SetSleep(func(d time.Duration) { *sleeps = append(*sleeps, d) })
	return c, sleeps
}

// always answers one thing to everything.
func always(status int, body string) func(rec recorded, n int) (int, string, map[string]string) {
	return func(rec recorded, n int) (int, string, map[string]string) {
		return status, body, nil
	}
}

func TestStreamsReadsTheStreamMapShapeThePlanNames(t *testing.T) {
	f := newFake(t, always(http.StatusOK, `{
		"front-door": {"producers": ["rtsp://cam/live"], "consumers": ["http"]},
		"back-yard":  {"producers": [], "consumers": []},
		"host": "go2rtc.local:1984",
		"rtsp": {"listen": ":8554"}
	}`))
	c, _ := clientFor(f)

	streams, err := c.Streams(context.Background())
	if err != nil {
		t.Fatalf("Streams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("streams are the streams, not the sidecar's own metadata: %+v", streams)
	}
	front := streams["front-door"]
	if len(front.Producers) != 1 || front.Producers[0] != "rtsp://cam/live" {
		t.Fatalf("producers: %+v", front.Producers)
	}
	if !front.Online() {
		t.Fatal("a stream with a producer is online")
	}
	back, ok := streams["back-yard"]
	if !ok {
		t.Fatal("an unfed stream is present: configured-and-offline is a real state")
	}
	if back.Online() {
		t.Fatal("a stream with no producer is not online")
	}
	if rec := f.at(0); rec.method != http.MethodGet || rec.path != "/api/streams" {
		t.Fatalf("streams call: %s %s", rec.method, rec.path)
	}
}

func TestStreamsReadsTheObjectsTheRunningSidecarAnswers(t *testing.T) {
	// go2rtc marshals producers/consumers as objects with format/address/codecs,
	// not strings. The probe answer and camera.status are built from these, so a
	// decoder that only accepts strings reports every real camera as offline.
	f := newFake(t, always(http.StatusOK, `{
		"front-door": {
			"format": "rtsp",
			"address": "rtsp://cam:554/live",
			"producers": [{"Format":"rtsp","Address":"rtsp://cam:554/live","Codecs":[{"codec":"h264"}]}],
			"consumers": [{"Format":"http","Address":"http://10.0.0.5/console"}]
		}
	}`))
	c, _ := clientFor(f)

	streams, err := c.Streams(context.Background())
	if err != nil {
		t.Fatalf("Streams: %v", err)
	}
	front := streams["front-door"]
	if len(front.Producers) != 1 || front.Producers[0] != "rtsp://cam:554/live" {
		t.Fatalf("an object producer must name its address: %+v", front.Producers)
	}
	if len(front.Consumers) != 1 || front.Consumers[0] != "http://10.0.0.5/console" {
		t.Fatalf("object consumers: %+v", front.Consumers)
	}
}

func TestStreamsAcceptsAWrappedStreamsMember(t *testing.T) {
	f := newFake(t, always(http.StatusOK, `{"streams": {"s1": {"producers": ["a"], "consumers": []}}}`))
	c, _ := clientFor(f)

	streams, err := c.Streams(context.Background())
	if err != nil {
		t.Fatalf("Streams: %v", err)
	}
	if len(streams) != 1 || !streams["s1"].Online() {
		t.Fatalf("wrapped map: %+v", streams)
	}
}

func TestStreamsTreatsAnUnfedStreamObjectAsPresentAndOffline(t *testing.T) {
	// Every field of go2rtc's stream is omitempty: an unfed stream marshals to {}.
	f := newFake(t, always(http.StatusOK, `{"garage": {}}`))
	c, _ := clientFor(f)

	streams, err := c.Streams(context.Background())
	if err != nil {
		t.Fatalf("Streams: %v", err)
	}
	info, ok := streams["garage"]
	if !ok || info.Online() {
		t.Fatalf("an empty stream object is present and offline: %+v", streams)
	}
}

func TestStreamsRejectsANonJSONAnswer(t *testing.T) {
	f := newFake(t, always(http.StatusOK, `<html>proxy said no</html>`))
	c, _ := clientFor(f)

	if _, err := c.Streams(context.Background()); !errors.Is(err, ErrGo2rtcUnreachable) {
		t.Fatalf("a peer answering HTML is not the stream map: %v", err)
	}
}

func TestBasicAuthIsSentOnlyWhenConfigured(t *testing.T) {
	f := newFake(t, always(http.StatusOK, `{}`))

	anonymous, _ := clientFor(f)
	if _, err := anonymous.Streams(context.Background()); err != nil {
		t.Fatalf("anonymous Streams: %v", err)
	}
	if f.at(0).authSet {
		t.Fatal("no credentials configured, no Authorization header: an empty pair is what breaks an anonymous sidecar")
	}

	f2 := newFake(t, always(http.StatusOK, `{}`))
	authed := NewClient(&config.Config{Go2rtcURL: f2.url(), Go2rtcUsername: "admin", Go2rtcPassword: "pass"})
	if _, err := authed.Streams(context.Background()); err != nil {
		t.Fatalf("authed Streams: %v", err)
	}
	if rec := f2.at(0); !rec.authSet || rec.authUser != "admin" {
		t.Fatalf("basic auth from GO2RTC_USERNAME: %+v", rec)
	}
}

func TestPutStreamSendsTheNameAndSourcePairs(t *testing.T) {
	f := newFake(t, always(http.StatusOK, ""))
	c, _ := clientFor(f)

	err := c.PutStream(context.Background(), "front-door", "rtsp://cam:554/live")
	if err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	rec := f.at(0)
	if rec.method != http.MethodPut || rec.path != "/api/streams" {
		t.Fatalf("put: %s %s", rec.method, rec.path)
	}
	if got := rec.query.Get("src"); got != "rtsp://cam:554/live" {
		t.Fatalf("?src= is the source, verbatim: %q", got)
	}
	if got := rec.query.Get("name"); got != "front-door" {
		t.Fatalf("?name= is the slug: %q", got)
	}
	if rec.body != "" {
		t.Fatalf("the source travels in the query, not the body: %q", rec.body)
	}
}

func TestPutStreamRefusesAnEmptySource(t *testing.T) {
	f := newFake(t, always(http.StatusOK, ""))
	c, _ := clientFor(f)

	if err := c.PutStream(context.Background(), "front-door", "  "); !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("an empty source is a bug, not a request: %v", err)
	}
	if f.count() != 0 {
		t.Fatal("nothing was sent")
	}
}

func TestDeleteStreamSendsBothQueryPairs(t *testing.T) {
	// go2rtc's apiStreams deletes delete(streams, src): the pair it reads is src.
	// A delete carrying only ?name= answers 200 and leaves the stream pulling a
	// source the operator believes they removed.
	f := newFake(t, always(http.StatusOK, ""))
	c, _ := clientFor(f)

	if err := c.DeleteStream(context.Background(), "front-door"); err != nil {
		t.Fatalf("DeleteStream: %v", err)
	}
	rec := f.at(0)
	if rec.method != http.MethodDelete || rec.path != "/api/streams" {
		t.Fatalf("delete: %s %s", rec.method, rec.path)
	}
	if got := rec.query.Get("name"); got != "front-door" {
		t.Fatalf("?name=: %q", got)
	}
	if got := rec.query.Get("src"); got != "front-door" {
		t.Fatalf("?src= is the pair go2rtc deletes by: %q", got)
	}
}

func TestApplyConfigPostsTheWholeYAMLAndConfirmsViaStreams(t *testing.T) {
	yml := "api: { listen: \":1984\" }\nstreams:\n  front-door: [\"rtsp://cam:554/live\"]\n"
	f := newFake(t, func(rec recorded, n int) (int, string, map[string]string) {
		if rec.path == "/api/config" {
			return http.StatusOK, "", nil
		}
		return http.StatusOK, `{"front-door": {"producers": ["rtsp://cam:554/live"]}}`, nil
	})
	c, _ := clientFor(f)

	if err := c.ApplyConfig(context.Background(), []byte(yml)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	var config *recorded
	confirmations := 0
	for _, rec := range f.all() {
		if rec.path == "/api/config" {
			config = &rec
		} else {
			confirmations++
		}
	}
	if config == nil {
		t.Fatal("no config write reached the sidecar")
	}
	if config.method != http.MethodPost {
		t.Fatalf("the whole-file write is a POST, not a PATCH merge: %s", config.method)
	}
	if config.body != yml {
		t.Fatalf("the body is the rendered YAML verbatim:\n got %q\nwant %q", config.body, yml)
	}
	if !strings.HasPrefix(config.contentType, "application/yaml") {
		t.Fatalf("config content type: %q", config.contentType)
	}
	if confirmations == 0 {
		t.Fatal("the write is confirmed by reading the stream map back")
	}
}

func TestApplyConfigRefusesAnEmptyBody(t *testing.T) {
	f := newFake(t, always(http.StatusOK, ""))
	c, _ := clientFor(f)

	// The endpoint replaces the whole file: an empty body is not "no change", it
	// is a sidecar with no streams, i.e. every camera in the installation gone.
	if err := c.ApplyConfig(context.Background(), []byte("  \n")); !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("empty config: %v", err)
	}
	if f.count() != 0 {
		t.Fatal("nothing was sent")
	}
}

func TestApplyConfigCountsABodyCarrying500AsARejectionNotAFailureToReach(t *testing.T) {
	// go2rtc's config endpoint validates the YAML and answers the parse error as a
	// 500. Retrying it three times and calling it "unreachable" is how a bad
	// source string is reported as a dead sidecar.
	f := newFake(t, func(rec recorded, n int) (int, string, map[string]string) {
		if rec.path == "/api/config" {
			return http.StatusInternalServerError, "yaml: line 4: did not find expected key", nil
		}
		return http.StatusOK, `{}`, nil
	})
	c, sleeps := clientFor(f)

	err := c.ApplyConfig(context.Background(), []byte("streams: [\n"))
	if !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("a 500 carrying the reason is go2rtc answering: %v", err)
	}
	if !strings.Contains(err.Error(), "did not find expected key") {
		t.Fatalf("the operator needs go2rtc's own words: %v", err)
	}
	if f.count() != 1 {
		t.Fatalf("a decisive answer is not retried, got %d requests", f.count())
	}
	if len(*sleeps) != 0 {
		t.Fatalf("no backoff on a rejection, slept %v", *sleeps)
	}
}

func TestApplyConfigReportsAConfigFileDisabledSidecar(t *testing.T) {
	// go2rtc answers 410 Gone when it was started with no config file path (-c
	// '{inline}' or no file): POST /api/config can never land there, and the
	// operator has to be told that, not that go2rtc is down.
	f := newFake(t, func(rec recorded, n int) (int, string, map[string]string) {
		if rec.path == "/api/config" {
			return http.StatusGone, "", nil
		}
		return http.StatusOK, `{}`, nil
	})
	c, _ := clientFor(f)

	err := c.ApplyConfig(context.Background(), []byte("streams: {}\n"))
	if !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("410 is an answer: %v", err)
	}
	if !strings.Contains(err.Error(), "410") {
		t.Fatalf("the status names the config-file-disabled case: %v", err)
	}
}

func TestApplyConfigFailsWhenTheStreamMapNeverConfirms(t *testing.T) {
	f := newFake(t, func(rec recorded, n int) (int, string, map[string]string) {
		if rec.path == "/api/config" {
			return http.StatusOK, "", nil
		}
		return http.StatusServiceUnavailable, "", nil
	})
	c, _ := clientFor(f)
	c.confirmFor = 0 // the window is closed before the first poll

	err := c.ApplyConfig(context.Background(), []byte("streams: {}\n"))
	if !errors.Is(err, ErrGo2rtcUnreachable) {
		t.Fatalf("an unconfirmed config write is not an applied config: %v", err)
	}
	if !strings.Contains(err.Error(), "did not answer within") {
		t.Fatalf("the message must name the confirm window: %v", err)
	}
}

func TestRetryPolicyIsThreeRetriesAtFiveHundredMilliseconds(t *testing.T) {
	f := newFake(t, always(http.StatusServiceUnavailable, ""))
	c, sleeps := clientFor(f)

	_, err := c.Streams(context.Background())
	if !errors.Is(err, ErrGo2rtcUnreachable) {
		t.Fatalf("bare 5xx after every attempt: %v", err)
	}
	if f.count() != DefaultRetries+1 {
		t.Fatalf("%d attempts = the request plus %d retries, got %d",
			DefaultRetries+1, DefaultRetries, f.count())
	}
	if len(*sleeps) != DefaultRetries {
		t.Fatalf("%d sleeps between %d attempts, got %v", DefaultRetries, DefaultRetries+1, *sleeps)
	}
	for _, d := range *sleeps {
		if d != DefaultBackoff {
			t.Fatalf("backoff is the plan's 500 ms, got %v", d)
		}
	}
	if DefaultBackoff != 500*time.Millisecond {
		t.Fatal("DefaultBackoff is 500 ms")
	}
}

func TestRetryRecoversATransientBare5xx(t *testing.T) {
	f := newFake(t, func(rec recorded, n int) (int, string, map[string]string) {
		if n == 1 {
			return http.StatusServiceUnavailable, "", nil
		}
		return http.StatusOK, `{"front-door": {"producers": ["rtsp://cam/live"]}}`, nil
	})
	c, sleeps := clientFor(f)

	streams, err := c.Streams(context.Background())
	if err != nil {
		t.Fatalf("the second attempt is the answer: %v", err)
	}
	if !streams["front-door"].Online() {
		t.Fatal("streams decoded on the retry")
	}
	if len(*sleeps) != 1 {
		t.Fatalf("one backoff for one retry, got %v", *sleeps)
	}
}

func TestTransportFailureIsUnreachableAfterTheRetries(t *testing.T) {
	f := newFake(t, always(http.StatusOK, `{}`))
	f.server.Close() // the sidecar is gone
	c, sleeps := clientFor(f)

	_, err := c.Streams(context.Background())
	if !errors.Is(err, ErrGo2rtcUnreachable) {
		t.Fatalf("nothing listening is unreachable: %v", err)
	}
	if len(*sleeps) != DefaultRetries {
		t.Fatalf("a connection refused is retried on the same policy, got %v", *sleeps)
	}
}

func TestCanceledContextStopsTheRetryLoop(t *testing.T) {
	f := newFake(t, always(http.StatusServiceUnavailable, ""))
	c, sleeps := clientFor(f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Streams(ctx)
	if !errors.Is(err, ErrGo2rtcUnreachable) {
		t.Fatalf("a dead context is unreachable: %v", err)
	}
	if f.count() > 1 {
		t.Fatalf("no retries after the caller is gone, got %d requests", f.count())
	}
	if len(*sleeps) != 0 {
		t.Fatalf("no backoff to spend, got %v", *sleeps)
	}
}

func TestRefusedCredentialsAreNamedAsUnreachable(t *testing.T) {
	f := newFake(t, always(http.StatusUnauthorized, "Unauthorized"))
	c, _ := clientFor(f)
	c.Username = "admin"
	c.Password = "wrong"

	_, err := c.Streams(context.Background())
	if !errors.Is(err, ErrGo2rtcUnreachable) {
		t.Fatalf("a 401 is 'the sidecar is not usable': %v", err)
	}
	if !strings.Contains(err.Error(), "refused the credentials") {
		t.Fatalf("say which knob to turn: %v", err)
	}
	if f.count() != 1 {
		t.Fatalf("the credentials will not start matching on the third try, got %d", f.count())
	}
}

func TestRejectedAnswerQuotesTheGo2rtcBody(t *testing.T) {
	f := newFake(t, always(http.StatusNotFound, "stream not found"))
	c, _ := clientFor(f)

	_, err := c.Frame(context.Background(), "gone-door", 640, "5s")
	if !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("404 is go2rtc saying no: %v", err)
	}
	if !strings.Contains(err.Error(), "stream not found") {
		t.Fatalf("the body is the diagnosis: %v", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("the status belongs in the message: %v", err)
	}
	if f.count() != 1 {
		t.Fatalf("a refusal is not retried, got %d", f.count())
	}
}

func TestRejectedAnswerBoundsTheQuotedBody(t *testing.T) {
	f := newFake(t, always(http.StatusNotFound, strings.Repeat("x", 10_000)))
	c, _ := clientFor(f)

	_, err := c.Frame(context.Background(), "gone-door", 0, "")
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if len(err.Error()) > 600 {
		t.Fatalf("a proxy's HTML page does not belong in the log: %d chars", len(err.Error()))
	}
}

func TestFrameSendsSourceWidthAndCache(t *testing.T) {
	f := newFake(t, always(http.StatusOK, "\xff\xd8\xff\xd9"))
	c, _ := clientFor(f)

	frame, err := c.Frame(context.Background(), "front-door", 640, "5s")
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	if string(frame) != "\xff\xd8\xff\xd9" {
		t.Fatalf("the JPEG bytes are the answer: %q", frame)
	}
	rec := f.at(0)
	if rec.method != http.MethodGet || rec.path != "/api/frame.jpeg" {
		t.Fatalf("frame: %s %s", rec.method, rec.path)
	}
	if rec.query.Get("src") != "front-door" || rec.query.Get("width") != "640" || rec.query.Get("cache") != "5s" {
		t.Fatalf("frame query pairs: %v", rec.query)
	}
}

func TestFrameOmitsWidthAndCacheWhenUnset(t *testing.T) {
	f := newFake(t, always(http.StatusOK, "\xff\xd8\xff\xd9"))
	c, _ := clientFor(f)

	if _, err := c.Frame(context.Background(), "front-door", 0, ""); err != nil {
		t.Fatalf("Frame: %v", err)
	}
	query := f.at(0).query
	if _, present := query["width"]; present {
		t.Fatalf("width 0 means 'ask for the native size': %v", query)
	}
	if _, present := query["cache"]; present {
		t.Fatalf("no cache requested, no cache pair: %v", query)
	}
}

func TestFrameReportsAnEmptyAnswerAsNoProducer(t *testing.T) {
	// go2rtc answers 200 with an empty body when nothing is feeding the source.
	// Handing that to the console as a JPEG is how a dead camera becomes a broken
	// image tag with no explanation.
	f := newFake(t, always(http.StatusOK, ""))
	c, _ := clientFor(f)

	_, err := c.Frame(context.Background(), "front-door", 640, "5s")
	if !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("an empty frame is not a snapshot: %v", err)
	}
	if !strings.Contains(err.Error(), "no producer") {
		t.Fatalf("say why: %v", err)
	}
}

func TestWebRTCOfferExchangesSDP(t *testing.T) {
	offer := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\n"
	answer := "v=0\r\no=go2rtc 2 2 IN IP4 127.0.0.1\r\n"
	// WHEP (application/sdp) answers 201 Created; raw SDP answers 200. Any 2xx is
	// the answer.
	f := newFake(t, func(rec recorded, n int) (int, string, map[string]string) {
		return http.StatusCreated, answer, map[string]string{"Content-Type": "application/sdp"}
	})
	c, _ := clientFor(f)

	got, err := c.WebRTCOffer(context.Background(), "front-door", offer)
	if err != nil {
		t.Fatalf("WebRTCOffer: %v", err)
	}
	if got != answer {
		t.Fatalf("the answer is the SDP body: %q", got)
	}
	rec := f.at(0)
	if rec.method != http.MethodPost || rec.path != "/api/webrtc" {
		t.Fatalf("offer: %s %s", rec.method, rec.path)
	}
	if rec.query.Get("src") != "front-door" {
		t.Fatalf("?src= names the stream: %v", rec.query)
	}
	if rec.body != offer {
		t.Fatalf("the offer is the body: %q", rec.body)
	}
}

func TestWebRTCOfferIsNeverRetried(t *testing.T) {
	// Every offer opens a media session on the sidecar. Retrying one buys no
	// answer and pays for it in connections nobody is watching.
	f := newFake(t, always(http.StatusInternalServerError, "no codecs"))
	c, sleeps := clientFor(f)

	_, err := c.WebRTCOffer(context.Background(), "front-door", "v=0\r\n")
	if !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("a 500 with the reason is an answer: %v", err)
	}
	if f.count() != 1 {
		t.Fatalf("an offer is sent once, got %d", f.count())
	}
	if len(*sleeps) != 0 {
		t.Fatalf("no backoff on an offer, got %v", *sleeps)
	}
}

func TestClientUsesTheConfiguredBaseURL(t *testing.T) {
	f := newFake(t, always(http.StatusOK, `{}`))
	c := NewClient(&config.Config{Go2rtcURL: f.url() + "/"})

	if _, err := c.Streams(context.Background()); err != nil {
		t.Fatalf("Streams: %v", err)
	}
	if got := f.at(0).path; got != "/api/streams" {
		t.Fatalf("a trailing slash on GO2RTC_URL must not become //api: %q", got)
	}
}
