"""S5-09: the per-camera loop, the supervisor, and the env-only entrypoint.

The supervisor's job is reconciliation: the config payload is the truth about which
cameras want detection, and the threads have to follow it without a restart. The
entrypoint's job is to fail loudly on a missing model and to exit 0 on SIGTERM,
because `docker stop` is not a crash.
"""

from __future__ import annotations

import logging
import os
import subprocess
import sys
import threading
import time

import pytest

from nvr_detector import __main__ as entrypoint
from nvr_detector import pipeline as pipeline_module
from nvr_detector.client import ServerClient
from nvr_detector.pipeline import CameraPipeline, Worker

from conftest import (
    FakeClock,
    StubResult,
    StubServer,
    StubSession,
    camera_cfg,
    detection,
    make_frame,
    write_tiny_onnx,
)

DETECTOR_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FRAME = make_frame(width=640, height=360)


class FakeModel:
    """Stands in for `YoloOnnx`: the pipeline only ever calls `run`."""

    def __init__(self, detections=None) -> None:
        self.detections = list(detections) if detections is not None else [detection()]
        self.calls: list[dict] = []

    def run(self, frame, min_confidence=0.45, min_area_ratio=0.004):  # noqa: ANN001, ANN201
        self.calls.append({"min_confidence": min_confidence, "min_area_ratio": min_area_ratio})
        return list(self.detections)


def stub_client(script=None, **kwargs) -> ServerClient:
    """A client that answers the first calls from the script and is a quiet, healthy
    server afterwards — these tests are about the pipeline, not the transport."""
    session = StubSession(script or [], default=StubResult(200, {"accepted": 0, "events": []}))
    return ServerClient("http://server:8080", "tok", session=session, sleep=lambda _s: None, **kwargs)


def pipeline_for(camera=None, model=None, client=None, frames=None, clock=None, **kwargs) -> CameraPipeline:
    return CameraPipeline(
        camera=camera or camera_cfg(),
        model=model or FakeModel(),
        client=client or stub_client(),
        frame_source=lambda _camera: iter(frames if frames is not None else [FRAME]),
        # One second per frame: comfortably slower than any camera's interval, so the
        # pacing never hides what the gate and the model did.
        clock=clock or FakeClock(1000.0, 1.0),
        sleep=lambda _s: None,
        **kwargs,
    )


# -- one frame through the chain ------------------------------------------


def test_a_person_in_a_zone_produces_one_open_signal_in_the_buffer():
    client = stub_client()
    pipeline = pipeline_for(client=client)
    signals = pipeline.process_frame(FRAME, 1000.0)

    assert len(signals) == 1
    assert signals[0]["action"] == "open"
    assert signals[0]["event_key"] == "cam-1:z1:intrusion:1000"
    assert client.buffered() == 1, "the signal waits in the client's buffer, not on the network"


def test_the_camera_floors_reach_the_model():
    """`sensitivity` and `min_area_ratio` are camera columns; the session is shared
    across cameras, so the floors have to travel with the call."""
    camera = camera_cfg(sensitivity=0.6, min_area_ratio=0.02)
    pipeline = pipeline_for(camera=camera)
    pipeline.process_frame(FRAME, 1000.0)
    assert pipeline.model.calls[0] == {"min_confidence": 0.6, "min_area_ratio": 0.02}


def test_the_motion_gate_runs_before_the_model_in_continuous_mode():
    pipeline = pipeline_for(frames=[FRAME, FRAME.copy(), FRAME.copy()])
    pipeline.run(threading.Event())
    assert pipeline.frames_read == 3
    assert pipeline.inference_runs == 1, "two identical frames are two frames nobody inferred on"


def test_the_gate_is_inert_for_an_events_camera():
    """An `events` camera's frames are the trigger. Gating them could drop the one
    frame that opens the recording window, and the operator would get a clip that
    starts a second late."""
    camera = camera_cfg(record_mode="events")
    pipeline = pipeline_for(camera=camera, frames=[FRAME, FRAME.copy(), FRAME.copy()])
    assert pipeline.gate.enabled is False
    pipeline.run(threading.Event())
    assert pipeline.inference_runs == 3


