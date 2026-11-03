"""`python -m nvr_detector` — the worker's entrypoint.

Everything the operator can change is an environment variable, and every one of
them has a default that works in the compose file this repository ships:

    NVR_SERVER_URL     http://server:8080        where the internal API lives
    NVR_WORKER_TOKEN   empty                     the bearer; empty means the
                                                  server answers 403, and that is
                                                  said out loud
    DET_PROVIDERS      CPUExecutionProvider      comma list; CUDA is the overlay
    DET_MODEL_PATH     /models/yolov8n.onnx      exported at image build (S5-12)
    DET_HTTP_TIMEOUT   10                        seconds, for both directions
    DET_MAX_CAMERAS    12                        ceiling on camera threads
    GO2RTC_URL         http://go2rtc:1984        the mjpeg tap's base

A bad number is not fatal: it is logged and the default is used, which is how the
server's own config loader behaves, so one typo in one variable does not stop
recording on the other five cameras.

SIGTERM exits 0. The container runtime sends it on `docker stop`, and a worker that
exits 1 because it was *asked* to stop turns every redeploy into a crash loop in
the operator's log.
"""

from __future__ import annotations

import logging
import os
import signal
import sys
import threading
from typing import Optional

from .client import DEFAULT_SERVER_URL, ServerClient
from .model import YoloOnnx, providers_from_env
from .pipeline import DEFAULT_GO2RTC_URL, Worker

DEFAULT_MODEL_PATH = "/models/yolov8n.onnx"
DEFAULT_HTTP_TIMEOUT = 10.0
DEFAULT_MAX_CAMERAS = 12

LOG_FORMAT = "%(asctime)s %(levelname)-7s %(name)s: %(message)s"

EXIT_OK = 0
EXIT_FATAL = 2

log = logging.getLogger("nvr_detector.main")


def _env_raw(name: str, default: str) -> str:
    value = os.environ.get(name, "")
    return value if value.strip() else default


def _env_number(name: str, default: float) -> float:
    raw = _env_raw(name, str(default))
    try:
        return float(raw)
    except ValueError:
        log.warning("%s=%r is not a number; using %s", name, raw, default)
        return float(default)


def _env_int(name: str, default: int) -> int:
    raw = _env_raw(name, str(default))
    try:
        return int(raw)
    except ValueError:
        log.warning("%s=%r is not an integer; using %d", name, raw, default)
        return int(default)


def _configure_logging() -> None:
    level = logging.INFO
    if _env_raw("DET_LOG_LEVEL", "info").strip().lower() in ("debug", "trace"):
        level = logging.DEBUG
    logging.basicConfig(level=level, format=LOG_FORMAT, stream=sys.stdout)


def main(argv: Optional[list[str]] = None) -> int:
    _configure_logging()

    server_url = _env_raw("NVR_SERVER_URL", DEFAULT_SERVER_URL)
    token = os.environ.get("NVR_WORKER_TOKEN", "").strip()
    model_path = _env_raw("DET_MODEL_PATH", DEFAULT_MODEL_PATH)
    go2rtc_url = _env_raw("GO2RTC_URL", DEFAULT_GO2RTC_URL)
    http_timeout = _env_number("DET_HTTP_TIMEOUT", DEFAULT_HTTP_TIMEOUT)
    max_cameras = _env_int("DET_MAX_CAMERAS", DEFAULT_MAX_CAMERAS)
    providers = providers_from_env()

    log.info(
        "nvr_detector: server=%s model=%s providers=%s go2rtc=%s timeout=%.1fs max_cameras=%d",
        server_url,
        model_path,
        ",".join(providers),
        go2rtc_url,
        http_timeout,
        max_cameras,
    )

    # Handlers before anything slow: `docker stop` can arrive while a 12 MB model is
    # still loading, and a worker that dies on the default SIGTERM action turns a
    # redeploy into exit 143 in the operator's log for no reason.
    stop = threading.Event()

    def _request_stop(signum, _frame):  # noqa: ANN001 - the signal handler's signature
        if not stop.is_set():
            log.info("signal %d: stopping (open events are closed, signals flushed)", signum)
            stop.set()

    for name in ("SIGTERM", "SIGINT"):
        signum = getattr(signal, name, None)
        if signum is not None:
            try:
                signal.signal(signum, _request_stop)
            except ValueError:  # pragma: no cover - not the main thread, e.g. under a test runner
                pass

    if not token:
        log.warning(
            "NVR_WORKER_TOKEN is empty: the server answers /api/internal/* with 403 "
            "\"internal api disabled\", so this worker will poll and be refused"
        )

    # The model is checked before the first poll, so a container built without its
    # exported tensor says so immediately instead of after a minute of config polls.
    if not os.path.exists(model_path):
        log.error(
            "no model at %s — the detector image exports yolov8n.onnx at build time "
            "(card S5-12); set DET_MODEL_PATH to point at one",
            model_path,
        )
        return EXIT_FATAL
    try:
        model = YoloOnnx(model_path, providers)
    except Exception as exc:  # noqa: BLE001 - any failure to load means there is no worker
        log.error("cannot load %s: %s", model_path, exc)
        return EXIT_FATAL
    log.info(
        "model: %s loaded (providers=%s, input %s, output %s)",
        model.path,
        ",".join(model.providers),
        model.input_name,
        model.output_name,
    )

    client = ServerClient(server_url, token, timeout=http_timeout)
    worker = Worker(
        client,
        model,
        max_cameras=max_cameras,
        go2rtc_url=go2rtc_url,
        http_timeout=http_timeout,
    )

    try:
        worker.run(stop)
    except KeyboardInterrupt:  # pragma: no cover - the handler above normally catches it first
        stop.set()
        worker.shutdown()
    finally:
        client.close()
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
