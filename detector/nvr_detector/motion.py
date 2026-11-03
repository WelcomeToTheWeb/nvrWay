"""The CPU gate: skip inference when nothing moved.

On a 4 vCPU box running YOLOv8 for four cameras, inference is the cost. A camera
looking at an empty driveway at 5 FPS spends every one of those frames proving
nothing is there, so the gate downscales hard, compares against the previous
frame, and skips the model when the difference is under 1% of the pixels.

It is deliberately coarse: 160 px wide, grayscale, a fixed threshold. The gate is
not trying to detect anything — it is trying to be much cheaper than the model and
wrong rarely enough that a real event is never missed. A 16×16 block that moved is
~1.6% of a 160×90 frame, which is over the line; a leaf-blowing tree that changes
10% of it is under the line's cost anyway, because the model then finds nothing
and the frame was cheap to skip in the first place.

**The gate never runs on an `events` camera.** That mode records only while an
event window is open, so the frames it is being asked about *are* the trigger: a
gate that skipped a quiet frame could skip the first frame of the event that was
supposed to open the window. The pipeline hands it `enabled=False` and the gate
says yes to every frame.
"""

from __future__ import annotations

import logging

import cv2
import numpy as np

log = logging.getLogger("nvr_detector.motion")

# The plan's gate, as numbers with names.
MOTION_WIDTH = 160
MOTION_THRESHOLD = 17
MOTION_RATIO = 0.010


class MotionGate:
    """Answers whether this frame is worth running the model on.

    The first frame is always worth it: there is nothing to compare it against,
    and "no previous frame" is not evidence of an empty scene.
    """

    def __init__(
        self,
        width: int = MOTION_WIDTH,
        threshold: int = MOTION_THRESHOLD,
        ratio: float = MOTION_RATIO,
        enabled: bool = True,
    ) -> None:
        self.width = int(width)
        self.threshold = int(threshold)
        self.ratio = float(ratio)
        self.enabled = bool(enabled)
        self._previous: np.ndarray | None = None
        self.last_ratio = 0.0
        self.skipped = 0
        self.passed = 0

    def set_enabled(self, enabled: bool) -> None:
        """Turn the gate on or off — off means every frame is inferred on, and the
        comparison state is dropped so the first frame after the switch is not
        compared against a frame from before it."""
        if self.enabled != bool(enabled):
            self.enabled = bool(enabled)
            self._previous = None

    def should_infer(self, frame: np.ndarray) -> bool:
        """True means "run the model". False means "this frame is the same as the
        last one, and the last one was already looked at"."""
        if not self.enabled:
            self.passed += 1
            return True

        small = self._thumbnail(frame)
        previous = self._previous
        self._previous = small
        if previous is None or previous.shape != small.shape:
            self.passed += 1
            return True

        diff = cv2.absdiff(small, previous)
        _, mask = cv2.threshold(diff, self.threshold, 255, cv2.THRESH_BINARY)
        changed = float(cv2.countNonZero(mask)) / float(mask.size)
        self.last_ratio = changed

        if changed < self.ratio:
            self.skipped += 1
            return False
        self.passed += 1
        return True

    def reset(self) -> None:
        self._previous = None

    def _thumbnail(self, frame: np.ndarray) -> np.ndarray:
        gray = cv2.cvtColor(frame, cv2.COLOR_BGR2GRAY)
        height, width = gray.shape
        if width <= self.width:
            return gray
        scale = self.width / float(width)
        return cv2.resize(
            gray,
            (self.width, max(1, int(round(height * scale)))),
            interpolation=cv2.INTER_AREA,
        )
