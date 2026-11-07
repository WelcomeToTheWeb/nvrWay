"""Provider honesty at session open (S9-06).

The trap this suite pins: a CUDA build of onnxruntime with no GPU reachable
from the container does not fail — it opens the session, runs it on CPU, and
warns only on the ORT logger. If the worker's `providers` attribute kept the
*requested* list, the startup line would claim CUDA forever on a box that
quietly infers on CPU. `YoloOnnx._open` reconciles against the session's
active providers so the log says what is running.
"""

import logging
from types import SimpleNamespace

import pytest

from nvr_detector import model as model_module
from nvr_detector.model import YoloOnnx


class _SilentFallback:
    """Stands in for `InferenceSession`: accepts CUDA, runs CPU."""

    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    def get_inputs(self):
        return [SimpleNamespace(name="images")]

    def get_outputs(self):
        return [SimpleNamespace(name="output0")]

    def get_providers(self):
        return ["CPUExecutionProvider"]


def test_a_silent_cuda_fallback_is_said_out_loud(monkeypatch, caplog):
    # The suite runs on the CPU build, where CUDA is not even available and
    # the *other* warning path fires first; to reach the silent path we stand
    # in for a GPU build's provider list as well.
    monkeypatch.setattr(model_module, "get_available_providers",
                        lambda: ["CUDAExecutionProvider", "CPUExecutionProvider"])
    monkeypatch.setattr(model_module, "InferenceSession", _SilentFallback)
    with caplog.at_level(logging.WARNING, logger="nvr_detector.model"):
        session = YoloOnnx("/nonexistent/model.onnx", ["CUDAExecutionProvider", "CPUExecutionProvider"])

    # The attribute — and with it the startup line — carries the ACTIVE list.
    assert session.providers == ["CPUExecutionProvider"]
    assert any("were requested but the session opened on" in r.getMessage()
               for r in caplog.records), \
        "the silent fallback must produce a worker-level warning, not only an ORT one"


def test_an_honest_cuda_session_keeps_its_providers(monkeypatch):
    class _Honest(_SilentFallback):
        def get_providers(self):
            return ["CUDAExecutionProvider", "CPUExecutionProvider"]

    monkeypatch.setattr(model_module, "get_available_providers",
                        lambda: ["CUDAExecutionProvider", "CPUExecutionProvider"])
    monkeypatch.setattr(model_module, "InferenceSession", _Honest)
    session = YoloOnnx("/nonexistent/model.onnx", ["CUDAExecutionProvider", "CPUExecutionProvider"])
    assert session.providers == ["CUDAExecutionProvider", "CPUExecutionProvider"]


@pytest.mark.parametrize("requested", [["CPUExecutionProvider"], []])
def test_the_cpu_baseline_is_untouched_by_the_reconciliation(requested, monkeypatch):
    """The reconciliation must not rewrite the baseline path: on a CPU build
    asking for CPU, nothing is silent and nothing changes."""
    monkeypatch.setattr(model_module, "InferenceSession", _SilentFallback)
    session = YoloOnnx("/nonexistent/model.onnx", requested)
    if requested:
        assert session.providers == ["CPUExecutionProvider"]
