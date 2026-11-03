"""The server is the source of truth; this is how the worker asks it.

Two calls, both behind the worker token (`WORKER_TOKEN`, S2-08):

    GET  /api/internal/v1/detector/config   every 5 s
    POST /api/internal/v1/signals           every 2 s, at most 40 per call

The config poll is what makes the worker stateless about the installation: a
camera added in the console shows up as a thread within one poll, and one deleted
disappears, with no restart and no message pushed to this process.

The signal path is written for the case where the server is not there. A worker
that drops signals on a connection error loses events; a worker that buffers them
and re-sends does not, because the server dedupes on `event_key` (S6-03). When the
buffer itself overflows, the oldest signals go — and the count is said out loud,
because "some events were shorter than they should have been" is a fact the
operator can act on and a silent drop is not.
"""

from __future__ import annotations

import logging
import threading
import time
from typing import Callable, Optional

import requests
from pydantic import BaseModel, ConfigDict, Field, ValidationError

log = logging.getLogger("nvr_detector.client")

# The plan's timings and limits, as named constants.
CONFIG_POLL_SECONDS = 5.0
FLUSH_SECONDS = 2.0
MAX_SIGNALS_PER_POST = 40
MAX_BUFFERED_SIGNALS = 40
MAX_RETRIES = 3
BACKOFF_BASE_SECONDS = 5.0
BACKOFF_MAX_SECONDS = 60.0
DEFAULT_TIMEOUT = 10.0

DEFAULT_SERVER_URL = "http://server:8080"
DETECTOR_CONFIG_PATH = "/api/internal/v1/detector/config"
SIGNALS_PATH = "/api/internal/v1/signals"


class ZoneConfig(BaseModel):
    """One zone rule, as the server's `zones_json` column carries it (S3-04)."""

    model_config = ConfigDict(extra="ignore")

    id: str = ""
    name: str = ""
    polygon: list[list[float]] = Field(default_factory=list)
    classes: list[str] = Field(default_factory=list)
    rule: str = "intrusion"
    dwell_seconds: float = 0.0
    severity: int = 1
    # A zero floor means "the camera's", which `zones.evaluate` resolves by taking
    # the higher of the two — a zone may raise a floor, never lower one.
    min_confidence: float = 0.0
    min_area_ratio: float = 0.0
    enabled: bool = True


class CameraConfig(BaseModel):
    """One camera the worker should be watching.

    `extra="ignore"` is deliberate: a server that grows the payload must not break
    a worker that was built before the field existed.
    """

    model_config = ConfigDict(extra="ignore")

    id: str
    slug: str = ""
    name: str = ""
    det_stream: str = ""
    fps: int = 5
    width: int = 640
    height: int = 360
    sensitivity: float = 0.45
    min_area_ratio: float = 0.004
    zones: list[ZoneConfig] = Field(default_factory=list)
    record_mode: str = "continuous"
    providers: list[str] = Field(default_factory=list)

    def stream_name(self) -> str:
        """The go2rtc stream to read. The server sends `<slug>_det`; the fallback
        is the same rule, so a payload that omits it still finds its frames."""
        return self.det_stream or f"{self.slug}_det"


class DetectorConfig(BaseModel):
    """The whole config payload."""

    model_config = ConfigDict(extra="ignore")

    cameras: list[CameraConfig] = Field(default_factory=list)
    settings: dict = Field(default_factory=dict)

    def event_max_seconds(self, default: int = 300) -> int:
        raw = self.settings.get("event_max_seconds", default)
        try:
            value = int(raw)
        except (TypeError, ValueError):
            return default
        return value if value > 0 else default


def _backoff_delay(attempt: int, base: float = BACKOFF_BASE_SECONDS, cap: float = BACKOFF_MAX_SECONDS) -> float:
    """The plan's `min(5 * 2^n, 60)`: 5 s, 10 s, 20 s, and never a minute more."""
    return min(base * float(2 ** attempt), cap)


