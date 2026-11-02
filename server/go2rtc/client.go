// Package go2rtc is the control plane's client for the go2rtc sidecar: the
// stream registry, the config file, the snapshot tap, and the SDP probe.
//
// OurWay has no equivalent because it has no media sidecar. This package is
// written against go2rtc's own API surface, checked against the upstream source
// on 2026-11-02, and three of its facts shape the code below:
//
//   - `DELETE /api/streams` removes the stream named by **src**, not by `name`
//     (`internal/streams/api.go`: `delete(streams, src)`). A delete that carries
//     only `?name=` answers 200 and removes nothing — the worst possible failure
//     for a camera an operator believes they deleted, because the stream keeps
//     pulling a dead RTSP source forever. So DeleteStream sends both pairs: the
//     plan's `name`, and the `src` go2rtc reads.
//   - `POST /api/config` answers **500** for a body that fails YAML validation
//     (`internal/api/config.go`) and **410 Gone** when go2rtc was started without
//     a config file path at all. Both are decisive answers carrying the reason. A
//     retry loop over them turns "this source string is broken" into "go2rtc is
//     down" in the operator's log, so a 4xx, and a 5xx that brought a body, are
//     rejections — only a silent transport failure and a bare 5xx are retried.
//   - The stream map's `producers` / `consumers` entries are objects
//     (`{format, address, codecs}`) in the running server and plain strings in
//     this project's test fake, so the decoder accepts both shapes instead of
//     picking one and breaking the other.
//
// Browsers speak to go2rtc's `/api/ws` directly (S8-05), so `WebRTCOffer` is for
// server-side probing only: v1 carries no media relay, and never will.
package go2rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"nvr/server/config"
)

// The client's shape, fixed by the plan (Step 3): three attempts on a request
// that a retry can fix, 500 ms apart, and a 5 s window in which a config write
// has to be visible in the stream map before it counts as applied.
const (
	DefaultRetries     = 3
	DefaultBackoff     = 500 * time.Millisecond
	DefaultTimeout     = 10 * time.Second
	DefaultConfirmFor  = 5 * time.Second
	DefaultConfirmTick = 250 * time.Millisecond
)

// Read ceilings. go2rtc answers these endpoints with data the server hands to
// browsers and ffmpeg, and an unbounded read of a peer's answer is a memory
// claim with no owner: the stream map is a few kilobytes, a frame is a JPEG.
const (
	maxStreamMapBytes = 4 << 20
	maxFrameBytes     = 8 << 20
	maxSDPBytes       = 1 << 20
	maxQuoteChars     = 300
)

var (
	// ErrGo2rtcUnreachable: no answer, or an answer with nothing in it, after
	// every attempt. This is the "the sidecar is not there" state, and the state
	// boot must log without dying in (S3-05).
	ErrGo2rtcUnreachable = errors.New("go2rtc unreachable")
	// ErrStreamRejected: go2rtc answered, and its answer is no. The quoted body is
	// the whole point of this error — go2rtc's own words are what tell an operator
	// that the RTSP URL is bad, the stream name is bad, or the config file is
	// disabled, and a wrapper that drops them leaves a support question with no
	// answer in it.
	ErrStreamRejected = errors.New("go2rtc rejected the request")
)

// StreamInfo is one go2rtc stream as the control plane needs it: what feeds it,
// and what is watching it. The probe answer (S3-03) is built from Producers, and
// `camera.status`'s `producers` field is these strings.
type StreamInfo struct {
	Producers []string `json:"producers"`
	Consumers []string `json:"consumers"`
}

// Online reports whether the stream is actually being fed. A stream row that
// exists with no producer is a camera whose source is not connecting — which is
// exactly the difference between "configured" and "online" that the tile shows.
func (s StreamInfo) Online() bool { return len(s.Producers) > 0 }

// Client drives one go2rtc sidecar.
type Client struct {
	URL      string
	Username string
	Password string
	HTTP     *http.Client

	retries     int
	backoff     time.Duration
	confirmFor  time.Duration
	confirmTick time.Duration
	sleep       func(time.Duration)
	now         func() time.Time
}

// NewClient builds the client from configuration. go2rtc has no auth by default,
// so empty credentials mean "send no Authorization header" rather than "send an
// empty one" — the latter is what turns a working anonymous sidecar into a 400.
func NewClient(cfg *config.Config) *Client {
	return &Client{
		URL:         cfg.Go2rtcURL,
		Username:    cfg.Go2rtcUsername,
		Password:    cfg.Go2rtcPassword,
		HTTP:        &http.Client{Timeout: DefaultTimeout},
		retries:     DefaultRetries,
		backoff:     DefaultBackoff,
		confirmFor:  DefaultConfirmFor,
		confirmTick: DefaultConfirmTick,
		sleep:       time.Sleep,
		now:         time.Now,
	}
}

