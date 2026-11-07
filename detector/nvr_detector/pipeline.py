"""One camera's loop, and the supervisor that keeps the loops matched to the rows.

The pipeline is the plan's chain, in order, with nothing beside it:

    frame → motion gate → model → tracker → zones → events → signal buffer

Each stage is a module of its own, so each can be argued about on its own; this
module only wires them and paces them.

Two rules worth naming:

*   **The camera row decides what runs.** The server already filtered the config
    payload to `detection_enabled && active`, so a camera switched off in the
    console stops being watched within one poll. A row whose payload *changed*
    restarts that camera's loop: a rewritten zone set should not inherit dwell
    timers from the rules that were just deleted.
*   **`DET_MAX_CAMERAS` is a ceiling, and hitting it is said.** A box that starts
    a thread per camera on a 12-camera installation while it can only infer four
    of them is a box where every camera is late, and the operator deserves to know
    which ones were dropped.
"""

from __future__ import annotations

import logging
import threading
import time
from dataclasses import dataclass, field
from typing import Callable, Iterable, Optional

import numpy as np

from .client import (
    CONFIG_POLL_SECONDS,
    DEFAULT_SERVER_URL,
    FLUSH_SECONDS,
    HEARTBEAT_SECONDS,
    CameraConfig,
    DetectorConfig,
    ServerClient,
)
from .events import EventMachine
from .mjpeg import MjpegStream, stream_url
from .model import YoloOnnx
from .motion import MotionGate
from .tracker import Tracker
from .zones import ZoneEvaluator

log = logging.getLogger("nvr_detector.pipeline")

# go2rtc's API base. The same env row the server reads, so one operator setting
# names the media host once for the whole installation.
DEFAULT_GO2RTC_URL = "http://go2rtc:1984"

FrameSource = Callable[[CameraConfig], Iterable[np.ndarray]]


@dataclass
class CameraPipeline:
    """One camera, one thread."""

    camera: CameraConfig
    model: YoloOnnx
    client: ServerClient
    go2rtc_url: str = DEFAULT_GO2RTC_URL
    go2rtc_auth: Optional[tuple[str, str]] = None
    http_timeout: float = 10.0
    frame_source: Optional[FrameSource] = None
    clock: Callable[[], float] = time.time
    sleep: Callable[[float], None] = time.sleep
    event_max_seconds: int = 300

    tracker: Tracker = field(init=False, default_factory=Tracker)
    evaluator: ZoneEvaluator = field(init=False, default_factory=ZoneEvaluator)
    gate: MotionGate = field(init=False, default=None)
    machine: EventMachine = field(init=False, default=None)
    frames_read: int = field(init=False, default=0)
    inference_runs: int = field(init=False, default=0)

    def __post_init__(self) -> None:
        # The gate is a continuous-mode CPU saver. An `events` camera's frames *are*
        # the trigger, so gating them could skip the frame that opens the window.
        self.gate = MotionGate(enabled=self.camera.record_mode == "continuous")
        self.machine = EventMachine(
            camera_id=self.camera.id,
            record_mode=self.camera.record_mode,
            event_max_seconds=self.event_max_seconds,
        )

    def process_frame(self, frame: np.ndarray, ts: float) -> list[dict]:
        """One frame through the chain. Returns the signals it produced."""
        self.frames_read += 1
        if not self.gate.should_infer(frame):
            return []
        self.inference_runs += 1

        detections = self.model.run(
            frame,
            min_confidence=self.camera.sensitivity,
            min_area_ratio=self.camera.min_area_ratio,
        )
        tracks = self.tracker.update(detections)
        hits = self.evaluator.evaluate(
            self.camera,
            tracks,
            ts,
            frame_wh=(int(frame.shape[1]), int(frame.shape[0])),
        )
        signals = self.machine.transition(hits, frame, ts)
        for signal in signals:
            self.client.add(signal)
        return signals

    def run(self, stop: threading.Event) -> None:
        """Read frames until `stop`, then close whatever is still open."""
        interval = 1.0 / max(1, int(self.camera.fps))
        next_at = 0.0
        log.info(
            "pipeline: %s: watching %s at %d fps (record_mode=%s, %d zone rule(s), gate=%s)",
            self.camera.slug,
            self.camera.stream_name(),
            self.camera.fps,
            self.camera.record_mode,
            len(self.camera.zones),
            "on" if self.gate.enabled else "off (events mode)",
        )
        try:
            for frame in self._frames(stop):
                if stop.is_set():
                    break
                now = self.clock()
                if now < next_at:
                    continue
                next_at = now + interval
                self.process_frame(frame, now)
        except Exception as exc:  # noqa: BLE001 - one camera's loop must not take the worker with it
            log.exception("pipeline: %s: loop died: %s", self.camera.slug, exc)
        finally:
            for signal in self.machine.close_all(self.clock()):
                self.client.add(signal)
            log.info(
                "pipeline: %s: stopped after %d frame(s), %d inferred",
                self.camera.slug,
                self.frames_read,
                self.inference_runs,
            )

    def _frames(self, stop: threading.Event) -> Iterable[np.ndarray]:
        if self.frame_source is not None:
            return self.frame_source(self.camera)
        url = stream_url(self.go2rtc_url, self.camera.stream_name())
        stream = MjpegStream(
            url,
            name=self.camera.slug,
            timeout=self.http_timeout,
            auth=self.go2rtc_auth,
            sleep=self.sleep,
            stop=stop,
        )
        return iter(stream)


