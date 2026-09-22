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

## Cross-architecture builds: arm64 on an amd64 machine

Building for another architecture on the **default** builder fails, and the
error does not say why:

```
$ docker buildx build --platform linux/arm64 .
#5 [2/2] RUN uname -m
#5 0.224 exec /bin/sh: exec format error
```

That reads like a broken Dockerfile or a bad base image. It is neither. The
default `docker` driver builds inside dockerd's own BuildKit, which carries no
emulator, so a foreign `RUN` needs a `binfmt_misc` handler registered on the
host — and a stock WSL2 machine has none.

**A container-driver builder needs nothing installed**, because official
BuildKit images bundle the emulators:

```powershell
docker buildx create --name cross --driver docker-container --bootstrap
docker buildx build --builder cross --platform linux/arm64 .
# aarch64
```

Measured on this engine: the build succeeds, and the host's `binfmt_misc` table
is **untouched** afterwards — the emulators live inside the builder container
(`/usr/bin/buildkit-qemu-aarch64` and friends), not on your machine.

`skrog doctor` reports which case you are in, as information rather than a
warning: most people never cross-build.

### Why Skrog does not register the handlers for you

Docker Desktop does — its docs say multi-platform builds work "by default...
using the QEMU that's bundled within the Docker Desktop VM" — so this is one
step Desktop does not ask of you.

Registering handlers is not a distro-local act, though. **Every WSL2 distro
shares one utility VM and one `binfmt_misc` table** — same kernel, same
`boot_id` — so handlers Skrog registered would change how your Ubuntu executes
foreign binaries too, and would silently replace any that `tonistiigi/binfmt`
or your distro's `qemu-user-static` had put there. That is the same category as
`~/.wslconfig`, which [`skrog wsl-config apply`](vm-sizing.md) treats as
needing your consent rather than doing on your behalf.

So it stays a thing you opt into — and for **builds**, the one command above
is the answer for almost everyone and needs no opt-in at all.

## Running a foreign-architecture container

Everything above is about `docker build`. Running one is a different problem
with a different answer, and the buildx recipe does nothing for it — a builder
builds; it cannot run a container.

```powershell
docker run --rm --platform linux/amd64 alpine uname -m
# exec /bin/uname: exec format error
```

This matters most on **Windows on ARM**, where the majority of images on
Docker Hub are amd64-only. Pulling one works; starting it does not.

Skrog can register the interpreter for you, and does not until asked:

```powershell
skrog config set emulation.platforms linux/amd64
skrog restart
```

That is the whole setup. The same command now works:

```powershell
docker run --rm --platform linux/amd64 alpine uname -m
# x86_64
```

Both directions work, and `--platform` takes either spelling of the
architecture — `linux/arm64` and `linux/aarch64` are the same thing to docker:

```powershell
# on an amd64 machine, with emulation.platforms = linux/arm64
docker run --rm --platform linux/aarch64 ubuntu:26.04 uname -m
# aarch64
```

**You only ever name the other one.** Skrog refuses to register a handler for
the architecture you are already running on, because routing native binaries
through QEMU would slow everything down for no benefit:

```
PS> skrog config set emulation.platforms linux/amd64      # on an amd64 machine
skrog: amd64 is this machine's own architecture; it needs no emulation.
  Registering an interpreter for it would route native binaries through
  QEMU and slow everything down for no benefit.
```

The key takes a comma-separated list, but with emulators shipped for
`linux/amd64` and `linux/arm64` only, one of the two is always your own — so in
practice there is exactly one value to set, and it is the other architecture.

Compose picks this up with nothing extra; `platform:` on a service is the same
mechanism:

```yaml
services:
  arch:
    image: ubuntu:26.04
    platform: linux/arm64
    command: uname -m     # aarch64
```

The engine rootfs already carries the emulator — `qemu-x86_64` in the arm64
rootfs, `qemu-aarch64` in the amd64 one — so nothing is downloaded. The
handler is registered on every engine start, because `binfmt_misc` lives in
the kernel and `wsl --shutdown` wipes it, and it is **removed again on
`skrog stop` and on uninstall**.

Read the consent paragraph above before setting it: this is the change that
reaches your other distros, which is exactly why it is a key you set rather
than a default you discover.

Two things worth knowing:

- **It is slow.** QEMU user-mode emulation is several times slower than
  native. Fine for `apk add` and a smoke test, painful for a compile.
- **What changes is not only what you asked for.** An amd64-only image that
  fails fast today will start succeeding *slowly* instead, with nothing
  announcing it. `skrog doctor` reports which handlers are live.

Skrog ships emulators for `linux/amd64` and `linux/arm64` only. Those are what
images are really published for; the architectures below them are a list of
things that exist rather than things anyone runs on a Windows laptop.

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

## Architecture note: where each binary comes from

Both architectures get all four tools, but **not from the same place**, and the
difference is worth knowing before you audit them.

| tool | amd64 | arm64 |
|---|---|---|
| `docker` | Docker's published static build | **built by Skrog** from `docker/cli` source |
| `compose` | upstream release | upstream release |
| `buildx` | upstream release | upstream release |
| `docker-credential-wincred` | upstream release | upstream release |

### Why one of them is ours

Upstream publishes no Windows arm64 `docker.exe`. `download.docker.com`'s
static tree contains exactly one directory, `x86_64/`, and `docker/cli`
attaches no binaries to its releases at all. So on Windows on ARM,
`skrog cli install` used to lay down compose, buildx and the credential helper
— all of which *do* have real arm64 builds — and skip the one command anybody
types ([#450](https://github.com/wslkit/skrog/issues/450)).

`docker/cli` is Apache-2.0, pure Go and fully vendored, so Skrog builds it:
from a tag pinned to an exact commit, in the same pinned Go toolchain as the
engine, with SLSA build provenance and a cosign-signed checksum, published as
a `dockercli-v*` release of this repository. `third_party/docker-cli/` is the
whole of it.

**And you can reproduce it.** The build is deterministic — no timestamp, no
build host in the output — so running `third_party/docker-cli/build.sh` on
any machine with Docker produces a byte-identical binary. First verified
2026-09-21: a local build and the CI build of commit `4a63305d` both came out
at `910087c0d9a80ac0e7802f8c48b98cdc7fd890c78e0dab5f0472bee856ac50a0`,
28,319,232 bytes.

That is a stronger answer than the provenance attestation on its own. The
attestation says *we* built it; reproducibility means you do not have to take
our word for what from.

### Why the other one is not

amd64 keeps coming from Docker, deliberately:

- **Anyone can check it.** Those bytes are Docker's own. Re-download the zip
  and compare it against the `sha256` pinned in
  `internal/dockercli/manifest.json`, with no reference to Skrog. A binary we
  build can only be checked against us.
- **Blast radius.** Building it ourselves would put every user behind our
  build, rather than only the arm64 users who have no alternative.
- **A bump stays a URL and a hash**, not a rebuild.

It is **not** about code signing. Docker's published Windows CLI is not
Authenticode-signed either — measured on the binary `skrog cli install` places
— so nothing is being preserved on one side and lost on the other.

### It is meant to end

When upstream publishes a Windows arm64 `docker.exe`, the manifest points at
it, `third_party/docker-cli/` is deleted, and both architectures are Docker's
again. This is a stopgap with an exit condition, not a component Skrog has
taken ownership of.

### What `docker version` shows

The arm64 build reports the pinned version and the upstream commit it was
built from. It deliberately does **not** claim `Docker Engine - Community`,
which is Docker's build string for Docker's builds — that field is left empty
rather than filled in with something untrue.

