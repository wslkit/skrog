<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/skrog-mark-ondark.svg">
    <img alt="Skrog" src="assets/skrog-mark.svg" width="120" height="120">
  </picture>
</p>

<h1 align="center">Skrog</h1>

<p align="center"><em><strong>skrog</strong> (n., Norwegian) — the hull: the body of the ship that carries the cargo and keeps the sea out.</em></p>

<p align="center">
  <a href="https://wslkit.github.io/skrog/"><img alt="Docs" src="https://img.shields.io/badge/docs-wslkit.github.io%2Fskrog-0a7d84"></a>
  <a href="https://github.com/wslkit/skrog/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/wslkit/skrog/actions/workflows/ci.yml/badge.svg?branch=main"></a>
  <a href="https://github.com/wslkit/skrog/releases"><img alt="Latest release" src="https://img.shields.io/github/v/release/wslkit/skrog?include_prereleases&sort=semver&label=release&color=0a7d84"></a>
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/github/license/wslkit/skrog?color=2F3B45"></a>
  <img alt="Go" src="https://img.shields.io/github/go-mod/go-version/wslkit/skrog?color=00ADD8">
  <img alt="Platform: Windows 10/11 + WSL2" src="https://img.shields.io/badge/platform-Windows%2010%2F11%20%2B%20WSL2-2F3B45">
</p>

A minimal, invisible way to run the upstream open source **Docker Engine on Windows** via WSL2.
No license fees, no Electron, no Kubernetes — install once, `docker ps` works forever, on
laptops and CI runners alike.