@dataclass
class _Managed:
    camera: CameraConfig
    thread: threading.Thread
    stop: threading.Event
    fingerprint: str


class Worker:
    """The supervisor: poll the config, keep one thread per camera that wants one."""

    def __init__(
        self,
        client: ServerClient,
        model: YoloOnnx,
        *,
        max_cameras: int = 12,
        go2rtc_url: str = DEFAULT_GO2RTC_URL,
        go2rtc_auth: Optional[tuple[str, str]] = None,
        http_timeout: float = 10.0,
        frame_source: Optional[FrameSource] = None,
        clock: Callable[[], float] = time.time,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self.client = client
        self.model = model
        self.max_cameras = int(max_cameras)
        self.go2rtc_url = go2rtc_url
        self.go2rtc_auth = go2rtc_auth
        self.http_timeout = float(http_timeout)
        self.frame_source = frame_source
        self.clock = clock
        self.sleep = sleep

        self._managed: dict[str, _Managed] = {}
        self._slugs: tuple[str, ...] = ()
        self._event_max_seconds = 300
        self._server_warned = False

    def camera_slugs(self) -> tuple[str, ...]:
        return self._slugs

    def apply_config(self, config: DetectorConfig) -> None:
        """Reconcile the threads to the payload. The server has already dropped the
        cameras with detection off or the camera inactive."""
        self._event_max_seconds = config.event_max_seconds(self._event_max_seconds)

        wanted = list(config.cameras)
        if self.max_cameras > 0 and len(wanted) > self.max_cameras:
            dropped = [camera.slug or camera.id for camera in wanted[self.max_cameras :]]
            wanted = wanted[: self.max_cameras]
            log.warning(
                "worker: DET_MAX_CAMERAS=%d: %d cameras want detection, so these were not "
                "started: %s",
                self.max_cameras,
                len(config.cameras),
                ", ".join(dropped),
            )

        by_id = {camera.id: camera for camera in wanted}

        for camera_id, managed in list(self._managed.items()):
            camera = by_id.get(camera_id)
            if camera is None or _fingerprint(camera) != managed.fingerprint:
                managed.stop.set()
                del self._managed[camera_id]

        for camera in wanted:
            if camera.id in self._managed:
                continue
            pipeline = CameraPipeline(
                camera=camera,
                model=self.model,
                client=self.client,
                go2rtc_url=self.go2rtc_url,
                go2rtc_auth=self.go2rtc_auth,
                http_timeout=self.http_timeout,
                frame_source=self.frame_source,
                clock=self.clock,
                sleep=self.sleep,
                event_max_seconds=self._event_max_seconds,
            )
            stop = threading.Event()
            thread = threading.Thread(
                target=pipeline.run,
                args=(stop,),
                name=f"camera-{camera.slug or camera.id}",
                daemon=True,
            )
            self._managed[camera.id] = _Managed(
                camera=camera, thread=thread, stop=stop, fingerprint=_fingerprint(camera)
            )
            thread.start()

        slugs = tuple(sorted(camera.slug or camera.id for camera in wanted))
        if slugs != self._slugs:
            self._slugs = slugs
            log.info(
                "worker: watching %d camera(s): %s",
                len(slugs),
                ", ".join(slugs) if slugs else "(none: no camera has detection enabled)",
            )

    def run(
        self,
        stop: threading.Event,
        poll: float = CONFIG_POLL_SECONDS,
        flush_every: float = FLUSH_SECONDS,
        heartbeat: float = HEARTBEAT_SECONDS,
    ) -> None:
        """Poll, reconcile, flush, beat — until `stop`. Then close the events and
        drain. The heartbeat (S9-07) goes out on the first pass, so a freshly
        started worker reads fresh immediately, and every `heartbeat` seconds
        after; it is never buffered, because a beat describes *now*."""
        next_poll = 0.0
        next_flush = time.monotonic()
        started = time.monotonic()
        next_heartbeat = 0.0
        while not stop.is_set():
            now = time.monotonic()
            if now >= next_poll:
                config = self.client.get_config()
                if config is not None:
                    self.apply_config(config)
                else:
                    self._warn_no_server()
                next_poll = now + poll
            if time.monotonic() >= next_flush:
                self.client.flush()
                next_flush = time.monotonic() + flush_every
            if time.monotonic() >= next_heartbeat:
                self.client.heartbeat(time.monotonic() - started)
                next_heartbeat = time.monotonic() + heartbeat
            stop.wait(0.2)
        self.shutdown()

    def _warn_no_server(self) -> None:
        if not self._server_warned:
            self._server_warned = True
            log.warning(
                "worker: no config from %s yet — the server may still be starting; "
                "retrying every %.0fs",
                self.client.base_url or DEFAULT_SERVER_URL,
                CONFIG_POLL_SECONDS,
            )

    def shutdown(self) -> None:
        """Stop the camera loops (each closes its own open events), then drain."""
        for managed in self._managed.values():
            managed.stop.set()
        for managed in self._managed.values():
            managed.thread.join(timeout=self.http_timeout + 5.0)
        self._managed.clear()
        self.client.flush()
        log.info(
            "worker: stopped; %d signal(s) sent, %d dropped",
            self.client.signals_sent,
            self.client.signals_dropped,
        )


def _fingerprint(camera: CameraConfig) -> str:
    """What the worker cares about on one camera row. A payload that differs in any
    of it restarts that camera's loop rather than running stale rules."""
    return camera.model_dump_json()
