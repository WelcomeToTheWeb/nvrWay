package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"nvr/server/config"
	"nvr/server/models"
	"nvr/server/sidecar"
)

// S3-07 — the restart half of a camera write.
//
// go2rtc instantiates its streams at startup (measured on 1.9.15, S3-05/S3-08),
// so the config write is the durable half and the recreate is the live half. What
// the suite measures is the contract between them: which writes restart, in what
// order, and what the console is told when the recreate fails.

// restartFake records every restart, and records what the sidecar had been told at
// the moment of the restart — the order assertion needs a witness inside the call,
// because "apply then restart" and "restart then apply" produce the same counts
// and opposite deployments.
type restartFake struct {
	mu      sync.Mutex
	media   *mediaFake
	calls   int
	desired [][]string
	applies []int
	err     error
	missing []string
	live    []string
}

func (f *restartFake) Restart(_ context.Context, desired []string) (sidecar.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.desired = append(f.desired, desired)
	if f.media != nil {
		f.applies = append(f.applies, f.media.configCount())
	}
	report := sidecar.Report{
		Restarted: f.err == nil,
		Desired:   desired,
		Live:      f.live,
		Missing:   f.missing,
		Took:      "1ms",
	}
	if f.err != nil {
		return report, f.err
	}
	return report, nil
}

func (f *restartFake) snapshot() (calls int, desired [][]string, applies []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.desired, f.applies
}

func (f *restartFake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.desired, f.applies = 0, nil, nil
}

