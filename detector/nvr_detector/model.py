"""YOLOv8 (ONNX) inference: letterbox in, filtered boxes out.

The model file is not a dependency, it is an artifact: `yolov8n.pt` is downloaded
and exported to ONNX at image build time (card S5-12), so the container carries a
hash-pinned tensor and never a training run.

Three decisions live here rather than in a config file:

*   **The class set is a decision, not a knob.** `DETECTION_CLASSES` is the nine
    COCO classes worth filing an event about — person, bicycle, car, motorcycle,
    bus, truck, dog, cat, bear. Everything else in COCO is dropped, because a
    `traffic light` or a `cup` crossing a driveway polygon is not something an
    operator wants woken up for, and an event table full of them hides the person.
    `IGNORED_CLASSES` is the complement, derived from `CLASS_NAMES` so it cannot
    drift out of date when the list is re-read against a new COCO release.
*   **Boxes come back in frame pixels.** The console draws them on the snapshot,
    and the zone rules normalise a bottom-center point by the frame size. Model
    space is 640×640 with letterbox padding; nobody downstream wants to know about
    the padding, so `postprocess` maps back through the transform that
    `preprocess` applied.
*   **CPU is the verified provider.** `DET_PROVIDERS` is the operator's list
    (comma-separated, forwarded by the server in the detector config payload); the
    CUDA path ships as an overlay and falls back to CPU with a log line rather
    than dying on a box without the container toolkit.
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass
from typing import Optional, Sequence

import cv2
import numpy as np
from onnxruntime import InferenceSession, get_available_providers

log = logging.getLogger("nvr_detector.model")

# The COCO class list in the order YOLOv8 emits it, verbatim. The index of a name
# here is the class id the model reports, so this list is the contract between the
# exported tensor and the words in an event row.
CLASS_NAMES: tuple[str, ...] = (
    "person",
    "bicycle",
    "car",
    "motorcycle",
    "airplane",
    "bus",
    "train",
    "truck",
    "boat",
    "traffic light",
    "fire hydrant",
    "stop sign",
    "parking meter",
    "bench",
    "bird",
    "cat",
    "dog",
    "horse",
    "sheep",
    "cow",
    "elephant",
    "bear",
    "zebra",
    "giraffe",
    "backpack",
    "umbrella",
    "handbag",
    "tie",
    "suitcase",
    "frisbee",
    "skis",
    "snowboard",
    "sports ball",
    "kite",
    "baseball bat",
    "baseball glove",
    "skateboard",
    "surfboard",
    "tennis racket",
    "bottle",
    "wine glass",
    "cup",
    "fork",
    "knife",
    "spoon",
    "bowl",
    "banana",
    "apple",
    "sandwich",
    "orange",
    "broccoli",
    "carrot",
    "hot dog",
    "pizza",
    "donut",
    "cake",
    "chair",
    "couch",
    "potted plant",
    "bed",
    "dining table",
    "toilet",
    "tv",
    "laptop",
    "mouse",
    "remote",
    "keyboard",
    "cell phone",
    "microwave",
    "oven",
    "toaster",
    "sink",
    "refrigerator",
    "book",
    "clock",
    "vase",
    "scissors",
    "teddy bear",
    "hair drier",
    "toothbrush",
)

# The classes that may open an event. Everything else COCO knows about is dropped
# at the model boundary, so no later stage has to remember to drop it again.
DETECTION_CLASSES: frozenset[str] = frozenset(
    {
        "person",
        "bicycle",
        "car",
        "motorcycle",
        "bus",
        "truck",
        "dog",
        "cat",
        "bear",
    }
)

# Derived, so it cannot drift: the complement of DETECTION_CLASSES over the 80
# names the model can report.
IGNORED_CLASSES: frozenset[str] = frozenset(CLASS_NAMES) - DETECTION_CLASSES

_DETECTION_IDS: frozenset[int] = frozenset(CLASS_NAMES.index(name) for name in DETECTION_CLASSES)

# The plan's NMS window and its ceiling on boxes per frame.
NMS_IOU_THRESHOLD = 0.45
NMS_MAX_BOXES = 100

# Letterbox padding colour. 114 is the value ultralytics trains with; a black pad
# teaches the network about edges it never saw in training.
LETTERBOX_PAD_VALUE = 114

DEFAULT_IMGSZ = 640

# The camera floors the plan names, and what the server's `sensitivity` /
# `min_area_ratio` columns default to. Kept here so a worker pointed at an old
# server that sends neither still behaves like the plan says.
DEFAULT_MIN_CONFIDENCE = 0.45
DEFAULT_MIN_AREA_RATIO = 0.004


@dataclass(frozen=True)
class Detection:
    """One box, in pixels of the frame it came from."""

    cls: int
    conf: float
    x1: float
    y1: float
    x2: float
    y2: float

    @property
    def name(self) -> str:
        return CLASS_NAMES[self.cls]

    @property
    def width(self) -> float:
        return self.x2 - self.x1

    @property
    def height(self) -> float:
        return self.y2 - self.y1

    def area_ratio(self, frame_w: int, frame_h: int) -> float:
        """The box's share of the frame. A 6×6 box in a 640×360 frame is noise;
        a person who fills a tenth of it is not."""
        area = max(0.0, self.width) * max(0.0, self.height)
        return area / float(frame_w * frame_h)

    def as_box(self) -> tuple[float, float, float, float]:
        """`[x1, y1, x2, y2]` — the shape the IoU helpers and the event row use."""
        return (self.x1, self.y1, self.x2, self.y2)

    def as_object(self) -> dict:
        """The shape the server stores on the event row:
        `{"class": …, "conf": …, "box": [x1, y1, x2, y2]}`."""
        return {
            "class": self.name,
            "conf": round(float(self.conf), 4),
            "box": [round(float(v), 2) for v in (self.x1, self.y1, self.x2, self.y2)],
        }


@dataclass(frozen=True)
class Letterbox:
    """The transform one frame went through on the way to the model.

    Keeping it is what lets a box in model space be mapped back to the frame:
    subtract the padding, divide by the scale, clip. Without it a box at the edge
    of the padded square lands outside the picture the operator is looking at.
    """

    scale: float
    pad_x: int
    pad_y: int
    frame_w: int
    frame_h: int

    def to_frame_x(self, x: np.ndarray | float) -> np.ndarray | float:
        return (np.asarray(x, dtype=np.float64) - self.pad_x) / self.scale

    def to_frame_y(self, y: np.ndarray | float) -> np.ndarray | float:
        return (np.asarray(y, dtype=np.float64) - self.pad_y) / self.scale


def letterbox_frame(frame: np.ndarray, imgsz: int = DEFAULT_IMGSZ) -> tuple[np.ndarray, Letterbox]:
    """Resize into an `imgsz` square without distortion, padding the short axis.

    Returns the model blob (`float32`, RGB, 0..1, CHW, batched) and the transform,
    which `postprocess` needs to map boxes back.
    """
    frame_h, frame_w = frame.shape[0], frame.shape[1]
    scale = min(imgsz / frame_h, imgsz / frame_w)
    new_w = max(1, int(round(frame_w * scale)))
    new_h = max(1, int(round(frame_h * scale)))
    resized = cv2.resize(frame, (new_w, new_h), interpolation=cv2.INTER_LINEAR)

    pad_x = (imgsz - new_w) // 2
    pad_y = (imgsz - new_h) // 2
    canvas = np.full((imgsz, imgsz, 3), LETTERBOX_PAD_VALUE, dtype=np.uint8)
    canvas[pad_y : pad_y + new_h, pad_x : pad_x + new_w] = resized

    rgb = cv2.cvtColor(canvas, cv2.COLOR_BGR2RGB)
    blob = rgb.astype(np.float32) / 255.0
    blob = np.transpose(blob, (2, 0, 1))[np.newaxis, ...]
    return blob, Letterbox(scale=scale, pad_x=pad_x, pad_y=pad_y, frame_w=frame_w, frame_h=frame_h)


def box_iou(boxes: np.ndarray, reference: np.ndarray) -> np.ndarray:
    """IoU of each `[x1, y1, x2, y2]` row in `boxes` against one box, vectorised."""
    if boxes.size == 0:
        return np.zeros(0, dtype=np.float64)
    x1 = np.maximum(boxes[:, 0], reference[0])
    y1 = np.maximum(boxes[:, 1], reference[1])
    x2 = np.minimum(boxes[:, 2], reference[2])
    y2 = np.minimum(boxes[:, 3], reference[3])
    inter = np.clip(x2 - x1, 0.0, None) * np.clip(y2 - y1, 0.0, None)
    area_a = np.clip(boxes[:, 2] - boxes[:, 0], 0.0, None) * np.clip(boxes[:, 3] - boxes[:, 1], 0.0, None)
    area_b = max(0.0, reference[2] - reference[0]) * max(0.0, reference[3] - reference[1])
    union = area_a + area_b - inter
    return np.where(union > 0, inter / union, 0.0)


def nms(
    boxes: np.ndarray,
    scores: np.ndarray,
    iou_threshold: float = NMS_IOU_THRESHOLD,
    max_boxes: int = NMS_MAX_BOXES,
) -> list[int]:
    """Greedy non-maximum suppression: keep the best box, drop everything that
    overlaps it by more than `iou_threshold`, repeat.

    Greedy is enough and it is what the plan names. A pair at IoU 0.62 is one
    person the model saw twice, and the higher-confidence copy is the one kept.
    """
    order = np.argsort(-np.asarray(scores, dtype=np.float64))
    keep: list[int] = []
    remaining = order
    while remaining.size and len(keep) < max_boxes:
        best = int(remaining[0])
        keep.append(best)
        if remaining.size == 1:
            break
        rest = remaining[1:]
        ious = box_iou(boxes[rest], boxes[best])
        remaining = rest[ious <= iou_threshold]
    return keep


def providers_from_env(value: Optional[str] = None) -> list[str]:
    """`DET_PROVIDERS` as a list: comma-separated, blanks dropped, CPU default."""
    raw = value if value is not None else os.environ.get("DET_PROVIDERS", "")
    providers = [part.strip() for part in raw.split(",")]
    providers = [part for part in providers if part]
    return providers or ["CPUExecutionProvider"]


class YoloOnnx:
    """One ONNX YOLOv8 session, plus the pre- and post-processing around it."""

    def __init__(
        self,
        path: str | os.PathLike[str],
        providers: Optional[Sequence[str]] = None,
        imgsz: int = DEFAULT_IMGSZ,
    ) -> None:
        self.path = str(path)
        self.imgsz = int(imgsz)
        self.providers = list(providers) if providers else providers_from_env()
        self.session = self._open(self.providers)
        self.input_name = self.session.get_inputs()[0].name
        self.output_name = self.session.get_outputs()[0].name

    def _open(self, providers: list[str]) -> InferenceSession:
        """Open the session with the providers this build actually has.

        `DET_PROVIDERS=CUDAExecutionProvider` on a CPU build of onnxruntime is not an
        error: the library warns and quietly runs on CPU. That is the right outcome
        and the wrong way to find out, so the request is checked against
        `get_available_providers()` first and the substitution is said out loud. A GPU
        overlay that failed to install should look like what it is, and a box with no
        container toolkit should keep inferring.
        """
        available = list(get_available_providers())
        wanted = [p for p in providers if p in available]
        missing = [p for p in providers if p not in available]
        if missing:
            fallback = wanted or (["CPUExecutionProvider"] if "CPUExecutionProvider" in available else available[:1])
            log.warning(
                "model: providers %s are not available in this onnxruntime build (it has: %s); "
                "using %s — the CUDA overlay is opt-in (docker-compose.gpu.yml) and needs "
                "nvidia-container-toolkit on the host",
                ", ".join(missing),
                ", ".join(available) or "none",
                ", ".join(fallback) or "nothing",
            )
            wanted = fallback
        if not wanted:
            wanted = list(providers)  # nothing to choose from: let onnxruntime report the failure
        try:
            session = InferenceSession(self.path, providers=wanted)
        except Exception as exc:  # noqa: BLE001 - a provider that advertises itself and then fails to load
            if wanted != ["CPUExecutionProvider"] and "CPUExecutionProvider" in available:
                log.warning("model: %s failed to open (%s); using CPUExecutionProvider", wanted, exc)
                wanted = ["CPUExecutionProvider"]
                session = InferenceSession(self.path, providers=wanted)
            else:
                raise
        # The build *having* a provider is not the session *using* it: with the
        # CUDA wheel installed but no GPU reachable from the container,
        # onnxruntime opens the session anyway, runs it on CPU, and warns only
        # on its own logger. The startup line must say what is running, not
        # what was asked for — otherwise a GPU overlay that lost its device
        # looks healthy forever. (Observed 2026-11-07: exactly this, with the
        # log claiming CUDA while nvidia-smi showed no process.)
        active = session.get_providers()
        silent = [p for p in wanted if p not in active]
        if silent:
            log.warning(
                "model: %s were requested but the session opened on %s — inference is "
                "running there, not on the requested providers (a CUDA provider with no "
                "GPU reachable is the usual cause: check the container's device access)",
                ", ".join(silent),
                ", ".join(active) or "nothing",
            )
            wanted = active
        self.providers = wanted
        return session

    def preprocess(self, frame: np.ndarray) -> np.ndarray:
        """The plan's `preprocess`: letterbox, BGR→RGB, /255, CHW, batch, float32."""
        blob, _ = letterbox_frame(frame, self.imgsz)
        return blob

    def run(
        self,
        frame: np.ndarray,
        min_confidence: float = DEFAULT_MIN_CONFIDENCE,
        min_area_ratio: float = DEFAULT_MIN_AREA_RATIO,
    ) -> list[Detection]:
        """Infer one frame and return the boxes that survived every filter."""
        frame_h, frame_w = frame.shape[0], frame.shape[1]
        blob, box_map = letterbox_frame(frame, self.imgsz)
        out = self.session.run([self.output_name], {self.input_name: blob})[0]
        return self.postprocess(
            out,
            (frame_w, frame_h),
            min_confidence=min_confidence,
            min_area_ratio=min_area_ratio,
            box_map=box_map,
        )

    def postprocess(
        self,
        out: np.ndarray,
        frame_wh: tuple[int, int],
        min_confidence: float = DEFAULT_MIN_CONFIDENCE,
        min_area_ratio: float = DEFAULT_MIN_AREA_RATIO,
        box_map: Optional[Letterbox] = None,
    ) -> list[Detection]:
        """See the module-level `postprocess`, which this delegates to."""
        return postprocess(
            out,
            frame_wh,
            min_confidence=min_confidence,
            min_area_ratio=min_area_ratio,
            box_map=box_map,
        )


