"""S5-04: the greedy IoU tracker.

Identity is what makes an intrusion one event instead of five. These tests are
written around the two ways that goes wrong: an id that dies when a detection
flickers for a second, and two ids that swap when their boxes cross.
"""

from __future__ import annotations

from nvr_detector.tracker import IOU_THRESHOLD, MAX_AGE_FRAMES, Tracker

from conftest import detection


def shifted(offset: float) -> tuple[float, float, float, float]:
    return (100.0 + offset, 50.0, 140.0 + offset, 150.0)


def test_a_slowly_moving_person_keeps_one_id_for_five_frames():
    tracker = Tracker()
    ids = []
    for frame in range(5):
        tracks = tracker.update([detection(box=shifted(frame * 4.0))])
        assert len(tracks) == 1
        ids.append(tracks[0].id)
    assert len(set(ids)) == 1, f"one person, five ids: {ids}"
    assert tracks[0].hits == 5


def test_a_detection_that_flickers_for_six_frames_resumes_the_same_id():
    """The plan's expiry: six frames without a match is still the same person. At
    5 fps that is 1.2 s, which is roughly how long a person is occluded by a doorframe."""
    tracker = Tracker()
    first = tracker.update([detection()])[0].id
    for frame in range(1, MAX_AGE_FRAMES + 1):
        live = tracker.update([])
        assert len(live) == 1, "a track inside its grace window is still live, so a zone still sees it"
        assert live[0].missed == frame, "and it ages exactly one frame per frame"

    resumed = tracker.update([detection()])
    assert [t.id for t in resumed] == [first], "a six-frame gap must not open a second event"
    assert resumed[0].missed == 0


def test_a_detection_that_flickers_for_seven_frames_starts_a_new_id():
    tracker = Tracker()
    first = tracker.update([detection()])[0].id
    for _ in range(MAX_AGE_FRAMES + 1):
        tracker.update([])
    assert tracker.live == 0, "the grace window ran out and the track expired"
    after = tracker.update([detection()])
    assert after[0].id != first
    assert after[0].id > first, "ids are monotonic per camera, so a new one is never reused"


def test_a_box_that_jumped_across_the_frame_is_a_different_object():
    """IoU below the plan's 0.30 does not match, however confident both boxes are."""
    tracker = Tracker()
    left = tracker.update([detection(box=(0.0, 0.0, 100.0, 100.0))])[0].id
    tracks = tracker.update([detection(box=(500.0, 0.0, 600.0, 100.0))])
    assert len(tracks) == 2, "the old track survives its grace period; the jump is a new id"
    assert {t.id for t in tracks} == {left, tracks[1].id}
    assert tracks[1].id != left


def test_two_tracks_that_cross_keep_their_ids():
    """A at (0,0,100,100) and B at (40,0,140,100) overlap each other at IoU 0.43, so
    a matcher that takes detections in arrival order hands A the box that belongs to
    B and the two people trade identities mid-crossing.

    Matching the best pair first — (d1,B) at IoU 1.0 and (d2,A) at IoU 1.0 — keeps
    each id on its own trajectory, which is the whole point of the greedy order.
    """
    tracker = Tracker()
    id_a = tracker.update([detection(box=(0.0, 0.0, 100.0, 100.0))])[0].id
    id_b = tracker.update([detection(box=(0.0, 0.0, 100.0, 100.0)), detection(box=(40.0, 0.0, 140.0, 100.0))])[1].id

    tracks = tracker.update([detection(box=(40.0, 0.0, 140.0, 100.0)), detection(box=(0.0, 0.0, 100.0, 100.0))])
    by_id = {t.id: t.detection.as_box() for t in tracks}
    assert set(by_id) == {id_a, id_b}, f"the crossing spawned a third id: {sorted(by_id)}"
    assert by_id[id_a] == (0.0, 0.0, 100.0, 100.0)
    assert by_id[id_b] == (40.0, 0.0, 140.0, 100.0)


def test_two_people_standing_apart_get_two_ids():
    tracker = Tracker()
    tracks = tracker.update(
        [
            detection(box=(0.0, 0.0, 100.0, 100.0)),
            detection(box=(300.0, 0.0, 400.0, 100.0)),
        ]
    )
    assert len(tracks) == 2
    assert len({t.id for t in tracks}) == 2


def test_one_detection_cannot_be_claimed_by_two_tracks():
    tracker = Tracker()
    first = tracker.update([detection(box=(0.0, 0.0, 100.0, 100.0))])[0].id
    # A second track that happens to sit on the same box: one detection, one owner.
    tracks = tracker.update([detection(box=(0.0, 0.0, 100.0, 100.0))])
    assert len(tracks) == 1
    assert tracks[0].id == first


def test_the_thresholds_are_the_plans():
    assert IOU_THRESHOLD == 0.30
    assert MAX_AGE_FRAMES == 6


def test_ids_do_not_repeat_within_a_camera_even_across_lifetimes():
    tracker = Tracker()
    seen = set()
    for cycle in range(4):
        track = tracker.update([detection(box=(0.0, 0.0, 100.0, 100.0))])[0]
        seen.add(track.id)
        for _ in range(MAX_AGE_FRAMES + 1):
            tracker.update([])
    assert len(seen) == 4, f"ids were reused: {sorted(seen)}"