// SetClock replaces the clock the confirm window is measured on, so a test that
// wants an expired window says so with a duration instead of sleeping through it.
func (c *Client) SetClock(now func() time.Time) { c.now = now }

// SetSleep replaces the wait between attempts. The suite asserts the retry
// policy — attempts and the 500 ms spacing — without spending a second per case.
func (c *Client) SetSleep(sleep func(time.Duration)) { c.sleep = sleep }

// Streams reads the whole stream map: `GET /api/streams`.
//
// The map is the only place the server can learn what go2rtc is really running,
// which is why the config write (ApplyConfig) and the probe (S3-03) both end
// here.
func (c *Client) Streams(ctx context.Context) (map[string]StreamInfo, error) {
	streams, err := c.streams(ctx, true)
	if err != nil {
		return nil, err
	}
	return streams, nil
}

// streams fetches the map, with the retry policy as a parameter: the confirm
// window polls, and a polling loop that retries inside itself multiplies
// DefaultRetries by the number of polls.
func (c *Client) streams(ctx context.Context, retry bool) (map[string]StreamInfo, error) {
	resp, err := c.send(ctx, http.MethodGet, "/api/streams", nil, "", nil, retry)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStreamMapBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the stream map: %v", ErrGo2rtcUnreachable, err)
	}
	return parseStreamMap(body)
}

// PutStream registers (or replaces) one stream: `PUT /api/streams?src=…&name=…`.
//
// go2rtc reads `name` and `src` here, and `name` defaults to `src` when it is
// absent — which is how a source URL ends up as a stream name in a system whose
// contract is that the slug *is* the stream name. Both pairs are always sent.
func (c *Client) PutStream(ctx context.Context, name, src string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: PUT /api/streams needs a stream name", ErrStreamRejected)
	}
	if strings.TrimSpace(src) == "" {
		return fmt.Errorf("%w: PUT /api/streams needs a source", ErrStreamRejected)
	}
	query := url.Values{"src": []string{src}, "name": []string{name}}
	resp, err := c.send(ctx, http.MethodPut, "/api/streams", query, "", nil, true)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// DeleteStream removes one stream: `DELETE /api/streams?name=…&src=…`.
//
// Both pairs, for the reason in the package comment: go2rtc deletes by `src`, and
// a request naming only `name` succeeds without deleting.
func (c *Client) DeleteStream(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: DELETE /api/streams needs a stream name", ErrStreamRejected)
	}
	query := url.Values{"src": []string{name}, "name": []string{name}}
	resp, err := c.send(ctx, http.MethodDelete, "/api/streams", query, "", nil, true)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ApplyConfig replaces go2rtc's whole config file and waits for the change to be
// real: `POST /api/config` with the rendered YAML, then the stream map answers
// within the confirm window.
//
// The confirm is not decoration. `POST /api/config` writes the file and answers;
// go2rtc re-reads it and re-creates the streams, and a config that go2rtc cannot
// apply is a camera row an operator saved while the sidecar kept serving the
// previous file. S3-05's rule — every camera write re-renders the config, so
// config cannot drift from rows — is only true if a write that go2rtc ignored is
// reported as a failure.
//
// The whole-file POST is deliberate: `PATCH` merges, and a merge is how a deleted
// camera survives in the file, which is precisely the drift S3-05 forbids.
func (c *Client) ApplyConfig(ctx context.Context, yml []byte) error {
	if len(strings.TrimSpace(string(yml))) == 0 {
		// This is a whole-file replacement: an empty body is not "no changes", it
		// is a config file with no streams in it, and every camera in the
		// installation disappears from the sidecar. A renderer bug must not be
		// able to do that to a running system.
		return fmt.Errorf("%w: refusing to apply an empty go2rtc config", ErrStreamRejected)
	}
	resp, err := c.send(ctx, http.MethodPost, "/api/config", nil, "application/yaml", yml, true)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return c.confirmConfig(ctx)
}

// confirmConfig polls the stream map until it answers, within the window.
func (c *Client) confirmConfig(ctx context.Context) error {
	deadline := c.now().Add(c.confirmFor)
	var lastErr error
	for {
		if _, err := c.streams(ctx, false); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if !c.now().Before(deadline) {
			return fmt.Errorf("%w: config written, go2rtc's stream map did not answer within %s: %v",
				ErrGo2rtcUnreachable, c.confirmFor, lastErr)
		}
		c.sleep(c.confirmTick)
	}
}

