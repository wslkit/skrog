# Skrog

> **skrog** *(n., Norwegian)* — the hull: the body of the ship that carries the cargo and keeps the sea out.

A minimal, invisible way to run the upstream open source Docker Engine on Windows via WSL2. **No license fees, no Electron, no Kubernetes** — install once, `docker ps` works forever, on laptops and on CI runners alike.

| | |
|---|---|
| License | Apache-2.0 |
| Language | Go |
| Target | WSL 2.x (Store/MSI) on a virtualization-capable host — not a Windows build number · Windows 11 primary; Windows 10 22H2 tested and working (past EOS Oct 2025, so ESU fleets only) |
| Engine | upstream moby (dockerd) |
| Binary budget | < 15 MB |
| Telemetry | none |

---

## 01 · Positioning

Docker Desktop requires a paid subscription for most companies. Rancher Desktop carries Kubernetes and an Electron UI whether you want them or not. Podman Desktop swaps the runtime itself. Microsoft's new WSL Containers (`wslc`, public preview June 2026, GA targeted fall 2026) mimics the docker *CLI*, and under it every session runs a real Moby engine — but it exposes **no Docker endpoint**: dockerd is started with no `-H`, so it listens on a unix socket inside the session VM and nowhere else, and there is no named pipe, TCP port or inbound route a Docker client could name. The engine is complete and unreachable, so compose, Testcontainers, IDE integrations, and anything that mounts `docker.sock` cannot talk to it. The remaining corner — *"I just want the real `docker` to work on Windows, free, with zero ceremony"* — is served today only by DIY wiki guides and dormant side projects. Skrog productizes that corner and nothing else.

### In scope

- Upstream Docker Engine (Linux containers) in a dedicated WSL2 distro
- Named-pipe bridge so stock `docker.exe` works from any Windows shell — the real API, byte for byte
- Lifecycle: install, autostart at logon, crash recovery, sleep/resume survival, clean uninstall — supervised, headless, no desktop app
- Headless / CI mode: unattended install, JSON output, engine version pinning, no human interaction after setup (a logged-on session is required — see §06)
- Desktop parity details: `host.docker.internal`, WSL distro integration, credential helper, volume path translation
- Housekeeping: VHDX compaction, memory reclaim, engine upgrades with rollback
- `doctor` — diagnoses WSL, VPN/DNS, proxy, and networking failures with actionable fixes
- Bundled docker CLI + compose + buildx, wired via `docker context`

### Out of scope — permanently

- Windows containers (Linux containers only; say it loudly in the README)
- Kubernetes in any form
- Container-management GUI, dashboards, extensions — Portainer and lazydocker already work against Skrog because it's the real engine
- Image registries, scanning, build acceleration services
- GitHub/GitLab *hosted* Windows runners — they don't expose nested virtualization, so WSL2 cannot start there; no tool can fix that
- macOS / Linux hosts (Colima and native engines own those)

Scope discipline is the moat: the moment a Kubernetes checkbox or a management UI appears, this converges on Rancher Desktop with fewer resources. The tray menu never grows past six items.

---

## 02 · Architecture

```
┌───────────────────────────── Windows host ─────────────────────────────┐
│  single Go binary, three run modes (CLI / supervisor / tray)           │
│                                                                        │
│  Provisioner        Pipe proxy ★          Supervisor       Tray+doctor │
│  rootfs download    \\.\pipe\docker_engine starts at logon, status,    │
│  verify, wsl        ⇄ engine socket,      health, restart, start/stop, │
│  --import           path translation      sleep/resume    diagnostics  │
│                                                                        │
│   │ wsl.exe stdio relay (v0.1) → guest agent over AF_HYPERV vsock (v0.2)│
│  ┌──────── WSL2 distro "skrog-engine" — Alpine rootfs ~80 MB ────────┐ │
│  │  dockerd · containerd · buildkit · skrog-agent                    │ │
│  │  → /var/run/docker.sock                                            │ │
│  └────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
```

Everything Docker Desktop does on Windows is assembly of free parts — the engine is Apache-2.0, WSL2 ships with Windows, the CLI is open source. Skrog is the glue, and the pipe proxy is the load-bearing component the project is named for: stock `docker.exe` expects `\\.\pipe\docker_engine`, and bridging it faithfully (streams, hijacked connections for `attach`/`exec`, half-closes, Windows→WSL volume path translation) is what makes every existing tool — Testcontainers, IDEs, CI scripts — work unmodified.

**Endpoints.** Every endpoint is a bog-standard dockerd endpoint, so `DOCKER_HOST`, `docker context`, and every SDK behave exactly as Docker documents them:

