"""S5-06: the open/extend/close state machine, and the snapshot that rides on the open.

The `event_key` is the load-bearing part: the server dedupes on it, so a worker
that re-mints the key on every signal turns one intrusion into a dozen rows. These
tests assert the key is minted once and reused, and that the close that ends it is
decided by the *absence* of hits — which is the half of a state machine a test suite
usually forgets.
"""

from __future__ import annotations

import base64
from datetime import datetime, timezone

import cv2
import numpy as np
import pytest

from nvr_detector.events import (
    ACTION_CLOSE,
    ACTION_EXTEND,
    ACTION_OPEN,
    DEFAULT_EVENT_MAX_SECONDS,
    IDLE_CLOSE_SECONDS,
    SIGNAL_KEYS,
    SNAPSHOT_MAX_BYTES,
    SNAPSHOT_QUALITY,
    SNAPSHOT_WIDTH,
    EventMachine,
    rfc3339,
    scale_to_width,
    snapshot_jpeg,
)
from nvr_detector.model import CLASS_NAMES
from nvr_detector.zones import Hit

from conftest import detection

CAMERA_ID = "cam-1"


def hit(zone_id: str = "z1", zone_name: str = "driveway", rule: str = "intrusion", severity: int = 3, cls: str = "person", track_id: int = 1) -> Hit:
    det = detection(cls=cls)
    return Hit(
        zone_id=zone_id,
        zone_name=zone_name,
        rule=rule,
        severity=severity,
        track_id=track_id,
        detection=det,
        class_name=CLASS_NAMES[CLASS_NAMES.index(cls)],
    )


def machine(event_max_seconds: int = DEFAULT_EVENT_MAX_SECONDS, snapshot: bool = True, **kwargs) -> EventMachine:
    return EventMachine(
        camera_id=CAMERA_ID, record_mode="continuous", event_max_seconds=event_max_seconds, snapshot=snapshot, **kwargs
    )


def noise_frame(width: int = 1280, height: int = 720) -> np.ndarray:
    """The worst case a JPEG encoder meets: every pixel uncorrelated with its
    neighbour, so the bytes do not shrink."""
    rng = np.random.default_rng(7)
    return rng.integers(0, 256, size=(height, width, 3), dtype=np.uint8)


def decode(signal: dict) -> bytes:
    return base64.b64decode(signal["snapshot_base64"])


# -- the key and the shape of a signal -------------------------------------


def test_the_first_hit_opens_an_event_keyed_by_camera_zone_rule_and_open_time():
    signals = machine().transition([hit()], noise_frame(640, 360), 1000.0)
    assert len(signals) == 1
    signal = signals[0]
    assert signal["action"] == ACTION_OPEN
    assert signal["event_key"] == "cam-1:z1:intrusion:1000"
    assert signal["camera_id"] == CAMERA_ID
    assert signal["zone_id"] == "z1" and signal["zone_name"] == "driveway"
    assert signal["rule"] == "intrusion"
    assert signal["severity"] == 3
    assert signal["classes"] == ["person"]
    assert signal["record_mode"] == "continuous"


def test_a_signal_carries_exactly_the_plans_keys():
    signal = machine().transition([hit()], noise_frame(640, 360), 1000.0)[0]
    assert tuple(signal.keys()) == SIGNAL_KEYS


def test_the_key_is_minted_once_and_every_later_signal_carries_the_same_one():
    """This is what makes a re-sent signal after a dropped connection one event
    instead of two."""
    m = machine()
    opened = m.transition([hit()], None, 1000.0)[0]
    extended = m.transition([hit()], None, 1005.0)[0]
    closed = m.transition([], None, 1020.0)[0]
    assert opened["action"] == ACTION_OPEN
    assert extended["action"] == ACTION_EXTEND
    assert closed["action"] == ACTION_CLOSE
    assert opened["event_key"] == extended["event_key"] == closed["event_key"] == "cam-1:z1:intrusion:1000"


