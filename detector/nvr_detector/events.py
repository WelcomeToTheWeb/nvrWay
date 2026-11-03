"""The event state machine: open, extend, close — and the frame that proves it.

An event is not a detection. It is a *run* of detections that a zone rule is
satisfied by, under one `event_key`, and the server mirrors this same machine
(S6-03): `open` inserts the row, `extend` moves its window and appends boxes,
`close` ends it. The worker owns the transitions because it is the only thing
looking at the frames; the server owns the clock, the row, and the dedupe.

Three things this module is careful about:

*   **The key is minted once, at the open.** `event_key` is
    `<camera_id>:<zone_id>:<rule>:<open_unix>`, and every later signal for the
    same occurrence carries the same string — that string is how a re-sent signal
    after a dropped connection becomes one event instead of two.
*   **The snapshot is the open's, and there is exactly one.** It is the frame that
    contains the box that triggered, which is the frame an operator wants to look
    at. Extends carry no image; a worker that sent one per extend would be paying
    to re-photograph the same scene.
*   **The timestamp is a courtesy.** `ts` is this machine's clock, and the server
    validates its shape and then throws it away. Nothing here can put an event in
    the future.
"""

from __future__ import annotations

import base64
import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Optional, Sequence

import cv2
import numpy as np

log = logging.getLogger("nvr_detector.events")

# The plan's close rule: an event ends when nothing has satisfied it for this long.
IDLE_CLOSE_SECONDS = 10.0

# And its ceiling: a person who stands in a zone forever must stop being one event
# eventually, or the clip that proves it never gets assembled.
DEFAULT_EVENT_MAX_SECONDS = 300

# The snapshot: quality 82 at width 640, and 200 KB is the ceiling the plan names.
# A JPEG over that is a webhook payload and a snapshot endpoint that both got
# slower so an operator can see one frame.
SNAPSHOT_QUALITY = 82
SNAPSHOT_WIDTH = 640
SNAPSHOT_MAX_BYTES = 200 * 1024
_QUALITY_LADDER = (SNAPSHOT_QUALITY, 75, 65, 55, 45)

ACTION_OPEN = "open"
ACTION_EXTEND = "extend"
ACTION_CLOSE = "close"

# The signal body, in the plan's key order. The server ignores order and so should
# the reader of this list — its point is that the set of keys is a contract, and
# `test_signals.py` asserts it against the server's own decoder.
SIGNAL_KEYS: tuple[str, ...] = (
    "camera_id",
    "event_key",
    "action",
    "rule",
    "zone_id",
    "zone_name",
    "severity",
    "classes",
    "ts",
    "objects",
    "snapshot_base64",
    "record_mode",
)


def rfc3339(ts: float) -> str:
    """This machine's clock as an RFC3339 string, in UTC.

    The server parses it with `time.RFC3339` and then discards it; the shape still
    has to be right, because a signal whose timestamp will not parse is a signal it
    refuses.
    """
    return datetime.fromtimestamp(float(ts), tz=timezone.utc).isoformat()


def scale_to_width(frame: np.ndarray, width: int = SNAPSHOT_WIDTH) -> np.ndarray:
    """Downscale to `width` px wide, never up. A 640×360 tap is already the right
    size; blowing a 320 px frame up to 640 costs bytes and adds no detail."""
    height, current = frame.shape[0], frame.shape[1]
    if width <= 0 or current <= width:
        return frame
    scale = width / float(current)
    return cv2.resize(
        frame,
        (width, max(1, int(round(height * scale)))),
        interpolation=cv2.INTER_AREA,
    )


def snapshot_jpeg(
    frame: np.ndarray,
    quality: int = SNAPSHOT_QUALITY,
    width: int = SNAPSHOT_WIDTH,
    max_bytes: int = SNAPSHOT_MAX_BYTES,
) -> bytes:
    """The frame as a JPEG that fits the cap.

    Quality walks down the ladder until the bytes fit. If even 45 does not fit, the
    smallest attempt is returned and said out loud: a snapshot that is a little
    blocky is still proof, and a snapshot silently dropped because it was large is
    not.
    """
    scaled = scale_to_width(frame, width)
    best: Optional[bytes] = None
    for q in _QUALITY_LADDER:
        ok, buf = cv2.imencode(".jpg", scaled, [int(cv2.IMWRITE_JPEG_QUALITY), q])
        if not ok:
            continue
        data = buf.tobytes()
        if best is None or len(data) < len(best):
            best = data
        if len(data) <= max_bytes:
            return data
    if best is None:
        raise RuntimeError("cv2.imencode produced no JPEG for a "
                           f"{frame.shape[1]}x{frame.shape[0]} frame")
    log.warning(
        "events: snapshot is %d bytes at quality %d, over the %d byte cap; sending it anyway",
        len(best),
        _QUALITY_LADDER[-1],
        max_bytes,
    )
    return best


def encode_signal(
    *,
    camera_id: str,
    record_mode: str,
    event_key: str,
    action: str,
    rule: str,
    zone_id: str,
    zone_name: str,
    severity: int,
    classes: Sequence[str],
    ts: float,
    objects: Sequence[dict],
    snapshot_base64: str = "",
) -> dict:
    """One signal, with exactly the plan's keys."""
    return {
        "camera_id": camera_id,
        "event_key": event_key,
        "action": action,
        "rule": rule,
        "zone_id": zone_id,
        "zone_name": zone_name,
        "severity": int(severity),
        "classes": list(classes),
        "ts": rfc3339(ts),
        "objects": list(objects),
        "snapshot_base64": snapshot_base64,
        "record_mode": record_mode,
    }


