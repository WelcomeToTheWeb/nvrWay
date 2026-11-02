# The GPU path on this box (unprivileged LXC on Proxmox)

Written 2026-11-02 while setting up the test GPU for nvrWay Step 5. Card:
`kanban/cards/S3-08.md`.

**Who this is for:** the operator with root on the Proxmox host. Everything here
is host-side; nothing in it can be done from inside the container.

## The measured state

| Layer | State inside the container |
|---|---|
| GPU | `AD106M [GeForce RTX 4070 Max-Q]`, PCI `0000:01:00.0`, bound to `nvidia` |
| Kernel module | `615.78.08` (`/proc/driver/nvidia/version`), loaded, majors `195`/`506`/`509` registered |
| Container type | unprivileged LXC: `/proc/self/uid_map` = `0 100000 65536`, PVE kernel `7.0.14-26-pve` |
| Device nodes | **absent**, and un-creatable: `mknod` → EPERM with the full capability set |
| Userspace driver | installed 2026-11-02 at **615.78.08**, the module's own version |
| NVML | loads the library, answers `Driver Not Loaded` |
| CDI | `nvidia-ctk cdi list` → `Found 0 CDI devices` |
| Docker | runs inside the container; `nvidia` runtime registered; `--gpus all` → `ERROR_LIBRARY_NOT_FOUND` before the install, device-unreachable after it |

