"""Zone rules: which box counts, where it has to be, and for how long.

A zone is a polygon in **normalized** coordinates (0..1 of the detection frame),
so changing a camera's resolution does not silently invalidate a rule the operator
drew last month. The server validates that shape (S3-04); this module only reads it.

The point that has to land inside the polygon is the **bottom center** of the box,
not its center: a person's feet, not their torso. A rule drawn on a driveway
should fire when someone is standing in it, not when their head crosses an
imaginary line halfway up a wall, and a box hanging off the top of the frame
because someone is close to the camera should still count.

Two rules exist, and the difference is time:

*   `intrusion` — a qualifying box is inside the polygon now.
*   `loiter` — the *same track id* has been inside continuously for
    `dwell_seconds`. Leaving resets the timer, so someone who wanders in and out
    never accumulates a loiter, which is the point: a loiter is a stay.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import TYPE_CHECKING, Optional, Sequence

from .model import IGNORED_CLASSES

if TYPE_CHECKING:  # pragma: no cover - schema lives with the payload that carries it
    from .client import CameraConfig, ZoneConfig

log = logging.getLogger("nvr_detector.zones")

# The rules the server's zone contract allows (models.Zone.Rule). Anything else in
# a row is a rule nothing can fire, and it is logged once rather than silently
# skipped forever.
RULE_INTRUSION = "intrusion"
RULE_LOITER = "loiter"


def point_in_polygon(x: float, y: float, polygon: Sequence[Sequence[float]]) -> bool:
    """Ray-cast containment, with the boundary counted as inside.

    A point exactly on an edge is decided before the ray-cast runs: the parity test
    alone makes that case depend on vertex order, and an operator who draws a line
    through the middle of their driveway expects the line itself to be part of the
    zone. Polygons are closed implicitly, so the last edge joins back to the first.
    """
    n = len(polygon)
    if n < 3:
        return False

    inside = False
    previous = _vertex(polygon, n - 1)
    for index in range(n):
        current = _vertex(polygon, index)
        if _on_segment(x, y, previous, current):
            return True
        px, py = previous
        cx, cy = current
        if (py > y) != (cy > y):
            # x at the crossing of this edge with the horizontal ray from the point.
            x_at = (cx - px) * (y - py) / (cy - py) + px
            if x < x_at:
                inside = not inside
        previous = current
    return inside


def centroid(detection, frame_w: int, frame_h: int) -> tuple[float, float]:
    """Bottom-center of a box, normalised to 0..1 of the frame.

    `((x1 + x2) / 2, y2)` — the horizontal middle at the bottom edge. For a
    640×360 frame and a box `[100, 50, 140, 150]` that is `(120/640, 150/360)` =
    `(0.1875, 0.41667)`.
    """
    return (
        ((detection.x1 + detection.x2) / 2.0) / float(frame_w),
        detection.y2 / float(frame_h),
    )


@dataclass(frozen=True)
class Hit:
    """One zone rule satisfied by one track on one frame."""

    zone_id: str
    zone_name: str
    rule: str
    severity: int
    track_id: int
    detection: object
    class_name: str
    dwell_seconds: float = 0.0

    def as_object(self) -> dict:
        return self.detection.as_object()  # type: ignore[attr-defined]


class ZoneEvaluator:
    """Per-camera: it remembers who has been inside which loiter zone since when.

    `evaluate` is the plan's `evaluate(camera, tracks, frame_ts)`; the state a
    loiter needs lives on the evaluator rather than in a module-level dict, so two
    cameras never share a timer and a camera whose config changed can be reset.
    """

    def __init__(self) -> None:
        self._inside: dict[tuple[str, int], float] = {}
        self._warned_rules: set[str] = set()

    def reset(self) -> None:
        self._inside.clear()

    def evaluate(
        self,
        camera: "CameraConfig",
        tracks: Sequence,
        frame_ts: float,
        frame_wh: Optional[tuple[int, int]] = None,
    ) -> list[Hit]:
        """Every rule satisfied by these tracks on this frame, in a stable order."""
        frame_w, frame_h = _frame_size(camera, frame_wh)
        hits: list[Hit] = []
        seen: set[tuple[str, int]] = set()

        for track in tracks:
            detection = track.detection
            if detection.name in IGNORED_CLASSES:
                continue
            for zone in camera.zones:
                if not zone.enabled:
                    continue
                if zone.classes and detection.name not in zone.classes:
                    continue
                # The camera floors a zone may raise but never lower, so the
                # effective floor is the higher of the two.
                if detection.conf < max(zone.min_confidence, camera.sensitivity):
                    continue
                if detection.area_ratio(frame_w, frame_h) < max(zone.min_area_ratio, camera.min_area_ratio):
                    continue
                point = centroid(detection, frame_w, frame_h)
                if not point_in_polygon(point[0], point[1], zone.polygon):
                    continue

                seen.add((zone.id, track.id))

                if zone.rule == RULE_INTRUSION:
                    hits.append(_hit(zone, track, detection, dwell=0.0))
                elif zone.rule == RULE_LOITER:
                    key = (zone.id, track.id)
                    first = self._inside.get(key)
                    if first is None:
                        first = frame_ts
                        self._inside[key] = first
                    dwell = frame_ts - first
                    if dwell >= zone.dwell_seconds:
                        hits.append(_hit(zone, track, detection, dwell=dwell))
                elif zone.rule not in self._warned_rules:
                    self._warned_rules.add(zone.rule)
                    log.warning(
                        "zones: %s: zone %r has rule %r, which is not intrusion or loiter; "
                        "it can never fire",
                        camera.slug,
                        zone.name,
                        zone.rule,
                    )

        # A track that left every loiter zone this frame has its timer dropped:
        # presence has to be continuous, and "continuous" is measured here.
        for key in [k for k in self._inside if k not in seen]:
            del self._inside[key]

        hits.sort(key=lambda h: (h.zone_id, h.track_id, h.rule))
        return hits


def _hit(zone: "ZoneConfig", track, detection, dwell: float) -> Hit:
    return Hit(
        zone_id=zone.id,
        zone_name=zone.name,
        rule=zone.rule,
        severity=zone.severity,
        track_id=track.id,
        detection=detection,
        class_name=detection.name,
        dwell_seconds=dwell,
    )


def _frame_size(camera: "CameraConfig", frame_wh: Optional[tuple[int, int]]) -> tuple[int, int]:
    """The frame the boxes are measured in.

    The pipeline passes the frame it just inferred on, which is the honest answer.
    The config payload's `width`/`height` are the fallback, and a camera row that
    set neither gets the plan's detection tap (640×360) rather than a division by
    zero.
    """
    if frame_wh and frame_wh[0] > 0 and frame_wh[1] > 0:
        return int(frame_wh[0]), int(frame_wh[1])
    if camera.width > 0 and camera.height > 0:
        return int(camera.width), int(camera.height)
    return 640, 360


def _vertex(polygon: Sequence[Sequence[float]], index: int) -> tuple[float, float]:
    point = polygon[index]
    return float(point[0]), float(point[1])


def _on_segment(
    x: float, y: float, a: tuple[float, float], b: tuple[float, float], eps: float = 1e-12
) -> bool:
    ax, ay = a
    bx, by = b
    cross = (bx - ax) * (y - ay) - (by - ay) * (x - ax)
    if abs(cross) > eps:
        return False
    return (
        min(ax, bx) - eps <= x <= max(ax, bx) + eps
        and min(ay, by) - eps <= y <= max(ay, by) + eps
    )
