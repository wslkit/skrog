# wslc as a full Docker engine

Microsoft ships a Docker engine with WSL. Almost nobody can reach it.

This page is the long version of why, what the engine inside a WSL container
session can and cannot do, and how to drive all of it from the **stock Docker
CLI** — no forks, no shims, no patched clients. Everything here was measured on
WSL 2.9.11.0 / Windows 10 22H2, not inferred from documentation.

If you only want to *use* it, [wslc as the engine](wslc-backend.md) is the
task-shaped page and [wslc and Skrog](wsl-containers.md) is the short
orientation. This one is for people who want to know how it works, or who are
building something else against the same surface.

## The one thing to take away

> **The limits people attribute to WSL containers are mostly the CLI's, not the
> engine's.**

Inside a session is a stock Moby `dockerd`. It answers the real Docker Engine
API. The `wslc` CLI exposes a deliberately narrow slice of it, and because that
CLI is the only thing most people ever touch, its narrowness gets mistaken for
the platform's.

Reach the socket instead and Compose, Testcontainers, Dev Containers, buildx and
`docker cp` all work — unmodified, against the stock CLI. Not emulated, not
approximated. The engine was always capable; the path to it was missing.

## What is actually in a session

A `wslc` session is a lightweight utility VM, separate from any WSL distro you
have installed:

| | |
|---|---|
| engine | Moby `dockerd` **25.0.3**, `GitCommit f417435` — a Microsoft build of stock Moby |
| API version | **1.44** (minimum 1.24) |
| socket | `/var/run/docker.sock` |
| root filesystem | tmpfs overlay over a read-only `system.vhd` |
| lifetime | idle-terminates when its activity refcount hits zero |
| storage | `%LOCALAPPDATA%\wslc\sessions\<name>\{storage,swap}.vhdx` |

Two properties in that table drive nearly every design decision downstream: the
root filesystem **does not persist**, and the VM **deletes itself** when it
believes it is idle.

## Why nothing can reach it

`dockerd` is started with no `-H` flag. It listens on a Unix socket inside the
VM and nothing else — no TCP, no vsock, no named pipe:

- A Unix socket is not reachable from Windows.
- The session VM's network is not a route you can dial from the host.
- The `wslc` CLI has no "expose the socket" mode.

So the engine is fully functional and completely unreachable. Every capability
below exists the moment you solve *that* one problem.

This is a deliberate narrowing on Microsoft's part, not an oversight — the CLI
is scoped to running containers, and a reachable engine socket is a much larger
security surface. Worth saying plainly, because "we found a gap" and "we used a
supported mechanism Microsoft documents" are different claims, and this is the
second.

## What the engine can do

Verified against a live session, driven by the stock Docker CLI over a bridge:

| | |
|---|---|
| Lifecycle | `run`, `create`, `start`, `stop`, `rm` |
| Streaming | `exec`, `logs`, `logs -f`, `attach`, `stats`, `events` |
| Inspection | `ps`, `inspect`, `images`, `version`, `system df` |
| Images | `pull`, `push`, `commit`, `save`, `load`, `tag` |
| Build | `build` **and** `buildx` |
| Data | `docker cp`, networks, named volumes |
| **Compose** | healthchecks, `depends_on` conditions, named volumes, published ports |
| **Testcontainers** | including the Ryuk reaper |
| **Dev Containers** | `devcontainer up`, `exec`, `postCreateCommand`, `containerEnv`, workspace mount |
| **Windows-folder bind mounts** | `-v C:\src\app:/app`, over virtiofs |

The last four are precisely the ones the `wslc` CLI cannot express. They are
worth separating out, because they are the evidence for the claim at the top:

- **Ryuk** needs the engine socket bind-mounted *into* a container. The CLI
  cannot express that mount at all. Over the API it is an ordinary bind.
- **Published ports** need a host-side relay (see below). The CLI has one, but
  it only ever binds `127.0.0.1`.
- **Windows folders** mount over virtiofs, which is the strongest practical
  reason to use this backend at all.

