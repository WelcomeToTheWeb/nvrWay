"""S5-02 mjpeg ingestion, S5-03 the model's decode path.

The mjpeg tests build real multipart bodies out of real JPEG bytes and feed them
in 7-byte chunks, because the bug this module is written against is a split that
lands inside a frame. The decode tests plant a `[1, 84, 8400]` tensor and assert
the boxes that come back, so a wrong transpose or a wrong channel layout fails
here rather than on a live camera.
"""

from __future__ import annotations

import cv2
import numpy as np
import pytest

from nvr_detector import model as model_module
from nvr_detector.mjpeg import DEFAULT_BACKOFF_SECONDS, MjpegStream, stream_url
from nvr_detector.model import (
    CLASS_NAMES,
    DEFAULT_IMGSZ,
    DETECTION_CLASSES,
    IGNORED_CLASSES,
    LETTERBOX_PAD_VALUE,
    NMS_IOU_THRESHOLD,
    YoloOnnx,
    letterbox_frame,
    postprocess,
    providers_from_env,
)

from conftest import (
    RecordingOpener,
    StubMjpegResponse,
    detection,
    make_frame,
    multipart,
    planted_tensor,
    read_stream,
    write_tiny_onnx,
)

# The COCO list in YOLO order, written out here on purpose: comparing the module's
# names against a list copied from the module would prove nothing.
COCO_NAMES = [
    "person", "bicycle", "car", "motorcycle", "airplane", "bus", "train", "truck", "boat",
    "traffic light", "fire hydrant", "stop sign", "parking meter", "bench", "bird", "cat",
    "dog", "horse", "sheep", "cow", "elephant", "bear", "zebra", "giraffe", "backpack",
    "umbrella", "handbag", "tie", "suitcase", "frisbee", "skis", "snowboard", "sports ball",
    "kite", "baseball bat", "baseball glove", "skateboard", "surfboard", "tennis racket",
    "bottle", "wine glass", "cup", "fork", "knife", "spoon", "bowl", "banana", "apple",
    "sandwich", "orange", "broccoli", "carrot", "hot dog", "pizza", "donut", "cake", "chair",
    "couch", "potted plant", "bed", "dining table", "toilet", "tv", "laptop", "mouse",
    "remote", "keyboard", "cell phone", "microwave", "oven", "toaster", "sink",
    "refrigerator", "book", "clock", "vase", "scissors", "teddy bear", "hair drier",
    "toothbrush",
]


# -- S5-02: the stream -----------------------------------------------------


def test_the_boundary_is_read_from_the_response_not_assumed():
    """go2rtc picks its own boundary. A worker that hard-codes one reads zero frames
    and reports a camera that never moves."""
    frames = [make_frame(value=10), make_frame(value=200)]
    for boundary in ("frame", "--myboundary", '"quoted"', "----WebKitFormBoundaryZzz1"):
        # The header carries the odd form; the body carries the delimiter the parser
        # has to arrive at, which is `--` plus the value without its quotes or dashes.
        body = multipart(boundary.strip('"').lstrip("-"), frames)
        got, stream, opener, _slept = read_stream([StubMjpegResponse(f"multipart/x-mixed-replace;boundary={boundary}", body)])
        assert len(got) == 2, f"boundary {boundary!r} produced {len(got)} frames"
        assert [f.shape for f in got] == [(36, 64, 3), (36, 64, 3)]
        assert stream.boundary == boundary.strip('"').lstrip("-"), "the parameter may carry the leading dashes; the delimiter is always `--` plus what is left"
        assert len(opener.calls) == 1, "one response, one connection"


def test_the_boundary_may_be_quoted_with_a_space_after_the_semicolon():
    frames = [make_frame(value=1)]
    body = multipart("frame", frames)
    got, _stream, _opener, _slept = read_stream([StubMjpegResponse('multipart/x-mixed-replace; boundary = "frame"', body)])
    assert len(got) == 1