def test_the_timestamp_is_an_rfc3339_utc_string_the_server_can_parse():
    """The server validates the shape and then uses its own clock, but a timestamp
    it cannot parse is a signal it refuses outright."""
    signal = machine().transition([hit()], None, 1000.5)[0]
    parsed = datetime.fromisoformat(signal["ts"])
    assert parsed.tzinfo is not None
    assert parsed == datetime.fromtimestamp(1000.5, tz=timezone.utc)
    assert rfc3339(0.0).startswith("1970-01-01T00:00:00")


def test_the_objects_array_is_the_shape_the_event_row_stores():
    signal = machine().transition([hit()], None, 1000.0)[0]
    assert signal["objects"] == [
        {"class": "person", "conf": pytest.approx(0.9), "box": [100.0, 50.0, 140.0, 150.0]}
    ]


def test_the_severity_is_the_highest_of_the_hits_that_opened_it():
    signals = machine().transition(
        [hit(zone_id="z1", severity=1), hit(zone_id="z1", severity=4, track_id=2)], None, 1000.0
    )
    assert len(signals) == 1, "two tracks in one zone are one event"
    assert signals[0]["severity"] == 4
    assert len(signals[0]["objects"]) == 2


def test_classes_accumulate_across_extends_without_repeating():
    m = machine()
    m.transition([hit(cls="person")], None, 1000.0)
    m.transition([hit(cls="person")], None, 1005.0)
    signals = m.transition([hit(cls="car")], None, 1010.0)
    assert sorted(signals[0]["classes"]) == ["car", "person"]


# -- closing ---------------------------------------------------------------


def test_an_event_closes_once_nothing_has_satisfied_it_for_ten_seconds():
    m = machine()
    m.transition([hit()], None, 1000.0)
    m.transition([hit()], None, 1005.0)
    assert m.transition([], None, 1010.0) == [], "five idle seconds is not a closed event"
    assert m.open_count == 1
    closed = m.transition([], None, 1015.0)
    assert len(closed) == 1 and closed[0]["action"] == ACTION_CLOSE
    assert m.open_count == 0


def test_the_idle_window_is_the_plans_ten_seconds():
    assert IDLE_CLOSE_SECONDS == 10.0
    assert DEFAULT_EVENT_MAX_SECONDS == 300


def test_an_event_still_active_past_event_max_seconds_is_closed_anyway():
    """A person who stands in the driveway forever is one event for five minutes and
    then a new one: an event that never ends is a clip nobody assembles."""
    m = machine(event_max_seconds=20)
    m.transition([hit()], None, 1000.0)
    m.transition([hit()], None, 1010.0)
    signals = m.transition([hit()], None, 1020.0)
    assert [s["action"] for s in signals] == [ACTION_EXTEND, ACTION_CLOSE]
    assert signals[1]["event_key"] == "cam-1:z1:intrusion:1000"
    assert m.open_count == 0

    # The next hit is a new occurrence, not a signal for an event the server has
    # already closed — a post-close extend would be refused with "event closed".
    reopened = m.transition([hit()], None, 1025.0)
    assert [s["action"] for s in reopened] == [ACTION_OPEN]
    assert reopened[0]["event_key"] == "cam-1:z1:intrusion:1025"


def test_a_hit_after_a_close_opens_a_new_event_under_a_new_key():
    m = machine()
    first = m.transition([hit()], None, 1000.0)[0]["event_key"]
    m.transition([], None, 1015.0)
    second = m.transition([hit()], None, 1030.0)[0]
    assert second["action"] == ACTION_OPEN
    assert second["event_key"] == "cam-1:z1:intrusion:1030"
    assert second["event_key"] != first


def test_two_zones_open_two_events_and_each_closes_on_its_own_schedule():
    m = machine()
    signals = m.transition([hit(zone_id="z1"), hit(zone_id="z2", zone_name="gate")], None, 1000.0)
    assert [s["event_key"] for s in signals] == ["cam-1:z1:intrusion:1000", "cam-1:z2:intrusion:1000"]
    closed = m.transition([hit(zone_id="z2", zone_name="gate")], None, 1015.0)
    assert [s["event_key"] for s in closed if s["action"] == ACTION_CLOSE] == [
        "cam-1:z1:intrusion:1000"
    ], "only the idle one closes; the other is still being hit"