class ServerClient:
    """The two endpoints, the retries, and the buffer between the pipeline and them.

    `session` is the seam: the suite hands in a stub that answers 500 twice and
    200 once, and the retry policy becomes an assertion instead of a wait. `sleep`
    is the same idea for the backoff.
    """

    def __init__(
        self,
        base_url: str = DEFAULT_SERVER_URL,
        token: str = "",
        *,
        timeout: float = DEFAULT_TIMEOUT,
        retries: int = MAX_RETRIES,
        max_batch: int = MAX_SIGNALS_PER_POST,
        max_buffer: int = MAX_BUFFERED_SIGNALS,
        backoff_base: float = BACKOFF_BASE_SECONDS,
        backoff_max: float = BACKOFF_MAX_SECONDS,
        session: Optional[requests.Session] = None,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = float(timeout)
        self.retries = int(retries)
        self.max_batch = int(max_batch)
        self.max_buffer = int(max_buffer)
        self.backoff_base = float(backoff_base)
        self.backoff_max = float(backoff_max)
        self._session = session if session is not None else requests.Session()
        self._sleep = sleep

        self.config: Optional[DetectorConfig] = None
        self.signals_dropped = 0
        self.signals_sent = 0
        self._buffer: list[dict] = []
        self._lock = threading.Lock()

    # -- config ------------------------------------------------------------

    def get_config(self) -> Optional[DetectorConfig]:
        """Poll the camera set. None means "the server did not answer usefully", and
        the caller keeps working with the config it already had."""
        status, body = self._request("GET", DETECTOR_CONFIG_PATH)
        if status != 200 or not isinstance(body, dict):
            log.warning("client: detector config: HTTP %s", status)
            return None
        try:
            parsed = DetectorConfig.model_validate(body)
        except ValidationError as exc:
            log.error("client: detector config payload did not parse: %s", exc)
            return None
        self.config = parsed
        return parsed

    # -- signals -----------------------------------------------------------

    def add(self, signal: dict) -> None:
        """Buffer one signal, dropping the oldest when the buffer is full."""
        with self._lock:
            self._buffer.append(signal)
            overflow = len(self._buffer) - self.max_buffer
            if overflow > 0:
                del self._buffer[:overflow]
                self.signals_dropped += overflow
                log.warning(
                    "client: signals_dropped=%d: the signal buffer holds at most %d, "
                    "dropping the oldest %d",
                    self.signals_dropped,
                    self.max_buffer,
                    overflow,
                )

    def buffered(self) -> int:
        with self._lock:
            return len(self._buffer)

    def flush(self) -> bool:
        """Send up to `max_batch` buffered signals. True means they are on the
        server; False means the buffer still holds them."""
        with self._lock:
            if not self._buffer:
                return True
            batch = self._buffer[: self.max_batch]

        ok, _ = self.post_signals(batch)
        if ok:
            posted = {id(signal) for signal in batch}
            with self._lock:
                # By identity, not by count: signals may have been dropped from the
                # front of the buffer while this POST was in flight.
                self._buffer = [s for s in self._buffer if id(s) not in posted]
        return ok

    def post_signals(self, signals: list[dict]) -> tuple[bool, Optional[dict]]:
        """POST the batch, retrying a refusal `retries` times with the plan's backoff.

        A 4xx is not retried: the server said no, not "not now", and a worker that
        retries a 401 will be retrying it when it comes back up.
        """
        attempt = 0
        body: Optional[dict] = None
        while True:
            status, body = self._request("POST", SIGNALS_PATH, {"signals": list(signals)})
            if status == 200:
                self.signals_sent += len(signals)
                return True, body
            if 400 <= status < 500 and status != 429:
                log.warning("client: signals refused with HTTP %s: %s", status, _error_of(body))
                return False, body
            if attempt >= self.retries:
                log.warning(
                    "client: signals lost after %d attempts (HTTP %s: %s); %d signal(s) stay buffered",
                    attempt + 1,
                    status,
                    _error_of(body),
                    len(signals),
                )
                return False, body
            delay = _backoff_delay(attempt, self.backoff_base, self.backoff_max)
            log.warning(
                "client: signals: HTTP %s (%s), attempt %d of %d, retrying in %.0fs",
                status,
                _error_of(body),
                attempt + 1,
                self.retries + 1,
                delay,
            )
            self._sleep(delay)
            attempt += 1

    # -- transport ---------------------------------------------------------

    def _request(self, method: str, path: str, payload: Optional[dict] = None) -> tuple[int, Optional[dict]]:
        url = self.base_url + path
        headers = {"Authorization": f"Bearer {self.token}"} if self.token else {}
        try:
            response = self._session.request(
                method, url, json=payload, timeout=self.timeout, headers=headers
            )
        except Exception as exc:  # noqa: BLE001 - requests raises a dozen classes for "no server"
            log.warning("client: %s %s: %s", method, url, exc)
            return 0, None
        status = int(response.status_code)
        try:
            body = response.json()
        except ValueError:
            body = None
        return status, body if isinstance(body, dict) else None

    def close(self) -> None:
        close = getattr(self._session, "close", None)
        if callable(close):
            close()

    def run(
        self,
        stop: threading.Event,
        on_config: Optional[Callable[[DetectorConfig], None]] = None,
        poll: float = CONFIG_POLL_SECONDS,
        flush_every: float = FLUSH_SECONDS,
    ) -> None:
        """Poll and flush until `stop` is set. Runs on the caller's thread; the
        worker's main loop is the caller, and the camera threads are its children."""
        next_poll = 0.0
        next_flush = time.monotonic()
        while not stop.is_set():
            now = time.monotonic()
            if now >= next_poll:
                config = self.get_config()
                if config is not None and on_config is not None:
                    on_config(config)
                next_poll = now + poll
            if time.monotonic() >= next_flush:
                self.flush()
                next_flush = time.monotonic() + flush_every
            stop.wait(0.2)


def _error_of(body: Optional[dict]) -> str:
    if not body:
        return "no response body"
    return str(body.get("error", body))