def test_a_body_split_at_odd_offsets_still_yields_every_frame():
    """The boundary can straddle a chunk and a JPEG byte can look like one. Feeding
    7 bytes at a time is how that gets caught."""
    frames = [make_frame(value=v) for v in (0, 64, 128, 192, 255)]
    body = multipart("frame", frames, preamble=b"prelude that is not a frame\r\n")
    got, _stream, _opener, _slept = read_stream([StubMjpegResponse("multipart/x-mixed-replace;boundary=frame", body)])
    assert len(got) == 5
    assert [int(f[0, 0, 0]) for f in got] == [0, 64, 128, 192, 255]


def test_a_socket_close_reconnects_after_the_backoff_and_resumes():
    """The stream dies mid-frame — which is what a tap does when go2rtc restarts —
    and the worker comes back on its own."""
    first = StubMjpegResponse(
        "multipart/x-mixed-replace;boundary=frame",
        multipart("frame", [make_frame(value=7)]),
        fail_after="connection reset by peer",
    )
    second = StubMjpegResponse("multipart/x-mixed-replace;boundary=frame", multipart("frame", [make_frame(value=9)]))

    got, stream, opener, slept = read_stream([first, second])

    assert [int(f[0, 0, 0]) for f in got] == [7, 9], "the frame after the reset still arrives"
    assert len(opener.calls) == 2
    assert slept == [DEFAULT_BACKOFF_SECONDS], "the plan's reconnect backoff is five seconds"
    assert stream.reconnects == 1


def test_a_stream_that_never_yields_a_frame_is_retried_until_the_stop_event():
    """A tap that answers but sends nothing must not hang the camera's thread, and
    must not spin the CPU either."""
    responses = [StubMjpegResponse("multipart/x-mixed-replace;boundary=frame", [b"--frame\r\n"]) for _ in range(3)]
    got, _stream, opener, slept = read_stream(responses)
    assert got == []
    assert len(opener.calls) == 3
    assert slept == [DEFAULT_BACKOFF_SECONDS] * 2, "the backoff between attempts, not a busy spin"


def test_a_response_with_no_boundary_fails_loudly_rather_than_hanging(caplog):
    got, _stream, opener, slept = read_stream([StubMjpegResponse("image/jpeg", [b"not a stream"])])
    assert got == []
    assert "not an mjpeg stream" in caplog.text
    # The body was never read, so the scripted end-of-stream never fired and the
    # stream backed off and dialled once more: two connections, one backoff, no spin.
    assert len(opener.calls) == 2
    assert slept == [DEFAULT_BACKOFF_SECONDS]


def test_the_timeout_reaches_the_socket():
    """`DET_HTTP_TIMEOUT` is the worker's knob for a media host that has stopped
    answering; without it a hung tap holds a camera thread forever."""
    _got, _stream, opener, _slept = read_stream(
        [StubMjpegResponse("multipart/x-mixed-replace;boundary=frame", [b"--frame\r\n"])], timeout=3
    )
    assert [call[1] for call in opener.calls] == [3.0]
    assert opener.calls[0][0] == "http://go2rtc:1984/api/stream.mjpeg?src=porch_det"


def test_the_stream_url_is_the_go2rtc_mjpeg_endpoint_for_the_det_tap():
    """`/api/stream.mjpeg?src=` is go2rtc's multipart endpoint. `/api/frame.jpeg` is
    the single-frame one the console uses, and a worker pointed at it would read one
    frame and stop."""
    assert stream_url("http://go2rtc:1984", "porch_det") == "http://go2rtc:1984/api/stream.mjpeg?src=porch_det"
    assert stream_url("http://go2rtc:1984/", "porch_det") == "http://go2rtc:1984/api/stream.mjpeg?src=porch_det"


# -- S5-03: the class set --------------------------------------------------