def test_close_all_ends_every_open_event():
    """SIGTERM. An event left open in the database is an event whose clip nobody
    scheduled, which is the difference between a gap and a missing recording."""
    m = machine()
    m.transition([hit(zone_id="z1"), hit(zone_id="z2", zone_name="gate")], None, 1000.0)
    assert m.open_count == 2
    closed = m.close_all(1002.0)
    assert [s["action"] for s in closed] == [ACTION_CLOSE, ACTION_CLOSE]
    assert [s["event_key"] for s in closed] == ["cam-1:z1:intrusion:1000", "cam-1:z2:intrusion:1000"]
    assert m.open_count == 0
    assert m.close_all(1003.0) == [], "closing twice does not emit twice"


# -- the snapshot ----------------------------------------------------------


def test_the_snapshot_rides_on_the_open_alone():
    m = machine()
    opened = m.transition([hit()], noise_frame(640, 360), 1000.0)[0]
    extended = m.transition([hit()], noise_frame(640, 360), 1005.0)[0]
    closed = m.transition([], noise_frame(640, 360), 1020.0)[0]
    assert opened["snapshot_base64"]
    assert extended["snapshot_base64"] == ""
    assert closed["snapshot_base64"] == ""


def test_the_snapshot_is_the_frame_that_triggered_the_event():
    raw = decode(machine().transition([hit()], noise_frame(640, 360), 1000.0)[0])
    assert raw[:2] == b"\xff\xd8", "the snapshot has to be a JPEG the console can show"
    image = cv2.imdecode(np.frombuffer(raw, dtype=np.uint8), cv2.IMREAD_COLOR)
    assert image is not None and image.shape == (360, 640, 3)


def test_a_large_frame_is_scaled_to_640_wide_before_it_is_encoded():
    signal = machine().transition([hit()], noise_frame(1280, 720), 1000.0)[0]
    raw = decode(signal)
    assert len(raw) <= SNAPSHOT_MAX_BYTES, f"snapshot is {len(raw)} bytes, over the cap"
    image = cv2.imdecode(np.frombuffer(raw, dtype=np.uint8), cv2.IMREAD_COLOR)
    assert image.shape[1] == SNAPSHOT_WIDTH


def test_a_small_frame_is_not_blown_up():
    """Up-scaling a 320 px tap to 640 costs bytes and adds nothing the camera saw."""
    scaled = scale_to_width(np.zeros((180, 320, 3), dtype=np.uint8))
    assert scaled.shape == (180, 320, 3)


def test_a_frame_that_will_not_compress_is_still_sent_and_says_so(caplog):
    """The cap is a budget, not a veto. A snapshot that is a little blocky is still
    proof; a snapshot dropped because it was large is a gap in the console."""
    raw = snapshot_jpeg(noise_frame(1280, 720), max_bytes=2048)
    assert raw[:2] == b"\xff\xd8"
    assert len(raw) > 2048
    assert "over the" in caplog.text and "sending it anyway" in caplog.text


def test_the_snapshot_quality_is_the_plans():
    assert SNAPSHOT_QUALITY == 82


def test_an_event_survives_a_snapshot_that_fails(caplog, monkeypatch):
    """A broken encoder costs the operator a picture, not the event itself."""
    monkeypatch.setattr("nvr_detector.events.snapshot_jpeg", lambda *a, **k: (_ for _ in ()).throw(RuntimeError("no encoder")))
    signals = machine().transition([hit()], noise_frame(640, 360), 1000.0)
    assert signals[0]["action"] == ACTION_OPEN
    assert signals[0]["snapshot_base64"] == ""
    assert "snapshot failed" in caplog.text