def test_frames_are_paced_at_the_cameras_fps():
    """A 15 fps tap pointed at a 5 fps camera row is three times the inference for no
    additional information, so the row's `fps` is a ceiling the loop honours."""
    pipeline = pipeline_for(frames=[FRAME] * 20, clock=FakeClock(0.0, 0.1, limit=1.9))
    seen: list[float] = []
    original = pipeline.process_frame

    def recording(frame, ts):
        seen.append(ts)
        return original(frame, ts)

    pipeline.process_frame = recording  # type: ignore[method-assign]
    pipeline.run(threading.Event())

    assert len(seen) < 20, "the loop passed frames it did not process"
    assert len(seen) >= 8, "pacing that starves the pipeline is as wrong as pacing that ignores the row"
    gaps = [b - a for a, b in zip(seen, seen[1:])]
    assert all(gap >= 0.2 - 1e-9 for gap in gaps), f"processed faster than the row's 5 fps: {gaps}"


def test_the_loop_closes_what_is_still_open_when_the_frames_end():
    """The frames ending is how a stream dies and how SIGTERM lands. An event left
    open at that moment is an event whose clip nobody assembles."""
    client = stub_client()
    pipeline = pipeline_for(client=client, frames=[FRAME])
    pipeline.run(threading.Event())
    actions = [s["action"] for s in client._buffer]
    assert actions == ["open", "close"]


def test_a_camera_whose_frames_raise_does_not_take_the_worker_down(caplog):
    def exploding_source(_camera):
        yield FRAME
        raise ConnectionError("go2rtc went away")

    pipeline = CameraPipeline(
        camera=camera_cfg(),
        model=FakeModel(),
        client=stub_client(),
        frame_source=exploding_source,
        clock=FakeClock(1000.0, 1.0),
        sleep=lambda _s: None,
    )
    pipeline.run(threading.Event())  # must not raise
    assert "loop died" in caplog.text
    assert pipeline.client._buffer[-1]["action"] == "close", "the event still gets closed"


# -- the supervisor --------------------------------------------------------


def config_payload(*slugs, settings=None) -> dict:
    return {
        "settings": settings or {},
        "cameras": [
            {
                "id": f"cam-{slug}",
                "slug": slug,
                "name": slug.title(),
                "det_stream": f"{slug}_det",
                "fps": 5,
                "width": 640,
                "height": 360,
                "sensitivity": 0.45,
                "min_area_ratio": 0.004,
                "record_mode": "continuous",
                "providers": ["CPUExecutionProvider"],
                "zones": [
                    {
                        "id": "z1",
                        "name": "driveway",
                        "polygon": [[0.0, 0.0], [1.0, 0.0], [1.0, 1.0], [0.0, 1.0]],
                        "classes": [],
                        "rule": "intrusion",
                        "dwell_seconds": 0,
                        "severity": 3,
                        "min_confidence": 0.0,
                        "min_area_ratio": 0.0,
                        "enabled": True,
                    }
                ],
            }
            for slug in slugs
        ],
    }


def worker_for(payload, max_cameras=12, script=None) -> tuple[Worker, ServerClient]:
    """A worker whose first config poll answers `payload`, then a quiet healthy
    server — these tests are about reconciliation, not the transport."""
    client = stub_client([StubResult(200, payload)] + list(script or []))
    worker = Worker(
        client,
        FakeModel(),
        max_cameras=max_cameras,
        frame_source=lambda _camera: iter([FRAME]),
        clock=FakeClock(1000.0, 1.0),
        sleep=lambda _s: None,
    )
    return worker, client


