#!/usr/bin/env bash
# gpu-probe.sh — prove, layer by layer, that a GPU is usable from this box.
#
# Why this is a script and not a memory: every layer below fails with an error
# that names the layer *below* it. `--gpus all` failing with
# `ERROR_LIBRARY_NOT_FOUND` is a missing userspace driver; `nvidia-smi` failing
# with "couldn't communicate with the NVIDIA driver" is missing device nodes;
# CUDA init failing with `CUDA failure 999: unknown error ; GPU=-1` at
# `cudaSetDevice` is a missing `/dev/nvidia-uvm`. Reading the layers in order is
# the only way to tell which one you are actually standing on.
#
# Card: kanban/cards/S3-08.md. Host-side steps: docs/gpu-on-lxc.md.
#
# Usage: scripts/gpu-probe.sh [--cuda]
#   --cuda runs the compute test (pulls ~2 GB of images and wheels; it is the
#   test that says "the detector can run here", not "the GPU is visible").

set -uo pipefail

if [[ ! -t 1 ]]; then export TERM=dumb; fi
if [[ -n "${NO_COLOR:-}" || "${TERM}" == "dumb" ]]; then G=""; R=""; Y=""; B=""; N="";
else G=$'\e[32m'; R=$'\e[31m'; Y=$'\e[33m'; B=$'\e[36m'; N=$'\e[0m'; fi

pass() { printf '%s  ✓%s %s\n' "$G" "$N" "$1"; }
fail() { printf '%s  ✗%s %s\n' "$R" "$N" "$1"; }
info() { printf '%s  ·%s %s\n' "$Y" "$N" "$1"; }
head_() { printf '%s%s%s\n' "$B" "$1" "$N"; }

CUDA=0
[[ "${1:-}" == "--cuda" ]] && CUDA=1
FAILS=0

# node_probe opens a character node and prints "ok" or the errno. The point is
# ENXIO (6): a node created by mknod with no module loaded behind it. Every other
# GPU check passes in that state, and CUDA is the only thing that notices.
node_probe() {
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$1" <<'PY' 2>/dev/null
import os, sys
try:
    fd = os.open(sys.argv[1], os.O_RDWR)
    os.close(fd)
    print("ok")
except OSError as e:
    print(e.errno)
PY
  else
    echo "no-python3"
  fi
}

head_ "1. kernel side: is the module loaded and the card bound?"
if [[ -r /proc/driver/nvidia/version ]]; then
  pass "module: $(head -1 /proc/driver/nvidia/version | sed 's/NVRM version: //')"
else
  fail "/proc/driver/nvidia/version is unreadable — no NVIDIA kernel module visible"
  ((FAILS++))
fi
if lspci -n 2>/dev/null | grep -qiE '0x10de'; then
  pass "$(lspci -n 2>/dev/null | grep -iE '0x10de' | head -1 | sed 's/^0000//')"
else
  info "lspci shows no NVIDIA device (a VM without a passthrough card, or lspci missing)"
fi
if [[ -e /sys/bus/pci/drivers/nvidia ]]; then
  pass "driver bound: $(readlink /sys/bus/pci/devices/0000:01:00.0/driver 2>/dev/null | sed 's#.*/##')"
fi

head_ "2. userspace: is the driver library set installed, and does it match?"
if command -v nvidia-smi >/dev/null 2>&1; then
  SMIV=$(nvidia-smi --version 2>/dev/null | head -1 || true)
  pass "nvidia-smi present ${SMIV:+($SMIV)}"
else
  fail "nvidia-smi absent — install the userspace driver at the module's version"
  ((FAILS++))
fi
if ldconfig -p 2>/dev/null | grep -q 'libcuda.so.1'; then
  pass "libcuda.so.1 present: $(ldconfig -p | grep -m1 'libcuda.so.1' | awk '{print $NF}')"
else
  fail "libcuda.so.1 absent — NVML will load and CUDA will never start"
  ((FAILS++))
fi

head_ "3. device nodes: the ones NVML needs, and the one CUDA needs"
for dev in /dev/nvidia0 /dev/nvidiactl /dev/nvidia-uvm; do
  if [[ -c $dev ]]; then
    # A character node can exist with no driver behind it — `mknod` creates the
    # file, and `modprobe` is what puts the module behind it. The node then opens
    # with ENXIO, and CUDA answers 999 with a perfectly present-looking /dev.
    OPENED=$(node_probe "$dev")
    if [[ "$OPENED" == "ok" ]]; then
      pass "$dev ($(stat -c '%Hr:%Lr %U:%G %a' "$dev" 2>/dev/null)) — opens"
    elif [[ "$OPENED" == "6" ]]; then
      fail "$dev exists and opens with ENXIO — no module behind it (host: modprobe nvidia-uvm)"
      ((FAILS++))
    else
      info "$dev opens with errno $OPENED"
    fi
  else
    fail "$dev missing"; ((FAILS++))
  fi
