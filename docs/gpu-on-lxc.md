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

## 3. Container: prove each layer

In this order; each line proves the layer above it and fails loudly if that layer
is missing.

```sh
nvidia-smi                                   # host nodes + container userspace
sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
sudo nvidia-ctk cdi list                     # expect 1 CDI device
docker run --rm --gpus all ubuntu:24.04 \
  sh -c 'ls /dev/nvidia*; ldconfig -p | grep -c libcuda'
```

When those four are green, Step 5's CUDA overlay (S5-07) has a test target and
`S5-06`'s GPU test target exists. Until then the detector's CPU path (S5-06) is
the one that is testable, which is the plan's own order — v1 runs on CPU, CUDA is
a build flag plus an overlay.

## What this is not

Not a plan change: the plan already makes CUDA optional. This is the deployment
note for one box, and the reason S5-07's acceptance depends on host config that
the app has no business touching.
