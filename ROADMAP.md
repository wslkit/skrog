# Skrog · Execution Roadmap

Companion to [PLAN.md](PLAN.md) §05 — that file says *what* each milestone contains and why; this one says *in what order, gated on what*.

> ## Read the dates below as history, not as a schedule
>
> This file was written the week of 2026-09-07 assuming one developer at a weekend cadence. **The project ran roughly five months ahead of it.** Every original target and what actually happened:
>
> | milestone | targeted | shipped |
> |---|---|---|
> | v0.1 | Sun Oct 4, 2026 | **2026-09-01** |
> | v0.2 | Sun Nov 8, 2026 | **2026-09-02** |
> | v0.3 | staged, no date | **2026-09-08** |
> | v0.4 | Sun Jan 24, 2027 | **2026-09-12** |
> | v0.5 | Sun Feb 21, 2027 | **2026-09-15** |
> | v0.6 | not planned | in progress |
>
> So a reader arriving today would conclude v0.5 is four months away when it shipped in September, and **"v1.0 — spring 2027" is anchored to the same broken calendar** — it is not a forecast, it is arithmetic from a start date that no longer describes anything. No new dates are put here to replace them: the gates are the contract, and inventing a second set of numbers that will also be wrong helps nobody.
>
> **What shipped, and when, is [CHANGELOG.md](CHANGELOG.md) and the [releases](https://github.com/wslkit/skrog/releases).** Treat those as authoritative and this file as the reasoning behind the ordering.
>
> The checkboxes below are also unmaintained — 22 unchecked against 1 ticked, across six shipped releases. They record what was *planned*, not what was *done*. Do not read an empty box as "not shipped"; `skrog doctor`, `skrog migrate` and the supervisor are all unticked and all shipped. Some items shipped under different names (`skrog update` → `skrog upgrade`, `skrog expose --tcp` → `skrog serve` + `skrog remote`, `skrog stats` → `skrog status --stats`).

**Strategic clock:** Microsoft targets wslc GA for **fall 2026**. wslc ships a real Moby engine but no Docker endpoint to reach it (#317), so it is not a substitute, and its GA press cycle is a free marketing wave. **v0.1 must be public and installable before that wave** so every "wslc can't run compose/Testcontainers" thread has a link-able answer. That makes early October a real deadline, not an aspiration.

---

## Phase 0 — Foundations & spikes (week of Sep 7, one week, evenings + weekend)

Everything here either de-risks the architecture or claims ground that gets more expensive later. Nothing downstream starts until the two spike gates pass.

| # | Task | Output | Gate |
|---|---|---|---|
| 0.1 | Reserve the name: GitHub org, winget/scoop/choco IDs, domain, USPTO/EUIPO sanity search | reservations done | — |
| 0.2 | **Spike A — manual end-to-end**: hand-built Alpine+dockerd tar, `wsl --import`, throwaway pipe relay script, `docker ps` from PowerShell (download.docker.com static bundle OK here only) | written spike notes: latency, half-close behavior, gotchas | **GO/NO-GO: the path works at all** |
| 0.3 | **Spike B — session-0**: **PROVISIONAL NO-GO, re-test pending.** Ran on a machine missing `NT VIRTUAL MACHINE\Virtual Machines` from `SeServiceLogonRight` — the documented cause of the error measured. Re-run on a clean VM. Original finding: WSL2 will not create its utility VM outside an interactive session — LocalSystem refused by name, a dedicated account fails in HCS with `ERROR_LOGON_TYPE_NOT_GRANTED` even holding Service+Batch+Interactive rights and local admin | the constraint, measured | CI story now rests on auto-logon; PLAN §03/§06/§09 rewritten |
| 0.4 | Repo skeleton: Apache-2.0, Go module, lint/test workflow, layout from PLAN §08 | pushable repo (private) | — |
| 0.5 | Rootfs CI pipeline v0: Alpine + engine **built from moby/containerd/runc source**, tar + SHA256 + SBOM as release asset | reproducible rootfs artifact | blocks 1.2 |

**Gate outcomes — both settled in week one, as intended.** Spike A: **GO**, the architecture works end to end. Spike B: **NO-GO**, and exactly the outcome this gate existed to catch — "no-login CI runner" is demoted to "requires auto-logon," and PLAN §03/§06/§09 were rewritten to say so. The finding cost a day; discovering it during v0.2 would have cost a rewrite of the service architecture, and discovering it after launch would have cost credibility.

---

## v0.1 — Plumbing (3 weekends → target **Sun Oct 4, 2026**, public repo + tagged release)

Goal: CLI-only, fresh Windows 11 VM → `docker run hello-world` in <5 min. Shippable and useful on its own.

| Weekend | Work | Depends on |
|---|---|---|
| W1 (Sep 12–13) | Pipe proxy v1: `go-winio` server, per-connection `wsl.exe` stdio relay; unit tests for half-close, hijacked streams (`exec -it`), concurrent connections | Spike A notes |
| W2 (Sep 19–20) | Windows→WSL volume path translation in the proxy (`-v C:\src:/app`); provisioner: preflight (`wsl --status`, exact enable-and-reboot instructions), rootfs download+verify, `wsl --import`, `daemon.json` (log rotation, `host.docker.internal` with NAT/mirrored-aware host IP), `--headless`, `--engine-version`, `--data-dir` | 0.5 rootfs artifact |
| W3 (Sep 26–27) | Bundle docker CLI + compose + buildx + wincred per version manifest; `skrog` context creation with Desktop-coexistence pipe fallback; PATH-shadowing detection; `skrog version --json`; `skrog uninstall`; acceptance runs on clean VM | W1 + W2 |

**Exit criteria (all must pass on a fresh Win11 VM):**
- [ ] install → `docker run hello-world` < 5 min, user never touches WSL
- [ ] compose stack (multi-service, healthcheck `depends_on`, Windows-path binds, `logs -f`) `up --build` → `down` clean
- [ ] minimal Testcontainers suite (one DB container test) passes against the pipe
- [ ] `docker exec -it` interactive shell works (hijack correctness)
- [ ] uninstall leaves the system byte-identical (context restored, no stray files)
- [ ] coexists with an installed Docker Desktop (fallback pipe name, both contexts usable)

**Retires risks:** architecture viability, Desktop coexistence, path translation. **Publish:** README with honest scope, comparison table stub, "wslc exposes no Docker endpoint — Skrog serves the real one" positioning paragraph.

---

## v0.2 — Always-on (target **Sun Nov 8, 2026**; running ~5 weeks ahead)

Goal: survives everything Windows throws at it, within a logged-on session (the Spike B caveat). Re-planned 2026-09-01 after v0.1's code finished early: stages replace the weekend schedule, ordered so each hardens what the next depends on. Dates stay as ceilings, not pacing.

**S0 — Acceptance first (gates tagging v0.1, feeds everything after).**
The e2e suite (#11) runs on any Windows machine with WSL2 — the development machine qualifies; a runner VM only automates it in CI later. Suite: install from the published release → proxy → hello-world, bind mount read *through a container*, exec, `logs -f` streaming, a compose stack, testcontainers-go, process-count-returns-to-baseline (#35's regression), Docker-Desktop-still-works, uninstall-leaves-nothing. `exec -it` with a real TTY stays a documented manual check. Green suite ⇒ tag v0.1.0 and cut the first app release.

**S1 — Supervisor.** One long-lived `skrog supervise` process, started at logon (scheduled task; a session-0 service is not possible pending #3's re-test), owning what `skrog proxy` does today plus: engine health loop, crash and `wsl --shutdown` recovery with backoff, sleep/resume + power-event recovery, single-instance locking, Event Log + rotating file. `skrog start/stop/restart/status --json` become real commands. Hard constraint from the Docker Desktop incident: manage only processes it spawned and the distro it owns — never kill by image name, never `wsl --shutdown`.

**S2 — vsock guest agent.** No longer just latency work: it is the *complete* fix for #35, since owning both ends makes client disconnects explicit instead of inferred, and it removes the socat dependency entirely. Ships inside the rootfs (rootfs re-release), started by the supervisor, with the socat relay kept as fallback. Begin with a half-day mini-spike: AF_HYPERV dial from Windows into the WSL utility VM, since VM-GUID discovery is the uncertain part.

**S3 — Adoption levers.** On-demand start + `idle-timeout` via `skrog config` (answers the idle-RAM complaint); `wsl-integrate` (socket into the user's other distros via `/mnt/wsl`); `migrate --from-desktop` via `docker save/load` + volume tar streaming — slower than VHDX surgery but cannot corrupt the source, and Desktop stays untouched if interrupted.

**S4 — Tray (6 items, forever) and the auto-logon runner playbook** (documented, never automated — PLAN §06).

**In-flight from earlier milestones:** #3 Spike B re-test (needs the clean VM; settles whether a service mode can exist alongside logon mode) and #35 (bounded now, closed by S2).

**Exit criteria:**
- [ ] e2e suite green on a real Windows+WSL2 machine, including compose and testcontainers-go
- [ ] kill `wsl.exe` / `wsl --shutdown` / reboot / sleep-resume → next `docker ps` works, zero clicks
- [ ] on a machine with auto-logon configured, a reboot brings the engine back and a container job runs with **nobody physically present**
- [ ] pipe round-trip latency within 2× of Docker Desktop on `docker version` (vsock agent)
- [ ] relay process count returns to baseline after killing clients mid-request (#35 closed, not bounded)
- [ ] `migrate --from-desktop` round-trips images + volumes on a machine with real Desktop state
- [x] idle-timeout stops the VM; first `docker ps` after cold-starts it — **measured: 4.8–6.3 s** (#398), not the "~2 s" this line assumed. An idle stop is `wsl --terminate`, so the wake pays a full distro boot plus dockerd startup; there is no cheaper resume to reach for
- [ ] Docker Desktop fully functional after a Skrog install → exercise → uninstall cycle

**Retires risks:** idle-RAM complaint, unattended recovery, the #35 leak. **Note:** wslc GA likely lands during this window — have the comparison post ready, and note wslc inherits the same session requirement.

---

## v0.3 — Doctor, corporate networks & self-contained (staged S1–S4)

*(Re-baselined 2026-09-01: v0.2 landed ~3 months ahead of the original calendar, and its scope grew by agreement — the two "wildcards" (CLI bundling, devcontainer validation) and a customization layer joined the milestone. Weekend dates dropped in favor of stages; the owner re-dates as capacity allows.)*

Goal: turn the WSL2 quirk zoo into a diagnosable surface, make the engine work on corporate networks, and cut the last dependency on Docker Desktop's binaries. Doctor is sequenced after two releases so real v0.1/v0.2 findings seed the check list — and v0.2 delivered exactly that seed (credential-helper PATH, the `run -i` half-close class, supervisor/status disagreement, the session-0 rights question).

| Stage | Work | Issues | Depends on |
|---|---|---|---|
| S0 | Review remediation: an independent 4-agent Fable review of v0.2.0 (breadth + security + concurrency + protocol) gated v0.3; its P1s land before feature work — pipe DACL, stop-vs-idle flap, vsock mutual auth, health-probe rework, idle vs shared-socket traffic. P2/P3 findings (#84–#94) fold into the later stages. | #79, #80, #81, #82, #72 | v0.2.0 + review |
| S1 | `doctor` framework (one check = one file + one test; `--json`, `--report` markdown, `--fix`), checks seeded from the real v0.1/v0.2 failure list and the S0 review findings; fix the supervisor-mutex bugs its checks reference | #61, #71 | S0 |
| S2 | Corporate networks: CA-trust injection + proxy passthrough + registry mirrors; validated engine-config surface (`config set engine.<key>`, `dockerd --validate` before apply); VPN battery (consented platform fixes, fingerprint DB, MTU/DNS fallbacks) | #62, #68, #63 | S1 (doctor hosts the diagnoses) |
| S3 | Housekeeping: sparse VHDX, `compact`, memory reclaim, `data-dir` relocation; `engine upgrade`/`rollback` staged swap; `stats` vmmem attribution; SSH-agent bridging | #64, #65 | v0.2 supervisor |
| S4 | Self-contained & declarative: bundle docker CLI + compose + buildx + credential helper (pinned fetch at install); `skrog install --config skrog.yaml`; lifecycle hooks; Dev Containers + Visual Studio container-tools validation (shim if needed) | #66, #69, #70, #67 | S2 (config surface) |

**Exit criteria:**
- [ ] doctor correctly diagnoses the **top 5 failure classes from actual v0.1/v0.2 issues** (measured against the tracker, not hypotheticals)
- [ ] a VPN-equipped test machine reaches a private registry through the tunnel after `doctor --fix` (CA + proxy + DNS all diagnosed, not guessed)
- [ ] `engine upgrade` → deliberate break → `rollback` restores a working engine with data intact
- [ ] a clean machine **without Docker Desktop** installs Skrog and `docker run hello-world` works using only bundled binaries
- [ ] `skrog install --config skrog.yaml` on a fresh machine converges to the same state as the equivalent flags, and re-running is a no-op
- [x] `docker build --ssh default` works — **no bridge needed** (#391). buildx dials
  `\\.\pipe\openssh-ssh-agent` itself and the forwarding channel rides the ordinary
  API connection, so it crosses the bridge unchanged. Verified end to end; the only
  gap was host-side (Windows ships `ssh-agent` disabled), now a `doctor` warning.

**Deliberately staged to v0.5, not lost:** network-aware profiles (#73), engine lockfile (#74), verified air-gap install (#75) — they compose the S2/S4 plumbing rather than precede it.

**Needs procured:** a test machine (or VM) with a real corporate VPN client — line this up before S2, not during it.

---

## v0.4 — Ship it (2 weekends → target **Sun Jan 24, 2027**)

Goal: the difference between a repo and a tool people install at work. Holiday gap absorbed here deliberately.

| Weekend | Work | Depends on |
|---|---|---|
| W10 (Jan 9–10) | Code signing (Azure Trusted Signing or SignPath OSS — apply for SignPath **during v0.3**, approval takes weeks); winget/scoop/choco manifests; WiX MSI; ARM64 builds; `skrog update --check` | v0.3 tagged |
| W11 (Jan 23–24) | `expose --tcp` mTLS; LAN port mirroring (opt-in); ADMX/registry policy layer; Defender-for-Endpoint WSL-plugin validation + doc; docs site, demo GIF, comparison table, CONTRIBUTING, issue templates with `doctor --report` pre-wired | W10 |

**Exit criteria:**
- [ ] `winget install skrog` on a clean machine — no SmartScreen wall
- [ ] MSI deploys silently via Intune-style unattended flags
- [ ] a security reviewer can walk checksum → SBOM → source commit for every shipped byte

**Lead-time items to start early:** winget package review queue (can take 1-2 weeks); SmartScreen reputation begins accruing only after signing, so sign *pre-release* builds as soon as a certificate exists.

**Code signing status (Sep 2026):** the SignPath Foundation application was declined for now -- the free programme wants an established user base -- with an invitation to reapply as visibility grows. Paying for a certificate is the other route and needs nobody's approval. That makes signing a decision rather than a scheduled item, so it has been lifted out of the weekend plan above and lives in [#77](https://github.com/wslkit/skrog/issues/77).

What actually moves with it is **the MSI and SmartScreen reputation** — an installer that asks for elevation unsigned is worse than a zip, and reputation is earned by signed downloads.

**winget and scoop do not, and that sentence used to say they did.** It was wrong, and the cost was real: it held the scoop bucket empty for no reason. Both ship unsigned binaries routinely — winget's `portable` type unpacks per-user with no elevation and no Authenticode, and ripgrep and fzf are distributed exactly that way. [scoop-skrog](https://github.com/wslkit/scoop-skrog) serves 0.8.0; winget is [#412](https://github.com/wslkit/skrog/issues/412). `RELEASING.md` corrected this for winget some time ago; this line is the same correction arriving where it should have arrived at the same moment.

---

## v0.5 — Power tools (2 weekends → target **Sun Feb 21, 2027**)

Kept out of v0.4 so the ship-it release stays tight.

| Weekend | Work |
|---|---|
| W12 (Feb 6–7) | `snapshot create/restore` (VHDX copy while stopped); `create --name` side-by-side engines, each its own distro + context |
| W13 (Feb 20–21) | ~~Opt-in prune policy~~ **shipped (#393)** — `prune.every` on the supervisor's own timer, age-guarded, never volumes; the tray disk-quota alert is still open and must not become a seventh tray item. ~~`cache enable` pull-through mirror~~ **shipped (#385)** — pinned `registry:2` on the engine, loopback only, and excluded from the busy probe so it does not suppress idle stops or the prune above; ~~PowerShell + bash completions~~ **shipped (#392)** — generated from `help --json` and CI-checked, like the command reference |

**Exit criteria:** snapshot → break everything → restore is bit-perfect; two engines run simultaneously with independent contexts.

---

## v1.0 — Stable (no date; see the note at the top)

Reliability guarantees, not features. **Acceptance:** two consecutive Windows feature updates absorbed with zero breaking issues filed.

Scored honestly as of 0.6.0, because "spring 2027" was never a forecast and the criteria are the only thing that means anything:

| criterion | state |
|---|---|
| e2e runner with nested virtualization | **Not done.** The suite exists (45 stages) and is wired into a workflow as of 0.6.0, but manual/nightly, not a gate. Self-hosted turned out unnecessary — hosted `windows-latest` has nested virtualization |
| Windows compatibility matrix (Win11 23H2/24H2+/Insider; Win10 22H2-ESU) | **Does not exist.** Every measured note in the repo is from Windows 10 22H2. There is no Windows 11 test evidence at all, which is worth knowing given the README calls it the primary target |
| semver + upgrade guarantees | **Partial.** [RELEASING.md](RELEASING.md#semver-while-we-are-pre-10) now says what each bump promises, and [docs/cli-json.md](docs/cli-json.md) is a real contract for the JSON and exit codes. Nothing covers the CLI flags, config keys or on-disk state |
| published pipe threat model | **~70%.** [docs/security.md](docs/security.md) is substantive and states its limitations, but it is not framed as a threat model — no asset or adversary enumeration |

Three of four are open, which is the answer to "is 1.0 close?": no. Dropping the pre-release flag at 0.6.0 is a much smaller and cheaper claim — "this works, use it, the surface can still move" — and is deliberately not the same promise.

---

## Decision gates & tripwires (standing)

| Trigger | Watch | Response |
|---|---|---|
| Spike A fails | week 1 | stop, rethink architecture |
| ~~Spike B fails all patterns~~ | **fired, week 1** | done: CI headline demoted, PLAN §03/§06/§09 rewritten before v0.1 published |
| WSL gains service-context support | continuous | would restore true sessionless operation and remove the auto-logon requirement for every WSL-based engine at once — revisit PLAN §06 if it ships |
| wslc GA announcement | fall 2026 (during v0.2) | publish comparison post same week; no roadmap change |
| ~~**microsoft/WSL#40976** gets a maintainer reply, milestone, or shipped endpoint~~ | **answered, Sep 2026** | Skrog serves one engine: Docker Engine in a WSL2 distro it owns (#451). The alternative backend that sat on Microsoft's session is removed. A first-party endpoint would still be interesting, but it is no longer a pivot Skrog is holding a position for — the Docker API is the product, and serving a different engine underneath it was the one thing that put that promise at risk |
| virtiofs reaches standard WSL distros | continuous | adopt immediately — attacks slow-9P bind mounts for free |
| WSL platform memory-reclaim ships broadly | continuous | shrink `compact` to a thin wrapper |
| Windows Insider feature-update flights | before each Windows FU | run e2e suite on Insider before the update GAs |

## Impact backlog — candidates, not commitments

Ideas that could raise the project's ceiling, held here until a milestone earns them. Each names the audience it unlocks; none may violate the scope rules in PLAN §01.

| Idea | Audience unlocked | Earliest slot |
|---|---|---|
| **`skrog enable-gpu`** — nvidia-container-toolkit in the rootfs, `docker run --gpus all` (WSL2 already passes CUDA through) | local-AI developers (Ollama, vLLM, CUDA builds) — the largest new Docker-on-Windows audience of 2025–26, and Desktop parity Skrog otherwise lacks | v0.5 |
| **`skrog install --config skrog.yaml`** — declarative fleet config (engine version, mirrors, proxy, data-dir) checked into the runner-provisioning repo | platform teams; makes the CI story infrastructure-as-code instead of flag soup | v0.3 |
| **`setup-skrog` GitHub Action + `skrog bake`** — prebaked ready-to-import VHDX for ephemeral runners (seconds, not minutes) | self-hosted CI at fleet scale; formalizes PLAN §06's caching aside into a product surface | v0.4 |
| **Validated Dev Containers + Visual Studio Container Tools compatibility** — test, document, and fix the pipe against VS Code devcontainers and full Visual Studio's Docker tooling (which historically probes for Desktop specifically) | the exact .NET enterprise audience that left Desktop over licensing; a compat shim here may be the single highest-leverage enterprise unlock | validate in v0.1 acceptance, shim if needed in v0.3 |
| **One-line bootstrap** — `irm https://skrog.dev/install.ps1 \| iex` (signed, checksum-verified) | everyone, pre-winget; the README's first command | v0.1 |
| **Publish the doctor VPN knowledge base as docs pages** — every fingerprinted failure gets a public URL | SEO: "WSL2 VPN DNS not working" searchers become users; turns support load into acquisition | v0.4 docs site |
| ~~**`skrog migrate --from-rancher / --from-podman`**~~ — **shipped (#387)**: both were a source-addressing question, not new transfer code. Rancher needs its dockerd backend; Podman is read over its Docker-compatible API, so nothing shells out to `podman` | the second- and third-place switcher pools | v0.5 |
| ~~**Opt-in local health metrics**~~ — **shipped (#390)**: `skrog status --prometheus` emits node_exporter textfile format. Textfile only, no listener — a resident endpoint would be a new network surface for no gain ([docs/monitoring.md](docs/monitoring.md)) | fleet operators who need runner health in their dashboards | v0.5 |
| **Sustainability** — GitHub Sponsors from day one; later a paid priority-support tier for fleets (the product stays 100 % free) | keeps the maintainer maintaining; enterprises *want* someone to pay | v0.4 |

## Working agreements

- **Every milestone tags a release** — even v0.1 — with the locked version manifest. No "install from main."
- **Determinism is the brand:** nothing auto-updates, nothing fetches "latest," ever. Break this once in CI and the sharpest wedge market is gone.
- **Issue tracker feeds doctor:** every v0.1/v0.2 support issue gets a `doctor-candidate` label; W7 starts by triaging that list.
- **Scope tripwire:** any proposed tray item beyond seven, or any container-management feature, is answered with the Portainer one-liner.
  - *Crossed once, recorded here rather than quietly:* "Check for updates" (#191) was added as a seventh item and the tripwire was never invoked. It stays — an update check is the one thing a tray is genuinely better at than a CLI, because it needs no one to remember to ask — but the number is now seven and the next item has to argue against this line, not against a count the product already exceeded.

*owner: zcsizmadia · created 2026-08-31 · re-baseline dates if any milestone slips by more than two weekends*
