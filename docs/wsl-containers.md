# WSL containers (wslc) and Skrog

Microsoft ships `wslc.exe` with WSL 2.9.3+, and it is heading for general
availability. If you have updated WSL recently you already have it. The question
this page answers is the one people actually arrive with:

> *Can I use Compose, Testcontainers, Dev Containers or buildx with `wslc`?*

**Today, no — and the reason is more interesting than "it's a different tool".**

## The short answer

Every `wslc` session boots a real Docker engine. Not a lookalike, not a
compatible reimplementation: **stock Moby**, built by Microsoft, listening on
`/var/run/docker.sock` inside the session VM.

What it does not have is an **endpoint**. `dockerd` is started with no `-H`
flag, so it listens on that guest unix socket and nowhere else. There is no
named pipe, no TCP port, and no inbound route into the session VM that a Docker
client could name. So the engine is complete, unmodified — and unreachable.

That is the whole gap. Not a missing API. A missing listener.

## What is actually inside a session

Verified on WSL 2.9.11.0, Windows 10 22H2 (build 19045), by asking the engine
directly from inside the session VM:

| | |
|---|---|
| Engine | dockerd **25.0.3**, API **1.44** (min 1.24), built 2026-05-31 |
| Runtime | containerd 2.2.4, runc 1.3.3, docker-init 0.19.0 |
| Socket | `srw-rw---- root docker /var/run/docker.sock` |
| Daemon flags | `dockerd --containerd /run/containerd/containerd.sock` — **no `-H`** |
| Storage | overlay2 on a per-session ext4 VHD |
| Builder | BuildKit (`Builder-Version: 2`) |
| GPU | CDI, enabled in `/etc/docker/daemon.json` |
| Guest OS | Azure Linux 3.0, kernel 6.18.40.1-microsoft-standard-WSL2 |
| File sharing | **virtiofs** — each Windows folder becomes its own share at `/mnt/{GUID}` |

The only TCP listener anywhere in that VM belongs to containerd's debug socket.

## Why `docker` cannot connect

`DOCKER_HOST` accepts four transports. Every one is a dead end here:

| scheme | why it cannot work |
|---|---|
| `unix://` | The socket is inside the VM's filesystem namespace, and Windows has no path to it |
| `tcp://` | dockerd is not listening on TCP — and nothing routes into the session VM from the host anyway |
| `npipe://` | A Windows construct; nothing inside a Linux VM can create one |
| `ssh://` | No sshd in the guest — it is a minimal image with `docker`, `curl`, `wget` and busybox |

The only channel in or out of the VM is WSL's own hvsocket message protocol,
spoken by `wslcsession.exe` on the Windows side. That protocol is not Docker. It
carries messages like `Mount`, `Exec`, `Connect`, `MapPort` and `UnixConnect` —
a general-purpose way to drive a Linux VM from Windows.

And here is the part worth knowing: **`wslcsession.exe` is itself a Docker
client.** It connects to that socket, consumes the full Docker API, and
re-publishes it as a different CLI and a different SDK. The API is spoken
fluently at both ends. It simply never leaves the VM.

## So what breaks

Anything that speaks Docker to a socket or a pipe, which is most of the
ecosystem:

- **Compose** — no endpoint to point at
- **Testcontainers** — needs both the API *and* mapped ports
- **Dev Containers**, **buildx**, **`act`**, **`gitlab-ci-local`**, **Dagger**
- Anything that bind-mounts `docker.sock` (docker-in-docker, Ryuk, agent sandboxes)

The `wslc` CLI itself is capable and pleasant — it runs, builds and networks
containers, with GPU support and a NuGet SDK for driving it from a Windows app.
If a first-party runtime with Microsoft behind it is what you want, use it. This
page is not an argument against `wslc`.

### This is a deliberate narrowing, not an oversight