// Frame pulls one JPEG from a source: `GET /api/frame.jpeg?src=…&width=…&cache=…`.
//
// This is the snapshot tap for the zone editor's background and for event
// snapshots (S3-03, S6-02). `cache` is go2rtc's own freshness window: with it, a
// frame is reused while it is that old, so a console polling snapshots cannot
// pin a camera's slow producer open.
func (c *Client) Frame(ctx context.Context, src string, width int, cache string) ([]byte, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("%w: GET /api/frame.jpeg needs a source", ErrStreamRejected)
	}
	query := url.Values{"src": []string{src}}
	if width > 0 {
		query.Set("width", strconv.Itoa(width))
	}
	if strings.TrimSpace(cache) != "" {
		query.Set("cache", strings.TrimSpace(cache))
	}
	resp, err := c.send(ctx, http.MethodGet, "/api/frame.jpeg", query, "", nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	frame, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the frame from %s: %v", ErrGo2rtcUnreachable, src, err)
	}
	if len(frame) == 0 {
		// go2rtc answers 200 with an empty body when nothing is producing. That is
		// not a JPEG, and handing it to the console as one is how a broken camera
		// becomes a broken image tag.
		return nil, fmt.Errorf("%w: %s answered no frame data (no producer is feeding it)",
			ErrStreamRejected, src)
	}
	return frame, nil
}

// WebRTCOffer exchanges SDP with go2rtc for a source: `POST /api/webrtc?src=…`.
//
// Server-side probing only — the console connects to go2rtc itself (S8-05). It is
// deliberately the one call with **no retries**: every offer opens a media session
// on the sidecar, so a retry policy here buys nothing and pays for it in
// connections nobody is watching.
func (c *Client) WebRTCOffer(ctx context.Context, src, offer string) (string, error) {
	if strings.TrimSpace(src) == "" {
		return "", fmt.Errorf("%w: POST /api/webrtc needs a source", ErrStreamRejected)
	}
	if strings.TrimSpace(offer) == "" {
		return "", fmt.Errorf("%w: POST /api/webrtc needs an offer", ErrStreamRejected)
	}
	query := url.Values{"src": []string{src}}
	resp, err := c.send(ctx, http.MethodPost, "/api/webrtc", query, "application/sdp", []byte(offer), false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxSDPBytes))
	if err != nil {
		return "", fmt.Errorf("%w: reading the SDP answer: %v", ErrGo2rtcUnreachable, err)
	}
	if len(strings.TrimSpace(string(answer))) == 0 {
		return "", fmt.Errorf("%w: %s answered an empty SDP answer", ErrStreamRejected, src)
	}
	return string(answer), nil
}

// send is the one HTTP path: endpoint, credentials, retries and the two answers.
//
// `retry` marks the requests a retry can fix — reads, and the idempotent writes
// (a whole-file config POST with the same body is the same file, so a second
// attempt after a dropped connection is safe). An answer that says no is not a
// transport failure, and re-asking a server that has already answered is how a
// 500 ms backoff turns one bad write into four.
func (c *Client) send(ctx context.Context, method, path string, query url.Values,
	contentType string, body []byte, retry bool) (*http.Response, error) {

	target := c.endpoint(path, query)
	attempts := 1
	if retry {
		attempts = c.retries + 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			c.sleep(c.backoff)
		}

		var reader *strings.Reader
		if body == nil {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(string(body))
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, fmt.Errorf("%w: %s %s: %v", ErrGo2rtcUnreachable, method, target, err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if c.Username != "" {
			req.SetBasicAuth(c.Username, c.Password)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			// The caller's deadline is the caller's: a context that is gone is not
			// a fault to retry, and a retry loop over a canceled context spends
			// the request budget after the request is already dead.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%w: %s %s: %v", ErrGo2rtcUnreachable, method, target, err)
			}
			continue
		}

		if resp.StatusCode >= 500 {
			quote := drain(resp)
			if strings.TrimSpace(quote) == "" {
				// A 5xx with nothing in it is the service falling over, which a
				// retry can outlive.
				lastErr = fmt.Errorf("%s answered %d", target, resp.StatusCode)
				continue
			}
			// A 5xx with the reason in it is an answer, not a collapse: go2rtc's
			// config endpoint validates YAML and returns the parse error as a 500.
			return nil, rejected(method, target, resp.StatusCode, quote)
		}
		if resp.StatusCode >= 400 {
			quote := drain(resp)
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				// Credentials, not a request go2rtc dislikes: the operator's
				// GO2RTC_USERNAME/GO2RTC_PASSWORD do not match the sidecar's
				// api.username/api.password. Named as unreachable because that is
				// the state the health check and the boot log have to report.
				return nil, fmt.Errorf("%w: %s answered %d — go2rtc refused the credentials"+
					" (GO2RTC_USERNAME/GO2RTC_PASSWORD vs the sidecar's api.username/api.password)",
					ErrGo2rtcUnreachable, target, resp.StatusCode)
			}
			return nil, rejected(method, target, resp.StatusCode, quote)
		}
		return resp, nil
	}

	return nil, fmt.Errorf("%w: %s %s: %v (after %d attempts)",
		ErrGo2rtcUnreachable, method, target, lastErr, attempts)
}