def test_only_the_cameras_in_the_payload_are_watched():
    """The server has already dropped the cameras with detection off or the camera
    inactive, so the payload *is* the set. A worker that watched everything go2rtc
    serves would infer on a camera the operator switched off."""
    worker, client = worker_for(config_payload("porch", "gate"))
    config = client.get_config()
    worker.apply_config(config)
    assert worker.camera_slugs() == ("gate", "porch")
    assert sorted(worker._managed) == ["cam-gate", "cam-porch"]

    # The operator deletes one, and its loop goes away with the next poll.
    worker.apply_config(config.model_copy(update={"cameras": config.cameras[:1]}))
    assert worker.camera_slugs() == ("porch",)
    assert list(worker._managed) == ["cam-porch"]


def test_det_max_cameras_is_a_ceiling_and_names_what_it_dropped(caplog):
    """The ceiling keeps the cameras the payload lists first — the order the operator
    added them in — and says which ones did not get a thread."""
    worker, client = worker_for(config_payload("porch", "gate", "drive"), max_cameras=2)
    worker.apply_config(client.get_config())
    assert sorted(worker._managed) == ["cam-gate", "cam-porch"]
    assert "DET_MAX_CAMERAS=2" in caplog.text
    assert "drive" in caplog.text, "the dropped camera is named, not merely counted"


def test_a_changed_camera_row_restarts_that_camera():
    """A rewritten zone set must not inherit dwell timers from the rules that were
    just deleted."""
    worker, client = worker_for(config_payload("porch"))
    config = client.get_config()
    worker.apply_config(config)
    first = worker._managed["cam-porch"]

    changed = config.model_copy(deep=True)
    changed.cameras[0].zones[0].rule = "loiter"
    changed.cameras[0].zones[0].dwell_seconds = 30
    worker.apply_config(changed)

    second = worker._managed["cam-porch"]
    assert second is not first, "the loop restarted rather than carrying the old rules"
    assert second.fingerprint != first.fingerprint


def test_the_worker_closes_open_events_and_drains_on_shutdown():
    worker, client = worker_for(config_payload("porch"))
    worker.apply_config(client.get_config())
    for managed in worker._managed.values():
        managed.thread.join(timeout=5)
    assert [s["action"] for s in client._buffer] == ["open", "close"]

    stop = threading.Event()
    stop.set()
    worker.run(stop, poll=0.05, flush_every=0.05)
    assert client.buffered() == 0, "shutdown flushed what the pipelines left behind"


def test_the_worker_survives_a_server_that_never_answers(caplog):
    """`docker compose up` starts the detector and the server in either order. A
    worker that died on the first refused poll would turn a restart into a crash
    loop, and the operator would see neither the cameras nor the reason."""
    client = stub_client([ConnectionError("connection refused")])
    worker = Worker(
        client,
        FakeModel(),
        frame_source=lambda _camera: iter([FRAME]),
        clock=FakeClock(1000.0, 1.0),
        sleep=lambda _s: None,
    )
    stop = threading.Event()
    threading.Timer(0.4, stop.set).start()
    worker.run(stop, poll=0.1, flush_every=0.1)
    assert worker.camera_slugs() == ()
    assert "no config from" in caplog.text


def test_the_worker_hands_its_credentials_to_the_taps_it_builds(monkeypatch):
    """S10-02: go2rtc's API enforces the basic auth the server pushes (S9-04),
    and the mjpeg tap is part of that API — a live stack proved a pipeline
    without credentials reads the 401 body forever and calls it "not an mjpeg
    stream". The credentials must survive Worker → CameraPipeline → MjpegStream,
    which is exactly the chain this test walks."""
    seen: list = []
    real = pipeline_module.MjpegStream

    class Capturing(real):
        def __init__(self, url, **kw):
            seen.append(kw.get("auth"))
            super().__init__(url, **kw)

        def __iter__(self):
            return iter(())

    monkeypatch.setattr(pipeline_module, "MjpegStream", Capturing)
    session = StubSession(
        [StubResult(200, config_payload("porch"))],
        default=StubResult(200, {"accepted": 0, "events": []}),
    )
    client = ServerClient("http://server:8080", "tok", session=session, sleep=lambda _s: None)
    worker = Worker(
        client,
        FakeModel(),
        go2rtc_auth=("nvr", "pw"),
        clock=FakeClock(1000.0, 1.0),
        sleep=lambda _s: None,
    )
    worker.apply_config(client.get_config())

    deadline = time.monotonic() + 5.0
    while not seen and time.monotonic() < deadline:
        time.sleep(0.05)
    assert seen == [("nvr", "pw")]
    worker.shutdown()