Worth stating plainly, because it explains why the gap is unlikely to close by
accident: `wslc`'s registry allowlists, Group Policy/Intune controls and plugin
hooks are enforced in `wslcsession.exe` **in front of** the daemon. A narrow,
non-Docker API is what makes those controls enforceable. Exposing raw `dockerd`
would undo them, which is very likely why
[the endpoint request](https://github.com/microsoft/WSL/issues/40976) has sat
without a reply.

Any serious proposal for a Docker endpoint on `wslc` has to answer that
objection, not ignore it.

## What Skrog does instead

Skrog does not wrap or patch `wslc`. It runs **upstream `dockerd` in its own
WSL2 distro** and serves the endpoint Windows is missing:
`\\.\pipe\docker_engine` — the pipe `docker.exe` already talks to.

```powershell
irm https://wslkit.github.io/skrog/install.ps1 | iex
skrog install
docker run --rm hello-world
```

Nothing on that list above has to know Skrog exists. Compose, Testcontainers,
Dev Containers, buildx, `act` and Dagger work because the thing answering is
genuinely `dockerd`, byte for byte.

What you get today, on a shipping release:

- **The real Docker API** on the pipe, at Docker Desktop's speed — the transport
  is a vsock guest agent, not a per-command process spawn
- **A pinned engine.** `skrog lock` writes `skrog.lock` naming dockerd,
  containerd, runc and BuildKit to the commit; CI installs that same file. Docker
  Desktop cannot pin an engine version, and neither can `wslc` — `wsl --update`
  moves it underneath you. Engine drift stops being a category of bug
- **Lifecycle that stays out of the way** — starts at logon, survives reboot,
  sleep/resume and `wsl --shutdown`, idle-stops when you are not using it
- **`skrog doctor`** for the WSL/VPN/DNS/proxy failures that make WSL2 engines
  miserable in corporate networks
- **Policy and audit** at the pipe: deny `--privileged`, restrict bind-mount
  sources, allowlist registries, and log every container-affecting API call
- **No licence fee, no Electron, no Kubernetes, no tray-app ceremony.**
  Coexists with Docker Desktop if you still need it

It is Windows-only and Linux-containers-only, one maintainer, and not yet
code-signed — so SmartScreen warns on first run. Those are the honest edges.

## Using a wslc session as Skrog's engine

**Experimental**, and the most interesting thing on this page: Skrog can serve
that unreachable engine. `internal/pipeproxy` does not care which VM a
`docker.sock` lives in, so pointing it at a session gets stock `docker` talking
to the engine Microsoft ships:

```powershell
skrog install --engine wslc   # makes it this machine's engine
skrog start
docker run --rm hello-world
```

Or, to try it without changing the install you have:

```powershell
skrog proxy --engine wslc     # starts the container session if none is running
docker --context skrog-wslc ps
```

Compose and Testcontainers work against it, published ports reach Windows, and
Windows folders bind-mount over **virtiofs** — roughly 2× the write and 4× the
read of the 9p transport a WSL2 distro uses for `/mnt/c`. An administrator's
deployed WSL container policy is enforced at the pipe rather than bypassed.

What it cannot do is pin the engine: Microsoft ships it and `wsl --update` moves
it underneath you, so `skrog lock` has nothing to record.

**[Using wslc as Skrog's engine](wslc-backend.md)** is the full guide — setup,
what works and what does not, the measured numbers, policy and audit, and an
honest pros-and-cons table for choosing between the two backends.

## Verifying any of this yourself

Every claim on this page came from asking the engine directly. With a `wslc`
session running:

```powershell
wslc system session list
wslc --session <name> system session run curl -s --unix-socket /var/run/docker.sock http://localhost/version
```

If that prints a Docker version payload, you have just talked to the engine
Microsoft ships and the one nothing on your machine can otherwise reach.

For the whole picture in one place — the capability envelope, how the bridge
works, and what we would want from upstream — see
[wslc as a full Docker engine](wslc-deep-dive.md).

Both layers of the protocol are open source, MIT licensed, in
[microsoft/WSL](https://github.com/microsoft/WSL): the COM interfaces in
`src/windows/service/inc/wslc.idl`, and the guest message protocol in
`src/shared/inc/lxinitshared.h`. Nothing here required reverse engineering.

Corrections are welcome — this page is
[a markdown file](https://github.com/wslkit/skrog/tree/main/docs) and a pull
request away from being right.
