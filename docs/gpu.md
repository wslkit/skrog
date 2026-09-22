# GPU access

NVIDIA is supported and validated. AMD is **experimental** and untested on
hardware -- see [AMD (experimental)](#amd-experimental) at the end.

Run CUDA workloads — Ollama, vLLM, PyTorch, `nvidia-smi` — in containers on the
Skrog engine:

```
skrog enable-gpu
docker run --rm --device nvidia.com/gpu=all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi
```

WSL2 already does the hard part: the Windows NVIDIA driver projects the CUDA
libraries into every WSL2 distro at `/usr/lib/wsl/lib` and exposes the GPU at
`/dev/dxg`. `skrog enable-gpu` installs a **Container Device Interface (CDI)**
spec so the engine injects those into containers. dockerd supports CDI natively
(on by default since Docker 28.3.0), so nothing else is configured.

## Requirements

- An **NVIDIA** GPU with a recent, WSL-capable Windows driver (`nvidia-smi` works
  in a WSL distro). AMD and Intel GPUs expose compute to WSL differently and are
  not wired up here.
- WSL up to date (`wsl --update`) and the Skrog engine installed.

`skrog enable-gpu` checks that the distro actually sees the GPU (`/dev/dxg` and
the WSL CUDA library) before enabling, and tells you which of driver/WSL/GPU is
missing if not.

## The invocation

Use the CDI device form:

```
docker run --rm --device nvidia.com/gpu=all <image> <cmd>
```

Compose:

```yaml
services:
  app:
    image: nvidia/cuda:12.4.1-base-ubuntu22.04
    deploy:
      resources:
        reservations:
          devices:
            - driver: cdi
              device_ids: ["nvidia.com/gpu=all"]
```

### `--gpus all` too — on rootfs 29.7.2-4 and later

`docker run --gpus all` (and VS Code Dev Containers' `"hostRequirements":
{"gpu": true}`, which passes `--gpus all`) also reaches the GPU, with one
condition: the engine must find an `nvidia-cdi-hook` binary **when dockerd
starts** — that is what makes moby register its NVIDIA GPU driver and route
`--gpus` to the CDI spec instead of the legacy runtime hook. Rootfs 29.7.2-4
and later ship it; `skrog enable-gpu` tells you which spelling your rootfs
supports. On an older rootfs `--gpus all` fails to find a CDI spec —
use `--device nvidia.com/gpu=all`, which works everywhere.

Two honest notes about that binary. It is the one glibc program in the
otherwise-musl rootfs (NVIDIA's `go-nvml` does not build on musl), built as a
static binary from the pinned toolkit tag; and it is **never executed** for GPU
injection — the spec is hookless — it only has to exist. `--gpus device=0` also
works: the spec names the single WSL GPU both `all` and `0`.

## Why this works on the musl engine

The Skrog engine is Alpine (musl libc). The usual NVIDIA container stack targets
glibc — `libnvidia-container` does not build for musl, and the standard WSL CDI
spec runs an `ldconfig` hook that also breaks inside musl. Skrog's spec is
**hookless**: it rbind-mounts `/usr/lib/wsl` into the container and sets
`LD_LIBRARY_PATH=/usr/lib/wsl/lib` — which both glibc and musl containers honor —
so the driver libraries are found with no hook, no `libnvidia-container`, and no
binary to build for Alpine.

Prefer **glibc** CUDA base images (`nvidia/cuda`, `ubuntu`); they are the
best-tested path. musl (Alpine) containers can find the libraries too via
`LD_LIBRARY_PATH`, but CUDA userspace on musl is its own adventure.

## Persistence and turning it off

The setting persists: the CDI spec is re-applied on every engine start, so a
reinstall keeps GPU access. dockerd reads CDI specs dynamically, so
`skrog enable-gpu` takes effect on the next `docker run` — no restart.

```
skrog enable-gpu --off      # remove the spec
skrog doctor                # reports GPU visible / enabled / spec installed
```

## AMD (experimental)

**Untested on real hardware.** This is written from AMD's ROCm-on-WSL
documentation, not from a machine anyone has run it on ([#185]). It is opt-in
by name so nothing claims to work until somebody with a Radeon reports back.
If you try it, please say so on that issue either way.

```
skrog enable-gpu --vendor amd
docker run --rm --device amd.com/gpu=all rocm/rocm-terminal rocm-smi
```

### Why it plausibly works

The mechanism is not NVIDIA-specific. `/dev/dxg` is Microsoft's **DXCore**
interface, not CUDA, and AMD's ROCm reaches the GPU through it the same way:
"ROCDXG communicates with the Windows GPU driver through Microsoft's DXCore
interface (/dev/dxg)". The Windows driver projects `libdxcore.so` into the
same `/usr/lib/wsl/lib`. So the AMD CDI spec is the NVIDIA one with a
different `kind` and without the `nvidia-smi` bind, and it does by CDI exactly
what AMD's own documentation tells you to pass to `docker run` by hand.

The ROCm userspace (`librocdxg.so`, `/opt/rocm`) comes from the **image**, not
the host — AMD states ROCDXG needs no Radeon Software for Linux packages in
the distro. That is also why the musl engine is irrelevant here: nothing is
installed into it either way.

### What you need

| | |
| --- | --- |
| Windows driver | **AMD Software: Adrenalin Edition 26.2.2 for WSL2** or newer |
| Hardware | the discrete Radeon lineup, plus Ryzen **Strix** / **Strix Halo** APUs |
| Container image | a **ROCm** image. ROCm supports Ubuntu 24.04/22.04 — glibc, so a musl image will not work whatever the spec mounts |

### Limits, all AMD's rather than Skrog's

- **multi-GPU** is not supported under WSL
- **MIGraphX** is not supported under WSL
- the ROCm build of **JAX** is "not currently enabled or validated" under WSL
- `docker run --gpus all` does **not** route here — that path is
  NVIDIA-specific inside moby. Use `--device amd.com/gpu=all`.

### Why you have to name the vendor

`enable-gpu` cannot detect AMD on its own. For NVIDIA the probe is
`libcuda.so.1`, which only the NVIDIA driver projects. AMD's equivalent is
`libdxcore.so` — and DXCore is vendor-neutral, present on NVIDIA machines too,
with no AMD-specific marker documented in that directory. So `--vendor amd`
takes you at your word: it verifies a WSL GPU projection exists, and trusts
you about whose it is. Guessing would be worse — a silent false positive on an
NVIDIA machine.

Switching vendors removes the other spec, so you never end up with two kinds
installed. `skrog enable-gpu --off` removes both.

[#185]: https://github.com/wslkit/skrog/issues/185
