"""S5-05: point-in-polygon, the bottom-center point, intrusion and loiter.

The polygon tests are written on a U-shape because a convex shape cannot tell a
ray cast from a bounding box. The centroid test is written on the plan's own box.
"""

from __future__ import annotations

import pytest

from nvr_detector.tracker import Track
from nvr_detector.zones import RULE_INTRUSION, RULE_LOITER, ZoneEvaluator, centroid, point_in_polygon

from conftest import FULL_FRAME_POLYGON, camera_cfg, detection, zone

U_POLYGON = [[0.0, 0.0], [1.0, 0.0], [1.0, 1.0], [0.7, 1.0], [0.7, 0.3], [0.3, 0.3], [0.3, 1.0], [0.0, 1.0]]
SQUARE = [[0.0, 0.0], [1.0, 0.0], [1.0, 1.0], [0.0, 1.0]]


def tracks_for(*dets, start_id: int = 1) -> list[Track]:
    return [Track(id=start_id + i, detection=det) for i, det in enumerate(dets)]


def evaluate(evaluator: ZoneEvaluator, camera, dets, ts: float, frame_wh=(640, 360)):
    return evaluator.evaluate(camera, tracks_for(*dets), ts, frame_wh=frame_wh)


# -- point_in_polygon ------------------------------------------------------


def test_a_concave_polygon_puts_the_notch_outside_and_the_arms_inside():
    """The U is the frame minus x∈[0.3,0.7], y∈[0.3,1]. A bounding-box test would
    call the notch inside, which is the difference between "in the driveway" and
    "on the path that leads to it"."""
    assert point_in_polygon(0.15, 0.5, U_POLYGON) is True
    assert point_in_polygon(0.85, 0.5, U_POLYGON) is True
    assert point_in_polygon(0.5, 0.15, U_POLYGON) is True
    assert point_in_polygon(0.5, 0.5, U_POLYGON) is False
    assert point_in_polygon(0.5, 0.95, U_POLYGON) is False


def test_a_point_on_an_edge_counts_as_inside():
    """An operator who draws a line through the middle of their driveway expects the
    line itself to be part of the zone. Parity alone would make that depend on
    vertex order."""
    for point in ((0.0, 0.5), (1.0, 0.5), (0.5, 0.0), (0.5, 1.0)):
        assert point_in_polygon(point[0], point[1], SQUARE) is True, f"{point} is on the boundary"
    # And a vertex, which is the case that breaks naive ray casts.
    assert point_in_polygon(0.0, 0.0, SQUARE) is True


def test_a_point_outside_is_outside_in_every_direction():
    for point in ((-0.01, 0.5), (1.01, 0.5), (0.5, -0.01), (0.5, 1.01), (2.0, 2.0)):
        assert point_in_polygon(point[0], point[1], SQUARE) is False, f"{point} is outside"


def test_a_diagonal_edge_still_counts_its_own_line_as_inside():
    triangle = [[0.0, 0.0], [1.0, 0.0], [0.0, 1.0]]
    assert point_in_polygon(0.5, 0.5, triangle) is True  # on the hypotenuse
    assert point_in_polygon(0.6, 0.6, triangle) is False
    assert point_in_polygon(0.2, 0.2, triangle) is True


def test_a_shape_that_is_not_a_polygon_is_never_inside():
    assert point_in_polygon(0.5, 0.5, []) is False
    assert point_in_polygon(0.5, 0.5, [[0.0, 0.0], [1.0, 1.0]]) is False


# -- centroid --------------------------------------------------------------


def test_the_centroid_is_the_bottom_center_of_the_box():
    """The plan's example box [100,50,140,150] on a 640×360 frame.

    Its formula is `((x1+x2)/2, y2)` normalised by frame size, which gives
    (120/640, 150/360) = (0.1875, 0.41667). The plan's own test example prints
    (0.21875, 0.4167) — that x is `x2/640`, not the midpoint, so the formula wins
    and the slip is recorded on card S5-05.
    """
    x, y = centroid(detection(box=(100.0, 50.0, 140.0, 150.0)), 640, 360)
    assert x == pytest.approx(0.1875)
    assert y == pytest.approx(0.4166666667)