- The named pipe on Windows (default, via the `skrog` context; falls back to `\\.\pipe\skrog_engine` when Docker Desktop owns the default pipe, so both coexist)
- A Unix socket shared to the user's other WSL distros at `/mnt/wsl/skrog/docker.sock`
- An opt-in mutual-TLS TCP endpoint (`skrog expose --tcp`) for tools that can't speak npipe

Never an unauthenticated TCP daemon.

---

## 03 · Control surfaces

**CLI — the product.** Every capability lives here; everything else is a veneer over it. Scriptable from day one: meaningful exit codes, `--json` on status and doctor, no interactive prompts in `--headless` mode. Day-to-day container work stays in `docker` — Skrog's CLI only manages the engine's existence.

**Tray — a status light, ≤ 6 items forever.** Status dot · Start/Stop/Restart · Open logs · Run doctor · Check updates · Quit. Each item invokes the CLI underneath. No settings dialogs, no container lists — that cap is a hard rule, because the tray is where scope creep would start.

**Supervisor — the always-on layer.** A supervisor process starts with the user's session and keeps the distro and dockerd alive: crash restart, recovery from `wsl --shutdown`, sleep/resume recovery. It runs headless — no UI, no tray required — so a CI runner needs no desktop app, only a session.

> **PROVISIONAL — this conclusion is under re-test.** Spike B ran on a machine whose `SeServiceLogonRight` is missing `NT VIRTUAL MACHINE\Virtual Machines` (S-1-5-83-0), which is the *documented* cause of the very error it measured (`0x80070569`). Interactive WSL works there without the group, but a session-0 VM plausibly needs it, so the failure may be a fixable misconfiguration rather than a platform limit. Re-test belongs on a clean VM without a corporate security baseline. Treat everything below about session 0 as unconfirmed until then. See #3.

