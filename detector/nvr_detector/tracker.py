"""A greedy IoU tracker: the same person keeps the same id.

The loiter rule is per `track_id` — "this *one* has been standing in the driveway
for 40 seconds" — so the tracker is not a nicety on top of detection, it *is* the
loiter contract. A tracker that hands a stationary person a new id every other
frame turns every loiter into an intrusion and back again.

The matching is greedy and best-first, which is what the plan names: compute every
pair's IoU, take the highest-scoring pair, repeat. Optimal assignment (Hungarian)
would cost more and buy nothing at 4–8 cameras × 5 FPS × a handful of boxes.

Two numbers are the whole policy:

*   **IoU >= 0.30 matches.** Below that the two boxes are not the same object;
    above it a person who moved a little between frames stays themselves.
*   **Six frames of nothing, then expiry.** A person occluded by a van for a
    second (six frames at 5 FPS) resumes the same id; someone who comes back a
    minute later is a new track, because nothing can honestly claim they are the
    same instance.
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .model import Detection, box_iou

# The plan's matching floor and its grace window, in frames.
IOU_THRESHOLD = 0.30
MAX_AGE_FRAMES = 6


@dataclass
class Track:
    """One tracked instance. `detection` is the last box that matched it, so a
    track in its grace window still carries a usable position."""

    id: int
    detection: Detection
    hits: int = 1
    missed: int = 0
    first_frame: int = 0
    last_frame: int = 0

    @property
    def name(self) -> str:
        return self.detection.name

    @property
    def box(self) -> tuple[float, float, float, float]:
        det = self.detection
        return det.x1, det.y1, det.x2, det.y2

    def as_object(self) -> dict:
        return self.detection.as_object()


@dataclass
class Tracker:
    """Per-camera. Ids are monotonic for the life of the instance, which is the
    life of the camera's pipeline thread."""

    iou_threshold: float = IOU_THRESHOLD
    max_age_frames: int = MAX_AGE_FRAMES

    _next_id: int = field(default=1, init=False)
    _frame: int = field(default=0, init=False)
    _tracks: dict[int, Track] = field(default_factory=dict, init=False)

    def update(self, detections: list[Detection]) -> list[Track]:
        """Match this frame's boxes to the live tracks.

        Returns every live track — matched, newly spawned, and still inside its
        grace window — sorted by id, so a caller's output order does not depend on
        dict iteration.
        """
        self._frame += 1
        boxes = np.array([d.as_box() for d in detections], dtype=np.float64) if detections else np.zeros((0, 4))

        matched_tracks: set[int] = set()
        matched_dets: set[int] = set()

        for track_index, det_index, iou in self._pairs(boxes):
            if track_index in matched_tracks or det_index in matched_dets:
                continue
            if iou < self.iou_threshold:
                break
            self._absorb(self._tracks[track_index], detections[det_index])
            matched_tracks.add(track_index)
            matched_dets.add(det_index)

        for det_index, det in enumerate(detections):
            if det_index not in matched_dets:
                self._spawn(det)

        for track in self._tracks.values():
            # A track spawned or matched on this frame has been seen this frame; it
            # is not also "missed". Counting it both ways ages every track a frame
            # early, which turns a six-frame gap into a five-frame one.
            if track.id not in matched_tracks and track.last_frame != self._frame:
                track.missed += 1

        # Expire on the frame *after* the grace window runs out, so a gap of
        # exactly max_age_frames still resumes the same id.
        for track_id in [t.id for t in self._tracks.values() if t.missed > self.max_age_frames]:
            del self._tracks[track_id]

        return sorted(self._tracks.values(), key=lambda t: t.id)

    def reset(self) -> None:
        """Forget every track. A camera whose zones were rewritten, or whose
        stream restarted, should not inherit ids from the run that ended."""
        self._tracks.clear()

    @property
    def live(self) -> int:
        return len(self._tracks)

    def _pairs(self, boxes: np.ndarray) -> list[tuple[int, int, float]]:
        """Every track/detection pair above the floor, best IoU first."""
        if not self._tracks or boxes.size == 0:
            return []
        tracks = sorted(self._tracks.values(), key=lambda t: t.id)
        pairs: list[tuple[int, int, float]] = []
        for track in tracks:
            ious = box_iou(boxes, np.array(track.box, dtype=np.float64))
            for det_index, iou in enumerate(ious):
                if iou >= self.iou_threshold:
                    pairs.append((track.id, det_index, float(iou)))
        pairs.sort(key=lambda p: p[2], reverse=True)
        return pairs

    def _absorb(self, track: Track, det: Detection) -> None:
        track.detection = det
        track.hits += 1
        track.missed = 0
        track.last_frame = self._frame

    def _spawn(self, det: Detection) -> None:
        track = Track(
            id=self._next_id,
            detection=det,
            first_frame=self._frame,
            last_frame=self._frame,
        )
        self._next_id += 1
        self._tracks[track.id] = track
