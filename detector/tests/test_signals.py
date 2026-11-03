"""S5-08: the config poll, the signal POST, the buffer between them.

The retry criterion is asserted over a real localhost socket, because "3 attempts"
is a claim about what the server sees, not about what a mock recorded. Everything
else runs against the injected session so the suite stays fast and offline.
"""

from __future__ import annotations

import itertools
import threading
import time

import pytest

from nvr_detector.client import (
    BACKOFF_BASE_SECONDS,
    BACKOFF_MAX_SECONDS,
    CONFIG_POLL_SECONDS,
    DEFAULT_SERVER_URL,
    DETECTOR_CONFIG_PATH,
    DetectorConfig,
    FLUSH_SECONDS,
    MAX_BUFFERED_SIGNALS,
    MAX_RETRIES,
    MAX_SIGNALS_PER_POST,
    SIGNALS_PATH,
    ServerClient,
    _backoff_delay,
)
from nvr_detector.events import SIGNAL_KEYS, encode_signal

from conftest import StubResult, StubServer, StubSession

# The plan's signal shape, transcribed from NVRWAY_V1_PLAN.md line 181 rather than
# imported, so a module that quietly renames a key fails here.
PLAN_SIGNAL_KEYS = (
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


def a_signal(**overrides) -> dict:
    base = dict(
        camera_id="cam-1",
        record_mode="continuous",
        event_key="cam-1:z1:intrusion:1000",
        action="open",
        rule="intrusion",
        zone_id="z1",
        zone_name="driveway",
        severity=3,
        classes=["person"],
        ts=1000.0,
        objects=[{"class": "person", "conf": 0.9, "box": [100, 50, 140, 150]}],
        snapshot_base64="/9j/4AAQSkZJRg==",
    )
    base.update(overrides)
    return encode_signal(**base)


def client_with(script, sleep=None, **kwargs) -> tuple[ServerClient, StubSession]:
    session = StubSession(script)
    return ServerClient("http://server:8080", "tok", session=session, sleep=sleep or (lambda _s: None), **kwargs), session


# -- the plan's numbers, named --------------------------------------------


def test_the_plan_intervals_and_limits_are_the_constants_the_code_uses():
    assert CONFIG_POLL_SECONDS == 5.0
    assert FLUSH_SECONDS == 2.0
    assert MAX_SIGNALS_PER_POST == 40
    assert MAX_BUFFERED_SIGNALS == 40
    assert MAX_RETRIES == 3
    assert BACKOFF_BASE_SECONDS == 5.0
    assert BACKOFF_MAX_SECONDS == 60.0
    assert DEFAULT_SERVER_URL.endswith(":8080")


def test_the_backoff_is_min_five_doubling_capped_at_sixty():
    assert [_backoff_delay(n) for n in range(4)] == [5.0, 10.0, 20.0, 40.0]
    assert _backoff_delay(4) == 60.0, "min(5 * 2^n, 60): it never waits longer than a minute"
    assert _backoff_delay(20) == 60.0


# -- serialization ---------------------------------------------------------


def test_signals_are_posted_as_the_plan_shapes_them():
    """`{"signals":[…]}`, and a signal whose keys are exactly the plan's, in order."""
    client, session = client_with([StubResult(200, {"accepted": 1, "events": []})])
    signal = a_signal()
    ok, _body = client.post_signals([signal])
    assert ok is True

    call = session.calls[0]
    assert call["method"] == "POST"
    assert call["url"] == "http://server:8080" + SIGNALS_PATH
    assert call["json"] == {"signals": [signal]}
    assert tuple(call["json"]["signals"][0].keys()) == PLAN_SIGNAL_KEYS
    assert tuple(signal.keys()) == SIGNAL_KEYS


def test_the_worker_token_rides_as_a_bearer_and_the_timeout_is_the_workers():
    client, session = client_with([StubResult(200, {}), StubResult(200, {"cameras": []})], timeout=3.5)
    client.post_signals([a_signal()])
    client.get_config()
    for call in session.calls:
        assert call["headers"] == {"Authorization": "Bearer tok"}
        assert call["timeout"] == 3.5


def test_the_server_answer_comes_back_so_a_rejection_can_be_logged():
    """`{"accepted":0,"events":[{"error":"unknown camera"}]}` is a 200 that means no.
    The client reports success at the transport level and hands the body back, which
    is what lets the pipeline name the camera the server could not find."""
    body = {"accepted": 0, "events": [{"event_key": "cam-1:z1:intrusion:1000", "error": "unknown camera"}]}
    client, _ = client_with([StubResult(200, body)])
    ok, returned = client.post_signals([a_signal()])
    assert ok is True
    assert returned["events"][0]["error"] == "unknown camera"


# -- the config poll -------------------------------------------------------


CONFIG_PAYLOAD = {
    "providers": ["CPUExecutionProvider"],
    "settings": {"event_max_seconds": 120, "something_new": True},
    "cameras": [
        {
            "id": "cam-1",
            "slug": "porch",
            "name": "Porch",
            "det_stream": "porch_det",
            "fps": 5,
            "width": 640,
            "height": 360,
            "sensitivity": 0.45,
            "min_area_ratio": 0.004,
            "record_mode": "events",
            "providers": ["CPUExecutionProvider"],
            "added_by_a_newer_server": "ignored",
            "zones": [
                {
                    "id": "z1",
                    "name": "driveway",
                    "polygon": [[0.0, 0.0], [1.0, 0.0], [1.0, 1.0], [0.0, 1.0]],
                    "classes": ["person"],
                    "rule": "loiter",
                    "dwell_seconds": 30,
                    "severity": 2,
                    "min_confidence": 0.5,
                    "min_area_ratio": 0.01,
                    "enabled": True,
                }
            ],
        }
    ],
}


def test_the_config_payload_becomes_the_cameras_the_worker_watches():
    client, session = client_with([StubResult(200, CONFIG_PAYLOAD)])
    config = client.get_config()
    assert config is not None
    assert len(config.cameras) == 1

    camera = config.cameras[0]
    assert (camera.id, camera.slug, camera.record_mode) == ("cam-1", "porch", "events")
    assert camera.stream_name() == "porch_det"
    assert camera.fps == 5 and (camera.width, camera.height) == (640, 360)
    assert camera.sensitivity == 0.45 and camera.min_area_ratio == 0.004

    zone = camera.zones[0]
    assert (zone.id, zone.rule, zone.dwell_seconds, zone.severity) == ("z1", "loiter", 30.0, 2)
    assert zone.classes == ["person"] and zone.enabled is True

    assert config.event_max_seconds() == 120
    assert session.calls[0]["url"] == "http://server:8080" + DETECTOR_CONFIG_PATH


def test_a_payload_from_an_older_server_still_works():
    """Every field the worker needs has a default, because a worker image outlives
    the server it was built against more often than the other way round."""
    client, _ = client_with([StubResult(200, {"cameras": [{"id": "cam-9", "slug": "gate"}]})])
    config = client.get_config()
    camera = config.cameras[0]
    assert camera.stream_name() == "gate_det"
    assert camera.fps == 5 and camera.sensitivity == 0.45 and camera.zones == []
    assert config.event_max_seconds() == 300


def test_a_config_payload_that_does_not_parse_is_reported_and_kept_quiet():
    client, _ = client_with([StubResult(200, {"cameras": [{"slug": "no id"}]})])
    assert client.get_config() is None


def test_a_refused_config_poll_leaves_the_previous_config_in_place():
    """The server is behind a reverse proxy that has not caught up. The worker keeps
    watching the cameras it already knew about rather than stopping all inference."""
    client, _ = client_with([StubResult(200, CONFIG_PAYLOAD), StubResult(403, {"error": "internal api disabled"})])
    first = client.get_config()
    assert client.get_config() is None
    assert client.config is first


# -- retries ---------------------------------------------------------------


def test_500_then_500_then_200_is_three_attempts_against_a_real_socket():
    """The card's criterion, run over a localhost socket: the server counts what it
    was actually asked."""
    attempts = itertools.count(1)
    script = {1: (500, {"error": "db locked"}), 2: (500, {"error": "db locked"}), 3: (200, {"accepted": 1, "events": []})}

    def handler(method, path, body):
        return script.get(next(attempts), (200, {}))

    with StubServer(handler) as server:
        slept = []
        client = ServerClient(server.url, "tok", sleep=slept.append)
        ok, _body = client.post_signals([a_signal()])
        assert ok is True

    assert server.count(SIGNALS_PATH) == 3, "the plan's 3 retries means three attempts, not three more"
    assert slept == [5.0, 10.0], "min(5*2^n, 60) between them"


def test_a_server_that_never_answers_gives_up_after_four_attempts_and_keeps_the_buffer():
    with StubServer(lambda method, path, body: (500, {"error": "no db"})) as server:
        slept = []
        client = ServerClient(server.url, "tok", sleep=slept.append)
        client.add(a_signal())
        assert client.flush() is False

    assert server.count(SIGNALS_PATH) == MAX_RETRIES + 1
    assert slept == [5.0, 10.0, 20.0]
    assert client.buffered() == 1, "a failed POST does not consume the signal"


def test_a_4xx_is_not_retried():
    """403 means the token is wrong. Retrying it four times would only delay the log
    line that says so, and would hammer a server that has already said no."""
    client, session = client_with([StubResult(403, {"error": "internal api disabled"})])
    ok, body = client.post_signals([a_signal()])
    assert ok is False
    assert body == {"error": "internal api disabled"}
    assert len(session.calls) == 1


def test_a_connection_error_counts_as_a_failed_attempt_and_is_retried():
    client, session = client_with(
        [ConnectionError("connection refused"), ConnectionError("connection refused"), StubResult(200, {"accepted": 1})],
        sleep=lambda _s: None,
    )
    ok, _body = client.post_signals([a_signal()])
    assert ok is True
    assert len(session.calls) == 3


# -- the buffer ------------------------------------------------------------


def test_the_buffer_survives_a_dead_server_and_flushes_on_recovery():
    client, session = client_with(
        [
            ConnectionError("connection refused"),
            ConnectionError("connection refused"),
            ConnectionError("connection refused"),
            ConnectionError("connection refused"),
            StubResult(200, {"accepted": 2, "events": []}),
        ],
        sleep=lambda _s: None,
    )
    first, second = a_signal(action="open"), a_signal(action="extend")
    client.add(first)
    client.add(second)

    assert client.flush() is False
    assert client.buffered() == 2
    assert client.flush() is True
    assert client.buffered() == 0
    assert client.signals_sent == 2

    posted = session.calls[-1]["json"]["signals"]
    assert [s["action"] for s in posted] == ["open", "extend"], "the order the events happened in is preserved"


def test_the_buffer_caps_at_forty_and_drops_the_oldest_with_a_counted_log_line(caplog):
    """A worker that cannot reach its server for ten minutes of a busy driveway has
    more than forty signals waiting. Dropping the oldest keeps the newest event
    windows alive; counting them is what lets an operator explain a short event."""
    client, _ = client_with([], sleep=lambda _s: None)
    for n in range(MAX_BUFFERED_SIGNALS + 5):
        client.add(a_signal(event_key=f"cam-1:z1:intrusion:{n}"))

    assert client.buffered() == MAX_BUFFERED_SIGNALS
    assert client.signals_dropped == 5
    assert "signals_dropped=5" in caplog.text
    assert "40" in caplog.text
    # The four that survived the cap are the newest ones, which are the ones whose
    # event windows still make sense to post.
    assert client._buffer[0]["event_key"] == "cam-1:z1:intrusion:5"


def test_the_oldest_signals_are_the_ones_that_go():
    client, _ = client_with([], sleep=lambda _s: None)
    for n in range(MAX_BUFFERED_SIGNALS + 1):
        client.add(a_signal(event_key=f"cam-1:z1:intrusion:{n}"))
    keys = [s["event_key"] for s in client._buffer]
    assert keys[0] == "cam-1:z1:intrusion:1", "the first signal was the one dropped"
    assert keys[-1] == f"cam-1:z1:intrusion:{MAX_BUFFERED_SIGNALS}"


def test_a_flush_posts_at_most_forty_signals_at_a_time():
    """The plan's two numbers are the same number, so the batch ceiling is shown by
    letting this client hold more than one POST's worth."""
    client, session = client_with(
        [StubResult(200, {"accepted": MAX_SIGNALS_PER_POST, "events": []})],
        sleep=lambda _s: None,
        max_buffer=MAX_SIGNALS_PER_POST + 20,
    )
    for n in range(MAX_BUFFERED_SIGNALS + 20):
        client.add(a_signal(event_key=f"cam-1:z1:intrusion:{n}"))
    assert client.flush() is True
    assert len(session.calls[0]["json"]["signals"]) == MAX_SIGNALS_PER_POST
    assert client.buffered() == 20


def test_signals_added_while_a_post_is_in_flight_are_not_double_counted():
    """The buffer is shared with the camera threads, so a flush must remove exactly
    the signals it posted — not merely the same number of them."""
    client, session = client_with(
        [StubResult(200, {"accepted": 1, "events": []}), StubResult(200, {"accepted": 1, "events": []})],
        sleep=lambda _s: None,
        max_batch=1,
        max_buffer=10,
    )
    client.add(a_signal(event_key="cam-1:z1:intrusion:1"))
    client.add(a_signal(event_key="cam-1:z1:intrusion:2"))
    assert client.flush() is True
    assert [s["event_key"] for s in client._buffer] == ["cam-1:z1:intrusion:2"]
    assert client.flush() is True
    assert client.buffered() == 0
    posted = [call["json"]["signals"][0]["event_key"] for call in session.calls]
    assert posted == ["cam-1:z1:intrusion:1", "cam-1:z1:intrusion:2"]


def test_an_empty_buffer_flushes_without_touching_the_transport():
    client, session = client_with([], sleep=lambda _s: None)
    assert client.flush() is True
    assert session.calls == []


# -- the loop --------------------------------------------------------------


def test_the_loop_polls_the_config_and_flushes_on_their_intervals():
    """Run at compressed intervals so the shape of the loop — poll, flush, poll —
    is asserted in under a second."""
    with StubServer(lambda method, path, body: (200, {"cameras": [], "settings": {}} if method == "GET" else {"accepted": 0, "events": []})) as server:
        client = ServerClient(server.url, "tok", timeout=2.0)
        stop = threading.Event()
        thread = threading.Thread(target=client.run, args=(stop,), kwargs={"poll": 0.15, "flush_every": 0.05}, daemon=True)
        thread.start()
        client.add(a_signal())
        time.sleep(0.6)
        stop.set()
        thread.join(timeout=5)

    assert server.count(DETECTOR_CONFIG_PATH) >= 3, "a 5 s poll, compressed to 0.15 s, must have run several times"
    assert server.count(SIGNALS_PATH) >= 1
    assert client.buffered() == 0, "the loop drains what the pipelines add"


def test_the_loop_hands_the_parsed_config_to_the_caller():
    with StubServer(lambda method, path, body: (200, CONFIG_PAYLOAD if method == "GET" else {"accepted": 0})) as server:
        client = ServerClient(server.url, "tok", timeout=2.0)
        seen = []
        stop = threading.Event()
        thread = threading.Thread(
            target=client.run, args=(stop, seen.append), kwargs={"poll": 0.1, "flush_every": 10.0}, daemon=True
        )
        thread.start()
        time.sleep(0.3)
        stop.set()
        thread.join(timeout=5)

    assert seen and isinstance(seen[0], DetectorConfig)
    assert seen[0].cameras[0].slug == "porch"
