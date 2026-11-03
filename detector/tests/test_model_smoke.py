"""The slow smoke: the real exported tensor against the real fixture photo.

This is the one test that proves the decode path matches what the model actually
emits — a planted `[1, 84, 8400]` tensor proves the decoder is self-consistent, and
only the shipped weights prove it is consistent with YOLOv8. It is marked `slow` so
CI runs it once, in the detector image, rather than on every push.

Both artifacts are gitignored (`/models/`, `*.onnx`, `testdata/bus.jpg`): the model
is exported at image build (S5-12) and the photo is fetched by
`scripts/fetch-fixture.sh` (S10-01). Without them the test skips, and says where to
get them.
"""

from __future__ import annotations

import os
from pathlib import Path

import cv2
import numpy as np
import pytest

from nvr_detector.model import CLASS_NAMES, YoloOnnx

pytestmark = pytest.mark.slow

REPO_ROOT = Path(__file__).resolve().parents[2]


def _find(name: str, env_var: str, extra: tuple[str, ...] = ()) -> Path | None:
    candidates = [os.environ.get(env_var), *extra, REPO_ROOT / "models" / name, REPO_ROOT / "testdata" / name, Path("/models") / name, Path("/testdata") / name]
    for candidate in candidates:
        if candidate and Path(candidate).is_file():
            return Path(candidate)
    return None


@pytest.fixture(scope="module")
def model_path() -> str:
    path = _find("yolov8n.onnx", "NVR_TEST_MODEL", extra=(os.environ.get("DET_MODEL_PATH", ""),))
    if path is None:
        pytest.skip(
            "no yolov8n.onnx: the detector image exports it at build time (card S5-12); "
            "locally, scripts/fetch-fixture.sh fetches the pinned weights (card S10-01)"
        )
    return str(path)


@pytest.fixture(scope="module")
def fixture_photo() -> np.ndarray:
    path = _find("bus.jpg", "NVR_TEST_IMAGE")
    if path is None:
        pytest.skip(
            "no testdata/bus.jpg: scripts/fetch-fixture.sh fetches it and records its "
            "sha256 in testdata/fixtures.json (card S10-01)"
        )
    image = cv2.imread(str(path))
    assert image is not None, f"{path} did not decode as an image"
    return image


def test_the_exported_model_is_the_shape_the_worker_expects(model_path):
    session = YoloOnnx(model_path, ["CPUExecutionProvider"])
    assert session.input_name == "images"
    assert session.output_name == "output0"
    assert session.providers == ["CPUExecutionProvider"]


def test_the_shipped_weights_find_a_person_in_the_fixture_photo(model_path, fixture_photo):
    """The plan's smoke: a person box at conf >= 0.5 in `bus.jpg` with `yolov8n.onnx`
    on CPU. If this fails, the export or the decode is wrong, and every event the
    worker reports is wrong with it."""
    session = YoloOnnx(model_path, ["CPUExecutionProvider"])
    detections = session.run(fixture_photo, min_confidence=0.5, min_area_ratio=0.0)

    people = [d for d in detections if CLASS_NAMES[d.cls] == "person"]
    assert people, f"no person in bus.jpg at conf 0.5; the model reported {[(CLASS_NAMES[d.cls], round(d.conf, 3)) for d in detections]}"
    person = max(people, key=lambda d: d.conf)
    assert person.conf >= 0.5

    height, width = fixture_photo.shape[0], fixture_photo.shape[1]
    assert 0.0 <= person.x1 < person.x2 <= width
    assert 0.0 <= person.y1 < person.y2 <= height
    assert person.area_ratio(width, height) >= 0.004, "a person in this photo is not a handful of pixels"

    print(f"smoke ok: {len(people)} person(s), best conf {person.conf:.3f} at {person.as_box()}")
