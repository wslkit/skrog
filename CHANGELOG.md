# Changelog

Notable changes per release. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning is
[semver](https://semver.org/), with what each bump promises while we are
pre-1.0 written down in [RELEASING.md](RELEASING.md#semver-while-we-are-pre-10).

This file starts at 0.6.0. Earlier releases have hand-written notes on their
[GitHub release pages](https://github.com/wslkit/skrog/releases) and are not
reconstructed here — inventing a tidy history after the fact would be less
useful than saying where the real one is.

## [0.7.0] — 2026-09-21

### Added

- **`skrog doctor` says when the engine is still on 9p**
  ([#327](https://github.com/wslkit/skrog/issues/327)). `wsl.virtiofs` has
  existed as a setting, and `docs/vm-sizing.md` has carried the measurements —
  reads **4.0×**, `ls -l` of 1000 files **3.1×** — since the evaluation that
  added it. Nothing ever told anyone. The key was there for people who already
  knew to look for it.

  It warns rather than merely noting, unlike the multi-arch check next door,
  and the difference is deliberate: every `docker run -v ${PWD}:/app` goes
  through this mount, so it is the common path rather than a capability you
  opt into — and the warning is permanently silenceable by taking the action,
  so it is not the kind that trains people to skim.

  The verdict comes from `/proc/mounts` in the running engine, not from
  `~/.wslconfig`, because those two disagree exactly when someone would ask:
  WSL below 2.9 ignores the key silently, and it takes effect only after
  `wsl --shutdown`. Both look like success in the config file. Skipped
  entirely when the engine is down — doctor never boots a distro to answer
  (#82).

- **Run containers built for another CPU architecture, if you ask**
  ([#462](https://github.com/wslkit/skrog/issues/462)).

  ```
  skrog config set emulation.platforms linux/amd64
  skrog restart
  ```

  This is for `docker run --platform`. On Windows on ARM most of Docker Hub
  is amd64-only: the pull succeeds and the container dies with `exec format
  error`, which is an engine that can fetch an image and not start it.

  Cross-architecture **builds** already worked and still need nothing — a
  `docker-container` buildx builder bundles its own emulators.
  `docs/docker-cli.md` has had that recipe since #384 and now covers both
  cases side by side, because they look like one problem and are not.

  **Off by default, and that is the substance of it.** `binfmt_misc` belongs
  to the kernel, and on WSL2 one kernel is shared by every distro in the
  utility VM — so a handler Skrog registers changes how your Ubuntu executes
  foreign binaries too, and replaces any that `tonistiigi/binfmt` or a
  distro's `qemu-user-static` had put there. `docs/docker-cli.md` argued from
  exactly that to "Skrog does not do this unasked", on the same consent
  grounds as `~/.wslconfig`; this key is the asking, and that section is
  amended rather than contradicted.

  Registered on every engine start, because `wsl --shutdown` wipes the table,
  and **removed again on `skrog stop` and on uninstall** — unconditionally, so
  that turning the setting off and stopping gives you your kernel back. When
  the key is empty, which is the default, the start path runs no extra command
  at all.

  The emulators ship in the rootfs (`qemu-aarch64` at 6.3 MB in the amd64
  image, `qemu-x86_64` at 3.6 MB in the arm64 one), so nothing is downloaded,
  and CI proves both directions on native runners of each architecture — the
  one thing the Windows e2e suite cannot test. `linux/amd64` and `linux/arm64`
  only.

  Because the emulator is *in the image*, this needs engine **29.8.1-3 or
  newer** — the revision that first carries it. On an older image the start
  path says `missing interpreter` and carries on without emulation, rather
  than registering a handler that points at nothing; `skrog engine upgrade`
  is the fix. A fresh `skrog install` is already on a new enough image.

  Worth knowing: it is slow, and the thing that changes is not only what you
  asked for — an amd64-only image that fails fast today will start succeeding
  *slowly* instead, with nothing announcing it. `skrog doctor` reports which
  handlers are live.

- **Windows on ARM: a `docker` CLI, built here because upstream ships none**
  ([#450](https://github.com/wslkit/skrog/issues/450)). `skrog cli install` on
  arm64 laid down compose, buildx and the credential helper — all of which
  have real arm64 builds — and skipped the one command anybody types.
  `download.docker.com`'s static tree has exactly one directory, `x86_64/`,
  and `docker/cli` attaches no release assets. It now installs all four, and
  **the arm64 toolchain is complete**: engine, CLI, plugins and helper.

  So Skrog builds it: `docker/cli` at a commit-pinned tag, in the same pinned
  Go toolchain as the engine, with SLSA provenance and a cosign-signed
  checksum, published as a `dockercli-v*` release of this repository. The
  build asserts the PE machine type is really `0xaa64` before anything is
  attached — a cross-compile that quietly produced an x86-64 binary would
  pass every other check and fail only on a user's ARM machine.

  **amd64 still comes from Docker**, deliberately. Those bytes are verifiable
  by anyone against the pinned digest with no reference to Skrog, and building
  them too would put every user behind our build rather than only the arm64
  users who have no alternative. It is not about code signing: Docker's
  published Windows CLI is not Authenticode-signed either. `docs/docker-cli.md`
  has the table and the reasoning, and the asymmetry ends when upstream ships
  an arm64 build. This amends the CLI-repackaging line in PLAN §09.

  The arm64 binary reports its version and upstream commit, and deliberately
  does **not** claim `Docker Engine - Community` — that is Docker's build
  string for Docker's builds.

  **The build is reproducible**, which matters more than the attestation: it
  has no timestamp and no build host in it, so the same commit produces the
  same bytes anywhere. Three independent builds — one local, two in CI — all
  came out at `910087c0d9a8…`, 28,319,232 bytes. Run
  `third_party/docker-cli/build.sh` and compare.

  One internal change came out of it: `zipEntry` moved from the component to
  the **asset** in `internal/dockercli/manifest.json`. Docker publishes the
  amd64 CLI as a zip and the arm64 binary is a bare `.exe` — one component,
  two shapes — and while that flag was per-component, staging arm64 would
  have tried to extract a zip entry from a Windows executable.

- **Windows on ARM: `skrog install` works**
  ([#388](https://github.com/wslkit/skrog/issues/388)). `skrog.exe` has
  shipped an arm64 build for months while the engine was amd64-only, so
  `install` on a Snapdragon or Surface machine downloaded a rootfs whose every
  binary was the wrong ISA — the SHA-256 passed, `wsl --import` succeeded, and
  then dockerd could not exec. As of `rootfs-v29.8.1-2` there is an arm64
  engine, and `skrog install` selects by host architecture with no flag.

  **Not verified on real hardware.** The arm64 engine is compiled natively
  from the same pinned sources as amd64 and CI boots it, but that is in a
  container on arm64 Linux — it has never run inside a WSL2 utility VM on
  Windows on ARM, because hosted arm64 Windows runners expose no nested
  virtualization and WSL2 cannot start there at all. `docs/install.md` says
  so, and [#458](https://github.com/wslkit/skrog/issues/458) collects the
  first report. The docker **CLI** on arm64 is still missing upstream
  ([#450](https://github.com/wslkit/skrog/issues/450)).

  The two halves, for anyone reading the diff:

  The build: `rootfs.yml` runs a matrix over native amd64 and arm64 runners,
  compiling the whole engine from upstream source on each and naming the
  result `skrog-rootfs-<version>-<rev>-<arch>.tar.gz`. Not QEMU — an emulated
  toolchain is a difference between what CI exercises and what users run.
  CI now boots the arm64 engine and diffs it against Docker's aarch64
  reference bundle on every change to `guest/rootfs/**`.

  The selection: `internal/release/manifest.json` is **schema 2**, where
  `engines[].rootfs` is a map keyed by GOARCH instead of a single object.
  `skrog install`, `engine upgrade`, `lock`, `bundle` and declarative install
  all pick the entry for `runtime.GOARCH`. `skrog engine list` reports what
  *this* machine can install, so an engine with no build for the host is
  marked accordingly rather than offered.

  A missing architecture and an unreleased one are now different errors,
  because the user's next move differs: nothing they wait for fixes the first.

  **A lock file and an air-gap bundle pin one architecture** — they always
  held one URL and one digest, and that is the point of them. `skrog lock` and
  `skrog bundle` now record the host's, and refuse rather than emit something
  unusable when there is no build for it. An arm64 bundle is built on an arm64
  machine, which is the rule air-gap transfer follows anyway.

### Removed

- **The WSL container session backend is gone**
  ([#451](https://github.com/wslkit/skrog/issues/451)). Skrog serves one
  engine: Docker Engine in a WSL2 distro it owns and can pin.

  `skrog install --engine wslc`, `skrog proxy --engine wslc`, the
  `wslc.ignore-plugins` setting, the `skrog-wslc` docker context, the `--agent`
  flags on `install`, `proxy` and `supervise`, the session doctor check and the
  `session` field in `skrog status --json` are all removed, along with
  `docs/wsl-containers.md`, `docs/wslc-backend.md` and `docs/wslc-deep-dive.md`.
  `skrog-agent` no longer ships beside `skrog.exe`; it lives in the rootfs,
  which is the only place it is used.

  **Why.** That backend served a *different engine* — Microsoft's, inside a
  session — with its own API version, its own command surface and its own
  limits. Skrog's whole promise is that the real Docker API answers on
  `\\.\pipe\docker_engine` and unmodified tooling works. A second backend that
  could not keep that promise put the promise itself in question, and the
  maintenance cost was paid on every feature.

  **If you installed with `--engine wslc`**, Skrog will tell you so and stop
  rather than misbehave. Move over with `skrog uninstall` then `skrog install`.
  Containers and images in the old session are **not** carried over: they
  belong to the other engine, so save anything you need with `wslc` first.

  `backend` stays in `skrog status --json` and `skrog version --json`, always
  `"distro"`. It is part of a pinned contract
  ([docs/cli-json.md](docs/cli-json.md)) and a reader that switches on it must
  keep parsing.

### Changed

- **`allow-registries` now applies to `docker plugin install`**
  ([#420](https://github.com/wslkit/skrog/issues/420)). A plugin pull names its
  registry, so it is judged exactly like `docker pull` — including
  `require-digest` if you have set it. Previously the plugin endpoints were
  grouped with swarm as "unattributable" and inherited the permissive **build**
  default, so a plain `allow-registries` let a plugin from any registry
  through. A plugin gets host device and mount access where an image gets a
  container, which made that a worse hole than the build one the default was
  chosen to tolerate.

  **This can refuse something that worked before:** installing a plugin from a
  registry your allowlist does not list now returns 403. A plugin from a listed
  registry is unaffected. Swarm and `/swarm/init` are unchanged — they still
  need `deny-unattributable-builds`, because a TaskSpec genuinely cannot be
  attributed.

### Fixed

- **Doctor remedies no longer collapse into one paragraph.** `wrapIndent` ran
  `strings.Fields` over the whole remedy, which splits on newlines too, so a
  remedy written as a sequence of commands rendered as prose:

  ```
  fix: needs WSL 2.9 or newer: skrog config set wsl.virtiofs true skrog
       wsl-config apply wsl --shutdown ~/.wslconfig is shared by every...
  ```

  Worse than ugly — it looks copy-pasteable and is not. The same flattening
  ran the numbered steps of the injected-modules remedy together. Author line
  breaks are preserved now, and each line wraps on its own keeping its
  indentation. Found while adding the virtiofs check, whose fix is three
  commands and was unusable as rendered.

The concurrency findings from the pre-0.6.0 review, which were filed but not
fixed in time for it, plus the first of the policy gaps.

- **A stalled upload could wedge the bridge and permanently disable
  idle-stop** ([#435](https://github.com/wslkit/skrog/issues/435)). The
  teardown for an abandoned request body closed the engine and then waited
  forever — which frees a writer blocked *writing*, and does nothing for one
  blocked *reading* a client that went quiet. `ActiveConns` then never dropped,
  so `maybeIdleStop` vetoed for the life of the process, silently, and shutdown
  hung holding the single-instance lock. A sleeping laptop mid-`docker build`
  was enough.
- **A 101 upgrade with a still-streaming body shared one `bufio.Reader`
  between two goroutines** ([#436](https://github.com/wslkit/skrog/issues/436))
  — the heap-corruption class of #166, which this package had already fixed
  once. The upgrade is now refused in that state: a failed `docker exec` is
  visible and retryable, a corrupted heap is neither.
- **A wedged `wslservice` could hang every docker command**
  ([#437](https://github.com/wslkit/skrog/issues/437)). The health probe ran
  under the reconciler's mutex with a context that never fires. Three parts:
  the COM call is bounded, `Engine.Running` gained an error so a *failed* probe
  is no longer read as a *stopped engine* (which used to provoke starting an
  engine that was already running), and the probe no longer holds the lock.
  A panicking COM call is also recovered and reported rather than taking the
  supervisor down.
- **`docker volume create` could reach a path `allow-bind-sources` forbids**
  ([#419](https://github.com/wslkit/skrog/issues/419)). `POST /volumes/create`
  was judged by nothing, and a `local`-driver volume can name a host path
  through `-o type=none -o o=bind -o device=...`. The container that mounted it
  afterwards carried only the volume's *name*, so nothing downstream caught it
  either. Now judged, including `device=/`. See
  [docs/policy.md](docs/policy.md) for what this means for third-party volume
  drivers.

Then the chain the acceptance suite found once it actually ran (#11). Each of
these was uncovered by the stage after the one before it was fixed, which is
the suite doing its job rather than a run of bad luck: #429 had been killing
the run two stages in since 0.6.0, so nothing behind it had ever executed in
CI at all.

- **`skrog restart --supervisor` keeps the pipe it was serving**
  ([#429](https://github.com/wslkit/skrog/issues/429)). 0.6.0 shipped with
  this in Known issues. The replacement supervisor re-ran pipe selection from
  scratch, so a `skrog supervise --pipe <custom>` setup came back on the
  default, `DOCKER_HOST` stopped working, and the error named a missing
  *file* rather than a moved pipe. The watchdog path had the same gap — skrogw
  relaunches through the same choke point — so a crash lost the pipe the same
  way.

  Two attempts failed before this one, both reading the pipe out of
  `endpoint.json`, and the third is the first that explains why neither could
  have worked. That record is cleared on a clean exit, and it must be (#288):
  a record outliving its process makes `skrog status` name a pipe nothing is
  listening on. **A record that is correctly deleted cannot also be a handoff
  channel.** So the two facts were separated by lifetime — `endpoint.json` is
  where the engine is answering *right now*, `served-pipe` is what the
  supervisor was *asked* to serve and survives the process that served it.

  Only a genuinely custom pipe is carried over. The default is not pinned,
  because normal selection takes it again when it is free and falls back
  correctly when Docker Desktop has it; the fallback is not pinned either, or
  a machine that stopped running Desktop would never take the default back.
  Ordinary installs see no change at all.

- **A dead `dockerd` reads as DOWN, not as "cannot tell"**
  ([#468](https://github.com/wslkit/skrog/issues/468)). `enginePing` pipes
  into `socat`, and `socat` exits non-zero when nothing is listening — an exit
  status that reached `engineRunning` as an *error* rather than as the answer
  "no". Since #437 the supervisor skips its tick entirely on a probe error,
  which is correct reasoning ("cannot tell" must not start an engine that is
  probably already running) applied to a value that was lying. **The
  supervisor stopped repairing a dead engine whenever the distro stayed up**,
  which is its whole job, and `enginePing` had claimed to handle exactly this
  since #82: a stale socket left by a crashed dockerd must read as down.

  Any dockerd that dies while its distro survives is this shape.
  `skrog reset --to <snapshot>` is simply the routine path that produces it
  reliably, and it sat behind an e2e stage that had never once run.

  A failed restore also says what it saw now, per case, because each wants a
  different next move: the probe failing, with the underlying error; the
  engine arriving just after the wait expired, which is a timeout too short
  for that machine rather than a broken restore; or genuinely down, with or
  without a supervisor — the last being the entire explanation, since on that
  branch nothing was ever going to start it. Every case names the distro. The
  old message named nothing, and the bug report written from it was three
  hypotheses and no evidence.

- **`skrog uninstall` no longer disowns a docker context its own supervisor
  set** ([#471](https://github.com/wslkit/skrog/issues/471)). `install`
  records the context it wired — normally `docker_engine`. A supervisor
  started with `--pipe <custom>` then re-points that same shared context at
  its own endpoint, and nothing writes that back to the manifest. Uninstall
  compared the live endpoint against the install-time value, concluded another
  install owned it, and left a `skrog` context pointing at a pipe it was about
  to delete — so every later `docker --context skrog` failed, on a machine
  that had just uninstalled Skrog. The #217 protection against reaching too
  far is right and stays; it was simply also not reaching far enough. The
  `served-pipe` record added for #429 turns out to be the missing fact here
  too.

- **`skrog uninstall` stops the supervisor**
  ([#472](https://github.com/wslkit/skrog/issues/472)). It removed the distro,
  the data directory, the manifest, the autostart entry and the docker
  context — and left the always-on process that serves the pipe running.
  Nothing was stopping it, and nothing could have: on a real install the
  supervisor is detached. `skrog start` spawns it and releases it, autostart
  launches it at logon, skrogw relaunches it after a crash, so uninstall was
  never its parent. The only uninstall that ever ran beside a live supervisor
  and still looked clean was the acceptance suite's, which kills its own child
  by handle first.

  What survived served a pipe into a distro that had just been unregistered,
  could re-point the shared context the step above had just unwired, rewrote
  endpoint records into the state directory being emptied, and held
  `skrog.exe` open — so on Windows the directory Skrog was installed into
  could not be deleted, which is what a package-manager uninstall does next.
  "Removed. Nothing else on the system was modified." was printed over all of
  it. A supervisor that will not exit is now a warning, not an aborted
  uninstall.

- **Autostart works on a profile with no `Run` key**
  ([#444](https://github.com/wslkit/skrog/issues/444)).
  `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` is created by Windows
  on demand, so a profile that has never registered a logon entry does not
  have one — the normal state of a fresh hosted runner, and of a new or
  freshly imaged user profile. All three entry points opened it expecting it
  to exist: `enable` could not register autostart at all, and `status` and
  `disable` returned errors, which made `skrog status` and `skrog doctor` fail
  outright. "The system cannot find the file specified" then read as a missing
  file and sent people looking for `skrogw.exe`. `enable` creates the key now,
  and an absent key reads as "not registered" rather than as a failure.

## [0.6.0] — 2026-09-18

**The first release not flagged as a pre-release.** Every earlier tag,
including the plain `vX.Y.Z` ones, was marked pre-release because
`RELEASING.md` only ever described that case. Dropping the flag is a claim
that the front page is true, so a large part of this release is making that
so — see **Fixed → Honesty** below, which is not a euphemism for "docs".

### Added

- **`skrog cache enable`** — a pull-through registry cache on the engine, so
  repeated pulls of the same image come off the local disk (#385).
- **Scheduled pruning** — `skrog config set prune.every 24h` lets the
  supervisor reclaim disk unattended. Off by default, never volumes, always an
  age guard (#393).
- **`skrog status --prometheus`** — the status reading as Prometheus text for
  node_exporter's textfile collector. No listener, no telemetry (#390).
- **Shell completions** for PowerShell and bash, generated from the binary and
  drift-checked in CI (#392).
- **`skrog migrate --from-rancher` / `--from-podman`** — Rancher Desktop and
  Podman join Docker Desktop as migration sources (#387).
- **Machine-wide policy layer** at `%ProgramData%\skrog\policy.yaml`, which
  the user layer may only tighten (#386). Read its limits in
  [docs/policy.md](docs/policy.md) before deploying it — see Fixed.
- **`deny-unattributable-builds`** — opt-in refusal of builds while a registry
  allowlist is in force (#376).
- **COM fast path for `wslservice`** — the distro list and terminate no longer
  spawn `wsl.exe`, which is ~85× faster on the supervisor's health tick
  (#356, #379, #380).
- **arm64 CI** — build and test on `windows-11-arm` (#389).
- **CodeQL and govulncheck** in CI, and `govulncheck` in `scripts/lint.ps1`
  (#414, #415).
- **`SECURITY.md`** and GitHub private vulnerability reporting.
- **`CHANGELOG.md`**, this file.
- `skrog doctor` gained two checks: SSH agent backing `docker build --ssh`
  (#391) and cross-architecture build capability (#384).

### Changed

Behaviour changes. **A call that used to succeed can now be refused** — which
is what a minor bump is for at 0.x, but read these before upgrading a fleet.

- **Policy judges `pull` and `push`, not only `create`** (#375, #377). An
  `allow-registries` rule that previously only affected `docker run` now also
  refuses `docker pull` and `docker push`.
- **`skrog upgrade --apply` refuses inside a package-manager directory**
  (#378) — if scoop or winget owns the binary, the package manager has to do
  the upgrade, and skrog now says so instead of fighting it.
- **The wslc backend refuses to serve when WSL plugins are registered**
  (#407). A working install can become a refusal; the message names the
  plugin.
- **Image references are resolved with Docker's own parser** (#374), so
  `ubuntu`, `library/ubuntu` and `docker.io/library/ubuntu` are one thing to
  the allowlist. Rules that relied on the old string matching may match
  differently.
- **`skrog start` is noticed immediately** rather than at the next health tick
  (#398).
- **New exit code `4`, "unsupported platform"** — see Fixed/arm64. Additive;
  existing codes are unchanged. Documented in
  [docs/cli-json.md](docs/cli-json.md).

### Fixed

- **arm64 installs now refuse instead of failing obscurely** (#388). skrog
  ships an arm64 CLI through three channels while the engine rootfs is
  amd64-only, and nothing checked: the download succeeded, the SHA-256 pin
  passed, `wsl --import` succeeded, and then dockerd could not exec. The
  failure never mentioned architecture. `skrog install` and `skrog engine
  upgrade` now exit `4` with an explanation; `--rootfs-url` stays open for
  anyone who built their own.
- **`deny-unattributable-builds` was a complete no-op.** `Watcher.DenyBuild`
  returned a hardcoded allow, and `Watcher` — not `Rules` — is what the bridge
  installs as its gate. The rule shipped, was documented, was reported active
  by `policy show`, and did nothing on any backend (#416).
- **The machine-wide policy layer failed open.** A machine `policy.yaml` that
  had never parsed left the layer empty rather than refusing, so one typo in
  an Intune deployment meant every machine that received it ran unenforced —
  while `skrog policy show` reported the file as broken (#416).
- **The automatic prune's age guard was applied to the log line, not the
  prune.** `guard()` appeared in three places, all `slog` calls; the value
  reaching `docker image prune -a` was the raw field. A policy built without
  an explicit window would have logged "keepSince: 168h" and swept every
  unused image.
- **`internal/wsl` stopped building for non-Windows** (#408). CI builds only
  Windows, so nothing caught it.
- **Seven CodeQL alerts** (#417), of which one was a real bug: `humanBytes`
  narrowed the engine's `uint64` sizes to `int64`, rendering a large byte
  count as `-1 B`.
- The tray's **"Run doctor"** item works. It shipped disabled and labelled
  "Run doctor (v0.3)" in every release since v0.3 — the release that shipped
  `skrog doctor`.

#### Honesty

Claims the product made that were not true. Listed separately because they are
the reason this release took the time it did, not because they are a lesser
category.

- **The README's two performance numbers are gone.** "~80 ms `docker version`,
  measured at parity with Desktop" — the project's own benchmark issue
  measured 226 ms, and contains no Docker Desktop column at all, so neither
  half had a measurement behind it. "~1 s engine start" was never measured
  either. Replaced with what #326 and #398 actually show, labelled as such.
- **`docs/policy.md` no longer claims the machine layer is a boundary against
  a standard user.** It is not: `%ProgramData%` is not administrator-only by
  default and skrog never checked, `SKROG_MACHINE_POLICY_DIR` redirects the
  whole layer, and the pipe is not the only route to the engine. It is
  tamper-evident fleet configuration, which is a real and useful thing, and is
  now what the page says (#418).
- **`docs/reference.md` described the pipe ACL vulnerability fixed in v0.3 as
  current behaviour** — the `--sddl` help said the default grants interactive
  users, which it has not since v0.3 (#416).
- **`docs/security.md` invited private reports and gave no channel**, while
  private reporting was disabled in the repository settings.
- **The README said the tray has "six menu items, forever"**; it has seven.
  The scope tripwire in `ROADMAP.md` now says seven and records that it was
  crossed once without being invoked.
- Known gaps found in the same review and left open rather than quietly
  papered over, each now documented where someone would look for it:
  `/volumes/create` is not judged (#419), `/plugins/pull` passes under a
  default allowlist (#420), and `cache --upstream` is unchecked against it
  (#421).

### Known issues

- **`skrog restart --supervisor` loses a custom pipe**
  ([#429](https://github.com/wslkit/skrog/issues/429)). The replacement
  supervisor re-runs pipe selection from scratch instead of reusing what it
  was serving, so a `skrog supervise --pipe <custom>` setup comes back on the
  default or fallback pipe and `DOCKER_HOST` stops working. Pre-existing, not
  new in 0.6.0; it surfaced because the acceptance suite now runs in CI and
  isolates itself with a custom pipe. Plain `skrog restart` — the one almost
  everyone wants — is unaffected.

Below are not ours, but you will hit them.

- **Container DNS fails on the wslc backend with WSL 2.9.12**
  ([#424](https://github.com/wslkit/skrog/issues/424)). A container is handed
  the Windows host's LAN router as its nameserver and it answers `SERVFAIL`,
  so `apk add`, `curl` and any build step reaching the network fail. Working
  on 2.9.11, broken on 2.9.12. **Workaround: `docker run --dns=1.1.1.1`**, or
  set `dns` in the engine config. Note `docker pull` still works — dockerd
  resolves on the session VM's behalf, not the container's — so a successful
  pull does not mean DNS is fine. The distro backend is unaffected.

### Upstream fixes worth knowing

- **WSL 2.9.12 fixes bind-mount file ownership on the wslc backend**
  ([microsoft/WSL#40719](https://github.com/microsoft/WSL/issues/40719)).
  Below 2.9.12, every file under a Windows share reported as `root:root` mode
  `0777` and `chmod` was a silent no-op, so a container running as a non-root
  user could not own the files it created. On 2.9.12 ownership and mode are
  both preserved. If you use the wslc backend with non-root containers, this
  is a reason to update WSL. The distro backend was never affected.

[Unreleased]: https://github.com/wslkit/skrog/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/wslkit/skrog/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/wslkit/skrog/compare/v0.5.1...v0.6.0
