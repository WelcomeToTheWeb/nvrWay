"""S5-07: the CPU motion gate.

The gate exists so a camera pointed at an empty driveway costs a thumbnail
comparison instead of a 640×640 inference pass. That makes it the one stage in the
pipeline that is allowed to *drop* a frame, so its numbers need tests: a gate that
is too eager misses intrusions, and a gate that is too loose is not a gate.
"""

from __future__ import annotations

import numpy as np

from nvr_detector.motion import MOTION_RATIO, MOTION_THRESHOLD, MOTION_WIDTH, MotionGate

from conftest import make_frame

# 160×90 is the size the gate compares at, so the arithmetic in these tests is the
# arithmetic the gate actually does: 1% of 14 400 px is 144 px.
W, H = 160, 90
PIXELS = W * H


def with_block(value: int = 255, x: int = 10, y: int = 10, size: int = 20) -> np.ndarray:
    frame = make_frame(width=W, height=H, value=0)
    frame[y : y + size, x : x + size] = value
    return frame


def test_the_gate_measures_a_160_px_wide_grayscale_thumbnail():
    """The whole point of the gate is that the comparison is ~14 400 px, not
    2 073 600. Assert the size, or a future change to the resize silently turns the
    CPU saver into the CPU hog."""
    gate = MotionGate()
    thumbnail = gate._thumbnail(np.zeros((1080, 1920, 3), dtype=np.uint8))
    assert thumbnail.shape == (90, 160)
    assert thumbnail.ndim == 2, "grayscale: three channels would triple the comparison for nothing"
    assert MOTION_WIDTH == 160


def test_the_first_frame_is_always_inferred():
    """No previous frame is not evidence of an empty scene — it is the absence of a
    scene to compare against."""
    gate = MotionGate()
    assert gate.should_infer(make_frame(width=W, height=H)) is True


def test_an_identical_frame_is_skipped():
    gate = MotionGate()
    frame = with_block()
    assert gate.should_infer(frame) is True
    assert gate.should_infer(frame.copy()) is False
    assert gate.should_infer(frame.copy()) is False
    assert gate.skipped == 2


def test_a_moved_block_is_inferred():
    """The case the gate must never swallow: the same amount of change, somewhere
    else. A person walking is a moved block, and a gate keyed on "how much of the
    frame is bright" rather than "how much of it differs" would miss them."""
    gate = MotionGate()
    assert gate.should_infer(with_block(x=10, y=10)) is True
    assert gate.should_infer(with_block(x=80, y=50)) is True
    assert gate.skipped == 0


def test_a_change_smaller_than_one_percent_of_the_thumbnail_is_skipped():
    """10×10 px of 14 400 is 0.69%, under the plan's 1%. A branch moving in the wind
    is not an event, and at 5 fps it would otherwise keep an event open forever."""
    gate = MotionGate()
    assert gate.should_infer(make_frame(width=W, height=H, value=0)) is True
    assert gate.should_infer(with_block(size=10)) is False
    assert gate.last_ratio < MOTION_RATIO


def test_a_change_bigger_than_one_percent_is_inferred():
    gate = MotionGate()
    assert gate.should_infer(make_frame(width=W, height=H, value=0)) is True
    assert gate.should_infer(with_block(size=20)) is True
    assert gate.last_ratio > MOTION_RATIO


def test_a_level_change_at_the_threshold_does_not_count():
    """The difference has to exceed 17, so a slow lighting change of 17 levels across
    the whole frame is still "nothing happened" — and 18 is not."""
    at_threshold = MotionGate()
    at_threshold.should_infer(make_frame(width=W, height=H, value=100))
    assert at_threshold.should_infer(make_frame(width=W, height=H, value=100 + MOTION_THRESHOLD)) is False

    over_threshold = MotionGate()
    over_threshold.should_infer(make_frame(width=W, height=H, value=100))
    assert over_threshold.should_infer(make_frame(width=W, height=H, value=100 + MOTION_THRESHOLD + 1)) is True


def test_the_constants_are_the_plans():
    assert MOTION_THRESHOLD == 17
    assert MOTION_RATIO == 0.010


def test_a_disabled_gate_infers_every_frame():
    """An `events` camera's frames are the trigger: gating them could skip the very
    frame that opens the recording window."""
    gate = MotionGate(enabled=False)
    frame = with_block()
    assert gate.should_infer(frame) is True
    assert gate.should_infer(frame.copy()) is True
    assert gate.should_infer(frame.copy()) is True
    assert gate.skipped == 0


def test_switching_the_gate_off_drops_the_comparison_state():
    """Re-enabling mid-stream must not compare the first new frame against a frame
    from before the switch — that is how a camera that was covered by a hand for a
    minute ends up inferring on the frame that uncovered it, which is correct, but
    the frame *after* must not be compared against the covered one either."""
    gate = MotionGate()
    gate.should_infer(make_frame(width=W, height=H, value=0))
    gate.set_enabled(False)
    assert gate.should_infer(make_frame(width=W, height=H, value=200)) is True
    gate.set_enabled(True)
    assert gate.should_infer(make_frame(width=W, height=H, value=200)) is True, "no stale previous frame"
    assert gate.should_infer(make_frame(width=W, height=H, value=200)) is False


def test_the_counters_are_kept_for_the_log():
    gate = MotionGate()
    gate.should_infer(make_frame(width=W, height=H, value=0))
    gate.should_infer(make_frame(width=W, height=H, value=0))
    gate.should_infer(make_frame(width=W, height=H, value=255))
    assert (gate.passed, gate.skipped) == (2, 1)
