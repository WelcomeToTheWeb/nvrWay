// Package sidecar restarts the go2rtc container, and reports what came up.
//
// Why this package exists at all is a fact measured against a live sidecar on
// 2026-11-02 (S3-05, S3-08): on go2rtc 1.9.15 a config write is durable and not
// live. `POST /api/config` rewrites the file and validates it; the streams in it
// are instantiated at **startup**, so a stream the server just added is absent
// from `GET /api/streams` until the sidecar boots from that file. No endpoint
// starts a stream at runtime — `PUT/POST /api/streams` write the file without
// starting it, `api/frame`/`api/stream.mp4` answer 404 for a config-only stream,
// and the sidecar does not watch the file. A restart makes the live map exactly
// the file the server wrote.
//
// The mechanism is the manager's decision of 2026-11-02: `docker compose up -d
// --no-deps --force-recreate <service>`. It is the plan's §4.4 rule with one
// narrow exception — the app may recreate the go2rtc service of its own compose
// project — so this package is deliberately not a Docker client. One command,
// one service, paths from configuration, and a disabled state that is a
// first-class answer rather than a nil check at every call site.
package sidecar

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"nvr/server/config"
	"nvr/server/go2rtc"
)

// Restarter makes the sidecar's live stream set match the config file the server
// just wrote. A NoopRestarter says honestly that it will not: the write is
// durable, and the stream is live at the sidecar's next boot.
type Restarter interface {
	Restart(ctx context.Context, desired []string) (Report, error)
}

// Report names the streams the sidecar came up with. The manager's error class is
// "the API is green and the tile is red": a restart that reports nothing leaves
// that gap exactly where it was.
type Report struct {
	Restarted bool     `json:"restarted"`
	Desired   []string `json:"desired"`
	Live      []string `json:"live"`
	Missing   []string `json:"missing"`
	Took      string   `json:"took"`
}

// Compose is the restart mechanism: `docker compose -f <file> up -d --no-deps
// --force-recreate <service>`, then a probe of the sidecar's live stream map.
type Compose struct {
	// ComposeFile is the project file the service lives in. Empty means disabled.
	ComposeFile string
	// ProjectDir is where compose runs, so relative paths inside the project
	// resolve the way the operator's own `docker compose up` resolves them.
	ProjectDir string
	// Service is the compose service name, `go2rtc` in deploy/compose.yml.
	Service string
	// Bin is the compose binary. Configurable because the test suite must be able
	// to prove the argument list without a Docker daemon, and because an operator
	// running the plugin (`docker compose`) and one running standalone
	// (`docker-compose`) are not the same program.
	Bin string
	// Timeout bounds the recreate. Swarm converges; this is a local recreate, and
	// anything past half a minute is a broken deploy, not a slow one.
	Timeout time.Duration
	// Settle is how long to give the sidecar before the first probe: the API
	// answering is not the same as the streams being instantiated.
	Settle time.Duration
	// ProbeBudget bounds the wait for the streams to appear.
	ProbeBudget time.Duration
	// Sleep is injectable for the same reason every sleep in this repo is: the
	// settle is a second long, and a test suite that sleeps twice a second is a
	// suite people skip.
	Sleep func(time.Duration)

	client *go2rtc.Client
}

// DefaultService is the compose service name the plan's deploy layout uses.
const DefaultService = "go2rtc"

// NewRestarter builds the restarter from configuration. A config with no compose
// file returns the Noop, not nil: "the restart is disabled, and here is what the
// console should say about it" is a state the caller has to report, and a nil
// interface is the shape that turns into a nil-pointer panic three files away.
func NewRestarter(cfg *config.Config, client *go2rtc.Client) Restarter {
	if cfg == nil || strings.TrimSpace(cfg.Go2rtcComposeFile) == "" {
		return &Noop{Reason: ErrNotConfigured.Error()}
	}
	return &Compose{
		ComposeFile: cfg.Go2rtcComposeFile,
		ProjectDir:  cfg.Go2rtcComposeDir,
		Service:     strings.TrimSpace(cfg.Go2rtcComposeService),
		Bin:         strings.TrimSpace(cfg.Go2rtcComposeBin),
		Timeout:     cfg.Go2rtcRestartTimeout,
		Settle:      cfg.Go2rtcRestartSettle,
		ProbeBudget: cfg.Go2rtcRestartProbeBudget,
		Sleep:       time.Sleep,
		client:      client,
	}
}

