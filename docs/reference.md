# Command reference

Every command, its flags and its exit codes — generated from the binary's own
`--help` output by `scripts/build-reference.ps1`, so it cannot drift from what
the CLI actually does. CI regenerates it and fails if the result differs.

Run `skrog <command> --help` for the same text in your terminal.

## Commands

- [`audit`](#audit) — print the container-affecting API audit log (`audit tail`)
- [`autostart`](#autostart) — start the supervisor at logon: enable, disable, status
- [`bundle`](#bundle) — pack the engine into a .zip for an air-gapped `install --offline`
- [`cache`](#cache) — pull-through registry cache on the engine: enable, disable, status
- [`cli`](#cli) — install the bundled docker CLI + compose + buildx (ditch Docker Desktop)
- [`compact`](#compact) — shrink the engine's virtual disk: fstrim + CompactVirtualDisk
- [`config`](#config) — list, get, or set Skrog settings (idle-timeout)
- [`doctor`](#doctor) — diagnose the host and engine; --fix applies safe remedies
- [`enable-gpu`](#enable-gpu) — install the NVIDIA CDI spec so containers can use the GPU
- [`engine`](#engine) — engine list, upgrade and rollback — pinned and reversible
- [`healthcheck`](#healthcheck) — readiness probe: exit 0 when a docker command would succeed (--wait)
- [`install`](#install) — provision the engine distro and start it
- [`lock`](#lock) — write a skrog.lock pinning the exact engine (reproducible installs)
- [`logs`](#logs) — supervisor, dockerd, or audit log; --follow, --json for shippers
- [`migrate`](#migrate) — copy images and volumes from Docker Desktop into the engine
- [`prewarm`](#prewarm) — pull a pinned image list ahead of need (runner warm-up, golden images)
- [`policy`](#policy) — local admission control for the docker API: show, check, test
- [`profile`](#profile) — save and switch named settings profiles (work/home)
- [`proxy`](#proxy) — serve the docker pipe in the foreground (debug mode)
- [`prune`](#prune) — reclaim disk: stopped containers, unused images, build cache
- [`relocate`](#relocate) — move the engine data dir to another drive
- [`remote`](#remote) — register and switch to a remote engine served over mutual TLS
- [`reset`](#reset) — reset the engine to a snapshot, unconditionally (runner clean slate)
- [`restart`](#restart) — stop the engine, then start it
- [`runner`](#runner) — runner check: verify auto-logon, autostart, supervisor and engine on an unattended host
- [`serve`](#serve) — expose the engine over the network with mutual TLS (`serve cert`)
- [`snapshot`](#snapshot) — save/restore/list the engine state (images, containers, volumes)
- [`start`](#start) — ensure the supervisor and engine are running
- [`status`](#status) — report supervisor, engine and desired state
- [`stop`](#stop) — stop the engine; it stays stopped until start
- [`supervise`](#supervise) — serve the pipe and keep the engine alive (the always-on layer)
- [`uninstall`](#uninstall) — remove the engine distro and Skrog's state
- [`upgrade`](#upgrade) — am I current? app, engine and bundled CLI in one answer
- [`wsl-integrate`](#wsl-integrate) — point docker inside your own WSL distros at the engine
- [`wsl-config`](#wsl-config) — right-size the WSL2 VM: show and apply ~/.wslconfig sizing, with consent
- [`version`](#version) — report every component version and which docker.exe is active

## Exit codes

Shared by every command, and part of the contract CI scripts branch on:

| code | meaning |
| --- | --- |
| 0 | success |
| 1 | error |
| 2 | usage — the command was called wrongly |
| 3 | asked about something that is not installed |

Individual commands add their own where they need a third answer; each one
says so in its help text below.

## audit

print the container-affecting API audit log (`audit tail`)

```
usage: skrog audit tail [--since <dur>] [-n <count>] [--json]
       skrog audit trace [--json|--raw] -- <cmd> [args...]

trace runs a command and then summarizes what it did to the engine — images
pulled, containers created, execs, builds — from the audit records written
while it ran (a trace for opaque CI YAML: `skrog audit trace -- act -j build`).
The command's exit code is propagated. Concurrent docker use during the run is
included in the summary.

tail prints the container-affecting API audit log — image pulls, container
create/start/stop/remove, exec and builds that crossed the bridge — as the
JSON lines they are recorded in. Enable recording with:

  skrog config set audit on

It takes effect on the next docker call — nothing to restart.

The record is derived from the request line only, never the body, so no
credentials or payloads are written.

Exit codes: 0 ok, 1 error, 2 usage.

flags:
  -json
    	emit the events as one JSON array instead of JSON lines (tail), or the summary as JSON (trace)
  -n int
    	show only the last N events (0 = all)
  -raw
    	trace: print the matching records as JSON lines instead of a summary
  -since duration
    	show only events newer than this (e.g. 30m, 2h)
  -state-dir string
    	override Skrog's state directory
```

## autostart

start the supervisor at logon: enable, disable, status

```
usage: skrog autostart enable|disable|status

Controls whether the supervisor starts at logon, via a per-user Run entry
(visible and switchable in Task Manager's Startup tab; no admin needed). The
entry runs skrogw.exe, the windowless launcher, so nothing flashes at logon.

install registers this by default; uninstall removes it.
```

## bundle

pack the engine into a .zip for an air-gapped `install --offline`

```
usage: skrog bundle [--engine-version <v>] [-o skrog-bundle.zip]

Packs the engine rootfs and a skrog.lock into one .zip for an air-gapped
install. Run this on a connected machine; the rootfs is downloaded and
checksum-verified, then copied into the bundle. On the isolated machine:

  skrog install --offline skrog-bundle.zip

installs entirely from the file — any network access on that step is a bug.

Exit codes: 0 ok, 1 error, 2 usage.

flags:
  -engine-version string
    	engine version to bundle (default: this build's default)
  -o string
    	shorthand for --output
  -output string
    	bundle path (default: skrog-bundle-<version>.zip)
  -state-dir string
    	override Skrog's state directory (rootfs download cache)
```

## cache

pull-through registry cache on the engine: enable, disable, status

```
usage: skrog cache enable|disable|status

Runs a pull-through registry cache on the engine, so a layer is pulled from
the internet once and served locally after that.

  skrog cache enable                    # cache Docker Hub
  skrog cache enable --upstream https://ghcr.io
  skrog cache status --json
  skrog cache disable                   # also deletes the cached layers
  skrog cache disable --keep-data       # ...unless you keep them

Why you would: Docker Hub rate-limits anonymous pulls PER IP, so a corporate
NAT or a runner fleet hits the limit as an organisation rather than as the
developer who sees the error. A slow or TLS-inspecting corporate link pays for
the same bytes every time. Both stop after the first pull.

It is the upstream registry:2 image in proxy mode, pinned by digest, with its
store in the skrog-cache-data volume — so `skrog prune`, `compact` and
`relocate` already account for its disk. dockerd reaches it on loopback,
which Docker treats as insecure-by-default, so nothing is exposed to the
network and no insecure-registries entry is needed.

It does not weaken `skrog policy`: a mirror changes where bytes come
from, not which image was asked for, and the rules judge the reference.

It does not hold the engine awake. The cache container is excluded from the
idle-stop and scheduled-prune probes, because it is infrastructure rather than
work.

Exit codes: 0 ok, 1 error, 2 usage, 3 not installed.

flags:
  -json
    	emit machine-readable JSON
  -keep-data
    	on disable, keep the cached layers
  -port int
    	loopback port inside the engine (default 5000)
  -state-dir string
    	override Skrog's state directory
  -upstream string
    	registry to cache (default: Docker Hub)
```

## cli

install the bundled docker CLI + compose + buildx (ditch Docker Desktop)

```
skrog cli: unknown subcommand "--help" (install|status|uninstall)
```

## compact

shrink the engine's virtual disk: fstrim + CompactVirtualDisk

```
usage: skrog compact [--no-trim] [--restart] [--dry-run] [--wait <dur>] [--json]

Shrinks the engine's virtual disk. A WSL2 distro's ext4.vhdx only ever grows:
delete 50 GB of images and the file on your drive stays exactly the same size.
Two things have to happen to get that space back, and this does both —

  1. fstrim inside the engine, so the guest says which blocks it freed;
  2. CompactVirtualDisk on the file, so the disk stops reserving them.

Either one alone reclaims nothing. No administrator rights are needed.

The engine is stopped for this (it cannot be compacted while attached) and
`--restart` brings it back. WSL keeps every distro's disk open while ANY distro is
running, so if another one is up — Docker Desktop's count — this refuses and
names it rather than running `wsl --shutdown` and killing your containers.
Stop them yourself and re-run.

  skrog compact --dry-run       # what it would do
  skrog compact --restart       # compact, then bring the engine back

Exit codes: 0 ok, 1 error, 2 usage, 3 not installed, 11 the disk is held.

flags:
  -distro string
    	WSL distro (default: from the install manifest)
  -dry-run
    	print the steps and change nothing
  -json
    	emit machine-readable JSON
  -no-trim
    	skip the in-guest fstrim (only if you just trimmed)
  -restart
    	start the engine again afterwards
  -state-dir string
    	override Skrog's state directory
  -wait duration
    	how long to wait for WSL to release the disk (default 1m30s)
```

## config

list, get, or set Skrog settings (idle-timeout)

```
usage: skrog config                    list all settings
       skrog config get <key>          print one value
       skrog config set <key> <value>  change one value
       skrog config export             print the install as a skrog.yaml

Skrog settings apply live: the supervisor re-reads this file when it changes,
so nothing here needs a restart to take effect. Where "live" needs a
qualifier, the setting below says so — settings that configure the engine
itself land when the engine next starts, which `skrog restart` asks for.

  idle-timeout   how long the bridge must be quiet (no connections, no running
                 containers) before the engine is stopped to reclaim its RAM;
                 the next docker command starts it again. A duration like 20m
                 or 1h, or "off" (the default).
  audit          record container-affecting API calls to audit.log in the state
                 dir; on/off ("off" by default). Takes effect on the next
                 docker call. See `skrog audit tail`.
  install.verify-signature  refuse the rootfs at install time unless its signature verifies
  disk.warn-below free-space floor under which `skrog doctor` warns, e.g. 10GB
  prune.every    how often the supervisor reclaims disk on its own: a duration
                 like 168h, or "off" (the default). It prunes stopped
                 containers and unused images older than prune.keep-since,
                 skipping entirely while containers are running. NEVER volumes.
  prune.keep-since how much history an automatic prune keeps; nothing younger is
                 touched. Defaults to 168h, and cannot be turned off — clear
                 the key to restore the default.
  prune.build-cache also drop the BuildKit cache on an automatic prune; on/off
                 ("off" by default, because the cache is expensive to rebuild).
  wslc.ignore-plugins serve the wslc backend even though WSL plugins are registered
                 on this machine; on/off ("off" by default). Their hooks do NOT
                 fire through the relay, so the default is to refuse rather
                 than silently disable an administrator's tooling. wslc only.

Engine settings (engine.<key>) are written into the engine's daemon.json,
validated with `dockerd --validate` before they replace the live file, and
applied by bouncing the engine (rolled back if it does not come back). Set an
empty value to clear a key. Lists are comma-separated; maps are k=v,k=v.

  engine.dns                      DNS servers for containers
  engine.dns-search               DNS search domains for containers
  engine.insecure-registries      registries reachable over HTTP or with an untrusted cert (host[:port] or CIDR)
  engine.log-driver               default container logging driver, e.g. json-file or local
  engine.log-opts                 logging driver options, e.g. max-size=10m,max-file=3
  engine.max-concurrent-downloads parallel layer pulls per image
  engine.max-concurrent-uploads   parallel layer pushes per image
  engine.mtu                      MTU for the default bridge network -- lower it under a VPN that clamps the tunnel MTU, or pulls hang mid-layer (#63)
  engine.registry-mirrors         pull-through mirror URLs, tried before Docker Hub
  engine.userland-proxy           relay published ports through docker-proxy instead of iptables NAT (Skrog defaults this to false: NAT is what makes -p ports reachable from Windows under mirrored networking)

Lifecycle hooks (hook.<event>) run a script on an engine event, time-bounded
and best-effort (a failure is logged, never blocks the lifecycle). The script
gets SKROG_EVENT and SKROG_STATE_DIR in its environment. Set an empty value
to clear one. Events:
  hook.post-start   after the engine starts (recovery or first start)
  hook.pre-stop     before the engine stops on `skrog stop`
  hook.on-idle-stop  after the idle timeout stops the engine
  hook.on-wake      after the engine cold-starts on demand

Corporate network (applied to the engine on its next start; `skrog restart`
asks for one):
  network.proxy          http(s):// proxy for the engine's registry pulls
  network.no-proxy       proxy bypass list (comma-separated)
  network.import-host-cas   trust the host's root CA store inside the engine
                             (the fix for a TLS-inspecting proxy; on/off)

Exit codes: 0 ok, 1 error, 2 usage.
  -json
    	emit machine-readable JSON (list)
  -state-dir string
    	override Skrog's state directory
```

## doctor

diagnose the host and engine; --fix applies safe remedies

```
usage: skrog doctor [--fix] [--json | --report]

Diagnoses the host: WSL2, the docker CLI on PATH, credential helpers, the
engine install, the docker context, supervisor/engine agreement, disk space,
and unattended-startup readiness. Each check prints a remedy when it is not OK.

--fix applies only the changes that are safe without elevation (currently:
setting the default WSL version to 2); everything else prints its remedy.

Exit codes: 0 all checks passed or only warnings, 1 one or more failed,
2 usage.

flags:
  -fix
    	apply the safely auto-remediable fixes, then re-check
  -json
    	emit machine-readable JSON
  -report
    	emit a Markdown report to paste into an issue
  -state-dir string
    	override Skrog's state directory
```

## enable-gpu

install the NVIDIA CDI spec so containers can use the GPU

```
usage: skrog enable-gpu [--off]

Enables NVIDIA GPU access for containers, then a container reaches the GPU with:

  docker run --rm --device nvidia.com/gpu=all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi

WSL2 already projects the Windows NVIDIA driver into the engine distro
(/usr/lib/wsl/lib, /dev/dxg); this installs a Container Device Interface (CDI)
spec so dockerd injects that into containers. No toolkit is installed in the
distro, and it works with the musl-based engine because the spec is hookless
(it sets LD_LIBRARY_PATH rather than running an ldconfig hook).

The setting persists: the spec is re-installed on every engine start, so a
reinstall keeps GPU access. --off removes it.

Requires an NVIDIA GPU with a WSL-capable driver. Intel GPUs are not wired up.

  --vendor amd   EXPERIMENTAL (#185), and never run against real hardware.

AMD reaches the GPU the same way -- ROCm's ROCDXG talks to the Windows driver
over the same /dev/dxg, and libdxcore.so is projected into the same
/usr/lib/wsl/lib -- so the spec is this one with kind amd.com/gpu. It is
written from AMD's documentation, not from observation, so it is opt-in by
name and reports itself as experimental. Needs AMD Software: Adrenalin Edition
26.2.2 for WSL2 or newer, and a ROCm container image (ROCm is Ubuntu-only, so
the image must be glibc; the engine's own musl is irrelevant because nothing
is installed in it). JAX, MIGraphX and multi-GPU are unsupported under WSL by
AMD, not by Skrog.

Note the vendor is taken at your word: the only library AMD projects here is
libdxcore.so, which is Microsoft's DXCore shim and is present on NVIDIA
machines too, so there is nothing to auto-detect it with.

Exit codes: 0 ok, 1 error, 2 usage, 3 not installed / no GPU.

flags:
  -distro string
    	WSL distro (default: from the install manifest)
  -off
    	disable GPU access (remove the CDI spec)
  -state-dir string
    	override Skrog's state directory
  -vendor string
    	GPU vendor: nvidia (default) or amd (EXPERIMENTAL, untested on hardware)
```

## engine

engine list, upgrade and rollback — pinned and reversible

```
usage: skrog engine list|upgrade|rollback

Engine security patches should not have to wait for an app release, and taking
one should not cost you your images.

  list       engines this build can install, and which one is installed
  upgrade    move to another engine, reversibly (--to <ref>)
  rollback   go back to the engine installed before the last upgrade

An upgrade swaps the engine binaries (dockerd, containerd, runc, buildkitd…)
out of a checksum-verified rootfs and leaves the filesystem alone, so
/var/lib/docker — every image, container and volume — is untouched. If the new
engine does not come back, the previous binaries are restored and the engine is
started again before the failure is reported.

Exit codes: 0 ok, 1 error, 2 usage, 3 not installed.
```

## healthcheck

readiness probe: exit 0 when a docker command would succeed (--wait)

```
usage: skrog healthcheck [--wait <duration>] [--json]

A readiness probe: exits 0 when a docker command would succeed right now —
the supervisor is serving the pipe and the engine is running or idle (an idle
engine wakes on the next docker call). Nothing is started; --wait keeps
probing while `skrog start` (or the logon autostart) brings the engine up.

  skrog healthcheck --wait 2m && docker run --rm hello-world

Exit codes: 0 ready, 1 not ready, 2 usage, 3 not installed.

flags:
  -json
    	emit machine-readable JSON
  -state-dir string
    	override Skrog's state directory
  -wait duration
    	keep probing until ready or this long has passed (e.g. 2m)
```

## install

provision the engine distro and start it

```
usage: skrog install [flags]

Provisions the Skrog engine distro: checks the host, downloads and verifies the
rootfs, imports it as a WSL2 distro, and starts the engine.

The rootfs is always checksum-verified. Version pinning is a contract: this
build installs exactly the components in its embedded manifest, and nothing is
fetched as "latest".

Exit codes: 0 ok, 1 error, 2 usage.

flags:
  -agent string
    	linux skrog-agent for the wslc backend (default: the one shipped beside skrog.exe)
  -config skrog config export
    	declarative install from a skrog.yaml (see skrog config export)
  -data-dir string
    	where the distro's VHDX lives (default: under the state dir)
  -distro string
    	WSL distro name (default: skrog-engine)
  -engine string
    	engine backend: distro (a WSL2 distro Skrog owns) or wslc (a WSL container session) [experimental] (default "distro")
  -engine-version string
    	engine version to install (default: this build's default)
  -headless
    	never prompt; for unattended and CI installs
  -json
    	emit the resulting manifest as JSON
  -locked skrog lock
    	install the exact engine pinned in a skrog.lock (see skrog lock)
  -no-autostart
    	do not register the supervisor to start at logon
  -no-verify-signature
    	skip the rootfs signature check for this install (the SHA-256 pin still applies)
  -offline skrog bundle
    	install entirely from an air-gap bundle .zip (see skrog bundle)
  -rootfs-sha256 string
    	expected rootfs SHA-256; required with --rootfs-url
  -rootfs-url string
    	override the rootfs URL (development)
  -state-dir string
    	override Skrog's state directory
```

## lock

write a skrog.lock pinning the exact engine (reproducible installs)

```
usage: skrog lock [--engine-version <v>] [-o skrog.lock]

Writes a skrog.lock pinning the exact engine this build installs: version,
rootfs URL and SHA-256, and component versions. Check it into a repo and every
developer and CI runner reproduces the same verified engine with:

  skrog install --locked skrog.lock

With no -o, the lock is printed to stdout.

Exit codes: 0 ok, 1 error, 2 usage.

flags:
  -engine-version string
    	engine version to lock (default: this build's default)
  -o string
    	shorthand for --output
  -output string
    	write to this file instead of stdout
```

## logs

supervisor, dockerd, or audit log; --follow, --json for shippers

```
usage: skrog logs [--source supervisor|dockerd|audit] [-n <lines>] [--follow] [--json]

  supervisor   the always-on bridge: engine starts/stops, recovery, idle stops
  dockerd      the engine daemon's own log, read from inside the distro
  audit        the container-affecting API record (needs `config set audit on`)

--follow survives log rotation (the supervisor and audit logs rotate at 5 MB).

Exit codes: 0 ok, 1 error, 2 usage, 3 not installed (dockerd).

flags:
  -follow
    	keep printing new lines as they arrive (Ctrl-C to stop)
  -json
    	one JSON object per line, {"source","line"} — for log shippers
  -n int
    	print the last N lines first (0 = all) (default 200)
  -source string
    	which log: supervisor, dockerd, or audit (default "supervisor")
  -state-dir string
    	override Skrog's state directory
```

## migrate

copy images and volumes from Docker Desktop into the engine

```
usage: skrog migrate --from-desktop|--from-rancher|--from-podman [--dry-run]

Copies images and named volumes from another engine into the Skrog engine, so
trying Skrog does not mean starting from an empty one.

  skrog migrate --from-desktop --dry-run    # what would move, and how big
  skrog migrate --from-rancher
  skrog migrate --from-podman

The copy is one-way and non-destructive: nothing in the source is changed or
removed, and an interrupted migration leaves it exactly as it was. Run it again
to resume — anything already copied is skipped. Images stream via docker
save|load, volumes via a streamed tar; neither stages a file on disk.

Sources:
  --from-desktop   Docker Desktop, via its desktop-linux context
  --from-rancher   Rancher Desktop, via its rancher-desktop context. Needs the
                   dockerd (moby) backend: with containerd selected there is no
                   Docker API to read, and nothing here can work around that
  --from-podman    Podman, via its Docker-compatible API on the machine's named
                   pipe. Nothing shells out to podman; the docker CLI talks to
                   it directly. For a non-default machine, --from-host
                   npipe:////./pipe/podman-machine-<name>

  --from-context / --from-host address any other engine directly.

Build cache is not migrated: it is not portable through save/load. Anonymous
(unnamed) volumes are skipped — they belong to specific containers, which do
not move.

Exit codes: 0 ok, 1 error, 2 usage.

flags:
  -docker string
    	path to the docker CLI (default: docker on PATH)
  -docker-host string
    	destination engine (default: the Skrog pipe this install serves)
  -dry-run
    	list what would move and how big it is, then stop
  -from-context string
    	source docker context, instead of one of the --from-* engines
  -from-desktop
    	migrate from Docker Desktop (the desktop-linux context)
  -from-host string
    	source engine endpoint, e.g. npipe:////./pipe/podman-machine-<name>
  -from-podman
    	migrate from Podman (its Docker-compatible API on the machine's named pipe)
  -from-rancher
    	migrate from Rancher Desktop (the rancher-desktop context; needs its dockerd backend)
  -only value
    	limit to images/volumes whose name contains this (repeatable)
  -state-dir string
    	override Skrog's state directory
```

## prewarm

pull a pinned image list ahead of need (runner warm-up, golden images)

```
usage: skrog prewarm [--concurrency <n>] [--json] <images.txt>

Pulls every image listed in the file — one reference per line, # comments and
blank lines ignored, digest pins encouraged — with a bounded number in flight.
A failed pull never stops the others; the exit code says whether all succeeded.

  skrog healthcheck --wait 2m && skrog prewarm images.txt

Pulls target whatever docker currently targets: the local engine, or the remote
selected with `skrog remote use`.

Exit codes: 0 all pulled, 1 some failed, 2 usage.

flags:
  -concurrency int
    	how many pulls run at once (default 3)
  -json
    	emit machine-readable JSON
```

## policy

local admission control for the docker API: show, check, test

```
usage: skrog policy show|check|test

Local admission control for the docker API. Skrog already sits in the request
path, so it can refuse a container the machine's owner has ruled out — no
elevation, no engine change, no daemon plugin.

  show    print the rules in effect, and where they come from
  check   validate the rules file without applying it
  test    judge a container-create body against the rules, and say why

Rules live in policy.yaml inside the state dir. A missing file means no rules.
Edits take effect on the next container create -- nothing to restart.

  deny-privileged:          true        # refuse --privileged
  deny-added-capabilities:  true        # refuse any --cap-add
  deny-capabilities:        [SYS_ADMIN] # ...or only these
  deny-host-namespaces:     true        # refuse --network/--pid/--ipc/--uts=host
  allow-bind-sources:       [C:\work]   # bind mounts may only come from here
  allow-registries:         [registry.example.com, "*.internal"]
  require-digest:           true        # images must be pinned by digest

This is a guardrail, not a security boundary: whoever owns the machine can
edit the file or bypass the bridge. It is for catching mistakes and for
shared and CI machines.

Exit codes: 0 ok, 1 error, 2 usage, 3 not installed, 13 the rules deny it.
```

## profile

save and switch named settings profiles (work/home)

```
usage: skrog profile                    list profiles (* = active)
       skrog profile create <name>      save the current settings as a profile
       skrog profile switch <name>      apply a profile's settings
       skrog profile show <name>        print a profile
       skrog profile delete <name>      remove a profile

A profile is a named set of the settings that change between networks — engine
registry mirrors, DNS, logging, the idle timeout, and lifecycle hooks (the
config keys `skrog config` manages). Switch profiles when you move between the
corporate VPN and home instead of hand-toggling each one; `skrog status` names
the active profile.

Switching applies the profile exactly: settings the profile does not set are
cleared, so the engine config always matches the named profile.

Exit codes: 0 ok, 1 error, 2 usage, 3 no such profile / not installed.
  -json
    	emit machine-readable JSON (list, show)
  -state-dir string
    	override Skrog's state directory
```

## proxy

serve the docker pipe in the foreground (debug mode)

```
usage: skrog proxy [flags]

Serves the Windows named pipe that stock docker.exe connects to, relaying it to
the engine inside the WSL2 distro. Runs in the foreground until interrupted.

This is the v0.1 way to run the bridge. A Windows service that supervises it
without a logged-in session is v0.2 work (issue #3).

Exit codes: 0 clean shutdown, 1 error, 2 usage, 3 no engine installed.

flags:
  -agent string
    	linux skrog-agent to place in the wslc session (default: the one shipped beside skrog.exe, else lifted from the engine distro)
  -distro string
    	WSL distro to relay to (default: from the install manifest)
  -engine string
    	engine backend: distro (a WSL2 distro Skrog owns) or wslc (a WSL container session) [experimental] (default "distro")
  -no-context
    	do not create or update the skrog docker context
  -no-path-translation
    	relay bytes verbatim, without translating Windows bind paths
  -pipe string
    	pipe to serve (default: \\.\pipe\docker_engine, or Skrog's own if that is taken)
  -sddl string
    	security descriptor for the pipe (advanced; default restricts to SYSTEM, administrators and the owning user)
  -socket string
    	engine socket inside the distro (default "/var/run/docker.sock")
  -state-dir string
    	override Skrog's state directory
```

## prune

reclaim disk: stopped containers, unused images, build cache

```
usage: skrog prune [--all] [--until <age>] [--build-cache] [--volumes] [--json]

Frees disk on the engine: stopped containers first (so their images become
unused), then unused images; --build-cache and --volumes widen it. A failed step
never stops the others. Acts on whatever docker currently targets — the local
engine or the remote selected with `skrog remote use`.

  skrog prune --until 168h               # keep anything from the last week
  skrog prune --all --build-cache        # the full sweep after a job

Exit codes: 0 ok, 1 a step failed, 2 usage.

flags:
  -all
    	remove all unused images, not only dangling (untagged) ones
  -build-cache
    	also prune the BuildKit cache
  -json
    	emit machine-readable JSON
  -until duration
    	only remove objects older than this, e.g. 168h (0 = any age)
  -volumes
    	also prune unused volumes — they hold data, so off by default
```

## relocate

move the engine data dir to another drive

```
usage: skrog relocate <new-directory> [--restart] [--dry-run] [--keep-archive] [--json]

Moves the engine's data directory — the virtual disk, and every image,
container and volume in it — to another drive. "Move it off C:" is what this
is for.

The engine is exported to a checksummed archive, the old distro is
unregistered, and the archive is imported at the new location. The archive is
written to the TARGET drive, because the reason to move is usually that the
current one is full. It is deleted once the new location is proven, and `--keep-archive`
keeps it.

Nothing is destroyed before the archive exists and its checksum has been
verified, so an interrupted move leaves your data recoverable — and if the
import fails, the error prints the exact command that restores it.

Because the archive and the new disk briefly coexist, the target needs roughly
twice the current disk's size free. That is checked before anything is touched.

  skrog relocate D:\skrog --dry-run      # what it would do, and what it needs
  skrog relocate D:\skrog --restart      # move it, then bring the engine back

Exit codes: 0 ok, 1 error, 2 usage, 3 not installed, 12 not enough space.

flags:
  -distro string
    	WSL distro (default: from the install manifest)
  -dry-run
    	print the steps and change nothing
  -json
    	emit machine-readable JSON
  -keep-archive
    	keep the transfer archive instead of deleting it
  -restart
    	start the engine again afterwards
  -state-dir string
    	override Skrog's state directory
```

## remote

register and switch to a remote engine served over mutual TLS

```
usage: skrog remote                                   list remotes
       skrog remote --host tcp://<h>:2376 --certs <dir> add <name>
       skrog remote use <name>|local
       skrog remote test <name>
       skrog remote remove <name>

The client side of `skrog serve --tcp`. `add` registers a remote engine's address
and its mutual-TLS material (the ca.pem, client cert and key the server's
`skrog serve cert` produced) and creates a docker context named skrog-<name>.
`use` makes that context current, so plain `docker` — and anything that follows
the docker context, such as VS Code Dev Containers — talks to the remote engine;
`use local` switches back to the local skrog context. Certificates are copied
under the state dir and never printed.

Flags come before the verb. Exit codes: 0 ok, 1 error, 2 usage, 3 no such remote.

flags:
  -certs add
    	directory with ca.pem + cert.pem/key.pem (or client.pem/client-key.pem) for add
  -host add
    	engine address for add, e.g. tcp://desktop:2376
  -json
    	emit machine-readable JSON (list, test)
  -state-dir string
    	override Skrog's state directory
```

## reset

reset the engine to a snapshot, unconditionally (runner clean slate)

```
usage: skrog reset --to <snapshot> [--json]

Replaces the engine with a snapshot, unconditionally: every image, container and
volume not in the snapshot is lost, running containers included. This is the
non-interactive form of `skrog snapshot restore --yes --force`, meant for
runner job scripts:

  skrog snapshot save golden          # once: after pulling your base images
  skrog reset --to golden             # before each job: clean slate, images present

Exit codes: 0 ok, 1 error, 2 usage, 3 no such snapshot / not installed.

flags:
  -json
    	emit machine-readable JSON
  -state-dir string
    	override Skrog's state directory
  -to string
    	snapshot to reset the engine to (required)
```

## restart

stop the engine, then start it

```
usage: skrog restart [--supervisor]

Stops the engine and starts it again.

By default this is the ENGINE only. The supervisor — the always-on process
that serves the docker pipe — keeps running across it, which is what lets a
restart be quick and the pipe stay put.

  --supervisor   also replace the supervisor process

Almost nothing needs `--supervisor`: settings are re-read live, and the engine
picks up config changes on this plain restart. Reach for it when the
supervisor itself is misbehaving, or after replacing skrog.exe on disk.
  -state-dir string
    	override Skrog's state directory
  -supervisor
    	recycle the supervisor process too, not just the engine
  -timeout duration
    	how long to wait for each phase (default: 1m to stop, 2m to start)
```

## runner

runner check: verify auto-logon, autostart, supervisor and engine on an unattended host

```
usage: skrog runner check [--json]

Verifies the pieces an unattended runner depends on, and names the missing one:

  auto-logon configured (Winlogon), for this account, without a clear-text
  password in the registry; the logon autostart registered; the supervisor
  running; the engine running or idle; and whether the machine sleeps on mains
  power, which would suspend a job mid-run.

Nothing is changed. The auto-logon account name is compared, never printed, and
the password value is only probed for existence.

Exit codes: 0 ready (warnings allowed), 1 not ready, 2 usage, 3 not installed.

flags:
  -json
    	emit machine-readable JSON
  -state-dir string
    	override Skrog's state directory
```

## serve

expose the engine over the network with mutual TLS (`serve cert`)

```
usage: skrog serve --tcp <addr>

Exposes the engine over the network with MUTUAL TLS, so a teammate or a CI
runner can target it. Only holders of a client certificate this machine's CA
signed can connect — the engine is never open to the network at large.

First mint the certificates (once):

  skrog serve cert --host <this-machine-hostname-or-ip>

then run the server:

  skrog serve --tcp 0.0.0.0:2376

On the client, copy ca.pem + client.pem + client-key.pem and:

  $env:DOCKER_HOST = "tcp://<host>:2376"
  $env:DOCKER_TLS_VERIFY = "1"
  $env:DOCKER_CERT_PATH = "<dir with the three files>"
  docker version

The engine must be running (`skrog start`); pair remote serving with
idle-timeout off so a remote client never meets a stopped engine.

Exit codes: 0 ok, 1 error, 2 usage, 3 not installed / no certs.

flags:
  -distro string
    	WSL distro (default: from the install manifest)
  -state-dir string
    	override Skrog's state directory
  -tcp string
    	address to serve on, e.g. 0.0.0.0:2376 (required)
```

## snapshot

save/restore/list the engine state (images, containers, volumes)

```
usage: skrog snapshot save <name>       capture the engine state
       skrog snapshot list                list snapshots
       skrog snapshot restore <name>      replace the engine with a snapshot
       skrog snapshot delete <name>

Saves and restores the whole engine state — every image, container and volume —
as a named, checksummed archive. "Set up a dev environment, snapshot it, trash
it during a risky test, restore it in seconds." Only Skrog's own distro is ever
touched.

save/restore refuse while containers are running (pass --force to override);
restore is destructive and needs --yes.

Exit codes: 0 ok, 1 error, 2 usage, 3 no such snapshot / not installed.

flags:
  -force
    	proceed even if containers are running
  -json
    	emit machine-readable JSON
  -state-dir string
    	override Skrog's state directory
  -yes
    	skip the confirmation prompt (required for restore)
```

## start

ensure the supervisor and engine are running

```
usage: skrog start

Records the desired state as running, launches the supervisor when none is
running, and waits for the engine to answer.
  -state-dir string
    	override Skrog's state directory
  -timeout duration
    	how long to wait for the engine (default 2m0s)
```

## status

report supervisor, engine and desired state

```
usage: skrog status [--json] [--stats] [--prometheus]

Reports the distro, whether the supervisor and engine are running, the desired
state the user last asked for, and — while a supervisor is running — the pipe
it actually bound, with the DOCKER_HOST spelling of it.

The endpoint is what the running supervisor recorded when it bound, not a
fresh guess: Docker Desktop can start or stop after Skrog chose, so recomputing
the answer could name a pipe nothing is serving. No supervisor, no endpoint.

Reads host-side files only — it never starts the engine to answer, and never
wakes an idle-stopped one. Safe to poll.

The engine is one of:

  running   answering the docker API
  idle      stopped by the idle timeout, ON PURPOSE; the next docker command
            wakes it. Not an error, and the exit code says so
  stopped   down, and staying down until `skrog start`

--stats adds engine, disk, VM, uptime and bridge counters. It is opt-in
because collecting them costs WSL calls a readiness probe should not pay; the
default shape is the pinned probe contract (docs/cli-json.md).

--prometheus emits the same numbers as Prometheus text, for node_exporter's
textfile collector (a scheduled task writes it into the collector directory).
It implies --stats. Nothing leaves this machine: there is no listener and no
telemetry — it is the operator measuring their own host.

Exit codes: 0 engine running or idle, 1 engine down, 2 usage, 3 not installed.
--prometheus always exits 0 when it could write metrics, because the engine's
state is IN the metrics: a scrape that failed because the engine was down would
discard exactly the reading worth having.
  -json
    	emit machine-readable JSON
  -prometheus
    	emit Prometheus text (node_exporter textfile format); implies --stats
  -state-dir string
    	override Skrog's state directory
  -stats
    	add engine, disk, VM, uptime and bridge statistics (needs a running engine for the first three)
```

## stop

stop the engine; it stays stopped until start

```
usage: skrog stop

Records the desired state as stopped and waits for the engine to stop. The
supervisor keeps honoring this until `skrog start` — a stopped engine stays
stopped. Only Skrog's own distro is touched, never other WSL distros.
  -state-dir string
    	override Skrog's state directory
  -timeout duration
    	how long to wait for the engine to stop (default 1m0s)
```

## supervise

serve the pipe and keep the engine alive (the always-on layer)

```
usage: skrog supervise [flags]

The always-on layer: serves the docker pipe AND keeps the engine alive —
crash restart with backoff, recovery from `wsl --shutdown` and sleep/resume,
honoring `skrog stop` until `skrog start`. One instance per install.

Runs in the foreground; `skrog start` spawns it in the background, and the
logon autostart (`skrog autostart`) runs it for you. Logs go to supervisor.log in the
state directory (rotated) as well as stderr.

flags:
  -agent string
    	linux skrog-agent for a wslc install (default: the one shipped beside skrog.exe)
  -distro string
    	WSL distro (default: from the install manifest)
  -no-context
    	do not create or update the skrog docker context
  -pipe string
    	pipe to serve (default: \\.\pipe\docker_engine, or Skrog's own if taken)
  -state-dir string
    	override Skrog's state directory
```

## uninstall

remove the engine distro and Skrog's state

```
usage: skrog uninstall [--yes]

Unregisters the engine distro and removes Skrog's own state. Nothing else on
the system is touched.

This DELETES the distro, and with it every image, container and volume it
holds. Export anything you want to keep first.

Exit codes: 0 ok, 1 error.

flags:
  -distro string
    	WSL distro name (default: from the install manifest)
  -state-dir string
    	override Skrog's state directory
  -yes
    	skip the confirmation prompt
```

## upgrade

am I current? app, engine and bundled CLI in one answer

```
usage: skrog upgrade [--check|--dry-run] [--yes] [--offline] [--json]

Reports whether the app, the engine and the bundled docker CLI are current,
then brings forward the two it owns — after showing you what it will do.

  skrog upgrade            everything
  skrog engine upgrade     just the engine

The app is reported and not applied by default, because replacing the running
binary restarts the supervisor and briefly drops the docker pipe. `--apply`
does it: the release zip is downloaded and checked against that release's
SHA256SUMS BEFORE anything on disk is touched, the binaries are moved aside
rather than overwritten, and a failure at any point leaves the install exactly
as it was.

  --check     report only; change nothing
  --apply     replace skrog.exe too, not just the engine and the CLI
  --dry-run   print exactly what would be applied, and apply nothing
  --yes       do not ask (runners)
  --force     replace the binary even when a package manager owns this install

--apply replaces the files. skrog.exe takes effect immediately, because the
supervisor is recycled onto the new one; skrogw.exe and skrogtray.exe take
effect when those processes next start, which for most people is the next
logon. The command says which is which rather than implying it all swapped.

Why this is not just a convenience: the engines `skrog engine upgrade` can
reach are pinned in THIS binary's manifest. A newer engine can therefore need
a newer skrog first — so "engine: current" is only ever true of the build you
are running, and this command says so when it matters.

Nothing here auto-updates or polls in the background. It runs when you ask,
and makes exactly one outbound request — to the releases API, for the app
version. The engine and CLI answers are local (both manifests are compiled
in), so `--offline` still reports those. Air-gapped installs should use it.

The CLI is applied before the engine: it is a file copy that costs no
downtime, where an engine upgrade stops and restarts the engine. A failed
engine upgrade therefore leaves the CLI already current rather than nothing
done, and `skrog engine rollback` reverses the engine half on its own.

Exit codes: 0 nothing to do or everything applied, 1 error, 2 usage,
3 something can be upgraded (--check and --dry-run only).

flags:
  -apply
    	also replace skrog.exe itself with the newer release
  -check
    	report only; change nothing
  -dry-run
    	print what would be applied, and apply nothing
  -force
    	replace the binary even when a package manager owns this install
  -json
    	emit machine-readable JSON (implies --check)
  -offline
    	skip the network check for the app version
  -state-dir string
    	override Skrog's state directory
  -timeout duration
    	how long to wait for the releases API (default 15s)
  -yes
    	skip the confirmation prompt (for runners)
```

## wsl-integrate

point docker inside your own WSL distros at the engine

```
usage: skrog wsl-integrate <distro>       wire a distro to the engine
       skrog wsl-integrate --remove <distro>
       skrog wsl-integrate                without arguments: list wired distros

Points docker inside one of your own WSL distros at the Skrog engine, by
writing /etc/profile.d/skrog.sh there (DOCKER_HOST to the engine socket shared at /mnt/wsl). Takes
effect in new login shells. Never touches a distro you did not name, and
`skrog uninstall` unwires everything it wired.

Note: with an idle-timeout configured, in-flight work over the shared socket
(a build or pull from the integrated distro) holds the engine up — it is never
stopped mid-operation. But between operations the engine can still idle-stop,
and a shared-socket client cannot wake a stopped engine on its own; run
`skrog start` first (or keep idle-timeout off) for long in-distro sessions.

Exit codes: 0 ok, 1 error, 2 usage.
  -remove
    	unwire the distro instead
  -state-dir string
    	override Skrog's state directory
```

## wsl-config

right-size the WSL2 VM: show and apply ~/.wslconfig sizing, with consent

```
usage: skrog wsl-config [show|apply] [--yes] [--json]

Right-sizes the WSL2 VM the engine runs in: memory, processors, swap and
autoMemoryReclaim, plus virtiofs for how Windows drives are mounted. Those
live in %USERPROFILE%\.wslconfig, which is GLOBAL — every WSL2 distro on this
machine shares it, Docker Desktop's included — so Skrog records what you asked
for and writes it only when you say so:

  skrog config set wsl.memory 4GB
  skrog config set wsl.processors 2
  skrog config set wsl.virtiofs true   # faster /mnt/c; needs WSL 2.9+
  skrog wsl-config apply            # shows the diff, asks, then writes

  show     the effective limits and any pending changes (default)
  apply    write the pending changes to ~/.wslconfig

--yes skips the prompt, for a runner where the owner has already decided;
re-running it changes nothing.

Sizing takes effect when the WSL VM next starts. Skrog will not restart it:
the only way is `wsl --shutdown`, which stops every distro on the machine.

Exit codes: 0 ok, 1 error, 2 usage.
```

## version

report every component version and which docker.exe is active

```
usage: skrog version [--json]

Reports every component version, which docker.exe actually runs, the active
docker context, and the negotiated engine API version.

Exit codes: 0 ok, 1 error, 3 no engine installed.

flags:
  -json
    	emit machine-readable JSON
  -state-dir string
    	override Skrog's state directory
```