## What it cannot do

Honest limits, separated by whose limit it is.

### The engine's limits

| | |
|---|---|
| **API 1.44, with zero headroom** | Docker CLI 29.x's *minimum* is 1.44, so the stock CLI negotiates down and works — but anything needing 1.45+ fails rather than degrades: newer BuildKit attestation/SBOM flags, some Compose fields, `docker debug` |
| **No cross-architecture execution** | `--platform` selects and pulls fine, but there is no qemu/binfmt, so a foreign binary cannot run: `exec /bin/true: exec format error`. `buildx --platform linux/arm64` clears `FROM` and dies at the first `RUN`. **Not a wslc-only trait:** the distro backend registers no handlers either, and neither does a plain Linux engine — see [cross-architecture builds](docker-cli.md#cross-architecture-builds-arm64-on-an-amd64-machine), which work on both through a container-driver builder |
| **No engine pinning** | Microsoft ships it. `wsl --update` can move it in either direction, silently. Reproducibility cannot be promised |

A trap worth stating because it is easy to inflict on yourself: pulling a
foreign platform **overwrites the local tag**. After
`docker pull --platform linux/arm64 busybox`, plain `docker run busybox` fails
with `exec format error` until you re-pull the native one.

### The platform's limits

| | |
|---|---|
| **DNS inside containers** | a session hands containers the Windows host's LAN router as their nameserver, and it answers `SERVFAIL` from inside the session VM. Routing is fine and the daemon itself resolves normally. `--dns=1.1.1.1` fixes run time; **nothing fixes build time**, so a Dockerfile or Dev Container Feature that reaches the network during a build fails. Reproduces with plain `wslc run`, so it is not the bridge — [#351](https://github.com/wslkit/skrog/issues/351) |
| **No named sessions** | the shipped CLI cannot create one, so anything bridging it shares the default session. `wslc system session terminate` then takes your containers down with it |
| **UDP published ports** | the relay is a stream transport |

## Getting a full Docker API out of it

Four problems, in the order they bite.

### 1. Reaching the socket: vsock, not the CLI

The obvious approach — shell out to `wslc` per request — is a non-starter: a
process spawn per API call, and roughly **360 spawns an hour** just to keep
state observed. So the CLI is used exactly twice, then never again while the
bridge is up:

1. find or create the session
2. stream a guest agent into it

After that, Windows dials the agent directly over **AF_HYPERV** (vsock) —
around **4 ms** — and the agent relays to `/var/run/docker.sock`. No process
spawn in the data path.

The VM GUID needed for that dial is discoverable from the registry **without
elevation**, so none of this needs an administrator.

A detail that turns out to matter: the vsock **port is the identity**. The
agent listens on ASCII `"hawc"` (`0x68617763`), one letter off the `"haws"` the
distro-backed agent uses. The dialer enumerates every running compute system and
takes the first that completes the handshake — so with both an engine distro and
a session running, a shared port would be ambiguous and you would reach
whichever VM answered first. Keying on the port means `VsockDialer{Port: AgentPort}`
can only ever find a session agent, in whichever VM it happens to live, with no
VM-GUID plumbing at all. A second port, `"hawf"`, carries TCP forwarding, so the
engine relay keeps a narrow contract.

### 2. Placing the agent, without leaking the secret

The agent and a per-run shared secret are streamed over `wslc system session run`
on **stdin**, byte-exact and sha256-verified in the guest.

Deliberately not over a virtiofs share: a share is a Windows folder readable by
every other process running as the user, and the secret is the only thing
stopping a sibling listener from impersonating the agent.

### 3. The VM vanishes underneath you

This is not an edge case, it is the normal operating mode. The root filesystem
is a tmpfs overlay, and the VM idle-terminates as soon as it has no running
containers — so the agent is discarded on every such cycle and the next boot has
none. Measured while bridging a live `docker` session: after one `run --rm`
container exited, the VM restarted with **uptime 21 s** and `/tmp/skrog-agent`
was simply gone; every subsequent request failed with "the pipe has been ended".

The fix is to treat a failed dial as "re-bootstrap", not "retry". Recovery is
serialised behind a mutex, because a VM restart produces a *burst* of failed
connections and each unguarded one would stream the agent binary into the guest
again.

### 4. The refcount cannot see your containers

The sharpest finding here. A session is torn down when its activity refcount
reaches zero, and the things holding that refcount are the ones WSLC knows
about: its own in-flight operations, and containers **it** created. Containers
created directly on the engine socket are invisible to that accounting — so the
session looks idle however busy the engine is, and the VM is destroyed with the
containers still running.

Same host, same fresh default VM, 80 s of silence:

| container created via | after 80 s |
|---|---|
| `wslc run -d` | still running |
| the engine socket | **exited 137, VM rebooted** |

WSLC provides the answer for exactly this case. From `WSLCProcess.h`:

> A root-namespace process is not tracked as a container, so it relies on this
> token to hold an activity reference on the owning session for as long as the
> client keeps the process alive, preventing the idle worker from tearing the VM
> down (and killing the process) underneath it.

So the lease is one long-lived root-namespace process, held open. That is a
*reference*, not a sample: no interval to tune, no window to miss. It costs one
resident `wslc.exe` — 10 MB working set, 2 MB private, **0.0 ms of CPU over a
measured minute** — against the 360 spawns an hour polling would need. One lease
covers the whole session, since the refcount is per-session.

It also has to be **pausable**. Without that the lease and a supervisor fight:
stopping the engine terminates the session, the blocking hold returns an error,
and a second later the lease takes it again — either resurrecting the VM the
user just stopped, or spinning a `wslc.exe` per second forever.

### 5. Published ports reach nowhere

`dockerd` publishes ports *inside* the session VM, and the relay that would
carry them to the host is driven from the Windows side by the CLI — which a
bridge talking straight to the socket never invokes. Measured: a port published
through the socket is unreachable from Windows on every address, so Compose port
mappings and Testcontainers' `getMappedPort()` fail outright until something
fills the gap.

A host-side forwarder closes it, and can be strictly more capable than the CLI's
own: `HostIP` comes from the client's `PortBindings`, so `0.0.0.0` and a specific
address both mean what Docker says they mean, rather than always `127.0.0.1`.

## A wrinkle if you script against the CLI

Creating the default session has an asymmetry that costs an afternoon if you
meet it the hard way:

```
wslc system session run ...                    # creates the default session
wslc system session run --session <name> ...   # requires it to already exist
```

With `--session` on a machine where every session is terminated, you get
"Session not found". So one bare call has to go first. Found by running against
a machine with everything torn down — not visible on a developer box that
already has a session up.

## What we would want from upstream

Ordered by how much difference it would make:

1. **Fix container DNS** ([#351](https://github.com/wslkit/skrog/issues/351)).
   Build-time network access is broken and there is no workaround, which rules
   out most real Dockerfiles.
2. **Let the refcount see engine-created containers**, or document the lease
   token as the supported answer. Today the correct behaviour is discoverable
   only by reading a header.
3. **Allow named sessions from the CLI**, so a tool can own its own VM instead
   of sharing the default one with whatever the user runs by hand.
4. **An opt-in reachable socket.** Everything above exists to work around
   `dockerd` having no `-H`. A supported, authenticated endpoint would make most
   of this unnecessary.
5. **A newer engine.** 25.0.3 / API 1.44 sits exactly on the stock CLI's minimum,
   so the supported window is already closed rather than merely narrow.

## Reproducing any of this

```powershell
# the engine and API version actually in there
skrog proxy --engine wslc
docker --context skrog-wslc version

# the refcount finding: start one of each, wait 80s, compare
wslc run -d --name viacli busybox sleep 600
docker --context skrog-wslc run -d --name viaapi busybox sleep 600

# the DNS finding, without any bridge in the path
wslc run --rm busybox nslookup example.com
```

`skrog proxy --engine wslc` writes no install manifest and registers no
autostart, so it leaves an existing setup alone.