def test_the_centroid_follows_the_feet_not_the_torso():
    """A person leaning in from the top of the frame has their feet lower down; the
    point that has to be in the zone is the one touching the ground."""
    standing = detection(box=(100.0, 0.0, 140.0, 300.0))
    x, y = centroid(standing, 640, 360)
    assert y == pytest.approx(300.0 / 360.0)
    assert y > 0.8, "a box that fills the frame counts at its bottom edge, not its middle"


# -- intrusion -------------------------------------------------------------


def test_intrusion_fires_when_the_feet_are_inside_the_zone():
    evaluator = ZoneEvaluator()
    camera = camera_cfg(zones=[zone(rule=RULE_INTRUSION, polygon=FULL_FRAME_POLYGON)])
    hits = evaluate(evaluator, camera, [detection()], 1000.0)
    assert len(hits) == 1
    hit = hits[0]
    assert (hit.zone_id, hit.rule, hit.severity) == ("z1", RULE_INTRUSION, 3)
    assert hit.class_name == "person"
    assert hit.dwell_seconds == 0.0


def test_intrusion_does_not_fire_when_the_point_is_outside():
    evaluator = ZoneEvaluator()
    right_half = [[0.5, 0.0], [1.0, 0.0], [1.0, 1.0], [0.5, 1.0]]
    camera = camera_cfg(zones=[zone(polygon=right_half)])
    assert evaluate(evaluator, camera, [detection(box=(100.0, 50.0, 140.0, 150.0))], 1000.0) == []
    # The same box shifted right of x=320 is inside.
    assert len(evaluate(evaluator, camera, [detection(box=(400.0, 50.0, 440.0, 150.0))], 1001.0)) == 1


def test_a_zone_that_named_a_class_ignores_the_others():
    evaluator = ZoneEvaluator()
    camera = camera_cfg(zones=[zone(classes=["person"])])
    assert evaluate(evaluator, camera, [detection(cls="car")], 1000.0) == []
    assert len(evaluate(evaluator, camera, [detection(cls="person")], 1000.0)) == 1


def test_the_effective_confidence_floor_is_the_higher_of_zone_and_camera():
    """A zone may raise a floor the operator set on the camera; it may not lower it.
    Otherwise one careless zone edit would silence a camera the operator had just
    made more sensitive."""
    camera = camera_cfg(sensitivity=0.45, zones=[zone(min_confidence=0.30)])
    evaluator = ZoneEvaluator()
    assert evaluate(evaluator, camera, [detection(conf=0.35)], 1000.0) == []
    assert len(evaluate(evaluator, camera, [detection(conf=0.50)], 1000.0)) == 1

    lenient = camera_cfg(sensitivity=0.25, zones=[zone(min_confidence=0.60)])
    assert evaluate(ZoneEvaluator(), lenient, [detection(conf=0.50)], 1000.0) == []


def test_the_effective_area_floor_is_the_higher_of_zone_and_camera():
    camera = camera_cfg(min_area_ratio=0.004, zones=[zone(min_area_ratio=0.0)])
    tiny = detection(box=(100.0, 50.0, 108.0, 58.0))  # 8×8 px of a 640×360 frame
    assert evaluate(ZoneEvaluator(), camera, [tiny], 1000.0) == []
    assert len(evaluate(ZoneEvaluator(), camera, [detection()], 1000.0)) == 1


def test_a_disabled_zone_never_fires():
    camera = camera_cfg(zones=[zone(enabled=False)])
    assert evaluate(ZoneEvaluator(), camera, [detection()], 1000.0) == []


