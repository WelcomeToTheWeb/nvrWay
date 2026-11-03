"""Shared fixtures for the detector suite.

Nothing here touches the network. The mjpeg source, the server and the ONNX
session each get a seam — a stub opener, a stub session, a real session over a
two-kilobyte model the suite writes itself — so the suite runs in a container with
no route to either service, and a test that hangs on a socket is a bug in the test.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Callable, Iterable, Optional

import numpy as np
import pytest

from nvr_detector.client import CameraConfig, ZoneConfig
from nvr_detector.mjpeg import MjpegStream
from nvr_detector.model import CLASS_NAMES, Detection

REPO_ROOT = Path(__file__).resolve().parents[2]


# -- frames and boxes ------------------------------------------------------


def make_frame(width: int = 64, height: int = 36, value: int = 0) -> np.ndarray:
    """A BGR frame of one flat colour."""
    return np.full((height, width, 3), int(value), dtype=np.uint8)


def detection(
    cls: str = "person",
    conf: float = 0.9,
    box: tuple[float, float, float, float] = (100.0, 50.0, 140.0, 150.0),
) -> Detection:
    assert cls in CLASS_NAMES, f"{cls} is not a COCO class this model can report"
    return Detection(
        cls=CLASS_NAMES.index(cls),
        conf=float(conf),
        x1=float(box[0]),
        y1=float(box[1]),
        x2=float(box[2]),
        y2=float(box[3]),
    )


def track(track_id: int, det: Optional[Detection] = None, missed: int = 0):
    from nvr_detector.tracker import Track

    return Track(id=track_id, detection=det or detection(), missed=missed)


SQUARE: tuple[float, float, float, float] = (0.0, 0.0, 1.0, 1.0)
FULL_FRAME_POLYGON = [[0.0, 0.0], [1.0, 0.0], [1.0, 1.0], [0.0, 1.0]]


def zone(
    zone_id: str = "z1",
    name: str = "driveway",
    rule: str = "intrusion",
    polygon: Optional[list] = None,
    classes: Optional[list] = None,
    dwell_seconds: float = 0.0,
    severity: int = 3,
    min_confidence: float = 0.0,
    min_area_ratio: float = 0.0,
    enabled: bool = True,
) -> ZoneConfig:
    return ZoneConfig(
        id=zone_id,
        name=name,
        polygon=polygon if polygon is not None else [list(p) for p in FULL_FRAME_POLYGON],
        classes=classes or [],
        rule=rule,
        dwell_seconds=dwell_seconds,
        severity=severity,
        min_confidence=min_confidence,
        min_area_ratio=min_area_ratio,
        enabled=enabled,
    )


def camera_cfg(
    camera_id: str = "cam-1",
    slug: str = "porch",
    record_mode: str = "continuous",
    zones: Optional[list] = None,
    sensitivity: float = 0.45,
    min_area_ratio: float = 0.004,
    fps: int = 5,
    width: int = 640,
    height: int = 360,
    **extra: Any,
) -> CameraConfig:
    return CameraConfig(
        id=camera_id,
        slug=slug,
        name=slug.title(),
        det_stream=f"{slug}_det",
        fps=fps,
        width=width,
        height=height,
        sensitivity=sensitivity,
        min_area_ratio=min_area_ratio,
        zones=zones if zones is not None else [zone()],
        record_mode=record_mode,
        providers=["CPUExecutionProvider"],
        **extra,
    )


# -- the mjpeg seam --------------------------------------------------------


class StubMjpegResponse:
    """What `requests.get(stream=True)` answers, reduced to what MjpegStream reads.

    `on_exhausted` fires when the scripted body has been read to the end. A real
    mjpeg body never ends, so the stream's only way to stop is its stop event; a
    scripted body does end, and this is how a test says "that was the last frame".
    """

    def __init__(
        self,
        content_type: str,
        chunks: Iterable[bytes],
        fail_after: Optional[str] = None,
        on_exhausted: Optional[Callable[[], None]] = None,
    ) -> None:
        self.headers = {"Content-Type": content_type}
        self._chunks = list(chunks)
        self._fail_after = fail_after
        self._on_exhausted = on_exhausted
        self.closed = False

    def iter_content(self, chunk_size: int = 8192) -> Iterable[bytes]:
        for chunk in self._chunks:
            for start in range(0, len(chunk), chunk_size):
                yield chunk[start : start + chunk_size]
        if self._on_exhausted is not None:
            self._on_exhausted()
        if self._fail_after:
            raise ConnectionError(self._fail_after)

    def close(self) -> None:
        self.closed = True


def multipart(boundary: str, frames: list[np.ndarray], preamble: bytes = b"") -> list[bytes]:
    """A real multipart mjpeg body, built from real JPEG bytes."""
    import cv2

    delimiter = f"--{boundary}".encode()
    parts = [preamble]
    for frame in frames:
        ok, buf = cv2.imencode(".jpg", frame)
        assert ok
        parts.append(delimiter + b"\r\nContent-Type: image/jpeg\r\n\r\n" + buf.tobytes())
    parts.append(delimiter + b"--\r\n")
    return parts


class RecordingOpener:
    """The `opener` seam: hands back scripted responses, records what it was asked.

    A real mjpeg endpoint never ends, so `MjpegStream` reconnects until it is told
    to stop — which means a scripted stream has to be told. The opener sets the stop
    event as it hands out the last scripted response, so the loop finishes after the
    frames the test described and never dials a connection the test did not write.
    """

    def __init__(self, responses: list[StubMjpegResponse], stop: Optional[threading.Event] = None) -> None:
        self.responses = list(responses)
        self.stop = stop if stop is not None else threading.Event()
        self.calls: list[tuple[str, float]] = []

    def __call__(self, url: str, timeout: float) -> StubMjpegResponse:
        self.calls.append((url, timeout))
        if not self.responses:
            # Asked for a connection the test did not script: say "stopped" rather
            # than raise, because the stream swallows every exception and would
            # otherwise reconnect forever against an empty script.
            self.stop.set()
            return StubMjpegResponse("multipart/x-mixed-replace;boundary=frame", [])
        return self.responses.pop(0)


def read_stream(
    responses: list[StubMjpegResponse],
    url: str = "http://go2rtc:1984/api/stream.mjpeg?src=porch_det",
    **stream_kwargs: Any,
) -> tuple[list[np.ndarray], MjpegStream, RecordingOpener, list[float]]:
    """Drain a scripted stream, with the reconnect backoff recorded rather than slept.

    Returns the frames, the stream, the opener (for its call log) and the backoffs it
    was asked to wait. Without the injected `sleep` a reconnect test would spend real
    seconds waiting for a five-second backoff the test only wants to read.

    The last scripted response stops the stream once its body has been read, which is
    how a test describes a finite number of frames against an endpoint that is meant
    to run forever.
    """
    stop = threading.Event()
    if responses:
        responses[-1]._on_exhausted = stop.set
    opener = RecordingOpener(responses, stop=stop)
    slept: list[float] = []
    stream = MjpegStream(url, opener=opener, stop=stop, sleep=slept.append, **stream_kwargs)
    frames = list(stream)
    return frames, stream, opener, slept


# -- the server seam -------------------------------------------------------


class StubResult:
    def __init__(self, status: int, body: Any = None) -> None:
        self.status_code = status
        self._body = body

    def json(self) -> Any:
        if self._body is None:
            raise ValueError("no JSON body")
        return self._body


class StubSession:
    """The `session` seam for ServerClient: scripted responses, recorded calls.

    The script runs out, the stub raises — unless the test handed in a `default`, in
    which case a quiet server answers instead. Strict by default on purpose: a test
    that gets an unexpected call should find out, and a pipeline test that only cares
    about the first two calls says so by passing a default.
    """

    def __init__(self, script: Optional[list[Any]] = None, default: Optional[StubResult] = None) -> None:
        self.script = list(script or [])
        self.default = default
        self.calls: list[dict] = []

    def request(self, method, url, json=None, timeout=None, headers=None) -> StubResult:  # noqa: ANN001, A002
        self.calls.append({"method": method, "url": url, "json": json, "timeout": timeout, "headers": headers or {}})
        if self.script:
            outcome = self.script.pop(0)
        elif self.default is not None:
            outcome = self.default
        else:
            raise AssertionError(f"the stub session ran out of scripted responses ({method} {url})")
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    def close(self) -> None:
        pass


class StubServer:
    """A real localhost HTTP server, for the criteria that name one.

    The handler is a callable `(method, path, body) -> (status, response_body)`, so
    a test scripts a 500 then a 200 and counts the attempts it was actually asked
    for — over a socket, not through a mock.
    """

    def __init__(self, handler: Callable[[str, str, Optional[dict]], tuple[int, Any]]) -> None:
        self.handler = handler
        self.requests: list[tuple[str, str, Optional[dict]]] = []
        outer = self

        class _Handler(BaseHTTPRequestHandler):
            def do_GET(self):  # noqa: N802
                outer._respond(self, "GET", None)

            def do_POST(self):  # noqa: N802
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length) if length else b""
                body = json.loads(raw) if raw else None
                outer._respond(self, "POST", body)

            def log_message(self, *args):  # noqa: ANN002 - silence the default stderr spam
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def _respond(self, request: BaseHTTPRequestHandler, method: str, body: Optional[dict]) -> None:
        self.requests.append((method, request.path, body))
        status, payload = self.handler(method, request.path, body)
        raw = json.dumps(payload).encode() if payload is not None else b""
        request.send_response(status)
        request.send_header("Content-Type", "application/json")
        request.send_header("Content-Length", str(len(raw)))
        request.end_headers()
        if raw:
            request.wfile.write(raw)

    @property
    def url(self) -> str:
        host, port = self.server.server_address[:2]
        return f"http://{host}:{port}"

    def __enter__(self) -> "StubServer":
        self.thread.start()
        return self

    def __exit__(self, *exc: Any) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def count(self, path: str) -> int:
        return sum(1 for method, request_path, _ in self.requests if request_path == path)


# -- time ------------------------------------------------------------------


class FakeSleep:
    """Records the waits instead of taking them, and can stop a loop."""

    def __init__(self) -> None:
        self.durations: list[float] = []
        self.on_sleep: Optional[Callable[[int], None]] = None

    def __call__(self, seconds: float) -> None:
        index = len(self.durations)
        self.durations.append(float(seconds))
        if self.on_sleep is not None:
            self.on_sleep(index)

    def total(self) -> float:
        return sum(self.durations)


@pytest.fixture
def fake_sleep() -> FakeSleep:
    return FakeSleep()


class FakeClock:
    """A clock that advances by `step` on every call, clamping at the end.

    The pipeline asks the clock once per frame and once when its loop ends, so a
    clock that ran out would raise StopIteration out of a `finally` block and blame
    the pipeline for the test's bookkeeping.
    """

    def __init__(self, start: float = 1000.0, step: float = 1.0, limit: Optional[float] = None) -> None:
        self.value = float(start)
        self.step = float(step)
        self.limit = float(limit) if limit is not None else None
        self.calls = 0

    def __call__(self) -> float:
        now = self.value
        self.calls += 1
        if self.limit is not None:
            self.value = min(self.limit, self.value + self.step)
        else:
            self.value += self.step
        return now


# -- a real ONNX session, two kilobytes of it ------------------------------


def write_tiny_onnx(path: Path, tensor: Optional[np.ndarray] = None) -> str:
    """Write a model that answers `output0` with `tensor` (or with its own input).

    Built with the same `onnx` version the image build exports with (S5-12), so the
    fixture and the shipped model come from one serialiser. `YoloOnnx` opens it like
    any other session, which is what makes the provider fallback and the
    preprocess→session→postprocess chain testable without a 12 MB download.
    """
    import onnx
    from onnx import TensorProto, helper, numpy_helper

    if tensor is None:
        graph = helper.make_graph(
            nodes=[helper.make_node("Identity", ["images"], ["output0"])],
            name="tiny_identity",
            inputs=[helper.make_tensor_value_info("images", TensorProto.FLOAT, [1, 3, 640, 640])],
            outputs=[helper.make_tensor_value_info("output0", TensorProto.FLOAT, [1, 3, 640, 640])],
        )
    else:
        value = numpy_helper.from_array(tensor.astype(np.float32), name="planted")
        graph = helper.make_graph(
            nodes=[helper.make_node("Identity", ["planted"], ["output0"])],
            name="tiny_planted",
            inputs=[helper.make_tensor_value_info("images", TensorProto.FLOAT, [1, 3, 640, 640])],
            outputs=[helper.make_tensor_value_info("output0", TensorProto.FLOAT, list(tensor.shape))],
            initializer=[value],
        )

    model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", 17)], producer_name="nvr-detector-tests")
    onnx.save(model, str(path))
    return str(path)


def planted_tensor(rows: list[tuple[int, float, tuple[float, float, float, float]]], frame: tuple[int, int] = (640, 360)) -> np.ndarray:
    """A `[1, 84, 8400]` YOLOv8 output with one anchor per row of `rows`.

    Each row is `(class id, confidence, (cx, cy, w, h))` in model pixels. Anchor 0
    is the top-left corner of the grid, so a test that plants a box knows exactly
    which box the decoder should read back.
    """
    tensor = np.zeros((1, 4 + len(CLASS_NAMES), 8400), dtype=np.float32)
    for index, (cls, conf, (cx, cy, w, h)) in enumerate(rows):
        tensor[0, 0, index] = cx
        tensor[0, 1, index] = cy
        tensor[0, 2, index] = w
        tensor[0, 3, index] = h
        tensor[0, 4 + cls, index] = conf
    return tensor


@pytest.fixture
def tiny_model(tmp_path) -> str:
    return write_tiny_onnx(tmp_path / "tiny.onnx")