It is a **free, open source alternative to Docker Desktop** — the same upstream `dockerd`
underneath, without the subscription larger companies now need, the desktop application, or
the auto-update that changes your engine mid-sprint. **Rancher Desktop** and **Podman
Desktop** are free too; Skrog differs by being headless, and by running dockerd itself
rather than a Docker-*compatible* API. [How it compares](#how-it-compares) is the honest
version, including where each of them wins.

**Status: pre-release.** Installable and working as a daily driver: install once and
the engine starts at every logon, heals itself, and answers `docker` from a resident
bridge whose own cost is tens of milliseconds
([measured](https://github.com/wslkit/skrog/issues/326); most of what you wait for is
`docker.exe` starting up, which every Windows Docker CLI pays).
What changed in each release is [CHANGELOG.md](CHANGELOG.md); read
[PLAN.md](PLAN.md) for the strategy and [ROADMAP.md](ROADMAP.md) for the
schedule, and the [issue tracker](https://github.com/wslkit/skrog/issues) is
the live state. Security reports go through
[SECURITY.md](SECURITY.md).

One maintainer, young, and not yet code-signed — so SmartScreen warns on first run. If that
matters more to you than the licence does, come back in a few months.

## Install

Requirements: WSL 2.x from the Store or MSI on a virtualization-capable machine.
Windows 11 is the primary target; Windows 10 22H2 (build 19045) is tested and works.

**[docs/install.md](docs/install.md) is the step-by-step guide** — what to download,
how to verify it, and what to run. The short version is below.

You also need a `docker` command, and **Skrog does not install one by default**: Docker
Desktop's works (Skrog coexists with it), and on a machine without it `skrog cli install`
fetches the upstream tools — see [docs/docker-cli.md](docs/docker-cli.md).

```powershell
irm https://wslkit.github.io/skrog/install.ps1 | iex
```

Or with [scoop](https://github.com/wslkit/scoop-skrog):

```powershell
scoop bucket add skrog https://github.com/wslkit/scoop-skrog
scoop install skrog
```

The one-liner downloads the newest release, **verifies it against the release's `SHA256SUMS`**,
unpacks it to `%LOCALAPPDATA%\Programs\skrog` and adds it to your PATH. It stops there
on purpose — it does not provision anything. Then:

1. `skrog install` — downloads the checksum-verified engine rootfs, imports it as the
   `skrog-engine` WSL2 distro, starts the engine, wires a `skrog` docker context, and
   registers the supervisor to start at logon (`--no-autostart` opts out)
2. `skrog start` — brings up the always-on bridge now (from your next logon it starts
   itself)
3. `docker run --rm hello-world`

If Docker Desktop is not running, Skrog serves `\\.\pipe\docker_engine` — the pipe
`docker` already talks to — so nothing else is needed. The `skrog` docker context
install wires up is for the other case: when Desktop owns that pipe, Skrog serves its
own, and `docker --context skrog ...` (or `docker context use skrog`) is how you reach
it. The install output names the pipe it took, and `docker context inspect skrog` shows
it at any time.

Binaries are **not Authenticode-signed**, so SmartScreen warns on first run. The
[SignPath Foundation](https://signpath.org)'s free programme declined for now — it is
for projects with an established user base — and invited a reapplication as visibility
grows; paying for a certificate is the other route and needs nobody's approval. Which
one, and when, is [#358](https://github.com/wslkit/skrog/issues/358). An MSI waits on the
same answer, since an unsigned installer asking for elevation is worse than a zip; a
winget package does not, and one is on the way. The
[code signing policy](docs/code-signing.md) says who could produce
a signed binary and how. What every release **does** carry today is SLSA build
provenance and a cosign-signed `SHA256SUMS` — two checks an Authenticode signature does
not give you, since they tie the artifact to a workflow and a commit. See
[verifying a download](docs/security.md#verifying-a-download).

Prefer to do it by hand? Download the zip for your architecture from the
[latest release](https://github.com/wslkit/skrog/releases), check it against
`SHA256SUMS`, and unpack it anywhere on your PATH — the script does nothing else.
[Read it first](scripts/install.ps1) if you would rather not pipe a URL into your shell;
it is the same file that URL serves.

On a CI runner, use [setup-skrog](https://github.com/wslkit/setup-skrog) instead.

`skrog.exe uninstall` removes everything Skrog created — the distro and all images and
volumes in it, the autostart entry, any distro integrations — and restores your previous
docker context. Nothing else on the system is touched.

## What it does today

- Upstream Docker Engine (Linux containers) in a dedicated WSL2 distro — the real API, byte
  for byte: compose, buildx, Testcontainers, `run -it`, bind mounts with Windows paths
- **Dev Containers, with no shim**: `devcontainer up` and VS Code's "Reopen in Container"
  build and run against Skrog unchanged — the `C:\…\project` → `/workspaces/…` bind mount
  is translated by the bridge, and Features, `postCreateCommand` and
  docker-outside-of-docker all behave as they do on any Linux engine
  ([docs/devcontainers.md](docs/devcontainers.md))
- **Always-on supervisor**: starts at logon, survives engine crashes, `wsl --shutdown`, and
  sleep/resume; `skrog start/stop/restart/status --json`. Settings apply live — the
  supervisor follows the file, so nothing here needs a restart; `skrog restart
  --supervisor` replaces the supervisor process itself on the rare occasion that helps
- **Low-overhead transport**: a vsock path to the engine, with an automatic fallback.
  Measured on the reference host (Win10 22H2, n=10,
  [#326](https://github.com/wslkit/skrog/issues/326)): `docker version` **226 ms**,
  `docker ps` **209 ms**. Most of that is `docker.exe` starting — the client-only spawn
  floor on the same machine is ~165 ms — so the bridge costs tens of milliseconds, not
  hundreds. **Docker Desktop has not been benchmarked on this host**, so there is no
  parity claim; this line used to assert "~80 ms, at parity with Desktop" and neither
  half had a measurement behind it
- **Idle RAM answer**: `skrog config set idle-timeout 30m` stops a quiet engine and wakes
  it on your next `docker` command. **Measured**
  ([#398](https://github.com/wslkit/skrog/issues/398)): the wake is **4.8–6.3 s** to an
  answered `docker ps`, against **5.0–8.4 s** for a full `skrog stop` then `start`. The
  wake is *not* meaningfully cheaper, and the reason this line used to give for expecting
  it to be — "only dockerd has to come back" — is wrong: an idle stop is
  `wsl --terminate`, the same operation `skrog stop` performs, so the wake pays the same
  distro boot (~1.1 s) and the same dockerd startup (~2.6–3.3 s). Skrog's own share of a
  start is about 160 ms. `~1 s` was a guess and it was off by five
- **`skrog doctor`**: diagnoses the WSL / PATH / credential-helper / ssh-agent / supervisor quirk zoo,
  with `--json`, `--report` (paste straight into an issue), and `--fix` for the safe subset;
  recognizes corporate VPNs (GlobalProtect, AnyConnect, Zscaler…) and prints the MTU/DNS fix
  ([docs/vpn.md](docs/vpn.md))
- **Validated engine settings**: `skrog config set engine.<key>` edits the engine's
  `daemon.json` (registry mirrors, logging, DNS…), checked with `dockerd --validate` before
  it applies and rolled back if the engine will not come back
- **Lifecycle hooks**: run your own script on post-start / pre-stop / on-idle-stop / on-wake
  ([docs/hooks.md](docs/hooks.md))
- **Declarative installs**: `skrog install --config skrog.yaml` (idempotent) and
  `skrog config export` — infrastructure-as-code for a fleet
  ([docs/declarative-install.md](docs/declarative-install.md))
- **NVIDIA GPU**: `skrog enable-gpu`, then `docker run --device nvidia.com/gpu=all …` runs
  CUDA workloads (Ollama, vLLM, PyTorch) — a hookless CDI spec that works on the musl engine,
  no toolkit installed ([docs/gpu.md](docs/gpu.md))
- **Bundled docker CLI**: `skrog cli install` installs the upstream docker CLI + compose +
  buildx + credential helper — checksum-pinned, nothing fetched as "latest" — so you can
  uninstall Docker Desktop entirely ([docs/docker-cli.md](docs/docker-cli.md))
- **amd64 and Windows on ARM**, both native end to end — `skrog.exe`, the engine rootfs,
  dockerd and the containers. Upstream publishes no Windows arm64 `docker.exe`, so Skrog
  builds that one from source, reproducibly
- **Published ports that reach further than `localhost`**: under WSL2's default networking
  `docker run -p 8080:80` binds `127.0.0.1` on the Windows side and nothing else, so your
  phone gets connection refused. `skrog doctor` says so, and one opt-in key relays the port
  to every interface — the moment its container starts, tied to container lifetime so
  nothing outlives what it points at. When even `localhost` fails, `skrog doctor` reads what
  the container itself listens on and names the usual culprit: a dev server bound to
  `127.0.0.1` inside the container, which `-p` can never reach
  ([docs/ports.md](docs/ports.md))

  ```powershell
  skrog config set network.publish-scope lan
  docker run --rm -p 8080:80 nginx        # now reachable from your phone
  ```
- **Multi-platform containers**: run an image built for the *other* architecture — the
  common case on Windows on ARM, where much of Docker Hub is still amd64-only. One
  opt-in key, no emulator to download (the engine rootfs already ships `qemu-user`):

  ```powershell
  skrog config set emulation.platforms linux/amd64   # or linux/arm64
  skrog restart

  docker run --rm --platform linux/amd64 alpine uname -m
  # x86_64   ...on an arm64 machine
  ```

  Opt-in because `binfmt_misc` is kernel state shared by **every** WSL2 distro, so
  registering handlers changes how your Ubuntu runs foreign binaries too — the same
  consent bar as `~/.wslconfig`. They are removed again on `skrog stop` and on uninstall,
  and `skrog doctor` tells you which are live. Cross-architecture *builds* need no opt-in
  at all ([docs/docker-cli.md](docs/docker-cli.md#running-a-foreign-architecture-container))
- **Remote engine over mutual TLS**: `skrog serve --tcp` exposes the engine to a
  teammate or CI runner, reachable only by holders of a client cert this machine's CA
  signed — off by default; on the client, `skrog remote add/use` makes it docker's default
  in one command ([docs/remote-engine.md](docs/remote-engine.md))
- **`skrog status --stats`**: container/image/volume counts and reclaimable space, the
  VHDX footprint, VM memory and CPUs (configured versus actual), engine and supervisor
  uptime with idle-stop history, and bridge counters including **which transport is live**
  — the one number that explains a slow `docker` with a healthy engine. `--prometheus`
  emits the same numbers for node_exporter's textfile collector, so a fleet's health
  lands in the dashboard you already run — local only, still no telemetry
  ([docs/monitoring.md](docs/monitoring.md))
- **`skrog top`: where Vmmem's memory went**. `docker stats` shows the containers; this
  shows the VM they run in. Measured at the same moment on the reference host, `docker
  stats` accounted for 11.8 MiB, while the VM was using 776 MiB and Windows held 880 MiB
  for it. `top` accounts for all of it (an excerpt; the full reading is in the docs):

  ```
  VM        4 CPUs, CPU 9.1%   memory 776.4 MiB used of 7.6 GiB (7.0 GiB available)
            used is 111.7 MiB processes, 333.9 MiB page cache, 84.1 MiB kernel, 246.6 MiB not itemised by the kernel
  Windows   vmmem 879.7 MiB (Task Manager's figure), working set 879.7 MiB, committed 1.0 GiB

                                                MEMORY     LIMIT      ANON       FILE     ...  PIDS  MEM PSI  IO PSI
  cache                                         5.7 MiB    256.0 MiB  4.6 MiB    0 B           6     0.0      0.0
  engine (dockerd, containerd, build)           412.5 MiB  -          101.0 MiB  296.1 MiB     101   0.0      0.2
  kernel and drivers, not charged to any group  340.7 MiB
  ```

  What `docker stats` cannot show:
  - the engine's own daemons, and the image layers they cached
  - kernel and driver memory
  - other WSL distros in the same VM
  - Windows' side of Vmmem, with a hint when Windows holds far more than the VM uses
  - each container's page cache, which `docker stats` subtracts
  - PSI stall figures that say whether anything is actually short

  Unlike a streaming `docker stats`, it never starts the engine and never keeps it from
  idling. `docker stats` remains the tool for per-container network traffic and for
  remote engines. How to read every line, and what to do when something looks off:
  [docs/memory.md](docs/memory.md)
- **Right-size the VM with consent**: `skrog config set wsl.memory 4GB` then
  `skrog wsl-config apply` shows the diff to the GLOBAL ~/.wslconfig and writes only on
  a yes (`--yes` for runners, idempotent) — [docs/vm-sizing.md](docs/vm-sizing.md)
- **Engine upgrades, reversibly**: `skrog engine upgrade` swaps the engine binaries out of a
  checksum-verified rootfs and leaves /var/lib/docker alone, so images and volumes survive;
  a new engine that does not come back is rolled back automatically
  ([docs/engine-upgrade.md](docs/engine-upgrade.md))
- **Disk hygiene**: `skrog prune` reclaims stopped containers, unused images and build cache
  through whatever docker targets; **`skrog compact`** then shrinks the engine's VHDX itself
  (`fstrim` + `CompactVirtualDisk`, no administrator rights, so it works on Windows Home);
  `skrog doctor` warns below a configurable free-space floor. `skrog config set
  prune.every 168h` hands the job to the supervisor — opt-in, age-guarded, and
  never volumes ([docs/housekeeping.md](docs/housekeeping.md))
- **Pull-through registry cache**: `skrog cache enable` runs the upstream `registry:2`
  on the engine so a layer is fetched from the internet once — the answer to Docker Hub's
  per-IP rate limit behind a corporate NAT or across a runner fleet. Loopback only, pinned
  by digest, and excluded from the idle-stop probe so it never holds the engine awake
  ([docs/registry-cache.md](docs/registry-cache.md))
- **Admission control**: a small, fixed rule set — refuse `--privileged`, restrict bind
  sources, require digests, allow only listed registries — judged at the pipe on create,
  pull and push, before the engine sees the request. A **machine-wide layer** under
  `%ProgramData%` merges with the user's, and the user layer can only *tighten* it, so a
  fleet can deploy rules by Intune or GPO. It is configuration management, not access
  control, and [docs/policy.md](docs/policy.md) is precise about the difference
- **Shell completions**: PowerShell and bash, generated from the binary and drift-checked
  in CI, so they cannot describe flags the build does not have
  ([docs/install.md](docs/install.md#tab-completion))
- **`skrog wsl-integrate <distro>`**: use the engine from inside your own WSL distros
- **`skrog migrate`**: copy images and volumes out of **Docker Desktop**, **Rancher
  Desktop** (`--from-rancher`) or **Podman** (`--from-podman`) — non-destructively and
  resumably (`--dry-run` first), so trying Skrog never means starting from an empty engine
- Optional status-light tray (`skrogtray.exe`) — seven menu items: start/stop/restart,
  open logs, run doctor, check for updates, quit. It never grows into a container manager
- Headless CI installs (`--headless`, exit codes, `--json` on every state-reporting command —
  the contract in [docs/cli-json.md](docs/cli-json.md)), `skrog healthcheck --wait` as a
  runner readiness probe, `skrog logs --json` for log shippers, `skrog prewarm images.txt`
  to pre-pull a pinned image list, version pinning as a contract (nothing fetches "latest"),
  no telemetry
- A logged-on session is required — a WSL2 platform constraint that binds every WSL-based
  engine; for CI runners see [docs/auto-logon-runner.md](docs/auto-logon-runner.md), and
  `skrog runner check` verifies the setup in one verdict
- The supervisor is watched: if it ever dies, `skrogw.exe` restarts it in about a second
  (backing off, with a crash budget) instead of leaving every `docker` command broken until
  the next `skrog start`
- **CI runners**: GitHub Actions via
  [setup-skrog](https://github.com/wslkit/setup-skrog), GitLab shell or docker
  executor, Testcontainers (Ryuk included) — [docs/ci-runners.md](docs/ci-runners.md)
- **Debug pipelines locally, against the engine your runner uses**: `act`,
  `gitlab-ci-local`, Dagger, and a BuildKit cache a laptop and a runner share; commit
  `skrog.lock` and both install the same engine to the commit —
  [docs/local-ci.md](docs/local-ci.md)
- **Kubernetes when you want it, never bundled**: `kind` and `k3d` clusters run on the
  engine, with `kubectl` and NodePort/Ingress reachable from Windows —
  [docs/kubernetes.md](docs/kubernetes.md)

## What's ahead

A winget package, and signed installers beyond it — tracked in the
[issue tracker](https://github.com/wslkit/skrog/issues). Data-dir relocation
shipped: see [`skrog relocate`](docs/housekeeping.md).

## How it compares

The honest version, including where the alternatives win.

**Docker Desktop** is why most people arrive: since 2021 it needs a paid subscription for
larger companies, and a lot of developers are told to stop using it. Skrog is the engine
underneath it — the same upstream dockerd — without the desktop application, the licence,
or the auto-update that changes your engine mid-sprint. Desktop gives you a GUI, Windows
containers, Kubernetes and macOS support; Skrog gives you none of those, deliberately.

**Rancher Desktop** also runs a real container engine on WSL2, free and open source, with a
GUI and k3s in the box. If you want a cluster or a graphical UI, take Rancher — Skrog has
neither and will not grow them. It is also cross-platform, where Skrog is Windows-only.
If you have already built up images there, `skrog migrate --from-rancher` copies them over
without touching Rancher.

**Podman Desktop** fronts podman, whose API is Docker-*compatible* rather than Docker. That
distinction is usually invisible and occasionally expensive: rootless defaults, compose
handled by a shim, and the corners where tooling reaches for dockerd's actual behaviour.
Skrog runs dockerd itself, so there is no compatibility surface to fall off. That same
compatible API is how `skrog migrate --from-podman` reads your images out.

**Microsoft's own WSL container tooling** ships with recent WSL releases and runs, builds
and networks Linux containers with a CLI of its own. If you want a first-party runtime
with Microsoft behind it and you do not need the Docker API, use it.

What it does not give you is **an endpoint**. Its sessions run a Moby engine on a unix
socket inside the session VM, started with no `-H`, and no named pipe, TCP port or
inbound route reaches it from Windows. So nothing that already speaks Docker can talk to
it: Compose, Testcontainers, Dev Containers, buildx, `act`, `gitlab-ci-local`, Dagger.
Skrog serves that endpoint — the real `docker` API on `\\.\pipe\docker_engine` — which is
why that list works here without any of those tools knowing Skrog exists.

**What none of them do** is let you pin the engine. `skrog lock` writes the exact dockerd,
containerd, runc and BuildKit commits; `setup-skrog` installs that same file on the runner.
Docker Desktop ships whatever version it ships, and updates it on its own schedule.

Reasons to pick something else, stated plainly: you need a GUI, Windows containers, macOS
or Linux, a bundled Kubernetes, a vendor with a support contract, or a runtime that ships
with the OS and you do not need the Docker API. Skrog is one maintainer and a young
project.

## What it will never be

Windows containers, Kubernetes, or a management GUI. Because Skrog serves the standard
Docker API, existing frontends (Portainer, lazydocker, VS Code) already work against it —
and a cluster is just containers, so `kind` and `k3d` run on it today
([docs/kubernetes.md](docs/kubernetes.md)) without Skrog owning a control plane.

## Repository layout

Standard Go project layout — the Go toolchain, not a framework, decides this shape:

```
cmd/skrog/     the CLI — the product; every capability lives here
cmd/skrogw/    windowless logon launcher and supervisor watchdog (no console flash)
cmd/skrogtray/ optional status-light tray; shells out to the CLI, holds no logic
internal/       implementation packages, compiler-enforced private to this module
  wsl/          every wsl.exe call, behind an interface so tests run anywhere
  ...           provision, pipeproxy, supervise, config, migrate, integrate,
                doctor, engineconfig, skrogfile, tray
guest/          Linux side: rootfs build scripts, vsock agent
docs/           operator docs, e.g. the unattended/auto-logon runner playbook
site/           the docs site: layouts and nav only -- content comes from docs/
test/e2e/       cross-package suite; the only part needing real WSL2
spike/          throwaway experiments, deleted once their issue closes
```

Running unattended (CI runners, build agents) needs a logged-on session, because
WSL2 cannot start from a Windows service — see
[docs/auto-logon-runner.md](docs/auto-logon-runner.md). Who can reach the engine
and where the trust boundaries lie (the pipe ACL, `wsl-integrate`'s shared
socket, the supply chain) is documented in [docs/security.md](docs/security.md).

Two Go conventions worth stating, since they surprise people arriving from other ecosystems:
**tests live beside the code they test** (`wsl.go` and `wsl_test.go` in the same folder — the
`_test.go` suffix is how the toolchain finds them, and package-private tests need it), and
there is no `src/` — the module root *is* the source root, and `internal/` is a compiler
rule (nothing outside this module can import it), not a naming preference. `test/e2e/` exists
only for suites that belong to no single package.

## Documentation

Also published as a site: **<https://wslkit.github.io/skrog/>**, generated
from these same files. Grouped below by the question you arrived with; every
page is reachable from here.

**Start**
[Installing Skrog](docs/install.md) ·
[Bundled docker CLI](docs/docker-cli.md) ·
[Dev Containers](docs/devcontainers.md)

**Keep it healthy**
[Housekeeping: prune, compact, relocate](docs/housekeeping.md) ·
[Where the VM's memory goes](docs/memory.md) ·
[Snapshots](docs/snapshots.md) ·
[Staying current](docs/upgrading.md) ·
[Engine upgrade and rollback](docs/engine-upgrade.md) ·
[VM sizing](docs/vm-sizing.md)

**At work**
[Corporate networks](docs/corporate-network.md) ·
[VPNs](docs/vpn.md) ·
[Air-gapped installs](docs/air-gap.md) ·
[Registry cache](docs/registry-cache.md) ·
[Security and trust boundaries](docs/security.md) ·
[Code signing policy](docs/code-signing.md) ·
[Audit log](docs/audit.md) ·
[Admission control](docs/policy.md)

**Fleet and CI**
[CI runners](docs/ci-runners.md) ·
[Unattended / auto-logon runners](docs/auto-logon-runner.md) ·
[Declarative install](docs/declarative-install.md) ·
[Profiles](docs/profiles.md) ·
[Remote engine over mTLS](docs/remote-engine.md) ·
[Local CI](docs/local-ci.md) ·
[Monitoring a fleet](docs/monitoring.md)

**Advanced**
[GPU (NVIDIA, and experimental AMD)](docs/gpu.md) ·
[Kubernetes](docs/kubernetes.md) ·
[Lifecycle hooks](docs/hooks.md) ·
[JSON output contract](docs/cli-json.md) ·
[Command reference](docs/reference.md)

**Project**
[Changelog](CHANGELOG.md) ·
[Reporting a vulnerability](SECURITY.md) ·
[Releasing](RELEASING.md) ·
[Bumping the engine and docker CLI](docs/bumping-upstream.md) ·
[Design notes](docs/design/) ·
[Per-workspace engines](docs/design/per-workspace-engine.md)

Editing the docs edits the site: `pwsh -File scripts/build-docs.ps1 -Serve`
previews it locally, and merging to `main` publishes it. Nothing in `site/` is
hand-written prose except the front page.

## License

[Apache-2.0](LICENSE). Docker and the Docker logo are trademarks of Docker, Inc.
Skrog is not affiliated with or endorsed by Docker, Inc.