def test_an_unknown_rule_is_reported_once_and_never_fires(caplog):
    camera = camera_cfg(zones=[zone(rule="flibbertigibbet")])
    evaluator = ZoneEvaluator()
    for _ in range(3):
        assert evaluate(evaluator, camera, [detection()], 1000.0) == []
    assert caplog.text.count("flibbertigibbet") == 1, "a rule nothing can fire is logged once, not per frame"


def test_two_zones_on_one_camera_can_both_fire():
    camera = camera_cfg(
        zones=[
            zone(zone_id="z1", name="driveway", polygon=FULL_FRAME_POLYGON),
            zone(zone_id="z2", name="gate", polygon=[[0.0, 0.0], [0.5, 0.0], [0.5, 1.0], [0.0, 1.0]]),
        ]
    )
    hits = evaluate(ZoneEvaluator(), camera, [detection()], 1000.0)
    assert [h.zone_id for h in hits] == ["z1", "z2"]


# -- loiter ----------------------------------------------------------------


def test_loiter_fires_only_after_the_dwell_has_elapsed():
    camera = camera_cfg(zones=[zone(rule=RULE_LOITER, dwell_seconds=30)])
    evaluator = ZoneEvaluator()

    assert evaluate(evaluator, camera, [detection()], 1000.0) == []
    assert evaluate(evaluator, camera, [detection()], 1020.0) == []
    assert evaluate(evaluator, camera, [detection()], 1029.9) == []
    hits = evaluate(evaluator, camera, [detection()], 1030.0)
    assert len(hits) == 1, "30 s of continuous presence is the rule"
    assert hits[0].rule == RULE_LOITER
    assert hits[0].dwell_seconds == pytest.approx(30.0)


def test_leaving_the_zone_resets_the_dwell_timer():
    """Presence has to be continuous. Someone who wanders in for ten seconds, out,
    and back in has not loitered, and a timer that kept running would say they had."""
    left_half = [[0.0, 0.0], [0.5, 0.0], [0.5, 1.0], [0.0, 1.0]]
    camera = camera_cfg(zones=[zone(rule=RULE_LOITER, dwell_seconds=30, polygon=left_half)])
    evaluator = ZoneEvaluator()
    inside = detection(box=(100.0, 50.0, 140.0, 150.0))
    outside = detection(box=(400.0, 50.0, 440.0, 150.0))

    evaluate(evaluator, camera, [inside], 1000.0)  # inside
    evaluate(evaluator, camera, [inside], 1010.0)  # still inside
    evaluate(evaluator, camera, [outside], 1015.0)  # gone
    assert evaluate(evaluator, camera, [inside], 1020.0) == []  # back, timer restarted
    assert evaluate(evaluator, camera, [inside], 1045.0) == []
    assert len(evaluate(evaluator, camera, [inside], 1050.0)) == 1


def test_two_people_in_a_loiter_zone_are_timed_separately():
    camera = camera_cfg(zones=[zone(rule=RULE_LOITER, dwell_seconds=10)])
    evaluator = ZoneEvaluator()
    left = detection(box=(100.0, 50.0, 140.0, 150.0))
    right = detection(box=(400.0, 50.0, 440.0, 150.0))
    tracks = [Track(id=1, detection=left), Track(id=2, detection=right)]

    evaluator.evaluate(camera, tracks, 1000.0)
    assert len(evaluator.evaluate(camera, tracks, 1010.0)) == 2, "both have been there 10 s"


def test_a_loiter_zone_and_an_intrusion_zone_on_the_same_camera_do_not_share_timers():
    camera = camera_cfg(
        zones=[
            zone(zone_id="z1", rule=RULE_INTRUSION),
            zone(zone_id="z2", name="gate", rule=RULE_LOITER, dwell_seconds=5),
        ]
    )
    evaluator = ZoneEvaluator()
    hits = evaluate(evaluator, camera, [detection()], 1000.0)
    assert [h.zone_id for h in hits] == ["z1"], "the intrusion fires now, the loiter has not dwelt"
    assert [h.zone_id for h in evaluate(evaluator, camera, [detection()], 1005.0)] == ["z1", "z2"]