// endpoint joins the configured base URL, the path and the query pairs.
func (c *Client) endpoint(path string, query url.Values) string {
	target := strings.TrimRight(c.URL, "/") + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return target
}

// rejected quotes go2rtc's own words, bounded: the body is the diagnosis, and an
// operator's log is not a place to paste a megabyte of HTML from a proxy.
func rejected(method, target string, status int, quote string) error {
	quote = strings.TrimSpace(quote)
	if quote == "" {
		quote = "no body"
	}
	if len(quote) > maxQuoteChars {
		quote = quote[:maxQuoteChars] + "…"
	}
	return fmt.Errorf("%w: %s %s answered %d: %s", ErrStreamRejected, method, target, status, quote)
}

// drain reads a short answer body and closes the response.
func drain(resp *http.Response) string {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<15))
	if err != nil {
		return ""
	}
	return string(body)
}

// parseStreamMap decodes `GET /api/streams`.
//
// go2rtc answers a map keyed by stream name, and the same endpoint answers
// server metadata in the same object (`host`, `rtsp`, `webrtc`, `version`), so
// membership is decided by shape: an entry counts as a stream when it is an
// object carrying stream fields — `format`, `address`, `producers` or
// `consumers` — or an empty object, which is what an unfed stream marshals to.
// Anything else is the sidecar describing itself.
//
// The map is returned complete, including streams with no producers: a camera
// whose source is down is present-and-offline, and "absent" is reserved for the
// config not containing it at all, because S3-05's boot check asks which of the
// two happened.
func parseStreamMap(body []byte) (map[string]StreamInfo, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("%w: the stream map is not a JSON object: %v", ErrGo2rtcUnreachable, err)
	}
	if top == nil {
		return nil, fmt.Errorf("%w: the stream map is not a JSON object", ErrGo2rtcUnreachable)
	}

	// A `streams` member wrapping the map is the shape this project's contract
	// documents; go2rtc itself answers the map at the top level. Accepting both
	// keeps one decoder for the fake and the sidecar.
	if wrapped, ok := top["streams"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(wrapped, &inner); err == nil && len(inner) > 0 {
			top = inner
		}
	}

	streams := map[string]StreamInfo{}
	for name, raw := range top {
		info, ok := streamEntry(raw)
		if !ok {
			continue
		}
		streams[name] = info
	}
	return streams, nil
}

// streamEntry decodes one entry, and reports whether it is a stream at all.
func streamEntry(raw json.RawMessage) (StreamInfo, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return StreamInfo{}, false // a string, a number, a null: metadata
	}
	if fields == nil {
		return StreamInfo{}, false
	}
	_, hasFormat := fields["format"]
	_, hasAddress := fields["address"]
	producers, hasProducers := fields["producers"]
	consumers, hasConsumers := fields["consumers"]
	if len(fields) > 0 && !hasFormat && !hasAddress && !hasProducers && !hasConsumers {
		return StreamInfo{}, false // e.g. {"listen": ":8554"} — the rtsp section
	}
	return StreamInfo{
		Producers: endpointLabels(producers, hasProducers),
		Consumers: endpointLabels(consumers, hasConsumers),
	}, true
}

// endpointLabels names the producers/consumers of a stream.
//
// Both shapes go2rtc has ever answered are accepted: the plain strings this
// project's contract uses, and the running server's objects, where `address` is
// the useful label (`rtsp://cam:554/live`, `webrtc:…`) and `format` the fallback
// (`rtsp`, `mp4`). The label is what `camera.status` broadcasts and what the
// probe answers, so it names the peer rather than an internal struct.
func endpointLabels(raw json.RawMessage, present bool) []string {
	if !present || len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	labels := make([]string, 0, len(items))
	for _, item := range items {
		var text string
		if err := json.Unmarshal(item, &text); err == nil {
			labels = append(labels, text)
			continue
		}
		var entry struct {
			Format  string `json:"format"`
			Address string `json:"address"`
		}
		if err := json.Unmarshal(item, &entry); err != nil {
			continue
		}
		label := strings.TrimSpace(entry.Address)
		if label == "" {
			label = strings.TrimSpace(entry.Format)
		}
		if label == "" {
			label = string(item)
		}
		labels = append(labels, label)
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}
