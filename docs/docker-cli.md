# Bundled docker CLI

Skrog runs the engine; you still need a `docker` command to talk to it. Docker
Desktop's `docker.exe` works fine (Skrog coexists with it), but if you want to
**remove Docker Desktop entirely**, Skrog can install the upstream command-line
tools itself:

```
skrog cli install
```

That fetches, checksum-verifies, and installs the exact versions pinned in this
build:

| Tool | What it is | Where it lands |
| --- | --- | --- |
| `docker` | the Docker CLI | `<state>\bin\docker.exe` (on PATH) |
| `docker compose` | Compose v2 plugin | `~/.docker/cli-plugins\docker-compose.exe` |
| `docker buildx` | Buildx plugin | `~/.docker/cli-plugins\docker-buildx.exe` |
| `docker-credential-wincred` | Windows credential helper | `<state>\bin` (on PATH) |

Nothing is fetched as "latest": the versions and per-download SHA-256 are compiled
into the Skrog binary, exactly like the engine (see [PLAN.md](../PLAN.md) §04).
The bytes come from each upstream project's own release assets — the docker CLI
from `download.docker.com`, compose/buildx/wincred from their GitHub releases —
so no Docker Desktop and no third-party mirror is involved.

## After installing

`skrog cli install` adds the `bin` directory to your **user** PATH (no elevation)
and prepends it, so a **new** terminal resolves `docker` to the bundled CLI. Verify:

```
docker version
docker compose version
docker buildx version
```

Another `docker` on PATH may win over the bundled one. `skrog doctor` flags it
and says what will actually move it:

```
[warn] bundled docker CLI: another docker shadows the bundled CLI on PATH
  bundled: ...\Skrog\bin\docker.exe
  active:  C:\Program Files\Docker\Docker\resources\bin\docker.exe
  active is on the SYSTEM PATH, which always resolves first
```

**Which PATH the other one is on decides what fixes it**, and `doctor` says which:

- **User PATH** — open a new terminal; a PATH change only reaches shells started
  after it. If it persists, that entry sits ahead of Skrog's; move Skrog's first.
- **System PATH** — a new terminal will *not* help, and neither will reordering
  your user PATH. Windows resolves the whole system PATH before the whole user
  PATH, and `skrog cli install` writes the user one so that it needs no
  elevation. Removing the directory from the system PATH, or removing the
  docker.exe in it, needs an administrator.
- **Neither** — nothing in the registry puts it there, so something in your
  shell's profile does.

Pass `--no-path` to install the tools without touching PATH (you add the directory
yourself).

## Building with a private dependency: `docker build --ssh`

`docker build --ssh default` works against Skrog, and Skrog does nothing to make
it work — which is the point. buildx dials the Windows OpenSSH agent
(`\\.\pipe\openssh-ssh-agent`) itself, and the forwarding channel rides the
ordinary API connection, so it crosses the bridge like every other request.
Private Go modules, npm and pip dependencies pulled over git all behave as they
do on a Linux engine.

The one thing that trips people is host-side: **Windows ships the `ssh-agent`
service disabled**, and the failure arrives at the end of a build rather than
before it:

```
ERROR: failed to convert agent config {default [] false}: invalid empty ssh agent
socket: Windows OpenSSH agent not available at \\.\pipe\openssh-ssh-agent.
```

`skrog doctor` warns about this before you spend a build on it. To fix it, in an
**elevated** shell:

```powershell
Set-Service ssh-agent -StartupType Automatic
Start-Service ssh-agent
ssh-add                     # back in your own shell
```

No administrator? Two ways through without the service: point `SSH_AUTH_SOCK` at
another agent, which buildx prefers over the pipe when it is set, or hand buildx
the key directly with `docker build --ssh default=C:\path\to\key`.

## Licensing

The bundled tools are open source and redistributable: the docker CLI, Compose,
and Buildx are Apache-2.0; the credential helper is MIT. Each project's LICENSE is
installed alongside the binaries in `<state>\bin\licenses`. Skrog is not
affiliated with or endorsed by Docker, Inc.; "Docker" is a trademark of Docker,
Inc.

## Uninstalling

```
skrog cli uninstall
```

removes the bundled tools and takes the `bin` directory back off your PATH. (Plain
`skrog uninstall` removes the engine; the CLI bundle is managed separately so you
can keep the CLI while reinstalling the engine.)

## Architecture note

Windows **amd64** gets all four tools. On Windows **arm64**, compose, buildx, and
the credential helper are available, but upstream does not yet publish a Windows
arm64 `docker.exe`; `skrog cli install` reports it as unavailable and installs
the rest. Use Docker Desktop's `docker` (which is arm64-native) until an upstream
arm64 CLI ships.

## Against the wslc backend

The bundled CLI also drives Skrog's experimental
[wslc backend](wslc-backend.md), with one thing worth knowing up front: the
engine Microsoft ships inside a session is **25.0.3, API 1.44**, and the CLI's
own minimum is 1.44. It negotiates down and works, with no headroom — anything
needing API 1.45 or newer fails rather than degrades.

[Behaviour differences from the distro backend](wslc-backend.md#behaviour-differences-from-the-distro-backend)
covers the rest: no cross-architecture execution, virtiofs bind-mount
permissions, and the several limits that belong to the `wslc` CLI rather than
to the engine and so do not apply here.