def test_the_worker_beats_the_heartbeat_on_the_shipped_loop():
    """S9-07: detector liveness is answered as data — the worker POSTs, the
    server stamps its own clock. The beat belongs to `Worker.run`, the loop the
    shipped binary runs; the client's own convenience loop is not it, and the
    first version of this beat was wired there, where the live stack never saw
    it. The beat goes out on the first pass — a freshly started worker reads
    fresh immediately — and every `heartbeat` seconds after."""
    session = StubSession([], default=StubResult(200, {"status": "recorded"}))
    client = ServerClient("http://server:8080", "tok", session=session, sleep=lambda _s: None)
    worker = Worker(
        client,
        FakeModel(),
        frame_source=lambda _camera: iter([FRAME]),
        clock=FakeClock(1000.0, 1.0),
        sleep=lambda _s: None,
    )
    stop = threading.Event()
    threading.Timer(0.45, stop.set).start()
    worker.run(stop, poll=10.0, flush_every=10.0, heartbeat=0.1)

    beats = [c for c in session.calls if c["url"].endswith("/api/internal/v1/heartbeat")]
    assert len(beats) >= 3, "a 30 s beat, compressed to 0.1 s, must have run several times"
    assert beats[0]["json"]["uptime_seconds"] >= 0, "the body carries uptime_seconds"
    polls = [c for c in session.calls if c["url"].endswith("/api/internal/v1/detector/config")]
    assert len(polls) == 1, "the beat must not disturb the poll interval"


def test_the_event_ceiling_from_the_settings_reaches_the_pipelines():
    worker, client = worker_for(config_payload("porch", settings={"event_max_seconds": 45}))
    worker.apply_config(client.get_config())
    managed = worker._managed["cam-porch"]
    assert managed.camera.id == "cam-porch"
    # The pipeline thread owns the machine; the worker handed it the ceiling.
    assert worker._event_max_seconds == 45


# -- the entrypoint --------------------------------------------------------


