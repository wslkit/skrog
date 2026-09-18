# Using wslc as Skrog's engine

**Experimental**, but a first-class install: `skrog install --engine wslc` sets
it up, `skrog start` keeps it running, and `skrog status` and `skrog version`
report it. Tracked in [#316](https://github.com/wslkit/skrog/issues/316).

[wslc and Skrog](wsl-containers.md) explains what a `wslc` session is and why
nothing on your machine can otherwise reach the Docker engine inside it. This
page is the other half: how to point Skrog at one, what you get, what you give
up, and how to decide whether that trade is worth taking.
[wslc as a full Docker engine](wslc-deep-dive.md) is the long version — how the
bridge works, and what we would want from upstream.

The short version, if you only read one paragraph: **take this backend if your
source tree lives on a Windows drive, and leave it alone if you need a pinned
engine.** Everything below is the detail behind those two sentences.

## Quick start

All you need is WSL 2.9.3+. To make it this machine's engine:

```powershell
skrog install --engine wslc
skrog start
docker run --rm hello-world
```

That is the whole thing. No session to create first, no agent to build, and no
`--context` to remember: the install serves the pipe `docker.exe` already talks
to, and registers the supervisor to start at logon like any other install.

There is nothing to download — Microsoft ships the engine, and every session
already has it — so the install is a check that the machine can do it and a
proof that it works, then a manifest recording the choice. It takes a few
seconds because the session VM boots once.

```
backend  wslc
session  wslc-cli-<you>
engine   Microsoft's, shipped with WSL 2.9.11.0
pipe     \\.\pipe\docker_engine  (default pipe is free)
```

`skrog status` and `skrog version` report the backend from then on:

```
backend     wslc  (Microsoft's engine, in a WSL container session)
session     wslc-cli-<you>
supervisor  running
engine      running
```

### Or try it without installing

`skrog proxy --engine wslc` runs the same bridge in the foreground. It writes
no install manifest and registers no autostart, so it leaves your existing
setup alone — but it is not literally inert: it creates or updates the
`skrog-wslc` docker context (pass `--no-context` to skip), writes `audit.log`
into the state dir when auditing is on, and keeps a `skrog-share-*` holder per
bound drive while it runs. Ctrl-C stops it and releases all of that.

```powershell
skrog proxy --engine wslc
docker --context skrog-wslc ps
```

### It runs alongside a normal install

The two backends **coexist**, and which pipe is used depends on whether this is
the machine's engine rather than on which backend it is:

| | pipe | context |
|---|---|---|
| `skrog install --engine wslc` | `\\.\pipe\docker_engine`, or `\\.\pipe\skrog_engine` if something else already holds it | `skrog` |
| `skrog proxy --engine wslc` | always `\\.\pipe\skrog_wslc` | `skrog-wslc` |

So an installed engine takes the pipe plain `docker` uses — unless Docker
Desktop is already serving it, in which case Skrog steps aside to its own, and
the install output says which it took. An ad-hoc `proxy` never competes for
either: a normal Skrog install keeps its pipe and its `skrog` context untouched
while you experiment.

Switching is the vocabulary you already have:

```powershell
docker context use skrog-wslc     # Microsoft's engine, in a session VM
docker context use skrog          # your own pinned engine, in a distro
docker context ls
```

The cost of running both at once is a second VM, roughly 820 MB. Stop the
bridge when you are not using it.

### Useful flags

| flag | why |
|---|---|
| `--pipe '\\.\pipe\docker_engine'` | serve somewhere else — including the default pipe, which is reasonable on a machine with no distro install |
| `--no-context` | do not create or update the `skrog-wslc` context; select with `$env:DOCKER_HOST` instead |
| `--state-dir <path>` | where `config.json`, `policy.yaml` and `audit.log` live |
| `--agent <path>` | a specific `skrog-agent` build; by default the one shipped beside `skrog.exe`, falling back to the engine distro's |

## How it works

Skrog does not wrap or drive the `wslc` CLI. The CLI is used exactly twice —
once to find the session, once to stream a guest agent into it — and never
again while the bridge is up:

1. **Find the session**, or start one. `wslc system session list`, falling back
   to the CLI's own default session — created on the spot if nothing is
   running. That last part has a wrinkle worth knowing if you script against
   the CLI: `system session run` *with* `--session` requires the session to
   already exist, and only a bare call without the flag creates it.
2. **Place the agent.** The `skrog-agent` binary and a per-run shared secret are
   streamed over `wslc system session run` on stdin, byte-exact and verified by
   sha256 in the guest.
3. **Talk over vsock.** From then on Windows dials the agent directly over
   AF_HYPERV — around 4 ms — and the agent relays to `/var/run/docker.sock`.
   No process spawn per request, and no `wslc` in the data path.

The VM GUID needed for that dial is discoverable from the registry without
elevation, so none of this needs an administrator.

Two things run alongside the relay:

- **A lease.** A session VM idle-terminates when nothing is using it, which
  would take the agent and your containers with it. Skrog holds one long-lived
  process in the session's root namespace, which takes WSLC's own activity
  reference — the same mechanism a running container uses. It costs no measurable
  CPU, and it is bounded so an orphaned bridge cannot pin a VM up forever.
- **A port watcher.** It follows the engine's `/events` stream and publishes or
  withdraws a host listener as containers with `-p` come and go.

## What works

Verified against a live session on WSL 2.9.11.0 / Windows 10 22H2:

| | |
|---|---|
| Container lifecycle | `run`, `create`, `start`, `stop`, `rm` |
| Streaming | `exec`, `logs`, `logs -f`, `attach`, `stats`, `events` |
| Inspection | `ps`, `inspect`, `images`, `version`, `system df` |
| Images | `pull`, `push`, `commit`, `save`, `load`, `tag` |
| Build | `build` and `buildx` |
| Data | `docker cp`, networks, named volumes |
| **Compose** | including healthchecks, `depends_on` conditions, named volumes and published ports |
| **Testcontainers** | including the Ryuk reaper |
| **Dev Containers** | `devcontainer up`, `exec`, `postCreateCommand`, `containerEnv`, and the workspace mount — which lands on virtiofs here. Features install on WSL 2.9+; they failed on 2.7.x (see below) |
| **Windows-folder bind mounts** | `-v C:\src\app:/app`, over virtiofs |

The last three are the ones that matter, because they are what the `wslc` CLI
cannot do:

- **Ryuk** needs the engine socket bind-mounted into a container. Skrog
  translates a `\\.\pipe\...` source to the session's own
  `/var/run/docker.sock`, which is the only thing a pipe can mean inside a Linux
  container. The `wslc` CLI cannot express that mount at all.
- **Published ports** reach Windows. `dockerd` publishes them inside the session
  VM, and the relay that would carry them to the host is driven from the Windows
  side — so a plain socket relay gets you a port that exists nowhere you can
  reach. Skrog runs its own relay and binds **the address your `-p` asked for**;
  `wslc`'s own relay only ever binds `127.0.0.1`.
- **Windows folders**, which have their own section below.

## What does not work

| | why |
|---|---|
| **UDP published ports** | the relay is a stream transport |
| **DNS inside containers** | **Fixed in WSL 2.9.** On 2.7.x a session handed containers the Windows host's LAN router as their nameserver and it answered `SERVFAIL` from inside the session VM, so any build reaching the network failed. Re-tested on 2.9.11: the same resolver now answers, and a build that runs `apk add` and `curl` succeeds. Below 2.9 the old behaviour stands and `--dns=1.1.1.1` fixes run time only ([#351](https://github.com/wslkit/skrog/issues/351)) |
| **Engine pinning** | Microsoft ships the engine; `skrog lock` has nothing to record |
| **A dedicated session** | the shipped CLI cannot create a named session, so Skrog shares the default one |
| `compact`, `snapshot`, `relocate`, `wsl-integrate`, `gpu`, `engine upgrade` | these operate on Skrog's own distro and have no meaning here |
| `skrog doctor` — partly | it gains a `wslc-session` check (CLI, session, guest agent), but its generic engine check still reads distro-shaped |
| `skrog lock`, `runner check` | there is no rootfs or engine version to pin, so reproducibility cannot be promised here |

Sharing the default session has a practical consequence worth stating: anything
you run with the `wslc` CLI by hand lands in the same VM as your containers, and
`wslc system session terminate` will take your containers down with it.

## Behaviour differences from the distro backend

Things a `docker` user hits here that they do not hit on `skrog-engine`. All of
it measured on WSL 2.9.11.0 / Windows 10 22H2 rather than inferred from the
`wslc` CLI's surface, which matters — several differences people expect from the
CLI do not exist over the engine socket.

### The one that will actually bite you: API 1.44

| | wslc session | skrog-engine |
|---|---|---|
| engine | 25.0.3 (Microsoft build, `GitCommit f417435`) | 29.8.0 |
| API | **1.44** | 1.56 |
| minimum API | 1.24 | 1.24 |

Docker CLI 29.x's *minimum* supported API is 1.44, so the bundled CLI negotiates
down and works — **with zero headroom**. Anything that requires 1.45 or newer
fails rather than degrades: newer BuildKit attestation and SBOM flags, some
Compose fields, `docker debug`.

`skrog upgrade` cannot fix this. `wsl --update` might, silently, in either
direction.

### No cross-architecture execution

`--platform` itself works — the daemon selects and pulls the requested platform
happily:

```
docker pull --platform linux/arm64 busybox      # succeeds
docker image inspect busybox --format '{{.Architecture}}'   # arm64
```

What is missing is qemu/binfmt, so a foreign binary cannot *run*:

```
docker run --rm --platform linux/arm64 busybox true
exec /bin/true: exec format error
```

A `buildx --platform linux/arm64` build gets through `FROM` and dies at the
first `RUN` for the same reason. So multi-arch builds are out, but pinning a
platform to the host's own architecture is fine.

A trap worth knowing, because it is easy to do to yourself: pulling a foreign
platform **overwrites the local tag**. After the `docker pull --platform
linux/arm64 busybox` above, plain `docker run busybox` fails with
`exec format error` until you re-pull the native one.

### What works here that the `wslc` CLI cannot do

The CLI's limits are the CLI's, not the engine's. Over the socket, all of these
work and are verified:

| | |
|---|---|
| `--network host` | exits 0 and joins the **session VM's** network namespace — not Windows' |
| `--privileged`, `--pid host`, `--cap-add` | all supported by dockerd and all reachable here |
| `docker network create` | bridge, ipvlan, macvlan, overlay and null drivers are present |
| guest-path binds (`-v /var/run/docker.sock:…`, `/tmp`) | the basis of Ryuk and docker-in-docker |
| `buildx build -o type=local` | the artifact lands on Windows |

`--network host` deserves the caveat: "host" is the session VM, so it gets you
the VM's interfaces, not the Windows host's. Published ports do not apply to a
container in host mode, and nothing in that namespace is reachable from Windows
except through Skrog's relay.

### Published ports bind what you asked for

`wslc`'s own relay only ever binds `127.0.0.1`. Skrog runs its own relay and
honours the address in `-p`:

```
docker run -d -p 0.0.0.0:18411:80 busybox httpd -f -p 80
netstat -an | findstr 18411
  TCP    0.0.0.0:18411    LISTENING
  TCP    [::]:18411       LISTENING
```

Reachable from the host's LAN address, not just loopback. TCP only — see
[What does not work](#what-does-not-work).

### Bind-mount metadata is virtiofs-flavoured

Files under a Windows share present as `-rwxrwxrwx root root`, and `chmod` is
silently a no-op:

```
-rwxrwxrwx  1 root root  3 /m/f.txt
chmod 600 /m/f.txt
-rwxrwxrwx  1 root root  3 /m/f.txt
```

Same class of surprise as `drvfs` on a distro, and it breaks anything that
insists on strict permissions — an `ssh` key, or a tool that refuses a
world-writable config.

### Registry credentials

`X-Registry-Auth` passes straight through, so `docker login` works and its state
lives in the Windows CLI's own config, exactly as on the distro backend. WSLC
keeps a *separate* credential store for `wslc registry login`; Skrog's pipe does
not read it, and logging in with one does not log you in on the other.

### GPU

The guest daemon has CDI enabled, so `--gpus` maps to `DeviceRequests` → CDI
inside dockerd 25. The runtime list is `runc` and `io.containerd.runc.v2` only —
there is no `nvidia` runtime, and there does not need to be. **`skrog gpu` is
for Skrog's own distro and will not help here**; the CDI spec comes from the
WSLC guest image.

### Windows 10 is fine

Microsoft's docs say Windows 11 22H2+. All of the above was measured on **Windows
10 22H2 (19045)**. Nothing in Skrog gates on the OS build, and nothing needs to.

## Windows folders, and the reason to use this backend at all

A session has no `/mnt/c`. Each Windows folder is instead its own **virtiofs**
share mounted at `/mnt/{GUID}`, and the GUID changes when the VM restarts.

Skrog builds a share table: the first bind from a drive starts one
`skrog-share-<drive>` holder container that mounts the drive and keeps the share
open, and the bind source is rewritten to the share's guest path. One holder per
drive, not one per bind — a Compose project with a dozen binds under `C:` starts
one. They are removed when the bridge stops, and a cached share is probed before
use so a VM restart re-establishes it rather than handing `dockerd` a path that
silently mounts empty.

Editing a file on Windows is visible in the container immediately, so the
ordinary edit-and-reload loop works.

### Measured: virtiofs versus 9p

The same Windows folder, the same `alpine`, bind-mounted through each backend
and timed **inside** the container. Mount types confirmed from `/proc/mounts`.

| | wslc (virtiofs) | distro (9p) | ratio |
|---|---|---|---|
| write 256 MB (`conv=fsync`) | **216.1 MB/s** | 114.7 MB/s | **1.9×** |
| read 256 MB (warm) | **800.2 MB/s** | 186.6 MB/s | **4.3×** |
| create 1000 files | **1.12 s** | 2.26 s | 2.0× |
| `ls -l` 1000 files | **0.27 s** | 0.71 s | 2.6× |
| read 1000 files | **1.20 s** | 2.67 s | 2.2× |
| delete 1000 files | **0.79 s** | 1.57 s | 2.0× |

So: **roughly 2× write, 4× read, 2–2.6× metadata.** These reproduce an
independent earlier run on the same host to within a few percent
([#326](https://github.com/wslkit/skrog/issues/326)), which is the main reason
to trust them.

The read row is a warm read — reads follow a write in the same VM and the guest
page cache helps both sides. That is the common case for a source tree, so it is
the number worth quoting, but it is not a cold-cache figure.

This is felt on exactly the workloads people complain about: `npm install`, a
gradle build, a large `docker build` context.

> **This gap is no longer a reason on its own to take this backend.** The 9p
> column above is the distro default, and the distro can use virtiofs too:
> `skrog config set wsl.virtiofs true` (#327). Measured on the same host that
> way, the distro reads at **811 MB/s** against this backend's 800 -- the same
> number. So if Windows-folder speed is what brought you here, try that switch
> first: it is one setting, it keeps the pinned engine, and it costs no second
> VM. See [vm-sizing.md](vm-sizing.md#wslvirtiofs-a-faster-mntc-and-the-one-key-here-that-is-not-about-size).

## Policy and audit

The engine socket sits behind `wslcsession`, which is where WSL enforces an
administrator's registry allowlist. Talking to the socket directly would bypass
that, so Skrog reads **WSL's own Group Policy configuration**
(`HKLM\Software\Policies\WSL`) and applies it at the pipe. The aim is that a
deployed allowlist means the same thing through this pipe as through `wslc` —
not that Skrog invents a second policy language next to it.

| policy | where Skrog applies it |
|---|---|
| `WSLContainerRegistryAllowlist` | `docker pull`, and the image named by `run`/`create` |
| `AllowWSLContainerPrivileged` | `--privileged` on create |
| `WSLContainerRegistryAllowlist` | `docker build` — **refused outright** while an allowlist is active |
| `WSLContainerRegistryAllowlist` | `docker push` and `docker plugin push` |

Push is gated because an allowlist that controls only inbound traffic governs
what may *enter* the machine and says nothing about what leaves it — and leaving
is the direction that moves data off it. WSL's own `wslc push` refuses a blocked
registry, so this is parity rather than a second reading of what an allowlist
means. Note the limit, which is the same one below: a container can still reach
any registry it likes over the network. Gating push raises the bar for an
accident, not for a determined local user.

Skrog's own [`policy.yaml`](policy.md) and the [audit log](audit.md) apply on
top. The audit log is worth turning on here for its own sake: it records every
container-affecting call with its outcome, including each denial and the reason,
which is a record WSLC itself does not keep.

A policy that cannot be read **at startup** is treated as a failure, not as "no
policy" — the bridge refuses to start. Guessing in the permissive direction is
how a bypass ships.

### Plugin hooks: Skrog refuses rather than silencing them

WSL also loads host-side **plugin DLLs** and calls them on container and
session events — `WSLPluginAPI_ContainerStarted` (whose return value can
*refuse* the container), `ContainerStopping`, `ImageCreated`, `ImageDeleted`,
`OnSessionCreated`. That is the integration point Defender-style tooling uses.

Those hooks fire from `wslcsession`, which this backend's socket relay skips.
And unlike the registry policy, **they cannot be stood in for**: the policy is
declarative, so Skrog can read the same keys and reach the same verdict, while
a plugin is third-party code with a veto. There is no substituting for code we
do not have.

So if any plugin is registered under
`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Lxss\Plugins`, **Skrog refuses
to serve the wslc backend** and names it:

```
skrog: this machine has WSL plugin(s) registered (defender-wsl), and they do NOT
fire through Skrog's docker.sock relay on the wslc backend (#406). A plugin can
refuse a container from WSLPluginAPI_ContainerStarted; through this pipe it is
never asked. Refusing to serve rather than silently disabling it.
```

Serving anyway is a legitimate choice — the plugin may be irrelevant to you, or
you may be the administrator who deployed it — but it should be one someone
made:

```powershell
skrog config set wslc.ignore-plugins on
```

The bridge then starts and logs a warning naming the plugins whose hooks will
not run.

**The distro backend is unaffected.** WSL plugins are a wslc mechanism; nothing
about `skrog-engine` involves them.

After that it is re-read on **every judged request**, so a policy deployed or
tightened while the bridge is running is honoured without a restart. That
matters more here than it sounds: the supervisor starts at logon and survives
sleep, resume and `wsl --shutdown`, so a snapshot taken at startup could go on
enforcing a months-old policy on a machine that looks correctly configured.

If a later re-read fails, Skrog keeps enforcing the last policy it read
successfully rather than falling back to the permissive default, and logs it
once. A transient registry failure must never *widen* what is enforced — but
refusing every request forever because of one blip would be its own outage,
which is why the strict "refuse to start" rule applies only to the first read.

### Verified against a real deployment

With `WSLContainerRegistryAllowlist = contoso.azurecr.io` and
`AllowWSLContainerPrivileged = 0` actually deployed:

| | |
|---|---|
| `docker pull busybox` | 403, naming the allowlist and what it permits |
| `docker pull contoso.azurecr.io/…` | passes the gate and reaches the network |
| `docker run --privileged` | 403, naming `AllowWSLContainerPrivileged` |
| the same image without `--privileged` | runs |
| `docker ps`, `images`, `version` | untouched |
| `wslc pull busybox` | `WSLC_E_REGISTRY_BLOCKED_BY_POLICY` |

That last row is the point: Microsoft's own CLI refuses the same image for the
same reason, so the two agree rather than Skrog inventing its own answer.

### What the allowlist is, and is not

It is **admission control at the Docker API**, not a network control. Skrog
judges the requests that cross its pipe; it does not stand between the engine
and the internet. Two consequences worth stating plainly:

- **A container can reach any registry it likes.** `docker run … curl` inside a
  container, or `buildx --driver docker-container` (which runs BuildKit *inside*
  a container), does its registry traffic in the guest where this gate never
  sees it.
- **Provenance is not tracked.** The rules judge the reference in the request.
  An image already on the machine can be renamed into an allowed one
  (`docker load` then `docker tag contoso.azurecr.io/anything:1`) and will then
  pass. Closing that needs image-provenance tracking, which this does not do —
  [#343](https://github.com/wslkit/skrog/issues/343).

What it does give you is that the *engine* will not fetch from, or run an image
named for, a registry the administrator forbade — which is what the WSL policy
it mirrors is for.

Endpoints that could fetch or run an image without naming it anywhere the gate
can judge — `/plugins/pull`, `/plugins/*/upgrade`, `/services/create`,
`/services/*/update`, `/swarm/init` — are **refused outright** while an
allowlist is active, on the same ground as a build.

### Builds: Skrog is stricter than WSL here

Worth being straight about, because it is the one place the two do not match.

`wslc image build` is **not** refused. It runs, and the allowlist is enforced
per source *inside* BuildKit, so a blocked base image fails with
`source "docker-image://docker.io/library/busybox:latest" denied by policy`.
`wslcsession` can do that because it drives BuildKit directly and attaches a
source policy to the solve request.

That policy is client-side, not daemon configuration — a build sent straight to
the session's `dockerd` inherits none of it, which is measured rather than
assumed. Matching it at the pipe would mean rewriting protobuf inside a hijacked
HTTP/2 stream. Until that exists, Skrog refuses the build instead of letting it
through.

The refusal covers `/session` and `/grpc` as well as `/build`: buildx has been
the default `docker build` since Docker 23 and never touches `/build`, so gating
only the classic endpoint would leave the allowlist void for every build a user
actually runs. When buildx then falls back to booting its own `moby/buildkit`
container, the pull gate stops that too.

## Pros and cons

### What you gain

- **Windows-folder I/O, at roughly 2× write and 4× read** over the 9p transport
  a WSL2 distro uses for `/mnt/c` by default. It is the only category where the
  difference is large -- but the distro can now use virtiofs too
  (`skrog config set wsl.virtiofs true`, #327) and reaches the same read speed,
  so try that first.
- **Microsoft's engine, supported by Microsoft.** If policy where you work says
  the container runtime has to be first-party, this is a way to have that *and*
  Compose, Testcontainers and the rest of the Docker ecosystem.
- **An administrator's WSL policy keeps working**, rather than being silently
  voided by installing something that talks to `docker.sock` directly.
- **An audit log** over an engine that otherwise keeps no record.
- **No second engine to maintain.** No `skrog engine upgrade`, no rootfs to
  keep current — `wsl --update` handles it.

### What it costs

- **No engine pinning.** This is the big one. `skrog lock` has nothing to
  record, `wsl --update` moves the engine underneath you, and engine drift goes
  back to being a category of bug you can hit. If reproducibility is why you came
  to Skrog, the distro backend is the answer and will stay so.
- **~820 MB for a second VM.** A wslc session is a whole separate VM; running
  both backends at once costs roughly that much extra.
- **An older engine.** 25.0.3 / API 1.44, against 29.x on the distro backend.
- **Experimental.** It is a first-class install now -- `skrog install --engine
  wslc`, `skrog start`, `status`, `version` and a `doctor` check all know it --
  but it is newer and less exercised than the distro backend, and the engine
  underneath is not one Skrog can pin.
- **You share the CLI's default session**, including with anything you run with
  `wslc` by hand.
- **Holder containers.** One `skrog-share-<drive>` per drive you bind from
  shows up in `docker ps` for as long as the bridge runs.
- **Sharing a drive exposes it to the session VM.** This is the same exposure a
  distro already has through `/mnt/c`, but it is worth knowing;
  [`allow-bind-sources`](policy.md) is how you narrow what containers may mount.
- **No UDP published ports.**

### What does *not* differ

Worth stating, because it is easy to assume otherwise and the measurements say
no ([#326](https://github.com/wslkit/skrog/issues/326)):

- **The control plane is a wash.** With both engines behind Skrog's own pipe and
  driven by the same `docker.exe` — `version` 69 vs 74 ms, `ps` 59 vs 59 ms,
  `run --rm busybox true` 505 vs 527 ms, `exec` 121 vs 135 ms. All within noise.
  Earlier figures showing wslc's control plane well ahead were comparing two
  CLIs, not two engines.
- **In-VM filesystem I/O is mixed with no winner.** Overlay writes favour wslc,
  named-volume writes favour the distro, reads are page cache on both sides.

### The decision, in one table

| if your… | then |
|---|---|
| source tree is on `C:` and builds/tests hammer it | try `skrog config set wsl.virtiofs true` on the distro backend FIRST (#327); it reaches the same speed and keeps a pinnable engine |
| work lives inside the Linux filesystem | distro backend; there is little in it |
| CI or team needs a byte-identical engine | distro backend, and it is not close |
| workplace requires a first-party runtime | **wslc backend** |
| machine is short on RAM | distro backend, unless you stop the other one |
| setup needs to survive reboot unattended | either — `skrog install --engine wslc` registers autostart like any install |

## Troubleshooting

**The first run is slow** — Skrog starts the container session when none is
running, and a cold session VM takes a few seconds to boot. Later runs join the
running one.

**Containers vanish after about 30 seconds** — the session VM idle-terminated,
which means the lease is not being held. Check the bridge is still running;
Skrog re-bootstraps the agent automatically on the next connection after a VM
restart, but anything that was running is gone.

**A bind mount is empty inside the container** — the share went stale across a
VM restart. Skrog probes for this, but if you see it, restart the bridge.

**A published port is not reachable** — check it is TCP. UDP is not relayed.
Windows Firewall will also prompt the first time Skrog binds a host listener.

**`docker` reaches the wrong engine** — check `docker context ls`. `skrog` is
the distro engine and `skrog-wslc` is the session; `docker context use` picks
one. If a context points somewhere stale, restarting that bridge rewrites it.

## Verifying any of this yourself

The engine inside a session answers directly, with no Skrog involved:

```powershell
wslc system session list
wslc --session <name> system session run curl -s --unix-socket /var/run/docker.sock http://localhost/version
```

If that prints a Docker version payload, you have just talked to the engine
Microsoft ships and the one nothing on your machine can otherwise reach.

Both layers of the protocol are open source, MIT licensed, in
[microsoft/WSL](https://github.com/microsoft/WSL): the COM interfaces in
`src/windows/service/inc/wslc.idl`, the guest message protocol in
`src/shared/inc/lxinitshared.h`, and the policy rules Skrog mirrors in
`src/windows/inc/wslpolicies.h`. Nothing here required reverse engineering.

## Where this should end up

Not a clever workaround. Microsoft exposing an endpoint officially — gated by
the same policy their CLI already enforces — at which point a Skrog backend
becomes a thin adapter and everyone else's tools work too. That is what
[microsoft/WSL#40976](https://github.com/microsoft/WSL/issues/40976) asks for,
and the measurements here exist to argue for it.

Corrections are welcome — this page is
[a markdown file](https://github.com/wslkit/skrog/tree/main/docs) and a pull
request away from being right.