`Driver Not Loaded` is the precise signal: the library is right, the device is
unreachable. A **version** problem answers `Driver/library version mismatch` —
which is what installing `libnvidia-compute-615` (Ubuntu's 615.71.09) over a
615.78.08 module produces. Do not do that.

## 1. Host: userspace and device nodes

Same runfile the container uses, on the host:

```sh
cd /usr/local/src/nvidia
sh NVIDIA-Linux-x86_64-615.78.08.run --silent --no-kernel-module
ldconfig
nvidia-modprobe -u -c=0        # /dev/nvidia0, /dev/nvidiactl, /dev/nvidia-uvm
nvidia-smi                     # must print the 4070 before going further
```

`--no-kernel-module` because the 615.78.08 module is already loaded;
`--no-xchecks` does not exist in this installer; the X and libglvnd warnings are
irrelevant on a headless box.

`sha256sum` of the file this repo copies:
`3e3f6dddc2ee767a08185300af7be3cb286ece76698d2dde3524599782d07ffe`
(source: `https://download.nvidia.com/XFree86/Linux-x86_64/615.78.08/NVIDIA-Linux-x86_64-615.78.08.run`).

## 2. Host: expose the devices to the container

Unprivileged LXC needs **both** the device cgroup allow and the bind mounts.

```sh
CT=$(pct list | awk '/piWay/{print $1}')
pct set "$CT" \
  --features nesting=1,keyctl=1 \
  --lxc-args "lxc.cgroup2.devices.allow = c 195:* rwm" \
  --lxc-args "lxc.cgroup2.devices.allow = c 506:* rwm" \
  --lxc-args "lxc.cgroup2.devices.allow = c 509:* rwm" \
  --lxc-args "lxc.mount.entry = /dev/nvidia0 dev/nvidia0 none bind,optional,create=file" \
  --lxc-args "lxc.mount.entry = /dev/nvidiactl dev/nvidiactl none bind,optional,create=file" \
  --lxc-args "lxc.mount.entry = /dev/nvidia-uvm dev/nvidia-uvm none bind,optional,create=file" \
  --lxc-args "lxc.mount.entry = /dev/nvidia-uvm-tools dev/nvidia-uvm-tools none bind,optional,create=file" \
  --lxc-args "lxc.mount.entry = /dev/nvidia-modeset dev/nvidia-modeset none bind,optional,create=file" \
  --lxc-args "lxc.mount.entry = /dev/nvidia-caps dev/nvidia-caps none bind,create=dir,optional"
pct restart "$CT"
```

Majors, from the host's own `/proc/devices`: `195 nvidia` / `nvidiactl`,
`506 nvidia-uvm`, `509 nvidia-caps`. If PVE shows `no_new_privs` on this container,
turn it off — nested Docker needs it off.

## 2b. The node that decides whether CUDA works

The list above was run on 2026-11-02 and the GPU came alive — `nvidia-smi`, CDI,
`--gpus all`. **CUDA still fails**, with `CUDA failure 999: unknown error ;
GPU=-1` at `cudaSetDevice`, because `/dev/nvidia-uvm` is not in the container
(`nvidia0`, `nvidiactl` and `nvidia-uvm-tools` are). NVML does not open
`/dev/nvidia-uvm`; the CUDA runtime does, and without it no context is created.

```sh
CT=$(pct list | awk '/piWay/{print $1}')
# On the host, this is what creates the node (the module must be loaded):
nvidia-modprobe -u -c=0
ls -l /dev/nvidia-uvm                     # c 506 0 — if it is missing, containers cannot fix it
pct set "$CT" --lxc-args "lxc.mount.entry = /dev/nvidia-uvm dev/nvidia-uvm none bind,optional,create=file"
pct restart "$CT"
```

Then regenerate the CDI spec **in the container** — the spec inherits whatever
nodes exist where it is generated, and a spec generated without `/dev/nvidia-uvm`
hands every container a GPU that CUDA cannot initialise:

```sh
sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
grep -c '/dev/nvidia-uvm' /etc/cdi/nvidia.yaml      # must be ≥ 1
```

## 2c. Which CUDA, and from where

Measured on 2026-11-02 with the plan's pin (`onnxruntime==1.31.1`):

- It requires **CUDA 13.x + cuDNN 9.x**. With CUDA 12 wheels the provider will not
  load: `libcublasLt.so.13: cannot open shared object file`. This box's driver
  reports `CUDA UMD Version: 13.4`, so CUDA 12 is the wrong build for it.
- From PyPI: `nvidia-cudnn-cu13` is a real wheel (`9.27.0.42`). `nvidia-cublas-cu13`
  and `nvidia-cuda-runtime-cu13` are `0.0.1` sdists that fail to build — **get
  `libcudart.so.13`/`libcublasLt.so.13` from the CUDA image** (e.g.
  `nvidia/cuda:13.1.0-runtime-ubuntu24.04`) **or apt**, not from pip.
- The plan's Step 9 GPU overlay (S5-07) has to carry that: a CUDA-13 base plus
  `pip install nvidia-cudnn-cu13`, and the CPU provider as the fallback the plan
  already requires.

## 3. Container: prove each layer

`scripts/gpu-probe.sh` runs the ladder below, in order, and names the layer that
failed. Add `--cuda` for the compute test (it pulls ~2 GB; it is the test that
answers "can the detector run here").

```sh
scripts/gpu-probe.sh
scripts/gpu-probe.sh --cuda
```

Which is, by hand:

```sh
nvidia-smi --query-gpu=name,compute_cap,memory.total --format=csv,noheader
sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
sudo nvidia-ctk cdi list
docker run --rm --gpus all ubuntu:24.04 sh -c 'ls /dev/nvidia*; ldconfig -p | grep -c libcuda'
docker run --rm --gpus all nvidia/cuda:13.1.0-runtime-ubuntu24.04 bash -lc \
  'pip install --break-system-packages onnxruntime-gpu==1.31.1 numpy onnx nvidia-cudnn-cu13 && \
   python3 -c "import onnxruntime as o; print(o.get_available_providers())"'
```

**Current state on this box (2026-11-02, after §2 ran):** lines 1–4 pass,
`/dev/nvidia-uvm` is missing (§2b), and the CUDA layer fails at `cudaSetDevice`
with 999. Nothing in Steps 3–4 depends on the GPU; the CPU detector path is
unaffected, and S5-07's CUDA overlay is what §2b closes.

When those four are green, Step 5's CUDA overlay (S5-07) has a test target and
`S5-06`'s GPU test target exists. Until then the detector's CPU path (S5-06) is
the one that is testable, which is the plan's own order — v1 runs on CPU, CUDA is
a build flag plus an overlay.

## What this is not

Not a plan change: the plan already makes CUDA optional. This is the deployment
note for one box, and the reason S5-07's acceptance depends on host config that
the app has no business touching.