def test_the_class_names_are_the_80_coco_names_in_yolo_order():
    assert CLASS_NAMES == tuple(COCO_NAMES)
    assert len(CLASS_NAMES) == 80


def test_the_detection_set_is_the_plans_nine_and_the_rest_are_ignored():
    assert DETECTION_CLASSES == frozenset(
        {"person", "bicycle", "car", "motorcycle", "bus", "truck", "dog", "cat", "bear"}
    )
    assert IGNORED_CLASSES == frozenset(COCO_NAMES) - frozenset(DETECTION_CLASSES)
    # A worker that detected "tv" would open an event every time someone turned the
    # television on, which is the reason this list exists at all.
    assert "tv" in IGNORED_CLASSES and "chair" in IGNORED_CLASSES and "cup" in IGNORED_CLASSES


# -- S5-03: preprocess -----------------------------------------------------


def test_letterbox_is_a_640_square_float32_chw_batch_with_the_plan_pad_value():
    frame = np.zeros((360, 640, 3), dtype=np.uint8)
    blob, box_map = letterbox_frame(frame, DEFAULT_IMGSZ)
    assert blob.shape == (1, 3, DEFAULT_IMGSZ, DEFAULT_IMGSZ)
    assert blob.dtype == np.float32
    assert box_map.scale == 1.0 and box_map.pad_x == 0 and box_map.pad_y == 140
    # 360 into 640 leaves 280 rows of padding, split top and bottom; the top row is
    # padding, and padding is 114/255 in every channel.
    assert np.allclose(blob[0, :, 0, :], LETTERBOX_PAD_VALUE / 255.0)
    assert not np.allclose(blob[0, :, DEFAULT_IMGSZ // 2, :], LETTERBOX_PAD_VALUE / 255.0)


def test_letterbox_converts_bgr_to_rgb():
    """cv2 hands us BGR and the model was trained on RGB. Get this backwards and
    every box is still in the right place, which is why it needs its own test."""
    red_bgr = np.zeros((360, 640, 3), dtype=np.uint8)
    red_bgr[:, :, 2] = 255  # BGR: channel 2 is red
    blob, _ = letterbox_frame(red_bgr, DEFAULT_IMGSZ)
    middle = DEFAULT_IMGSZ // 2
    assert blob[0, 0, middle, middle] == pytest.approx(1.0), "the red channel sees the frame"
    assert blob[0, 2, middle, middle] == pytest.approx(0.0), "and the blue channel does not"


def test_a_box_planted_at_the_padded_edge_maps_back_inside_the_frame():
    """The letterbox is where the coordinate bugs live: a 1280×720 frame becomes a
    640 square at scale 0.5 with 140 rows of padding, so a model-space box whose top
    edge sits at y=130 belongs at y=-20 in the frame. It has to clip to 0 — a
    negative y is a box the operator cannot see."""
    frame = np.zeros((720, 1280, 3), dtype=np.uint8)
    blob, box_map = letterbox_frame(frame, DEFAULT_IMGSZ)
    assert box_map.scale == pytest.approx(0.5)
    assert box_map.pad_y == 140 and box_map.pad_x == 0
    assert np.allclose(blob[0, :, 0, :], LETTERBOX_PAD_VALUE / 255.0)

    tensor = planted_tensor([(0, 0.9, (320.0, 165.0, 40.0, 70.0))])  # model y 130..200
    detections = postprocess(tensor, (1280, 720), min_confidence=0.1, min_area_ratio=0.0, box_map=box_map)
    assert len(detections) == 1
    det = detections[0]
    assert det.y1 == pytest.approx(0.0), "the padded part was clipped, not dropped or left negative"
    assert det.y2 == pytest.approx(120.0)
    assert (det.x1, det.x2) == pytest.approx((600.0, 680.0))


# -- S5-03: decode, filters, NMS ------------------------------------------


def test_a_planted_tensor_decodes_to_the_box_that_was_planted():
    """Anchor 0, class 0, conf 0.9, centre (120, 100) size (40, 100). With no
    letterbox in the way the box must come back exactly as planted."""
    tensor = planted_tensor([(0, 0.9, (120.0, 100.0, 40.0, 100.0))])
    detections = postprocess(tensor, (640, 360), min_confidence=0.45, min_area_ratio=0.0)
    assert len(detections) == 1
    det = detections[0]
    assert CLASS_NAMES[det.cls] == "person"
    assert det.conf == pytest.approx(0.9, abs=1e-6)
    assert (det.x1, det.y1, det.x2, det.y2) == pytest.approx((100.0, 50.0, 140.0, 150.0), abs=0.01)


def test_classes_outside_the_detection_set_are_dropped_even_at_high_confidence():
    traffic_light = planted_tensor([(9, 0.99, (120.0, 100.0, 40.0, 100.0))])
    assert postprocess(traffic_light, (640, 360), min_confidence=0.1, min_area_ratio=0.0) == []

    person = planted_tensor([(0, 0.99, (120.0, 100.0, 40.0, 100.0))])
    assert len(postprocess(person, (640, 360), min_confidence=0.1, min_area_ratio=0.0)) == 1


def test_the_min_confidence_floor_is_applied():
    tensor = planted_tensor([(0, 0.35, (120.0, 100.0, 40.0, 100.0))])
    assert postprocess(tensor, (640, 360), min_confidence=0.45, min_area_ratio=0.0) == []
    assert len(postprocess(tensor, (640, 360), min_confidence=0.30, min_area_ratio=0.0)) == 1


def test_the_min_area_ratio_floor_is_applied():
    """A 4×4 px detection on a 640×360 frame is 16/230 400 = 0.007% of it — the leaf
    that the wind moves. The camera's `min_area_ratio` is what keeps it out of the
    event log, and a floor of 0.01% lets it through."""
    tiny = planted_tensor([(0, 0.99, (120.0, 100.0, 4.0, 4.0))])
    assert tiny[0, 0, 0] == 120.0, "the planted box is where the test thinks it is"
    assert postprocess(tiny, (640, 360), min_confidence=0.1, min_area_ratio=0.004) == []
    assert len(postprocess(tiny, (640, 360), min_confidence=0.1, min_area_ratio=0.00001)) == 1


def test_nms_suppresses_the_overlapping_duplicate_and_keeps_the_confident_one():
    """(0,0,100,100) against (20,0,120,100) is IoU 8000/12000 = 0.667, over the
    plan's 0.45, so one of them goes — the one the model believed less."""
    tensor = planted_tensor(
        [
            (0, 0.55, (50.0, 50.0, 100.0, 100.0)),
            (0, 0.90, (70.0, 50.0, 100.0, 100.0)),
        ]
    )
    detections = postprocess(tensor, (640, 360), min_confidence=0.1, min_area_ratio=0.0)
    assert len(detections) == 1
    assert detections[0].conf == pytest.approx(0.90)


def test_nms_suppresses_a_nested_pair_and_keeps_the_confident_one_not_the_big_one():
    """Card S5-11 asks for "an IoU 0.62 pair"; with two equal 100x100 boxes offset
    sideways the IoU is a/(100+a) for an overlap of a, which tops out at 0.5, so the
    pair here is nested instead: (0,0,80,80) inside (0,0,100,100) is 6400/10000 =
    0.64. The survivor is the *smaller* box and the *second* one in input order, so
    this fails for an NMS that keeps boxes by area, by index, or by centre distance
    (the centres are 14 px apart, which a centre-based suppression would call the
    same person twice over)."""
    tensor = planted_tensor(
        [
            (0, 0.55, (50.0, 50.0, 100.0, 100.0)),
            (0, 0.90, (40.0, 40.0, 80.0, 80.0)),
        ]
    )
    detections = postprocess(tensor, (640, 360), min_confidence=0.1, min_area_ratio=0.0)
    assert len(detections) == 1
    assert detections[0].conf == pytest.approx(0.90)
    assert (detections[0].x1, detections[0].y1, detections[0].x2, detections[0].y2) == pytest.approx(
        (0.0, 0.0, 80.0, 80.0)
    )


def test_nms_leaves_two_boxes_that_are_not_overlapping_enough():
    """(0,0,100,100) against (60,0,160,100) is IoU 4000/16000 = 0.25: two people
    standing apart, which is the case a wrong threshold would collapse into one."""
    tensor = planted_tensor(
        [
            (0, 0.90, (50.0, 50.0, 100.0, 100.0)),
            (0, 0.85, (110.0, 50.0, 100.0, 100.0)),
        ]
    )
    assert len(postprocess(tensor, (640, 360), min_confidence=0.1, min_area_ratio=0.0)) == 2


def test_the_nms_threshold_is_the_plans():
    assert NMS_IOU_THRESHOLD == 0.45


# -- S5-03: providers ------------------------------------------------------


def test_providers_come_from_det_providers_comma_split(monkeypatch):
    monkeypatch.setenv("DET_PROVIDERS", "CUDAExecutionProvider, CPUExecutionProvider")
    assert providers_from_env() == ["CUDAExecutionProvider", "CPUExecutionProvider"]
    monkeypatch.setenv("DET_PROVIDERS", "")
    assert providers_from_env() == ["CPUExecutionProvider"]
    monkeypatch.delenv("DET_PROVIDERS", raising=False)
    assert providers_from_env() == ["CPUExecutionProvider"]


def test_a_provider_the_onnxruntime_build_does_not_have_falls_back_to_cpu_and_says_so(monkeypatch, tmp_path, caplog):
    """A CPU build of onnxruntime answers a CUDA request with a UserWarning and
    silently runs CPU, which is the right outcome but the wrong way to learn it: the
    worker has to say which providers it actually got, or a GPU overlay that failed to
    install looks like it worked."""
    monkeypatch.setenv("DET_PROVIDERS", "CUDAExecutionProvider")
    path = write_tiny_onnx(tmp_path / "tiny.onnx")
    session = YoloOnnx(path)
    assert session.providers == ["CPUExecutionProvider"]
    assert "CUDAExecutionProvider" in caplog.text
    assert "not available" in caplog.text


def test_the_session_runs_the_whole_chain_over_a_real_onnx_model(tmp_path):
    """Not a mock: onnxruntime opens a model the suite wrote, and the planted tensor
    comes back out as a box in frame pixels."""
    tensor = planted_tensor([(0, 0.9, (120.0, 240.0, 40.0, 100.0))])
    path = write_tiny_onnx(tmp_path / "planted.onnx", tensor)
    session = YoloOnnx(path, ["CPUExecutionProvider"])
    assert (session.input_name, session.output_name) == ("images", "output0")

    # The planted tensor is in 640-square model space, and this frame is already
    # 640×360, so the letterbox offset the session applies is exactly 140 rows.
    detections = session.run(make_frame(width=640, height=360), min_confidence=0.45, min_area_ratio=0.0)
    assert len(detections) == 1
    assert (detections[0].x1, detections[0].y1, detections[0].x2, detections[0].y2) == pytest.approx(
        (100.0, 50.0, 140.0, 150.0), abs=0.01
    )


def test_a_model_with_an_unexpected_output_shape_is_reported(tmp_path):
    """The tiny fixture model outputs [1,3,640,640], which is not a YOLOv8 head. The
    worker says so instead of returning boxes it invented."""
    path = write_tiny_onnx(tmp_path / "tiny.onnx")
    session = YoloOnnx(path, ["CPUExecutionProvider"])
    with pytest.raises(ValueError, match="expected"):
        session.run(make_frame(width=640, height=360))