It is *not* a session-0 Windows service, and cannot be: Spike B (#3) established that WSL2 will not create its utility VM outside an interactive session. `LocalSystem` is refused outright (`WSL_E_LOCAL_SYSTEM_NOT_SUPPORTED`), and a dedicated service account fails in the Host Compute Service with `ERROR_LOGON_TYPE_NOT_GRANTED` even holding Service, Batch and Interactive logon rights *and* local administrator. This is a WSL platform constraint, not a Skrog one — it binds Docker Desktop, Rancher and `wslc` identically. Unattended machines therefore need a logged-on session, which in practice means auto-logon (§06).

**GUI — deliberately never.** Because Skrog serves the standard Docker API, every existing frontend just works: Portainer, lazydocker, Dockge, VS Code. The README shows the Portainer one-liner — converting "where's the GUI?" into a demo of the compatibility promise.

```
skrog install [--headless] [--engine-version X]   # provision, wire context; unattended for fleets
        [--data-dir D:\skrog]                     # VHDX location — "move it off C:" is a top-3 request
skrog uninstall / reset                           # clean removal; factory reset offers image export first
skrog start / stop / restart / status [--json]    # engine lifecycle, machine-readable state
skrog config get / set <key> [value]              # idle-timeout, proxy, mirrors, data-dir — the only config surface
skrog update [--check]                            # self-update via release manifest; never automatic, never mid-pipeline
skrog version [--json]                            # every component + which docker.exe is actually in use
skrog doctor [--fix] [--report]                   # diagnose WSL/VPN/DNS/proxy/pipe/disk; emit markdown for issues
skrog engine upgrade / rollback                   # pinned, checksummed engine updates, independent of app releases
skrog compact                                     # VHDX compaction + memory reclaim
skrog wsl-integrate <distro>                      # share socket + docker CLI into the user's Ubuntu etc.
skrog expose --tcp                                # opt-in mTLS TCP endpoint for npipe-less tools
skrog logs                                        # engine + service logs
skrog migrate --from-desktop                      # pull images/volumes/build cache out of Docker Desktop
skrog stats                                       # vmmem attributed: engine vs other distros, reclaimable
skrog enable-qemu                                 # binfmt/QEMU for multi-arch buildx (--platform linux/arm64)
skrog snapshot create / restore                   # checkpoint entire engine state (VHDX copy while stopped)
skrog create --name <x> [--engine-version Y]      # side-by-side isolated engines, each its own context
skrog cache enable                                # local pull-through registry mirror
```

---

## 04 · Key decisions

**Go, single binary.** `Microsoft/go-winio` gives named pipes for free (it's what Docker itself uses on Windows); moby client libraries, trivial cross-compilation, and the Docker-ecosystem contributor pool are all Go. A thin native shell remains an option later if the UI ever needs toasts or MSIX — the core never moves.

**Two-stage pipe bridge.** v0.1 spawns `wsl.exe` per connection and relays stdio to the Unix socket — simple, secure, provable in a weekend. v0.2 replaces it with a persistent guest agent over AF_HYPERV vsock to eliminate the ~150 ms per-connection spawn cost. Never a TCP daemon: an unauthenticated `localhost:2375` is reachable by browsers and every local process; the named pipe gets an ACL for administrators + a local `Skrog Users` group (the `docker-users` pattern — covers multi-user machines and non-admin dev accounts; the installing user is added automatically). The threat model states it plainly: pipe access ≈ root in the VM ≈ read/write of any file the automounted drives expose — same trust boundary as Docker Desktop, documented rather than implied.

**Dedicated distro, not the user's.** Imported as `skrog-engine` so it never collides with someone's Ubuntu. Rootfs is Alpine + pinned static engine binaries, assembled by CI into a tar published as a checksummed GitHub release asset with an SBOM. Engine binaries are **built from moby/containerd/runc source in CI** (reproducible, SBOM-native) rather than redistributing `download.docker.com` artifacts — the code is Apache-2.0 but Docker Inc.'s compiled bundles are their distribution; building from source removes the question entirely and strengthens the supply-chain story. (`download.docker.com` static bundles remain the bootstrap shortcut for the week-1 manual spike only.) One distro holds engine and data together (Docker Desktop's own lesson — it dropped its `-data` companion distro); upgrades swap static binaries in place, so data is never at risk.

**Config touches, scoped.** Everything possible goes in per-distro config (`/etc/wsl.conf`, `daemon.json`) affecting only Skrog's distro. The global `.wslconfig` — memory limits, mirrored networking — is shared by the user's own distros, so anything global is written only with explicit consent (install prompt or `doctor --fix`), never silently.

**Bundle the docker CLI.** CLI + compose + buildx are Apache-2.0 and redistributable unmodified, alongside `docker-credential-wincred` so registry logins persist in Windows Credential Manager. Wired with `docker context create skrog` — coexists with Docker Desktop instead of fighting it, which makes trial adoption zero-risk (and `DOCKER_HOST` flips between them per-shell).

**Versioning contract.** Every Skrog release carries a **locked version manifest** — app, service, guest agent, rootfs, engine (dockerd/containerd/runc), CLI + compose/buildx/wincred plugins, each with version and checksum — published as JSON with the release. Nothing is fetched as "latest at install time"; the CLI is the newest stable *at release cut*, pinned. CLI↔engine skew is safe by design (Docker's API version negotiation) but bounded: `engine upgrade` refuses combinations outside the tested matrix unless forced, and upgrades are staged swaps with rollback. **`skrog version` makes it all visible**: every component version, which `docker.exe` is actually first on PATH and where it came from (Skrog's, Desktop's, winget's), the active context, and the negotiated API version — so "which docker am I running?" is a command, not an archaeology project.

**The Go toolchain is not pinned, and that is not a contradiction.** The versioning contract above covers what *ships* — engine, rootfs, CLI bundle, every one checksummed and moved only by a reviewed commit. The Go toolchain *compiles* it: never shipped, never fetched at install time, part of no promise to a user. So the `go` directive follows the dependencies rather than being held back, and a dependency that raises it raises it. Three reasons that costs nothing: Skrog is three commands, not a library, so the directive is a compatibility promise to nobody; `GOTOOLCHAIN` defaults to `auto`, so a contributor on an older Go downloads the newer toolchain instead of hitting a wall; and CI already builds with `go-version: stable`. Where it does bite is `GOTOOLCHAIN=local` and offline builds — accepted, and reversible in one commit by dropping to the newest dependency that keeps the old floor. Taking `x/sys` 0.48.0 moved the floor to Go 1.26; holding it would have meant `x/sys` 0.35.0, thirteen releases behind in the package that makes every registry and Win32 call this project makes. Not a trade worth making for a floor nobody is standing on (#306).

**Apache-2.0.** Matches moby/containerd, carries a patent grant, and keeps enterprise legal reviews short — the target audience is exactly the companies that left Docker Desktop over licensing.

**The name.** *Skrog* — Norwegian for the hull, the body of the ship that carries the cargo and keeps the sea out. It names what the product is: the vessel the containers live inside, on Windows. No "docker" or whale anywhere (Docker, Inc. trademarks — a launch-blocking constraint), and the namespace is genuinely empty — verified against GitHub, npm, winget, Chocolatey, Scoop and the open web (#1).

The project shipped v0.1.0–v0.3.1 as **Hawser**, after the mooring line, with "it holds fast" as the reliability promise. That name was chosen on the belief that its namespace was nearly empty — true of trademark registries, false of common-law use: [Finsys/hawser](https://github.com/Finsys/hawser), a 606-star Go agent for Docker host management, predated it by nine months. A Google search for "hawser" returned dictionaries and that project, never this one, so the SEO acquisition story below could never have worked. The rename is recorded in #1 with the full evidence and the eleven alternatives that were rejected.

**The mark.** The hull's **body plan** — the section a naval architect draws at each frame, with the centreline through it. It replaces the coiled rope inherited from *Hawser*, which was a picture of the old name and became an abstract shape under the new one. Being a section and not a view, it has no tapering end and is near enough square to fill an icon unaided. It is an outline rather than a silhouette, so the stroke weight is corrected per size — 15 design units at 512px, 28 at 16px — which is what stops the tray icon and favicon greying out; `tools/internal/mark` holds the geometry so the SVG masters and the rasters cannot drift apart.

---

## 05 · Roadmap

Milestone summaries below; the detailed execution roadmap — task breakdown, dependencies, calendar targets, decision gates — lives in [ROADMAP.md](ROADMAP.md).

### v0.1 — Plumbing (~3 weekends)

CLI-only, end to end. This alone is shippable and useful — everything after it is reliability and polish.

- [ ] Preflight: detect WSL2 / virtualization state (`wsl --status`); if missing, print the exact enable-and-reboot instructions rather than attempting elevation
- [ ] Rootfs pipeline in GitHub Actions: Alpine + pinned static dockerd/containerd/buildkit → tar → checksummed release asset + SBOM
- [ ] `skrog install`: download + verify rootfs, `wsl --import`, write `daemon.json` (log rotation on by default, `host.docker.internal` wired up — the guest side re-resolves the Windows host IP on start, since it differs between NAT and mirrored networking), start dockerd — with `--headless` and `--engine-version` from day one
- [ ] Pipe proxy v1: `go-winio` named-pipe server, per-connection `wsl.exe` stdio relay, correct half-close and stream-hijack handling, Windows→WSL volume path translation (`-v C:\src:/app` just works)
- [ ] Bundle docker CLI + compose + buildx + `docker-credential-wincred` into Skrog's own `bin\` dir per the version manifest; create and select the `skrog` context (fall back to own pipe name if Docker Desktop is present). Detect any existing `docker.exe` on PATH — never shadow it silently, report which binary wins
- [ ] `skrog version [--json]`: full component report — app/service/agent/rootfs/engine/CLI+plugin versions, which `docker.exe` is active on PATH and whose it is, active context, negotiated API version
- [ ] `skrog uninstall`: unregister distro, remove pipe, restore previous context — nothing else on the system was modified

**Acceptance:** On a fresh Windows 11 VM: install → `docker run hello-world` succeeds in under 5 minutes without the user touching WSL. A real compose stack (multi-service, healthcheck-gated `depends_on`, Windows-path bind mounts, `logs -f`) runs `up --build` → `down` cleanly — compose is a headline claim, and it exercises exactly the proxy's streaming, hijack, and path-translation correctness. A minimal Testcontainers suite (one .NET or Java test with a database container) passes against the pipe — Testcontainers is the other headline claim, and it exercises API version negotiation, port mapping, and the ryuk reaper's socket semantics.

### v0.2 — Always-on (~3 weekends)

The "it just disappears" milestone: survives everything Windows throws at it without user action — including running with nobody logged in.

- [ ] Windows service (`x/sys/windows/svc`): supervises the distro and dockerd, restarts on WSL crashes *and* user-initiated `wsl --shutdown`, starts at boot
- [ ] Start-at-logon supervision so CI runners work unattended: the supervisor and pipe proxy start with the session, since WSL2 cannot be driven from session 0 (Spike B, #3). Document the auto-logon setup for headless runners rather than automating it — writing credentials into LSA secrets on a user's behalf is a liability a security review would rightly object to
- [ ] Guest agent over AF_HYPERV vsock replaces per-connection spawning; latency parity with Docker Desktop
- [ ] On-demand start + optional idle shutdown (`skrog config set idle-timeout 30m`): engine starts on first pipe connection (~2s cold start), stops when no containers run and the pipe is quiet — answers the #1 laptop complaint (idle RAM)
- [ ] Sleep/resume and network-change recovery: re-verify socket health on power events, reconnect silently
- [ ] `skrog wsl-integrate`: socket + CLI shared into the user's other distros via `/mnt/wsl` (Desktop parity)
- [ ] `skrog migrate --from-desktop`: copy images, volumes, and build cache out of Docker Desktop's distro — turns "try Skrog" into a 10-minute reversible experiment instead of a fresh start; the single biggest adoption lever
- [ ] Tray icon (6 items, forever): status dot, start/stop/restart, open logs, run doctor, quit
- [ ] Structured logging to the Windows Event Log and a rotating file

**Acceptance:** Kill `wsl.exe`, reboot, or sleep/resume — the next `docker ps` works with no user action. On a machine with auto-logon configured, a reboot brings the engine back and a container job runs with nobody physically present.

### v0.3 — Doctor, VPN & housekeeping (~3 weekends)

The differentiator over DIY guides: turn the WSL2 quirk zoo — VPNs above all — from an issue-tracker flood into a diagnosable surface.

- [ ] `skrog doctor`: WSL version, virtualization, VPN DNS breakage, pipe ACL/health, port conflicts, MTU/route reachability, disk usage — each with an actionable fix, not just a red X; `--report` emits markdown ready to paste into a GitHub issue
- [ ] Session check in doctor: no interactive session means the engine cannot start at all (Spike B, #3). On a headless runner this is *the* first failure someone hits, and without a named check it looks like Skrog being broken — the remedy is the auto-logon setup from §06
- [ ] Version-sanity checks in doctor: PATH shadowing (a foreign `docker.exe` resolving ahead of Skrog's while the `skrog` context is active), CLI↔engine API skew outside the tested matrix, agent/rootfs/app manifest mismatch after a partial upgrade
- [ ] Prefer the platform fix for VPNs: apply `networkingMode=mirrored`, `dnsTunneling`, `autoProxy` where the Windows build supports them (global config → explicit consent)
- [ ] VPN known-issues database: fingerprint GlobalProtect / AnyConnect / Zscaler patterns → named fix; fallbacks for hostile setups (static DNS pinning, MTU clamping, opt-in `skrog vpn-workaround` relay in the wsl-vpnkit style)
- [ ] Corporate proxy + registry mirrors as first-class config: `HTTP_PROXY`/`NO_PROXY` for dockerd, mirror settings, doctor checks for both
- [ ] VHDX management: sparse VHDX where supported, one-click compaction, memory reclaim, sane `.wslconfig` defaults with consent; `skrog config set data-dir` relocates the VHDX to another drive (stop → move → re-register)
- [ ] `skrog engine upgrade`: pinned, checksummed engine updates with rollback — engine security patches decoupled from app releases
- [ ] `skrog stats`: vmmem attribution ("WSL VM total 12 GB — skrog-engine 3.1 GB, your Ubuntu 8.9 GB, reclaimable 4 GB") — defuses the most common WSL2 false alarm
- [ ] `skrog enable-qemu`: register binfmt/QEMU handlers so `buildx --platform linux/arm64` works out of the box (Desktop parity for multi-arch builds)
- [x] ~~SSH agent bridging~~ — **not needed, and not built (#391).** The premise was wrong: buildx dials `\\.\pipe\openssh-ssh-agent` itself and forwards the agent over the ordinary API connection, so `docker build --ssh default` already crosses the bridge unchanged. Verified end to end against a live engine. The only real gap was host-side — Windows ships the `ssh-agent` service disabled — which is now a `doctor` warning rather than a relay we own. `SSH_AUTH_SOCK` mounts into `wsl-integrate`'d distros remain unaddressed and unrequested.

**Acceptance:** The doctor correctly diagnoses the five most common failure classes reported in v0.1/v0.2 issues, and a GlobalProtect-equipped test machine reaches a registry through the tunnel after `doctor --fix`.

### v0.4 — Ship it (~2 weekends)

Distribution and trust — the difference between a repo and a tool people install at work.

- [ ] winget, scoop, and chocolatey manifests; WiX MSI for Intune/Ansible fleet deployment
- [ ] Windows ARM64 builds alongside x64 (Go cross-compiles; WSL2 and the engine run natively on ARM64 — Snapdragon dev laptops are a growing, underserved slice)
- [ ] `skrog update [--check]`: self-update from the signed release manifest — explicit invocation only, never automatic (determinism is the CI contract)
- [ ] Code signing -- route undecided (#77): the SignPath Foundation declined for now pending visibility, paying is the alternative. Published checksums, SLSA provenance and cosign are in place; SmartScreen reputation only starts accruing once signed
- [ ] `skrog expose --tcp`: opt-in mutual-TLS TCP endpoint with generated certs, for SDKs that can't speak npipe
- [ ] Opt-in LAN port mirroring (`netsh interface portproxy`) for published container ports
- [ ] Enterprise policy layer: ADMX template + registry keys for what admins actually lock down (pin/forbid engine versions, disable `expose --tcp`, lock registry mirrors, disable LAN mirroring)
- [ ] Validate and document compatibility with Microsoft Defender for Endpoint's WSL plugin, so `skrog-engine` workloads are visible to EDR
- [ ] Docs site, demo GIF, comparison table vs Desktop/Rancher/Podman/wslc, CONTRIBUTING + issue templates (doctor --report pre-wired into the bug template)

**Acceptance:** `winget install skrog` works on a clean machine with no SmartScreen wall.

### v0.5 — Power tools (~2 weekends)

Advanced-user features no other Docker product on Windows offers — kept out of v0.4 so the ship-it release stays tight.

- [ ] `skrog snapshot create / restore`: checkpoint the entire engine state (VHDX copy while stopped)
- [ ] `skrog create --name edge --engine-version 28.0-rc`: multiple side-by-side isolated engines, each its own distro + docker context — Desktop fundamentally can't
- [ ] Opt-in prune policy (scheduled `system prune` rules) + tray disk-quota alerts *before* the VHDX eats the drive
- [ ] `skrog cache enable`: local pull-through registry mirror (`registry:2`) wired into `daemon.json`
- [ ] PowerShell + bash completions for the skrog CLI

**Acceptance:** Snapshot → break everything → restore round-trips bit-perfect; two engines run simultaneously with independent contexts.

### v1.0 — Stable (ongoing)

Boring on purpose: reliability guarantees, not features.

- [ ] End-to-end suite on a self-hosted Windows runner with nested virtualization (GitHub-hosted runners can't run WSL2)
- [ ] Compatibility matrix across Windows 11 23H2/24H2+ / Insider, plus Windows 10 22H2-under-ESU as best-effort (mirrored networking and several doctor fixes are Win11-only — the matrix says which); WSL version pinning policy
- [ ] Semantic versioning, upgrade guarantees, published threat model for the pipe (so security teams can approve it)
- [ ] Checkpoint: the concrete tripwire is **movement on microsoft/WSL#40976** (a Docker Engine API endpoint for wslc — today unanswered, no milestone), not wslc GA itself, which ships without it. A maintainer reply, a milestone, or a `settings.yaml` endpoint in release notes triggers a deliberate positioning review
- [ ] Platform watch-list — wslc's under-the-hood work is WSL platform investment Skrog may inherit for free: adopt **virtiofs** file sharing the day it reaches standard distros (attacks the slow-9P bind-mount problem), fold **consomme networking** into `doctor --fix` as the preferred VPN remedy if exposed, and shrink `compact`/reclaim to a thin wrapper if the memory-reclaim improvements ship platform-wide. Also watch for **WSL gaining service-context support**: it would restore true sessionless operation (§06) and remove the auto-logon requirement for every WSL-based engine at once

**Acceptance:** Two consecutive Windows feature updates absorbed with zero breaking issues filed.

---

## 06 · CI/CD & automation

Possibly the sharpest wedge market: **self-hosted Windows CI runners that need Linux containers**. A .NET or desktop shop's build machines need Testcontainers, Linux image builds, or a database sidecar mid-pipeline — and today their options are a paid Docker Desktop license (which also wants an interactive session and a tray app, and auto-updates mid-pipeline), a second Linux runner, or hand-rolled WSL scripts that rot. The pain is monetary and acute, the buyer is a platform team that adopts CLI tools without a beauty contest, and there is no GUI expectation to disappoint.

```powershell
# runner provisioning script — GitHub Actions, GitLab shell executor, Jenkins, Azure DevOps alike
skrog install --headless --engine-version 27.5.1
skrog status --json   # assert health, then every pipeline step just uses `docker`
```

What makes it first-class rather than incidental: unattended everything (no prompts, meaningful exit codes, `--json`), **version pinning as a contract** (no auto-updates unless asked — determinism is a direct anti-feature of Docker Desktop), **no desktop app required** — a headless supervisor rather than a tray application that pops update dialogs mid-pipeline, MSI/winget for fleet rollout, and pre-baked rootfs VHDX caching so ephemeral runners provision in seconds. GitLab's docker executor pointing at Skrog's pipe for Linux jobs on a Windows runner is a bonus scenario.

**The session requirement — PROVISIONAL, see the note in §03.** The measurement below came from a machine missing `NT VIRTUAL MACHINE\Virtual Machines` from `SeServiceLogonRight`, a documented cause of the same error code. Until it is re-tested on a clean VM, treat this section as the pessimistic case rather than established fact. If the re-test passes, the original claim is restored and this section reverts.

Spike B (#3) measured: **WSL2 cannot create its utility VM outside an interactive session.** `LocalSystem` is refused by name; a dedicated service account fails inside the Host Compute Service with `ERROR_LOGON_TYPE_NOT_GRANTED` even when granted Service, Batch and Interactive logon rights and made a local administrator. There is no privilege left to grant.

So a runner needs a logged-on session, which for an unattended machine means **auto-logon of a dedicated account**, with Skrog starting at logon. After that one-time setup it is genuinely unattended: no prompts, no human present, reboots recover on their own.

This is a WSL constraint rather than a Skrog one, and it binds every alternative equally — Docker Desktop, Rancher Desktop and Microsoft's own `wslc` all need a session. An earlier draft of this plan claimed session-0 operation as a differentiator over Docker Desktop; that was wrong, and the measurement is in #3. The wedge does not depend on it: the pain being solved is licensing cost, mid-pipeline auto-updates, and non-determinism, none of which auto-logon touches.

**The honest cost:** auto-logon stores the account password in LSA secrets, recoverable by an administrator. Organizations that forbid it by policy cannot run Skrog unattended at all — nor any WSL-based engine. That belongs in the docs, and in `doctor` as a named check rather than a mystery.

Two boundaries, stated honestly: GitHub/GitLab *hosted* Windows runners cannot work (no nested virtualization — a hypervisor policy, not a software gap), and Hyper-V guest VMs used as runners need nested virtualization enabled on their host. Notably, Microsoft's `wslc` doesn't threaten this niche: CI workloads are exactly the Docker-API-dependent tools it can't run.

---

## 07 · VPN & corporate environments

The user behind a corporate VPN is the plan's hardest ordinary case — it's why doctor is a milestone, not a footnote. The key fact: the VPN **cannot break the control path** (docker CLI ⇄ engine is a local pipe, no routing involved); what breaks is **egress from inside WSL2** — DNS hijacked by the VPN client, routes/MTU claimed by full tunnels, internal registries reachable only through the tunnel plus a proxy.

The strategy, in order: **platform fixes first** (mirrored networking + DNS tunneling + auto-proxy resolve most VPN cases with zero per-VPN hacks), **diagnosis second** (doctor fingerprints known VPN clients and names the specific fix instead of failing silently — turning tribal Stack Overflow knowledge into a command), **workarounds last** (static DNS, MTU clamps, an opt-in relay that routes WSL traffic through the Windows side so it rides the VPN like any Windows app). Some enterprise full-tunnel configurations will still win — the honest promise is diagnosis with actionable output, which already beats Docker Desktop's silent failure mode.

The corporate trust checklist rides along: signed binaries and MSI from the first public release, reproducible rootfs with SBOM and checksums (a security team can diff exactly what's in the VM), a published threat model for the pipe, an ADMX/registry policy layer for admins (v0.4), documented compatibility with Defender for Endpoint's WSL plugin so container workloads stay visible to EDR, Apache-2.0 for short legal reviews, and **no telemetry** — nothing phones home except explicit update checks.

---

## 08 · Repository layout

```
skrog/
├── cmd/skrog/             # single entry: CLI, service, and tray run modes
├── internal/
│   ├── provision/          # rootfs download/verify, wsl --import, daemon.json
│   ├── pipeproxy/          # named pipe server ⇄ engine socket, path translation   ← load-bearing
│   ├── engine/             # lifecycle, health checks, upgrade/rollback, API client
│   ├── wsl/                # wsl.exe wrappers, .wslconfig, vhdx ops (mockable interface)
│   ├── doctor/             # diagnostic checks, each check = one file + one test; VPN fingerprints
│   ├── svc/                # supervisor: start-at-logon, power events, restart
│   └── tray/               # systray UI (6 items max)
├── guest/
│   ├── rootfs/             # Alpine build scripts → tar + SBOM (runs in CI)
│   └── agent/              # vsock guest agent: relay, health, housekeeping (v0.2)
├── installer/              # winget/scoop/choco manifests, WiX MSI
└── .github/workflows/      # build, rootfs pipeline, release signing
```

The `wsl` package hides every `wsl.exe` invocation behind an interface so unit tests run anywhere; only the e2e suite needs real nested virtualization.

---

## 09 · Risks

| Risk | Severity | Mitigation |
|---|---|---|
| Docker trademark | launch-blocking | No "docker" or whale in name/logo/package IDs (hence Skrog); nominative references only; ship CLI binaries unmodified under Apache-2.0; engine binaries built from source in CI, not repackaged from Docker's distribution channels. Legal sanity check before the repo goes public. |
| Microsoft's wslc absorbs the niche | strategic | wslc (GA expected fall 2026) mimics the CLI and runs a real Moby engine, but exposes no Docker endpoint — dockerd listens on a guest unix socket with no `-H`, and nothing reaches it from Windows — so compose, Testcontainers and socket-mounting tools cannot use it, and the endpoint request (microsoft/WSL#40976) sits unanswered with no milestone. Note the gap is a missing listener, not a missing engine: Microsoft narrows the surface deliberately, because their registry allowlist and plugin hooks are enforced in `wslcsession.exe` *in front of* dockerd. Ship fast and own the "real Docker API" framing. The pre-thought pivot is now measured rather than hypothetical (#316): Skrog's glue layer (doctor, service lifecycle, fleet policy, path-translating pipe) can sit on top of a wslc session — reached today over vsock (#320), or over Microsoft's own endpoint if #40976 ships. |
| ~~Session-0 / no-login WSL~~ — **settled, and negative** | resolved | Spike B (#3) measured it: WSL2 will not create its utility VM outside an interactive session. LocalSystem is refused by name; a dedicated service account fails in HCS with `ERROR_LOGON_TYPE_NOT_GRANTED` holding Service + Batch + Interactive rights and local admin. No longer a risk but a documented constraint (§03, §06). Cost of finding out: one day, in week one, exactly as intended. |
| Auto-logon forbidden by policy | medium | Unattended runners need a logged-on session, and auto-logon stores a password in LSA secrets. Fleets that ban it cannot run Skrog unattended — nor any WSL-based engine, so no competitor wins those machines either. Mitigation: state it in the docs, name it in `doctor`, and never automate credential storage on the user's behalf. Theoretical escape (out of scope): run the engine in a plain Hyper-V VM instead of WSL. |
| WSL2 behavior drift | ongoing | Pin a minimum WSL version; doctor detects mismatches; abstract every `wsl.exe` call; test Insider builds before Windows feature updates land. |
| CI can't run WSL2 | medium | GitHub-hosted Windows runners lack nested virtualization. Keep unit tests host-independent; one self-hosted runner (or a paid larger runner) for the e2e suite. |
| SmartScreen / unsigned binaries | **raised: high** | The free route is gated on the thing it would help fix. SignPath's Foundation programme declined pending an established user base; winget builds reputation fastest but wants a signed installer; reputation only accrues once signed. Paying breaks the loop and needs nobody's approval (#77). container-desktop's Defender-blocked installer is the cautionary tale. |
| Corporate VPN / DNS breakage | chronic | The #1 WSL2 support topic everywhere — treated as a product surface, not an issue label: platform fixes, fingerprint database, doctor --fix, opt-in relay (section 07). |
| Pipe security | design-time | ACL the pipe to interactive user + admins; TCP only as opt-in mutual-TLS; publish a short threat model so security teams can approve it. |

---

## 10 · Success metrics

- **< 5 min** — fresh machine → first successful `docker run`
- **< 15 MB** — installed Windows-side footprint (vs ~500 MB incumbents)
- **< 30 MB** — idle RAM of all Windows-side processes
- **0 clicks** — recovery after reboot, sleep, or WSL crash
- **0 prompts** — unattended install and operation on a CI runner (a logged-on session is required; auto-logon covers it)

---

## 11 · First week

1. **Claim the name.** Skrog is chosen — reserve the GitHub org, winget/scoop/choco IDs, and a domain; quick USPTO/EUIPO sanity search. Then never revisit.
2. **Manual spike — prove the whole path by hand.** Import a hand-built Alpine+dockerd tar, relay the pipe with a throwaway script, run `docker ps` from PowerShell. Every architectural risk surfaces here, before any real code exists.
3. ~~**Session-0 spike.**~~ **Done (#3): negative.** WSL2 will not create its utility VM outside an interactive session, under any account or privilege set. The CI story now rests on auto-logon, and §06 says so plainly.
4. **Repo + CI skeleton.** Apache-2.0, Go module, lint/test workflow, and the rootfs build pipeline publishing a checksummed tar + SBOM as a release asset.
5. **Pipe proxy in Go.** `go-winio` server + per-connection relay, with tests covering half-close, hijacked streams (`exec -it`), path translation, and concurrent connections.
6. **Wrap it: `skrog install`.** Provisioner + context wiring, tested on a clean Windows 11 VM. That's v0.1 within reach.

---

*draft 7 · 2026-09-01 · plan owner: zcsizmadia · name settled: Skrog · wslc tripwire: microsoft/WSL#40976 · execution detail: ROADMAP.md · spikes A+B settled (#2 GO, #3 NO-GO) · next review: after v0.1 acceptance*