@dataclass
class _OpenEvent:
    """One occurrence in flight."""

    key: str
    zone_id: str
    zone_name: str
    rule: str
    severity: int
    opened_at: float
    last_hit_at: float
    classes: list[str] = field(default_factory=list)

    def add_classes(self, names: Sequence[str]) -> None:
        for name in names:
            if name not in self.classes:
                self.classes.append(name)


class EventMachine:
    """One camera's open/extend/close state, keyed by (zone, rule).

    `transition` is called once per processed frame with the zone hits that frame
    produced — including the frames that produced none, because a close is decided
    by the absence of hits.
    """

    def __init__(
        self,
        camera_id: str,
        record_mode: str,
        event_max_seconds: int = DEFAULT_EVENT_MAX_SECONDS,
        idle_close_seconds: float = IDLE_CLOSE_SECONDS,
        snapshot: bool = True,
    ) -> None:
        self.camera_id = camera_id
        self.record_mode = record_mode
        self.event_max_seconds = float(event_max_seconds)
        self.idle_close_seconds = float(idle_close_seconds)
        self.snapshot = bool(snapshot)
        self._open: dict[tuple[str, str], _OpenEvent] = {}

    @property
    def open_count(self) -> int:
        return len(self._open)

    def transition(
        self,
        hits: Sequence,
        frame: Optional[np.ndarray],
        frame_ts: float,
    ) -> list[dict]:
        """Apply this frame's hits and return the signals they produced.

        Opens first, then extends, then closes: an operator reading the log sees
        the event begin before they see it end, which is the order it happened in.
        """
        groups: dict[tuple[str, str], list] = {}
        for hit in hits:
            groups.setdefault((hit.zone_id, hit.rule), []).append(hit)

        opens: list[dict] = []
        extends: list[dict] = []
        # Encoded at most once per frame: two zones opening on the same frame is
        # two events, and each of them gets the frame that proves it.
        snapshot_b64: Optional[str] = None

        for zone_id, rule in sorted(groups):
            group = groups[(zone_id, rule)]
            first = group[0]
            event = self._open.get((zone_id, rule))
            if event is None:
                event = _OpenEvent(
                    key=f"{self.camera_id}:{zone_id}:{rule}:{int(frame_ts)}",
                    zone_id=zone_id,
                    zone_name=first.zone_name,
                    rule=rule,
                    severity=max(h.severity for h in group),
                    opened_at=frame_ts,
                    last_hit_at=frame_ts,
                )
                self._open[(zone_id, rule)] = event
                event.add_classes([h.class_name for h in group])
                if snapshot_b64 is None:
                    snapshot_b64 = self._snapshot(frame) if (frame is not None and self.snapshot) else ""
                opens.append(self._signal(event, ACTION_OPEN, group, snapshot_b64, frame_ts))
            else:
                event.last_hit_at = frame_ts
                event.severity = max(event.severity, max(h.severity for h in group))
                event.add_classes([h.class_name for h in group])
                extends.append(self._signal(event, ACTION_EXTEND, group, "", frame_ts))

        closes = []
        for key in [k for k, ev in self._open.items() if k not in groups]:
            event = self._open[key]
            idle = frame_ts - event.last_hit_at >= self.idle_close_seconds
            if idle:
                del self._open[key]
                log.info(
                    "events: %s: %s closed (idle %.1fs)", self.camera_id, event.key, frame_ts - event.last_hit_at
                )
                closes.append(self._signal(event, ACTION_CLOSE, [], "", frame_ts))

        # The age ceiling is checked after the extends: a person who stands in a
        # zone forever is still being hit, and an event that never ends is a clip
        # nobody assembles. The next hit opens a fresh one under a fresh key.
        for key in [k for k in groups if k in self._open]:
            event = self._open[key]
            if frame_ts - event.opened_at >= self.event_max_seconds:
                del self._open[key]
                log.info(
                    "events: %s: %s closed (age %.1fs, still active; the next hit opens a new event)",
                    self.camera_id,
                    event.key,
                    frame_ts - event.opened_at,
                )
                closes.append(self._signal(event, ACTION_CLOSE, [], "", frame_ts))

        return opens + extends + closes

    def close_all(self, frame_ts: float) -> list[dict]:
        """Close everything still open. The worker is going down (SIGTERM), and an
        event left open in the database is an event whose clip nobody scheduled."""
        closes = []
        for key in sorted(self._open):
            event = self._open[key]
            closes.append(self._signal(event, ACTION_CLOSE, [], "", frame_ts))
        self._open.clear()
        return closes

    def _snapshot(self, frame: np.ndarray) -> str:
        try:
            return base64.b64encode(snapshot_jpeg(frame)).decode("ascii")
        except Exception as exc:  # noqa: BLE001 - an event without a snapshot beats no event
            log.warning("events: %s: snapshot failed: %s", self.camera_id, exc)
            return ""

    def _signal(self, event: _OpenEvent, action: str, hits: Sequence, snapshot_b64: str, frame_ts: float) -> dict:
        objects = [h.as_object() for h in hits]
        classes = list(event.classes) or [h.class_name for h in hits]
        return encode_signal(
            camera_id=self.camera_id,
            record_mode=self.record_mode,
            event_key=event.key,
            action=action,
            rule=event.rule,
            zone_id=event.zone_id,
            zone_name=event.zone_name,
            severity=event.severity,
            classes=classes,
            ts=frame_ts,
            objects=objects,
            snapshot_base64=snapshot_b64 if action == ACTION_OPEN else "",
        )
