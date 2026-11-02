package sidecar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nvr/server/config"
	"nvr/server/go2rtc"
)

// The compose restart is one command run on the operator's machine, and the suite
// runs it against a shell shim that records its arguments. That is deliberate:
// the assertion that matters is the exact command line — `--no-deps`,
// `--force-recreate`, the one service name — because the failure it prevents is
// the app recreating the stack it lives inside.

// composeShim writes a shell script that records every argument of every
// invocation to a file, and exits with `exitCode`. Its stdout is what the
// restarter is expected to carry into its error, so the failure case prints the
// words real compose prints.
func composeShim(t *testing.T, body string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args.txt")
	bin = filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" + fmt.Sprintf("printf '%%s\\n' \"$@\" >> %q\n", argsFile) + body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the compose shim: %v", err)
	}
	return bin, argsFile
}

func readArgs(t *testing.T, argsFile string) string {
	t.Helper()
	raw, err := os.ReadFile(argsFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the recorded arguments: %v", err)
	}
	return strings.Join(strings.Fields(string(raw)), " ")
}

// streamsFake is the sidecar's live map: the same endpoint the probe uses.
// streamsFake answers `body`, refusing the first `failFirst` requests: the API
// answering and the streams being instantiated are two events, and the suite has
// to be able to stand in for the gap between them. The counter is per HTTP
// request, not per client call, because the client's own retries consume answers.
func streamsFake(t *testing.T, failFirst int, body string) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/streams" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		n := call
		call++
		mu.Unlock()
		if n < failFirst {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &call
}

func clientFor(url string) *go2rtc.Client {
	c := go2rtc.NewClient(&config.Config{Go2rtcURL: url})
	c.SetSleep(func(time.Duration) {})
	return c
}

func noSleep(time.Duration) {}

func TestRestartRunsTheComposeRecreateAndReportsLive(t *testing.T) {
	bin, argsFile := composeShim(t, "")
	server, _ := streamsFake(t, 0, `{"front-door": {"producers": [{"url":"rtsp://cam:554/live"}]}, "front-door_det": {"producers": [{"url":"ffmpeg:front-door"}]}}`)

	c := &Compose{
		ComposeFile: filepath.Join(filepath.Dir(bin), "compose.yml"),
		ProjectDir:  filepath.Dir(bin),
		Bin:         bin,
		Sleep:       noSleep,
		Settle:      time.Second,
		client:      clientFor(server.URL),
	}

	report, err := c.Restart(context.Background(), []string{"front-door", "front-door_det"})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !report.Restarted {
		t.Fatalf("the recreate happened: %+v", report)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("both streams came up: %+v", report)
	}
	if len(report.Live) != 2 {
		t.Fatalf("the report names the live streams: %+v", report)
	}

	args := readArgs(t, argsFile)
	want := "compose --project-directory " + filepath.Dir(bin) + " -f " + filepath.Join(filepath.Dir(bin), "compose.yml") + " up -d --no-deps --force-recreate go2rtc"
	if args != want {
		t.Fatalf("the command line is exactly the one the operator would run:\n got %q\nwant %q", args, want)
	}
}