def boot(env: dict, wait_for: str, timeout: float = 25.0) -> tuple[int, str]:
    """Start `python -m nvr_detector` the way the container does, wait for a line,
    then SIGTERM it. Returns the exit code and everything it printed."""
    process = subprocess.Popen(
        [sys.executable, "-m", "nvr_detector"],
        cwd=DETECTOR_DIR,
        env=dict(
            os.environ,
            PYTHONPATH=DETECTOR_DIR,
            PYTHONUNBUFFERED="1",
            DET_LOG_LEVEL="info",
            **env,
        ),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    lines: list[str] = []
    deadline = time.monotonic() + timeout
    try:
        while time.monotonic() < deadline:
            line = process.stdout.readline()
            if not line:
                break
            lines.append(line)
            if wait_for in line:
                break
        else:
            pytest.fail(f"the worker never logged {wait_for!r} within {timeout}s:\n{''.join(lines)}")
    finally:
        process.terminate()  # SIGTERM
        try:
            rest = process.stdout.read()
        except Exception:  # noqa: BLE001 - the pipe may already be closed
            rest = ""
        code = process.wait(timeout=20)
    return code, "".join(lines) + rest


def test_a_missing_model_is_a_loud_fatal_exit(tmp_path):
    """A container built without its exported tensor should say so at once, not after
    a minute of config polls."""
    environ = dict(
        os.environ,
        PYTHONPATH=DETECTOR_DIR,
        PYTHONUNBUFFERED="1",
        DET_MODEL_PATH=str(tmp_path / "absent.onnx"),
        NVR_SERVER_URL="http://127.0.0.1:1",
    )
    result = subprocess.run(
        [sys.executable, "-m", "nvr_detector"],
        cwd=DETECTOR_DIR,
        env=environ,
        capture_output=True,
        text=True,
        timeout=20,
    )
    assert result.returncode == 2
    assert "no model at" in result.stdout + result.stderr


def test_the_worker_boots_against_a_stub_server_and_logs_the_camera_set_it_picked_up(tmp_path):
    """The card's criterion, run as a real process: env in, config poll out, the
    camera set named in the log, and SIGTERM answered with exit code 0."""
    model = write_tiny_onnx(tmp_path / "tiny.onnx")
    with StubServer(lambda method, path, body: (200, config_payload("porch", "gate") if method == "GET" else {"accepted": 0, "events": []})) as server:
        code, output = boot(
            {
                "NVR_SERVER_URL": server.url,
                "NVR_WORKER_TOKEN": "tok",
                "DET_MODEL_PATH": model,
                "DET_HTTP_TIMEOUT": "2",
                "GO2RTC_URL": "http://127.0.0.1:1",
            },
            wait_for="watching 2 camera(s)",
        )

    assert "watching 2 camera(s): gate, porch" in output
    assert "model:" in output and "loaded" in output
    assert code == 0, f"SIGTERM must exit 0, got {code}\n{output}"
    assert "stopping" in output
    assert "signal(s) sent" in output


def test_an_empty_worker_token_is_said_out_loudly(tmp_path):
    """Without it the server answers the internal API with 403 and the worker polls
    into a wall; the log has to say which of the two it is."""
    model = write_tiny_onnx(tmp_path / "tiny.onnx")
    code, output = boot(
        {"NVR_WORKER_TOKEN": "", "DET_MODEL_PATH": model, "NVR_SERVER_URL": "http://127.0.0.1:1"},
        wait_for="NVR_WORKER_TOKEN is empty",
        timeout=15,
    )
    assert code == 0


def test_the_environment_is_all_the_worker_needs_to_start(tmp_path):
    """Every variable the card names is read, and the ones that are unset fall back
    to the compose defaults — which is what makes the container startable with no
    configuration at all."""
    model = write_tiny_onnx(tmp_path / "tiny.onnx")
    code, output = boot(
        {"NVR_SERVER_URL": "http://127.0.0.1:1", "NVR_WORKER_TOKEN": "tok", "DET_MODEL_PATH": model, "DET_PROVIDERS": "CPUExecutionProvider"},
        wait_for="nvr_detector: server=",
        timeout=15,
    )
    assert "server=http://127.0.0.1:1" in output
    assert "model=" + str(model) in output
    assert "providers=CPUExecutionProvider" in output
    assert "max_cameras=12" in output
    assert "go2rtc=http://go2rtc:1984" in output
    assert "timeout=10.0s" in output
    assert code == 0


def test_a_bad_number_in_the_environment_falls_back_to_the_default_and_says_so(monkeypatch, caplog):
    """One typo in one variable must not stop inference on the other five cameras —
    which is how the server's own config loader behaves."""
    monkeypatch.setenv("DET_HTTP_TIMEOUT", "ten seconds")
    monkeypatch.setenv("DET_MAX_CAMERAS", "many")
    with caplog.at_level(logging.WARNING):
        assert entrypoint._env_number("DET_HTTP_TIMEOUT", 10.0) == 10.0
        assert entrypoint._env_int("DET_MAX_CAMERAS", 12) == 12
    assert "DET_HTTP_TIMEOUT" in caplog.text and "DET_MAX_CAMERAS" in caplog.text