done
if [[ ! -c /dev/nvidia-uvm ]]; then
  info "NVML works without /dev/nvidia-uvm; CUDA does not. A box that passes nvidia-smi"
  info "and fails cudaSetDevice with 999 is standing on exactly this line."
fi

head_ "4. NVML: does the card answer?"
if OUT=$(nvidia-smi --query-gpu=name,compute_cap,memory.total --format=csv,noheader 2>&1); then
  pass "NVML: $OUT"
else
  fail "NVML: $OUT"
  ((FAILS++))
fi

head_ "5. CDI: does the container runtime know about it?"
if command -v nvidia-ctk >/dev/null 2>&1; then
  if DEVS=$(nvidia-ctk cdi list 2>/dev/null | grep -c 'nvidia.com/gpu'); [[ "$DEVS" -gt 0 ]]; then
    pass "CDI devices: $DEVS ($(nvidia-ctk cdi list 2>/dev/null | grep 'nvidia.com/gpu' | tr '\n' ' '))"
  else
    fail "no CDI devices — regenerate: sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml"
    ((FAILS++))
  fi
else
  fail "nvidia-ctk absent — install nvidia-container-toolkit"
  ((FAILS++))
fi
if [[ -f /etc/cdi/nvidia.yaml ]] && ! grep -q '/dev/nvidia-uvm' /etc/cdi/nvidia.yaml; then
  info "the CDI spec contains no /dev/nvidia-uvm: it was generated on a host that has"
  info "no such node, so containers get a GPU that CUDA cannot initialise. Fix the host,"
  info "then regenerate — the spec inherits whatever nodes exist."
fi

head_ "6. docker --gpus all: does a container get the card?"
if docker info >/dev/null 2>&1; then
  if OUT=$(docker run --rm --gpus all ubuntu:24.04 sh -c 'ls /dev/nvidia* 2>/dev/null | wc -l' 2>&1); [[ "$OUT" -gt 0 ]]; then
    pass "container sees $OUT NVIDIA device node(s)"
  else
    fail "container sees no NVIDIA devices: $OUT"
    ((FAILS++))
  fi
else
  fail "no docker daemon visible from here"
  ((FAILS++))
fi

if [[ "$CUDA" == "1" ]]; then
  head_ "7. CUDA compute: the test that answers 'can the detector run here?'"
  info "onnxruntime 1.31.1 (the plan's pin) needs CUDA 13.x and cuDNN 9.x — CUDA 12"
  info "wheels fail on a CUDA 13 driver with 'CUDA failure 999'."
  if OUT=$(docker run --rm --gpus all \
      -e DEBIAN_FRONTEND=noninteractive \
      nvidia/cuda:13.1.0-runtime-ubuntu24.04 bash -lc '
        apt-get update -qq >/dev/null && apt-get install -y -qq python3 python3-pip >/dev/null
        python3 -m pip install --quiet --disable-pip-version-check --break-system-packages \
          onnxruntime-gpu==1.31.1 numpy onnx nvidia-cudnn-cu13 >/dev/null 2>&1
        SP=$(python3 -c "import site; print(site.getsitepackages()[0])")
        export LD_LIBRARY_PATH=$(python3 -c "import glob; print(\":\".join(glob.glob(\"$SP/nvidia/*/lib\")))"):/usr/local/cuda/lib64
        python3 - <<PY
import onnxruntime as ort, numpy as np, onnx
from onnx import helper, TensorProto
m = helper.make_model(helper.make_graph(
    [helper.make_node("Add", ["a","b"], ["c"])], "g",
    [helper.make_tensor_value_info("a", TensorProto.FLOAT, [4,4]),
     helper.make_tensor_value_info("b", TensorProto.FLOAT, [4,4])],
    [helper.make_tensor_value_info("c", TensorProto.FLOAT, [4,4])]),
    opset_imports=[helper.make_opsetid("", 17)])
m.ir_version = 8
onnx.save(m, "/tmp/add.onnx")
s = ort.InferenceSession("/tmp/add.onnx", providers=["CUDAExecutionProvider","CPUExecutionProvider"])
print("providers=" + ",".join(s.get_providers()))
out = s.run(None, {"a": np.ones((4,4),np.float32), "b": np.ones((4,4),np.float32)*2})
print("sum=" + str(float(out[0].sum())))
PY' 2>&1 | grep -E 'providers=|sum=' | tail -2); [[ "$OUT" == *"CUDAExecutionProvider"* ]]; then
    pass "$OUT"
  else
    fail "CUDA compute is not usable: ${OUT:-no output}"
    info "If this says CUDA failure 999 at cudaSetDevice, look at line 3 above."
    ((FAILS++))
  fi
else
  info "CUDA compute test skipped (pass --cuda)"
fi

echo
if [[ "$FAILS" == "0" ]]; then
  printf '%sGPU is reachable from this box.%s\n' "$G" "$N"
  exit 0
else
  printf '%s%d check(s) failed.%s The host-side steps are in docs/gpu-on-lxc.md.\n' "$R" "$FAILS" "$N"
  exit 1
fi