// TestRestartRecreatesOneServiceAndItsDependenciesStayUp: `--no-deps` is the whole
// reason this is safe to run from inside the app. A recreate that drags the
// manager, the recorder and the media tier down with the sidecar is an outage
// caused by saving a camera.
func TestRestartRecreatesOneServiceAndItsDependenciesStayUp(t *testing.T) {
	bin, argsFile := composeShim(t, "")
	server, _ := streamsFake(t, 0, `{"a": {"producers": [{"url":"rtsp://a"}]}}`)
	c := &Compose{ComposeFile: "compose.yml", Bin: bin, Sleep: noSleep, client: clientFor(server.URL)}

	if _, err := c.Restart(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	args := readArgs(t, argsFile)
	if !strings.Contains(args, "--no-deps") {
		t.Fatalf("--no-deps is what keeps the stack up: %q", args)
	}
	if !strings.Contains(args, "--force-recreate") {
		t.Fatalf("a plain up -d on an unchanged service does nothing: %q", args)
	}
	if strings.Contains(args, "manager") || strings.Contains(args, "recorder") {
		t.Fatalf("exactly one service is named: %q", args)
	}
	if !strings.HasSuffix(args, " go2rtc") {
		t.Fatalf("the default service is the plan's compose name: %q", args)
	}
}

// TestRestartReportsTheStreamsThatDidNotComeUp: go2rtc boots a stream whose source
// is broken into silence. "the API is green and the tile is red" is the manager's
// error class, and a report that names the absent stream is what closes it.
func TestRestartReportsTheStreamsThatDidNotComeUp(t *testing.T) {
	bin, _ := composeShim(t, "")
	server, _ := streamsFake(t, 0, `{"front-door": {"producers": [{"url":"rtsp://cam:554/live"}]}}`)
	c := &Compose{
		ComposeFile: "compose.yml", Bin: bin, Sleep: noSleep,
		ProbeBudget: 300 * time.Millisecond,
		client:      clientFor(server.URL),
	}

	report, err := c.Restart(context.Background(), []string{"front-door", "broken-cam"})
	if err != nil {
		t.Fatalf("an absent stream is reported, not a write failure: %v", err)
	}
	if len(report.Missing) != 1 || report.Missing[0] != "broken-cam" {
		t.Fatalf("the absent stream is named: %+v", report)
	}
	if len(report.Live) != 1 || report.Live[0] != "front-door" {
		t.Fatalf("the live one is named too: %+v", report)
	}
}

// TestRestartFailureCarriesTheCommandsOwnWords: compose's own sentence is the
// diagnosis ("no such service"), and dropping it turns a 502 into a shrug.
func TestRestartFailureCarriesTheCommandsOwnWords(t *testing.T) {
	bin, argsFile := composeShim(t, `echo "Service go2rtc is not running" >&2`+"\n"+`exit 17`)
	server, calls := streamsFake(t, 0, `{"front-door": {"producers": [{"url":"rtsp://cam:554/live"}]}}`)
	c := &Compose{ComposeFile: "compose.yml", Bin: bin, Sleep: noSleep, client: clientFor(server.URL)}

	report, err := c.Restart(context.Background(), []string{"front-door"})
	if err == nil {
		t.Fatal("a compose failure is an error")
	}
	message := err.Error()
	if !strings.Contains(message, "Service go2rtc is not running") {
		t.Fatalf("the command's words travel to the operator: %q", message)
	}
	if !strings.Contains(message, "exit status 17") {
		t.Fatalf("the exit code travels too: %q", message)
	}
	if report.Restarted {
		t.Fatalf("nothing was restarted: %+v", report)
	}
	if len(report.Missing) != 1 || report.Missing[0] != "front-door" {
		t.Fatalf("the desired streams are reported as not live: %+v", report)
	}
	if *calls != 0 {
		t.Fatalf("a failed recreate does not probe: the sidecar was never touched")
	}
	if readArgs(t, argsFile) == "" {
		t.Fatal("the command ran, and is recorded")
	}
}

// TestRestartWaitsForTheSidecarToAnswer: the API answering and the streams being
// instantiated are two events, and the recreate has to be measured against the
// second. The suite measures the wait with an injected clock, as every sleep in
// this repo is measured.
func TestRestartWaitsForTheSidecarToAnswer(t *testing.T) {
	bin, _ := composeShim(t, "")
	// "not yet" four times — one client call burns its own retries — then streams.
	server, _ := streamsFake(t, 4, `{"front-door": {"producers": [{"url":"rtsp://cam:554/live"}]}}`)

	slept := 0
	c := &Compose{
		ComposeFile: "compose.yml", Bin: bin,
		Sleep:       func(d time.Duration) { slept++ },
		Settle:      time.Second,
		ProbeBudget: 10 * time.Second,
		client:      clientFor(server.URL),
	}

	report, err := c.Restart(context.Background(), []string{"front-door"})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if len(report.Live) != 1 {
		t.Fatalf("the streams were found after the wait: %+v", report)
	}
	if slept < 2 {
		t.Fatalf("the settle and at least one probe retry waited: slept=%d", slept)
	}
}

// TestRestartGivesUpOnASidecarThatNeverAnswers, naming the outage: a recreate that
// comes back dead is a deploy problem, and the write's answer must say so.
func TestRestartGivesUpOnASidecarThatNeverAnswers(t *testing.T) {
	bin, _ := composeShim(t, "")
	server, _ := streamsFake(t, 1000, "")
	c := &Compose{
		ComposeFile: "compose.yml", Bin: bin, Sleep: noSleep,
		ProbeBudget: 200 * time.Millisecond,
		client:      clientFor(server.URL),
	}

	report, err := c.Restart(context.Background(), []string{"front-door"})
	if err == nil {
		t.Fatalf("a sidecar that never answers is an error: %+v", report)
	}
	if !strings.Contains(err.Error(), "did not answer after the recreate") {
		t.Fatalf("the error names the outage: %v", err)
	}
	if !report.Restarted {
		t.Fatalf("the recreate did happen: %+v", report)
	}
}

// TestRestartDisabledWithoutComposeFile: no compose project is a state, not a nil.
func TestRestartDisabledWithoutComposeFile(t *testing.T) {
	restarter := NewRestarter(&config.Config{}, nil)
	noop, ok := restarter.(*Noop)
	if !ok {
		t.Fatalf("an installation with no compose project gets the stated disabled state, got %T", restarter)
	}
	if !strings.Contains(noop.Reason, "no compose project configured") {
		t.Fatalf("the reason is in words the console can print: %q", noop.Reason)
	}

	report, err := noop.Restart(context.Background(), []string{"front-door"})
	if err != nil {
		t.Fatalf("disabled is not a failure: %v", err)
	}
	if report.Restarted {
		t.Fatalf("nothing was restarted: %+v", report)
	}
	if len(report.Missing) != 1 || report.Missing[0] != "front-door" {
		t.Fatalf("the stream waits for the next boot, and says so: %+v", report)
	}

	if got := NewRestarter(&config.Config{Go2rtcComposeFile: "compose.yml"}, nil); got == nil {
		t.Fatal("a configured installation never returns a nil Restarter")
	} else if _, isCompose := got.(*Compose); !isCompose {
		t.Fatalf("a configured installation restarts: %T", got)
	}
}

// TestRestartIsConfigurableForTheOperatorWhoDoesNotRunDockerCompose: the plugin and
// the standalone binary are different programs, and the service name is the
// operator's to choose.
func TestRestartIsConfigurableForTheOperatorWhoDoesNotRunDockerCompose(t *testing.T) {
	bin, argsFile := composeShim(t, "")
	server, _ := streamsFake(t, 0, `{"a": {"producers": [{"url":"rtsp://a"}]}}`)
	c := &Compose{
		ComposeFile: filepath.Join(filepath.Dir(bin), "go2rtc.yml"),
		ProjectDir:  filepath.Dir(bin),
		Service:     "media-sidecar",
		Bin:         bin,
		Sleep:       noSleep,
		client:      clientFor(server.URL),
	}
	if _, err := c.Restart(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	args := readArgs(t, argsFile)
	if !strings.HasSuffix(args, " media-sidecar") {
		t.Fatalf("the service name is configured, not hard-coded: %q", args)
	}
	if !strings.Contains(args, "-f "+filepath.Join(filepath.Dir(bin), "go2rtc.yml")) {
		t.Fatalf("the compose file is configured, not guessed: %q", args)
	}
}

// TestNewRestarterReadsTheConfigFields keeps the config surface honest: the
// restart is configured by the four settings docs/configuration.md names.
func TestNewRestarterReadsTheConfigFields(t *testing.T) {
	cfg := &config.Config{
		Go2rtcComposeFile:    "/etc/nvrway/compose.yml",
		Go2rtcComposeDir:     "/etc/nvrway",
		Go2rtcComposeService: "go2rtc-sidecar",
		Go2rtcComposeBin:     "docker",
		Go2rtcRestartTimeout: 45 * time.Second,
		Go2rtcRestartSettle:  3 * time.Second,
	}
	restarter, ok := NewRestarter(cfg, nil).(*Compose)
	if !ok {
		t.Fatal("a configured compose project yields the compose restarter")
	}
	if restarter.ComposeFile != cfg.Go2rtcComposeFile || restarter.ProjectDir != cfg.Go2rtcComposeDir {
		t.Fatalf("paths come from config: %+v", restarter)
	}
	if restarter.ServiceName() != "go2rtc-sidecar" {
		t.Fatalf("the service comes from config: %q", restarter.ServiceName())
	}
	if restarter.timeout() != 45*time.Second || restarter.settle() != 3*time.Second {
		t.Fatalf("the budgets come from config: %+v", restarter)
	}
	if restarter.budget() <= 0 {
		t.Fatalf("an unset probe budget has a default: %+v", restarter)
	}
}
