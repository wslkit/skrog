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
the engine starts at every logon, heals itself, and answers `docker` at the same speed as
Docker Desktop. Read [PLAN.md](PLAN.md) for the strategy and [ROADMAP.md](ROADMAP.md) for
the schedule; the [issue tracker](https://github.com/wslkit/skrog/issues) is the live state.

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

That downloads the newest release, **verifies it against the release's `SHA256SUMS`**,
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
one, and when, is [#77](https://github.com/wslkit/skrog/issues/77). An MSI waits on the
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
- **Docker Desktop speed**: a vsock transport to the engine (~80 ms `docker version`,
  measured at parity with Desktop), with an automatic fallback path
- **Idle RAM answer**: `skrog config set idle-timeout 30m` stops a quiet engine and
  cold-starts it (~1 s engine start) on your next `docker` command
- **`skrog doctor`**: diagnoses the WSL / PATH / credential-helper / supervisor quirk zoo,
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
- **Remote engine over mutual TLS**: `skrog serve --tcp` exposes the engine to a
  teammate or CI runner, reachable only by holders of a client cert this machine's CA
  signed — off by default; on the client, `skrog remote add/use` makes it docker's default
  in one command ([docs/remote-engine.md](docs/remote-engine.md))
- **`skrog status --stats`**: container/image/volume counts and reclaimable space, the
  VHDX footprint, VM memory and CPUs (configured versus actual), engine and supervisor
  uptime with idle-stop history, and bridge counters including **which transport is live**
  — the one number that explains a slow `docker` with a healthy engine
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
  `skrog doctor` warns below a configurable free-space floor
  ([docs/housekeeping.md](docs/housekeeping.md))
- **`skrog wsl-integrate <distro>`**: use the engine from inside your own WSL distros
- **`skrog migrate --from-desktop`**: copy images and volumes out of Docker Desktop,
  non-destructively and resumably (`--dry-run` first)
- Optional status-light tray (`skrogtray.exe`) — six menu items, forever
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

**Podman Desktop** fronts podman, whose API is Docker-*compatible* rather than Docker. That
distinction is usually invisible and occasionally expensive: rootless defaults, compose
handled by a shim, and the corners where tooling reaches for dockerd's actual behaviour.
Skrog runs dockerd itself, so there is no compatibility surface to fall off.

**`wslc`** is Microsoft's own, and if you have updated WSL recently you already have it —
`wslc.exe` ships with WSL 2.9.3+ and is headed for general availability. It runs, builds
and networks Linux containers, with GPU support, and there is a `Microsoft.WSL.Containers`
NuGet for driving containers from a Windows app. It is the strongest argument against
needing this project at all, and if you want a first-party runtime with Microsoft behind
it, use it.

What it does not give you is **an endpoint**. Every wslc session really does run a Moby
engine — dockerd on a unix socket inside the session VM — but it is started with no `-H`,
and no named pipe, TCP port or inbound route reaches it from Windows. So nothing that
already speaks Docker can talk to it: Compose, Testcontainers, Dev Containers, buildx,
`act`, `gitlab-ci-local`, Dagger. Skrog serves that endpoint — the real `docker` API on
`\\.\pipe\docker_engine` — which is why that list works here without any of those tools
knowing Skrog exists. `wslc` also
[cannot bind-mount WSL host paths yet](https://github.com/Microsoft/WSL/issues/40957),
which rules out docker-in-docker — a limitation of its CLI rather than of the engine
underneath. [wslc and Skrog](docs/wsl-containers.md) has the full picture: what is
actually inside a session, why `docker` cannot reach it, and how you can check both
yourself in one command.

Skrog can also **serve that engine**: `skrog install --engine wslc` makes a wslc
session this machine's engine, and `skrog start`, `status`, `version` and a
`doctor` check all know it. `skrog proxy --engine wslc` runs the same bridge in
the foreground if you would rather just try it. Compose, Testcontainers and
Windows-folder bind mounts all work. It is experimental, it cannot pin an
engine, and a session costs a second VM. [wslc as the engine](docs/wslc-backend.md)
is the full guide, with the measured numbers and a pros-and-cons table.

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
[Dev Containers](docs/devcontainers.md) ·
[wslc and Skrog](docs/wsl-containers.md) ·
[wslc as the engine](docs/wslc-backend.md)

**Keep it healthy**
[Housekeeping: prune, compact, relocate](docs/housekeeping.md) ·
[Snapshots](docs/snapshots.md) ·
[Staying current](docs/upgrading.md) ·
[Engine upgrade and rollback](docs/engine-upgrade.md) ·
[VM sizing](docs/vm-sizing.md)

**At work**
[Corporate networks](docs/corporate-network.md) ·
[VPNs](docs/vpn.md) ·
[Air-gapped installs](docs/air-gap.md) ·
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
[Local CI](docs/local-ci.md)

**Advanced**
[GPU (NVIDIA, and experimental AMD)](docs/gpu.md) ·
[Kubernetes](docs/kubernetes.md) ·
[Lifecycle hooks](docs/hooks.md) ·
[JSON output contract](docs/cli-json.md) ·
[Command reference](docs/reference.md)

**Contributing**
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