// ServiceName is the compose service to recreate, defaulted so a config that only
// names the compose file works.
func (c *Compose) ServiceName() string {
	if c.Service == "" {
		return DefaultService
	}
	return c.Service
}

func (c *Compose) bin() string {
	if c.Bin == "" {
		return "docker"
	}
	return c.Bin
}

func (c *Compose) timeout() time.Duration {
	if c.Timeout <= 0 {
		return 60 * time.Second
	}
	return c.Timeout
}

func (c *Compose) settle() time.Duration {
	if c.Settle <= 0 {
		return 2 * time.Second
	}
	return c.Settle
}

func (c *Compose) budget() time.Duration {
	if c.ProbeBudget <= 0 {
		return 30 * time.Second
	}
	return c.ProbeBudget
}

func (c *Compose) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Args is the exact command line, kept as a method so the suite can assert it and
// the operator can read what the server is about to run.
//
// `--no-deps` matters: recreating the sidecar must not drag the manager, the
// recorder, the detector and the media tier down with it.
func (c *Compose) Args() []string {
	argv := []string{"compose", "-f", c.ComposeFile, "up", "-d", "--no-deps", "--force-recreate"}
	if c.ProjectDir != "" {
		argv = []string{"compose", "--project-directory", c.ProjectDir, "-f", c.ComposeFile, "up", "-d", "--no-deps", "--force-recreate"}
	}
	return append(argv, c.ServiceName())
}

// Restart recreates the sidecar and reports which of `desired` streams came up.
//
// `desired` is the slug set the config write just put in the file. Missing is not
// an error: go2rtc instantiates a stream lazily per producer in some source types
// (a source that fails to start stays out of the live map), and "the recorder
// will not see this camera" is information the console needs, not a reason to fail
// a write that already succeeded.
func (c *Compose) Restart(ctx context.Context, desired []string) (Report, error) {
	started := time.Now()
	argv := c.Args()
	cmd := exec.CommandContext(ctx, c.bin(), argv...)
	cmd.Dir = c.ProjectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Report{
			Desired: desired,
			Missing: desired,
			Took:    time.Since(started).Round(time.Millisecond).String(),
		}, fmt.Errorf("the sidecar recreate failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	c.sleep(c.settle())
	live, probeErr := c.probe(ctx, desired)
	report := Report{
		Restarted: true,
		Desired:   desired,
		Live:      live,
		Took:      time.Since(started).Round(time.Millisecond).String(),
	}
	missing := make([]string, 0, len(desired))
	for _, slug := range desired {
		if !contains(live, slug) {
			missing = append(missing, slug)
		}
	}
	if len(missing) > 0 {
		report.Missing = missing
	}
	if probeErr != nil && len(desired) > 0 {
		return report, probeErr
	}
	return report, nil
}

// probe waits for the sidecar to answer, then for the desired streams to appear.
// The wait is bounded: a sidecar that never comes up is a deploy problem the
// caller must learn about now, not on the next write.
func (c *Compose) probe(ctx context.Context, desired []string) ([]string, error) {
	if c.client == nil {
		return nil, nil
	}
	deadline := time.Now().Add(c.budget())
	var lastLive []string
	var lastErr error
	for {
		streams, err := c.client.Streams(ctx)
		if err == nil {
			lastLive = nil
			for name := range streams {
				lastLive = append(lastLive, name)
			}
			all := true
			for _, slug := range desired {
				if _, ok := streams[slug]; !ok {
					all = false
				}
			}
			if all || time.Now().After(deadline) {
				return lastLive, nil
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastLive, fmt.Errorf("the sidecar did not answer after the recreate: %w", lastErr)
			}
			return lastLive, nil
		}
		c.sleep(500 * time.Millisecond)
	}
}

// Noop is the disabled state, stated.
type Noop struct {
	Reason string
}

// Restart reports that nothing was restarted, with the reason the console shows.
func (n *Noop) Restart(_ context.Context, desired []string) (Report, error) {
	return Report{Desired: desired, Missing: desired, Took: "0s"}, nil
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// ErrNotConfigured is what a caller gets when a deploy asks for a live restart
// with no compose project named — the message names the setting, because "the
// camera is saved and the stream is not live" is a sentence an operator can only
// act on if the reason is specific.
var ErrNotConfigured = errors.New("no compose project configured for the go2rtc restart")

// ErrRestart marks a restart failure so a caller can tell "the sidecar refused the
// config" from "the sidecar took the config and did not come back". The two are
// different repairs, and a 502 that merges them teaches the operator neither.
var ErrRestart = errors.New("the go2rtc sidecar was not restarted")