// TestRestartCountsPerWriteKind is the card's first criterion as a count: create,
// update and delete restart once each; the probe and the zone write restart none.
func TestRestartCountsPerWriteKind(t *testing.T) {
	fx := newCameraFixture(t)
	fake := &restartFake{media: fx.Media, live: []string{"front-door"}}
	fx.Handlers.Restarter = fake

	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	if calls, _, _ := fake.snapshot(); calls != 1 {
		t.Fatalf("create restarts exactly once: %d", calls)
	}

	status, _, raw := do(t, fx.Router, request{
		method: http.MethodPut, path: "/api/v1/cameras/" + id,
		body: map[string]any{"name": "Front Door West"}, bearer: fx.OperatorTok,
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %s", status, raw)
	}
	if calls, _, _ := fake.snapshot(); calls != 2 {
		t.Fatalf("update restarts exactly once more: %d", calls)
	}

	// The probe reads the sidecar and changes nothing: restarting for it would
	// drop every Live view for a request whose whole job is to look.
	status, _, raw = do(t, fx.Router, request{method: http.MethodPost, path: "/api/v1/cameras/" + id + "/probe", bearer: fx.OperatorTok})
	if status != http.StatusOK {
		t.Fatalf("probe: %d %s", status, raw)
	}
	status, _, raw = do(t, fx.Router, request{
		method: http.MethodPut, path: "/api/v1/cameras/" + id + "/zones",
		body: []map[string]any{zoneBody("z1")}, bearer: fx.OperatorTok,
	})
	if status != http.StatusOK {
		t.Fatalf("zones: %d %s", status, raw)
	}
	if calls, _, _ := fake.snapshot(); calls != 2 {
		t.Fatalf("the probe and the zone write restart nothing: %d", calls)
	}

	// A refused write restarts nothing: the viewer who cannot add a camera must
	// not be able to drop every Live view in the building by knocking on the door.
	fake.reset()
	status, _, _ = do(t, fx.Router, request{
		method: http.MethodPost, path: "/api/v1/cameras",
		body: cameraBody("viewer-tries", "rtsp://cam:554/live"), bearer: fx.ViewerTok,
	})
	if status != http.StatusForbidden {
		t.Fatalf("the viewer's create is refused: %d", status)
	}
	if calls, _, _ := fake.snapshot(); calls != 0 {
		t.Fatalf("a refused write restarts nothing: %d", calls)
	}

	// Delete restarts once: the removed camera's stream must stop being something
	// the sidecar boots from.
	status, _, raw = do(t, fx.Router, request{method: http.MethodDelete, path: "/api/v1/cameras/" + id, bearer: fx.AdminTok})
	if status != http.StatusOK {
		t.Fatalf("delete: %d %s", status, raw)
	}
	if calls, _, _ := fake.snapshot(); calls != 1 {
		t.Fatalf("delete restarts exactly once: %d", calls)
	}
}

// TestRestartSeesTheConfigAlreadyWritten is the order assertion. Recreating the
// sidecar with an old file boots the old camera list, so the restart must observe
// the config write behind it.
func TestRestartSeesTheConfigAlreadyWritten(t *testing.T) {
	fx := newCameraFixture(t)
	fake := &restartFake{media: fx.Media, live: []string{"front-door"}}
	fx.Handlers.Restarter = fake

	createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	calls, desired, applies := fake.snapshot()
	if calls != 1 {
		t.Fatalf("one restart: %d", calls)
	}
	if applies[0] < 1 {
		t.Fatalf("the config write happens before the restart: applies seen at restart = %d", applies[0])
	}
	if len(desired[0]) == 0 || desired[0][0] != "front-door" {
		t.Fatalf("the restart names the streams the file declares: %v", desired[0])
	}
	body := fx.Media.lastConfigBody()
	if !strings.Contains(body, "front-door") {
		t.Fatalf("the file the restart boots contains the camera: %s", body)
	}
}

// TestRestartNamesTheDetectionTapToo: the detection taps are streams in the same
// file, and a restart that reports only the primary streams reports a camera
// whose detection is silently absent.
func TestRestartNamesTheDetectionTapToo(t *testing.T) {
	fx := newCameraFixture(t)
	fake := &restartFake{media: fx.Media}
	fx.Handlers.Restarter = fake

	body := cameraBody("front-door", "rtsp://cam:554/live")
	body["detection_enabled"] = true
	createdCameraID(t, fx, body, fx.OperatorTok)

	_, desired, _ := fake.snapshot()
	if len(desired[0]) != 2 {
		t.Fatalf("the camera and its detection tap: %v", desired[0])
	}
	if desired[0][1] != "front-door_det" {
		t.Fatalf("the tap is named by its stream name: %v", desired[0])
	}
}

// TestRestartFailureKeepsTheRowAndNamesItself: the rows landed and the sidecar
// took the config; the recreate is what failed. The camera must still be there,
// and the answer must say which of the two facts the operator has.
func TestRestartFailureKeepsTheRowAndNamesItself(t *testing.T) {
	fx := newCameraFixture(t)
	fake := &restartFake{media: fx.Media, err: errors.New("compose exited 1: no such service go2rtc")}
	fx.Handlers.Restarter = fake

	status, out, raw := do(t, fx.Router, request{
		method: http.MethodPost, path: "/api/v1/cameras",
		body: cameraBody("front-door", "rtsp://cam:554/live"), bearer: fx.OperatorTok,
	})
	if status != http.StatusBadGateway {
		t.Fatalf("a write whose sidecar did not come back is not a success: %d %s", status, raw)
	}
	if out["config_applied"] != true {
		t.Fatalf("the config did apply — say so, do not blame it: %v", out)
	}
	if out["sidecar_restarted"] != false {
		t.Fatalf("the restart is the fact that failed: %v", out)
	}
	if !strings.Contains(fmt.Sprint(out["error"]), "sidecar not restarted") {
		t.Fatalf("the body names the repair: %v", out)
	}
	if !strings.Contains(fmt.Sprint(out["error"]), "no such service go2rtc") {
		t.Fatalf("the command's own words travel to the operator: %v", out)
	}

	// The row is not rolled back for a restart failure: the recorder polls rows,
	// and the next write repairs the sidecar.
	_, view, _ := do(t, fx.Router, request{method: http.MethodGet, path: "/api/v1/cameras", bearer: fx.OperatorTok})
	cameras, _ := view["cameras"].([]any)
	found := false
	for _, c := range cameras {
		row := c.(map[string]any)
		if row["slug"] == "front-door" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the camera row survives the restart failure: %v", view)
	}

	rows := auditRows(t, fx.Store, models.AuditCameraCreate)
	if len(rows) == 0 {
		t.Fatal("the write that half-succeeded is in the ledger")
	}
}

// TestBootRestartsOnlyWhenStreamsAreMissing: the boot applies the rendered file,
// and recreates the sidecar only when its live map is missing configured cameras.
// A boot that force-recreates the media sidecar every time drops the Live view of
// every viewer who just connected.
func TestBootRestartsOnlyWhenStreamsAreMissing(t *testing.T) {
	fx := newCameraFixture(t)
	fake := &restartFake{media: fx.Media, live: []string{"front-door"}}
	fx.Handlers.Restarter = fake

	createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	fake.reset()

	// The sidecar answers with nothing: the file and the live map disagree.
	fx.Media.setStreams(`{}`)
	report, err := fx.Handlers.BootstrapGo2rtc(context.Background())
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if !report.Applied {
		t.Fatalf("the boot applied the config: %+v", report)
	}
	calls, _, _ := fake.snapshot()
	if calls != 1 {
		t.Fatalf("a live map missing the configured camera restarts the sidecar once: %d", calls)
	}
	if !report.Restarted {
		t.Fatalf("the boot says it restarted the sidecar: %+v", report)
	}

	// The sidecar answers with the camera in it: nothing to repair, nothing to
	// restart, no Live views dropped.
	fake.reset()
	// Both streams the rendered file declares, tap included: the live map has to
	// match the file, not merely contain the camera.
	fx.Media.setStreams(`{"front-door": {"producers": ["rtsp://cam:554/live"]}, "front-door_det": {"producers": ["ffmpeg:front-door#video=mjpeg"]}}`)
	report, err = fx.Handlers.BootstrapGo2rtc(context.Background())
	if err != nil {
		t.Fatalf("boot 2: %v", err)
	}
	calls, _, _ = fake.snapshot()
	if calls != 0 {
		t.Fatalf("a boot that matches restarts nothing: %d (%+v)", calls, report)
	}
	if report.Restarted {
		t.Fatalf("the boot reports what it did: %+v", report)
	}
	if !strings.Contains(report.RestartNote, "already matches") {
		t.Fatalf("the note says why nothing happened: %+v", report)
	}
}

// TestRestartDisabledSaysTheStreamWaitsForTheBoot: an installation with no compose
// project configured has no way to make a write live, and the honest answer is a
// stated state, not a nil interface three files from a panic.
func TestRestartDisabledSaysTheStreamWaitsForTheBoot(t *testing.T) {
	fx := newCameraFixture(t)
	fx.Handlers.Restarter = sidecar.NewRestarter(&config.Config{}, nil)

	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	report := fx.Handlers.LastRestart
	if report.Restarted {
		t.Fatalf("nothing was configured to restart: %+v", report)
	}
	if len(report.Missing) == 0 || report.Missing[0] != "front-door" {
		t.Fatalf("the disabled restarter names the stream that waits for the boot: %+v", report)
	}

	// The write itself stays green: a deployment that cannot restart is a
	// deployment whose camera list still saves.
	status, _, _ := do(t, fx.Router, request{
		method: http.MethodPut, path: "/api/v1/cameras/" + id,
		body: map[string]any{"name": "Front Door West"}, bearer: fx.OperatorTok,
	})
	if status != http.StatusOK {
		t.Fatalf("a disabled restart does not fail the write: %d", status)
	}
}