def postprocess(
    out: np.ndarray,
    frame_wh: tuple[int, int],
    min_confidence: float = DEFAULT_MIN_CONFIDENCE,
    min_area_ratio: float = DEFAULT_MIN_AREA_RATIO,
    box_map: Optional[Letterbox] = None,
) -> list[Detection]:
    """`[1, 84, 8400]` to the boxes that count.

    The tensor is channels-first: row 0..3 are `cx, cy, w, h` and rows 4..83
    are one column per COCO class. So the class is `argmax` of the score row
    and its value is the confidence — there is no objectness column in v8.

    `box_map` is the letterbox that produced the tensor. Pass None when the
    tensor is already in frame coordinates (the suite plants one), and the
    boxes are used as they arrive.
    """
    frame_w, frame_h = frame_wh
    rows = _as_rows(out)

    scores = rows[:, 4:]
    if scores.shape[1] != len(CLASS_NAMES):
        raise ValueError(
            f"model reports {scores.shape[1]} classes, this build knows {len(CLASS_NAMES)}"
        )
    classes = np.argmax(scores, axis=1)
    confidences = scores[np.arange(classes.size), classes]

    keep = confidences >= min_confidence
    keep &= np.isin(classes, np.fromiter(_DETECTION_IDS, dtype=np.int64))
    if not keep.any():
        return []

    boxes = rows[keep, :4]
    classes = classes[keep]
    confidences = confidences[keep]

    cx, cy, w, h = boxes[:, 0], boxes[:, 1], boxes[:, 2], boxes[:, 3]
    x1, y1, x2, y2 = cx - w / 2.0, cy - h / 2.0, cx + w / 2.0, cy + h / 2.0
    if box_map is not None:
        x1, y1, x2, y2 = (
            box_map.to_frame_x(x1),
            box_map.to_frame_y(y1),
            box_map.to_frame_x(x2),
            box_map.to_frame_y(y2),
        )
    x1 = np.clip(x1, 0.0, frame_w)
    y1 = np.clip(y1, 0.0, frame_h)
    x2 = np.clip(x2, 0.0, frame_w)
    y2 = np.clip(y2, 0.0, frame_h)

    areas = np.clip(x2 - x1, 0.0, None) * np.clip(y2 - y1, 0.0, None)
    keep_area = areas / float(frame_w * frame_h) >= min_area_ratio
    if not keep_area.any():
        return []
    x1, y1, x2, y2 = x1[keep_area], y1[keep_area], x2[keep_area], y2[keep_area]
    classes = classes[keep_area]
    confidences = confidences[keep_area]

    corners = np.stack([x1, y1, x2, y2], axis=1)
    order = nms(corners, confidences, NMS_IOU_THRESHOLD, NMS_MAX_BOXES)
    return [
        Detection(
            cls=int(classes[i]),
            conf=float(confidences[i]),
            x1=float(corners[i][0]),
            y1=float(corners[i][1]),
            x2=float(corners[i][2]),
            y2=float(corners[i][3]),
        )
        for i in order
    ]


def _as_rows(out: np.ndarray) -> np.ndarray:
    """Normalise whatever shape the model reports to `[anchors, 4 + classes]`."""
    arr = np.asarray(out, dtype=np.float64)
    if arr.ndim == 3:
        arr = arr[0]
    if arr.ndim != 2:
        raise ValueError(f"unexpected model output shape {np.asarray(out).shape}, want [1, 84, 8400]")
    channels = 4 + len(CLASS_NAMES)
    if arr.shape[0] == channels and arr.shape[1] != channels:
        arr = arr.T
    if arr.shape[1] != channels:
        raise ValueError(f"unexpected model output shape {arr.shape}, want [anchors, {channels}]")
    return arr
