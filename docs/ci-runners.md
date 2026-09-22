# CI runners

Skrog turns a Windows machine into a Linux-container CI runner: the engine is
upstream dockerd in WSL2, so a job that runs on `docker` on a Linux runner runs
here too, with no per-runner Docker Desktop license and no auto-update that
changes the engine under a pipeline.

There are two halves to this, and they are independent:

- **Installing Skrog on a runner** — [setup-skrog](https://github.com/wslkit/setup-skrog)
  for GitHub Actions, the same script in `before_script` for GitLab, or a baked
  image (see `contrib/`).
- **Running jobs against the engine** — everything below. Anything that follows
  `docker` follows Skrog: `DOCKER_HOST`, or the `skrog` docker context.

## Point a runner at the engine

Which endpoint Skrog serves depends on whether Docker Desktop is in the way:

| Docker Desktop | pipe Skrog serves | what plain `docker` does |
|---|---|---|
| **not installed** — the usual runner | `\\.\pipe\docker_engine` | works, no flag and no context |
| running, holding the default pipe | `\\.\pipe\skrog_engine` | reaches **Desktop**; use the `skrog` context |

So on a runner you normally need nothing at all: Skrog serves the pipe `docker`
already talks to.

The install also wires a docker context named `skrog`, and `setup-skrog` exports
`DOCKER_CONTEXT=skrog` for the rest of the job. **That form is correct on either
machine**, because the context points at whichever pipe Skrog actually took — which
is why it is what the action exports rather than a hardcoded host.

For a tool that does not read docker contexts, ask Skrog what it is serving
rather than hardcoding a pipe name:

```powershell
$env:DOCKER_HOST = (skrog status --json | ConvertFrom-Json).endpoint.dockerHost
```

`skrog status` reports the endpoint the **running supervisor actually bound**,
so it is right on either machine; plain `skrog status` prints it too. The
equivalent via docker is `docker context inspect skrog --format
'{{.Endpoints.docker.Host}}'`, which asks docker what Skrog told it — the same
answer, one step further away.

## GitHub Actions (hosted `windows-latest`)

Hosted x64 runners can run the engine — no self-hosting, no auto-logon, nothing
to provision:

```yaml
jobs:
  build:
    runs-on: windows-latest
    steps:
      - uses: actions/checkout@v4
      - uses: wslkit/setup-skrog@v2
        with:
          version: 0.8.0        # pin it; "latest" resolves the newest release
      - run: docker run --rm alpine:3.20 echo hello
```

This is not a claim on trust: setup-skrog's own CI runs exactly this on every
push and prints `Hello from Docker!` from a hosted runner.

**`windows-11-arm` cannot do this.** Those runners are real Windows 11 ARM64
but expose no nested virtualization, so WSL2 never starts — `wsl --import`
fails with `HCS_E_HYPERV_NOT_INSTALLED` even though WSL itself installs and
`Microsoft-Hyper-V` reports `Enabled`. Nothing in a workflow changes it. Use
`install: false` there to stage `skrog.exe` for packaging steps, or an x64
runner for anything that needs the engine.

A self-hosted runner still buys things a hosted one cannot — a warm image
cache, a persistent build cache, and your own hardware — which is the section
below.

## GitHub Actions (self-hosted Windows runner)

```yaml
jobs:
  build:
    runs-on: [self-hosted, windows, skrog]
    steps:
      - uses: actions/checkout@v4
      - uses: wslkit/setup-skrog@v2
        with:
          version: 0.8.0        # pin it; "latest" resolves the newest release
      - run: docker run --rm alpine:3.20 echo hello
```

The runner needs a **logged-on interactive session** — WSL2 cannot start from a
Windows service. See [auto-logon-runner.md](auto-logon-runner.md); `skrog
runner check` gives one verdict on whether a host is set up correctly
(auto-logon and its account, autostart, the supervisor, engine health, and
whether the machine sleeps on mains power).

Two things GitHub-hosted runners get wrong for this workload and a Skrog runner
gets right: the engine is pinned by *you* (a committed `skrog.lock`, so laptop
and runner install the same dockerd, containerd, runc and BuildKit to the
commit), and Linux containers run natively rather than needing a Linux runner
in the fleet.

## GitLab CI

### Shell executor (simplest)

The Windows runner runs jobs in a shell; Skrog is just the engine `docker`
talks to:

```yaml
default:
  before_script:
    - Invoke-WebRequest https://raw.githubusercontent.com/wslkit/setup-skrog/v2/scripts/install-skrog.ps1 -OutFile install-skrog.ps1
    - pwsh -File install-skrog.ps1 -Version 0.8.0
    - $env:DOCKER_CONTEXT = 'skrog'

build:
  script:
    - docker run --rm alpine:3.20 echo hello
```

On a baked image (`contrib/packer`) drop the `before_script` entirely.

### Docker executor (jobs run *inside* containers)

Point the executor at the engine's pipe in the runner's `config.toml`:

```toml
[[runners]]
  name = "windows-skrog"
  executor = "docker"
  [runners.docker]
    # The pipe Skrog took. On a runner without Docker Desktop that is
    # //./pipe/docker_engine; confirm with `skrog status`, which names the
    # endpoint the running supervisor bound.
    host = "npipe:////./pipe/docker_engine"
    image = "alpine:3.20"
    privileged = false
    volumes = ["/cache"]
```

Confirm the host with `skrog status`, and note that the runner
service still needs the interactive session that keeps WSL2 alive — the
executor talks to the engine, but the engine is per-user.

`gitlab-runner exec` was removed in 17.0, so a pipeline cannot be run locally
with the real runner any more; use `gitlab-ci-local` (below) for that.

## Testcontainers

Testcontainers needs no special configuration beyond `DOCKER_HOST`. Two things
about it are worth stating, because both were bugs in Skrog before they were
features:

- **Mapped ports** are reached from Windows (`container.MappedPort` +
  `container.Host`), which needs `engine.userland-proxy=false` — Skrog's
  default. See [vpn.md](vpn.md#published-ports-under-mirrored-networking).
- **Ryuk**, the reaper container, mounts the engine's socket
  (`-v //./pipe/...:/var/run/docker.sock`). Skrog maps a Windows named pipe in
  a bind mount to the engine's own `/var/run/docker.sock`, so this works as it
  does on Docker Desktop. No `TESTCONTAINERS_RYUK_DISABLED` needed.

```powershell
# On a runner Skrog holds the default pipe, so nothing is needed:
go test ./...

# Alongside Docker Desktop, point at Skrog explicitly:
$env:DOCKER_CONTEXT = "skrog"; go test ./...
```

The acceptance suite runs a real Testcontainers module (Go, Ryuk enabled)
against the engine: `test/e2e/testcontainers`.

## Running pipelines locally

The point of a local engine is that a pipeline can be *debugged* locally, not
just executed in CI: `act` for GitHub Actions, `gitlab-ci-local` for GitLab,
Dagger, and a BuildKit cache a laptop and a runner can share. All of it — with
measured numbers and each tool's own limitations — is in
**[local-ci.md](local-ci.md)**.

## Housekeeping on a long-lived runner

A runner that never reboots accumulates images, build cache and volumes:

```
skrog prune --all --until 24h           # reclaim, with a report
skrog reset --to clean-slate            # restore a snapshot between jobs
skrog prewarm images.txt                # pull the pinned list ahead of need
```

`skrog doctor` warns before the data disk runs out (`disk.warn-below` is
configurable), which is the failure that otherwise shows up as an opaque
mid-build error. See [housekeeping.md](housekeeping.md) and
[snapshots.md](snapshots.md).

## What is not supported

- **Windows containers.** Skrog runs Linux containers via WSL2; a job that
  needs Windows containers needs a Windows-container engine.
- **Rootless / multi-user on one host.** The engine is per-user, in that user's
  interactive session. One runner account per host.
- **Running as a Windows service without a session.** WSL2 will not start; this
  is why auto-logon exists.
