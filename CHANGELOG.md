# Changelog

Notable changes per release. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning is
[semver](https://semver.org/), with what each bump promises while we are
pre-1.0 written down in [RELEASING.md](RELEASING.md#semver-while-we-are-pre-10).

This file starts at 0.6.0. Earlier releases have hand-written notes on their
[GitHub release pages](https://github.com/wslkit/skrog/releases) and are not
reconstructed here — inventing a tidy history after the fact would be less
useful than saying where the real one is.

## [Unreleased]

### Added

- **Published ports can reach further than `localhost`**
  ([#507](https://github.com/wslkit/skrog/issues/507),
  [#508](https://github.com/wslkit/skrog/issues/508)).
  `docker run -p 8080:80` works, and then it does not work from your phone.
  Under WSL2's default NAT networking dockerd publishes inside the distro and
  WSL's own forwarder binds `127.0.0.1` on the Windows side — so the container
  answers on the machine that started it and nowhere else, with nothing in
  `docker ps`, the logs or the engine saying so.

  **`skrog doctor` now says so**, silent unless a container is actually
  publishing something:

  ```
  [warn] published ports beyond this machine: port 18080 reachable from this machine only
  ```

  **And one opt-in key fixes it:**

  ```powershell
  skrog config set network.publish-scope lan
  ```

  The supervisor then watches what the engine publishes and opens matching
  listeners on every interface. They appear and disappear with the containers
  that published them, because a listener outliving its container is a port
  answering from somewhere surprising. Measured on the reference machine: the
  relay binds `0.0.0.0` **and** `[::]`, so the port answers over IPv6 too.

  **Opt-in, and loud about why.** "lan" puts your dev containers on the
  network — a development database with no password, published on a
  coffee-shop wifi, is reachable by everyone on that wifi. The same bar
  `emulation.platforms` is held to: a setting that reaches past the thing you
  were configuring is a decision, not a default to discover afterwards.

  TCP only, deliberately: a stream relay cannot carry datagrams, and a UDP port
  that silently dropped every packet would be worse than one that was never
  relayed. Ports below 1024 need elevation the supervisor does not have and are
  skipped with a line in the log.

  The other route — `networkingMode=mirrored` in `~/.wslconfig` — still works
  and is documented beside it. It is machine-wide, affecting every WSL distro,
  which is why `publish-scope` exists as the narrower tool.
  [docs/ports.md](docs/ports.md) has all of it, including the measurement and
  why this is **not** [#163](https://github.com/wslkit/skrog/issues/163) (that
  was mirrored networking breaking loopback itself, and it is fixed — here
  loopback is the only thing that works).

  **The relay follows container events** rather than a 5 s poll
  ([#510](https://github.com/wslkit/skrog/issues/510)), so a port is relayed
  when its container starts. Measured on the reference host, `docker run -p`
  to the first answer on the LAN address: 1.3–1.8 s, against 1.3–3.4 s on the
  poll and 0.5–1.1 s for the same server over `localhost` — most of it is the
  server starting.

  **Found before release, fixed in the same change:** with the scope at `lan`,
  the relay's poll could reach an idle-stopped engine through the socat
  fallback, and `wsl.exe` booted the distro straight back up — so turning the
  relay on quietly cancelled idle-stop. Driven on the reference host: the
  engine idle-stopped and was running again 6 s later. Every relay dial now
  waits for the supervisor to report the engine serving; the same run with the
  fix stayed stopped.

- **`skrog doctor` names the port trap `localhost` cannot fix**
  ([#510](https://github.com/wslkit/skrog/issues/510)). A dev server bound to
  `127.0.0.1` *inside* the container — the default for Vite, Next.js's dev
  server, Flask — cannot be reached through `-p` at all, and neither can a
  mapping whose right-hand side names a port nothing listens on. `docker ps`
  shows the mapping either way. The new `container-listeners` check reads each
  publishing container's sockets from its own network namespace, with nothing
  installed in the image:

  ```
  [warn] published ports have something -p can reach: web: port 8000 listens on 127.0.0.1 only, inside the container (published as 18081)
  ```

  Measured: `python -m http.server --bind 127.0.0.1` published with `-p`
  answered nothing, and `--bind ::` answered `200`.

- **`skrog top`: where the VM's memory and CPU go**
  ([#511](https://github.com/wslkit/skrog/issues/511)). `docker stats` shows
  containers; the number people worry about is Vmmem, and most of it is
  usually not a container. `top` shows each container, the engine's own
  daemons, page cache, other WSL distros sharing the VM, WSL itself and the
  kernel, next to what Windows says Vmmem holds (Task Manager's figure, read
  without elevation), with PSI stall figures that say whether anything is
  actually short. It refreshes like `docker stats`; `--once` (or
  `--no-stream`) and `--json` take one reading, and `--json --stream` prints
  one JSON object per line for `jq` or a log shipper.

  The VM's "used" is split into four parts that add up to it, including the
  share `/proc/meminfo` does not itemise at all — about 245 MiB on the
  reference host, most likely driver allocations. The first draft left that
  out and its parts summed to 319 of 592 MiB. When Windows holds much more
  for the VM than the VM uses (288 MiB on the reference host), `top` says so.
  Container rows carry each container's own memory limit (a dash when it has
  none, rather than the VM total `docker stats` prints) and its PIDs.

  It never starts the engine and never keeps it from idling: it reads inside
  the distro, not through the pipe. Driven on the reference host: with `top`
  refreshing every 2 s and a 1 minute idle timeout, the engine idle-stopped at
  66 s and stayed down, and `top` waited for it. Other distros are measured,
  not inferred — a running Ubuntu showed up as its own group. One refresh
  costs one `wsl.exe` round trip, about 0.2 s. [docs/memory.md](docs/memory.md)
  has how to read it and how it differs from `docker stats`.

### Fixed

- **`skrog doctor` says when Skrog will not start at logon**
  ([#515](https://github.com/wslkit/skrog/issues/515)). An install with no
  autostart entry works until the machine restarts, and then docker has no
  engine until someone runs `skrog start`. That is how this was found: a
  supervisor healthy for 26 hours, killed at shutdown, and nothing registered
  to start it again. Doctor used to mention a missing entry only as a skip
  that called it irrelevant outside headless hosts. A new `autostart` check
  now warns when an engine is installed, set to run, and not registered:

  ```
  [warn] starts at logon: Skrog will not start at logon: no autostart entry is registered
  ```

  It is quiet when the engine is stopped by request.

  **And the choice is now recorded**, as a new `autostart` setting:

  ```powershell
  skrog config set autostart on    # or off
  ```

  It writes or removes the logon entry at once, exactly like `skrog autostart
  enable|disable`, which now record the choice too. So do `install`,
  `install --no-autostart` and `install --config`. A missing entry the user
  asked for is then a real finding: doctor warns that "autostart is on, but no
  logon entry is registered", and **`skrog doctor --fix` registers it again**.
  An entry turned off on purpose is reported as that, and stays quiet. An
  install from before the setting keeps the heuristic, with no `--fix`: nothing
  says whether its missing entry was deliberate. `skrog config` shows the live
  state beside the setting when the two disagree.

  Driven on the reference host with the entry deleted from the Run key by
  hand: doctor warned, `--fix` re-registered it, and `config set autostart
  off` removed it and turned the check into "not started at logon, by choice".

## [0.8.0] — 2026-09-22

Admission control learns to ask where an image came from, the engine start path
finally says where its time goes, and a chain of silent failures gets found —
several of them by running the product rather than by reading the diff, and
four of them by an independent review of this release's own code.

### Added

- **`skrog doctor` catches a `prune.every` that stops the engine ever idling**
  ([#496](https://github.com/wslkit/skrog/issues/496)). The automatic prune
  talks to the engine through Skrog's own pipe, and that traffic restarts the
  idle window — so a prune interval at or below `idle-timeout` resets the clock
  before it can expire and the RAM is never returned.

  ```
  [warn] idle-stop versus automatic prune: prune.every 10m is not longer than
         idle-timeout 30m, so the engine will never idle-stop
         fix: set prune.every longer than idle-timeout:
              `skrog config set prune.every 2h` (or lower idle-timeout)
  ```

  Both settings are off by default, so nothing ships in this state — it takes
  turning both on *and* setting the prune shorter, which is the opposite of how
  either is normally tuned. But the symptom is "my engine never releases its
  RAM", which people notice, cannot explain, and have no reason to connect to a
  housekeeping setting.

  Judged from the **configuration**, not from the idle-stop counters. The
  counters look like the obvious evidence and are the weaker signal: a machine
  that has simply been busy also shows zero idle stops. Two durations and a
  comparison is a verdict; a counter at zero is a guess.

  The underlying fix — having the prune reach the engine without crossing the
  pipe — is still open. The issue's cheap route (`wsl -d skrog-engine docker …`)
  turns out not to exist: there is **no docker client in the engine distro**,
  only the daemon. The workable version talks the engine API directly, the way
  the idle probe and provenance already do, and wants its own release window
  rather than this one.

- **`skrog stop --supervisor`** ([#482](https://github.com/wslkit/skrog/issues/482)).
  There was no supported way to stop the supervisor, and the installer told
  people to use one: refusing to overwrite a running `skrog.exe`, it said
  *"Stop it first: `skrog stop`"*. `skrog stop` is the engine only — by design
  and by its own usage text — so the supervisor and its watchdog kept the
  binary locked, the user followed the instruction, retried, and got the
  identical error with no next step. Every in-place upgrade hit this.

  The flag stops the engine, then the supervisor. The watchdog goes with it
  without being asked, because a clean exit is final (`internal/watchdog`) —
  the same property that stops it racing `restart --supervisor`. The installer
  now names the command that works.

  Still the rarer intent by far, which is why it is a flag and not the
  default: nearly everything anyone wants from `stop` is the engine, and a
  supervisor that survives is what makes `skrog start` quick and keeps the pipe
  where it was.

- **`skrog doctor` checks the emulation you asked for is actually live**
  ([#480](https://github.com/wslkit/skrog/issues/480)).
  `skrog config set emulation.platforms linux/arm64` reports success and takes
  effect on the next engine start — where registration can fail. When it did,
  the only evidence was a `WARN` in `supervisor.log`: `skrog restart` printed
  "engine is running", and the container then died with `exec format error`,
  which is the exact symptom the setting was turned on to remove.

  ```
  [warn] foreign-architecture containers: emulation.platforms asks for
         linux/arm64, but no handler is registered for arm64
  ```

  Separate from the `cross-architecture builds` check next door, which reads
  the same table. That one asks *what can this machine do* and never warns,
  because a standing warning about a capability nobody uses is what teaches
  people to skim. This one asks *did what you asked for happen*, is silent
  unless you asked, and warns when the answer is no. The first cannot catch
  this: with no handlers at all it reports "the default builder is native-only
  … nothing is wrong here", which on a machine that requested emulation is
  wrong twice.

  The remedy names the likeliest cause, which is not guessable: the emulator
  ships **in the engine image**, and images before `29.8.1-3` do not carry it
  (#479).

- **`deny-unattributable-images`: refuse an image this machine has no record of**
  ([#343](https://github.com/wslkit/skrog/issues/343)).
  `allow-registries` judges a *reference*, and a reference is a label anyone
  can reattach. `docker load` a tarball, `docker tag` it
  `registry.example.com/app:1`, and every reference-based rule passes on bytes
  that never came from `registry.example.com`.

  Skrog now records where each image came from — keyed by image ID, written
  after a pull it allowed — and with this rule set, refuses to run one it has
  no record of. Keyed by ID is the whole point: retagging does not create
  provenance.

  Opt-in, and inert without `allow-registries`, exactly like
  `deny-unattributable-builds`; `skrog policy show` says so rather than letting
  you believe otherwise. It also reports the recorded/pre-existing split,
  because turning this on without knowing how much of a machine's image set has
  a record is how a security feature gets switched off again an hour later.

  **Images you already had are trusted.** The first start with this present
  records the existing image set as `pre-existing`, once. Refusing a machine's
  entire cache on upgrade is not a defensible default, and the count is
  reported rather than implied.

  **It is drift protection, not a boundary, and the docs say so.** Anyone who
  can run `wsl -d skrog-engine -u root docker load` talks to the engine
  directly and nothing at the pipe sees it (#418). What it catches is the
  ordinary accidental laundering: a tarball copied off a laptop, a `docker
  save` from a machine with different rules, an image left behind by a setup
  nobody remembers. It also **fails open** — an engine it cannot reach or a
  record it cannot read allows the request, because a container that will not
  start because a lookup timed out is the worse outcome.

  **It refuses your own builds too.** The build is not judged, but the image it
  produces has no recorded origin, so the `docker run` after it is refused — at
  the pipe a BuildKit build is an opaque gRPC stream with nothing to attribute
  it to. So this is a rule for machines that *consume* images (a CI runner, a
  locked-down workstation), not ones that build them. `docs/policy.md` says so
  where someone deciding whether to turn it on will read it, rather than
  leaving them to find out from a refusal.

  Applied by every listener that serves the engine, not just the supervisor's
  pipe: `skrog serve` and `skrog proxy` too, for the reason
  [#257](https://github.com/wslkit/skrog/issues/257) gives.

- **`skrog` logs where an engine start spent its time**
  ([#398](https://github.com/wslkit/skrog/issues/398)). One line per start:

  ```
  engine start phases probe=17ms prelaunch=1.085s launch=17ms
                      dockerdReady=2.96s agent=0s shareSocket=129ms total=4.206s
  ```

  #404 had to reconstruct this from the timestamps of three unrelated log
  lines, and a 1.2 s stretch sat unexamined in the gap between two of them for
  two releases. Anyone asking "where does a start go" now gets the answer from
  one run instead of arithmetic on a log.

  **What it immediately showed is that there is no five-second win in Skrog's
  own code.** Of a ~4.2 s start: ~1.1 s is the WSL distro booting, ~2.6–3.3 s is
  dockerd starting itself, and Skrog's own share — the health probe, the launch
  call, the socket share — is about **160 ms**. Two optimisations were written
  against this issue and measured; one is in this release and one was reverted,
  both described below.

  It also settled the ROADMAP item that has carried "measure the real number"
  since v0.2: the **idle-wake** path is **4.8–6.3 s**, against **5.0–8.4 s** for
  a full `stop`/`start`. The wake is not meaningfully cheaper, and the reason
  the README gave for expecting it to be was wrong — an idle stop is
  `wsl --terminate`, the same operation `skrog stop` performs, so the wake pays
  the same distro boot and the same dockerd startup. There is no cheaper resume
  path to reach for. The README and ROADMAP now say the measured numbers.

- **The engine agent starts alongside the wait for dockerd, not before it**
  ([#398](https://github.com/wslkit/skrog/issues/398)). Provisioning the agent
  secret and launching the agent are ~430 ms of `wsl` round trips, and they sat
  between launching dockerd and starting to wait for it — while dockerd was
  already busy taking ~3 s to come up. They now overlap that wait, and the
  phase log reads `agent=0s` on every start.

  Still fully started before `StartEngine` returns, on every exit path
  including the failure one: nothing may observe a half-started agent, because
  the symptom would not be a crash but a silent fall back to the socat relay at
  ~165 ms per connection instead of ~0.6 ms.

  **End to end this is worth about 100 ms, not 430** — median 4.50 s → 4.20 s
  over ten paired runs — because dockerd's own startup is the binding
  constraint and the agent work was already hiding inside it. Reported that way
  rather than quoting the 430 ms the phase log removes.

### Changed

- **`skrog version` and `skrog doctor` name the rootfs revision, not just its
  checksum** ([#484](https://github.com/wslkit/skrog/issues/484)).

  ```
  rootfs   29.8.1-3  (f1be49d99b9a...)
  ```

  It used to be the truncated SHA-256 alone — a value that identifies the bytes
  exactly and answers none of the questions asked of it. Which revision am I
  on, and is it the one that carries the emulator, were unanswerable from the
  output of the two commands whose job is to answer them. `doctor` was worse:
  it printed `version 29.8.1` one line above, so the two lines together gave
  the engine version twice and the revision never.

  The checksum stays, because it is what `--rootfs-sha256`, the manifest and a
  bug report speak, and it is the only identifier an image installed from
  outside the manifest has. Without a ref the line degrades to the checksum
  alone rather than inventing one.

  `version --json` gains `engineRef` as a **new field**; `rootfsSha256` and
  `version` are untouched, because `docs/cli-json.md` pins them and a reader
  switching on either must keep working.

  The naming rule moved to `internal/release` on the way. It had one caller
  when it was written and now has four, and a rule with four copies is a rule
  that drifts.

### Security

- **The registry cache's upstream is judged by policy, and may not be plain
  HTTP** ([#421](https://github.com/wslkit/skrog/issues/421)).
  `skrog cache enable --upstream <url>` wires a mirror into the engine, and
  dockerd applies `registry-mirrors` to **every unpinned Docker Hub pull**
  without changing the reference. So a mirror aimed at any host served that
  host's content under an allowed name, and `allow-registries` passed because
  the reference was never the thing in question.

  The upstream is now checked against the effective allowlist, and `http://` is
  refused unless `--insecure` is passed — a plaintext mirror for Docker Hub is
  a content-substitution position on every unpinned pull on the machine, held
  by anyone on the network path.

  Three places in the code and docs asserted this could not happen, on the
  ground that "a mirror changes where bytes come from, not which image was
  asked for". The premise is true and the conclusion does not follow: where the
  bytes come from is what a registry allowlist is for. All three are corrected
  rather than deleted.

- **A machine policy file that a standard user could have written is refused**
  ([#418](https://github.com/wslkit/skrog/issues/418)). `C:\ProgramData` ships
  with `BUILTIN\Users:(CI)(WD,AD)` and `CREATOR OWNER:(OI)(CI)(IO)(F)`, so on
  any machine where a fleet policy has not landed yet a standard user can
  create `ProgramData\skrog` first and own it. Loading was a bare `os.ReadFile`
  with no owner check, so the "machine layer" could be the user's own rules
  wearing its authority — worse than no machine layer, because `skrog policy
  show` reported it as in force.

  The owner must be `Administrators` or `SYSTEM`. Owner and not the full DACL,
  deliberately: the hole is an ownership one, and a hand-written ACE walker
  that gets an edge case wrong fails a correctly deployed fleet silently.

  **`skrog policy show` reports provenance now**, which is the honest half of
  the promise. The layer cannot be made unbypassable — the supervisor runs as
  the user — so what it can do is not lie about which bypass happened:

  ```
  machine rules: C:\ProgramData\skrog\policy.yaml  REFUSED
    ignored because it is owned by CONTOSO\alice, not by Administrators or SYSTEM.
  ```

  and the documented `SKROG_MACHINE_POLICY_DIR` redirect, which used to be
  indistinguishable from a machine that simply had no fleet policy:

  ```
  machine rules: none found
    SKROG_MACHINE_POLICY_DIR redirects the machine layer to C:\Users\me\empty,
    which holds no policy.yaml.
  ```

  `policy show --json` gains `machineProvenance` for the fleet-dashboard case:
  a machine whose policy was *refused* is the one worth an alert, and it looked
  identical to one that never had any.

  The third route in #418 — `wsl -d <distro> -u root`, `proxy
  --no-path-translation`, `wsl-integrate` — is unchanged and still documented
  as a limit. It is not fixable in this package.

### Fixed

- **Findings from an independent pre-release review.** Two reviewers were run
  over everything since v0.7.1 with no knowledge of why any of it was written.
  Between them they found four defects serious enough to have held the release,
  all of them in code added this cycle:

  - **The reconciler held its lock across the adopted-engine repair**, with no
    timeout — reintroducing [#437](https://github.com/wslkit/skrog/issues/437)
    on a path added days earlier. A `wslservice` that stopped answering would
    have parked the supervisor while holding `mu`, so every `docker` command
    hung instead of failing and `skrog status` could not even report it. The
    repair now runs outside the lock and bounded, like the health probe beside
    it, and a test holds it in flight and checks that `Demand` still answers.

  - **The provenance engine round trip had no timeout at all.** `resolveTimeout`
    bounded only the dial; after that it blocked in `ReadResponse` with no
    deadline, and the vsock dialer clears the deadline it used for its own
    handshake before handing the connection over. An engine that accepted the
    connection and wedged would hang `docker run` forever with no output, and —
    on the recording path, which runs on the relay goroutine — leave the
    bridge's client count above zero so idle-stop was vetoed for the life of
    the process. It goes through an `http.Client` now, the way the container
    probe already did.

  - **An unreadable store refused every container**, which is the exact
    opposite of what this page and the changelog both promised. `read` returned
    `nil` for any failure and `Lookup` could not tell "no record" from "could
    not read"; `sc.Err()` was never checked either, so one over-long line
    silently discarded every record after it. Read failures are now an error
    all the way up and the request is allowed, with `skrog policy show` saying
    the store could not be read rather than quietly reporting zero.

  - **Pre-existing images were never seeded on the ordinary first boot.**
    Seeding was gated on the store existing, and the store is also created by
    the first recorded pull — so on the normal sequence (supervisor starts
    before the engine, pull creates the store) the seed short-circuited
    forever and a machine's whole pre-upgrade image cache stayed
    unattributable, silently. It has its own marker file now.

  Also from the review: compaction could evict the record of an image that is
  **still installed** (a base image pulled months ago, refused today), so the
  180-day expiry is gone and compaction asks what the engine still holds before
  evicting anything; `ReapplyRunning` always returned nil, making the
  supervisor's warning about a failed re-apply dead code; and four tests
  asserted less than their names claimed — including the one added for the
  previous release's `Close` fix, which never reaches the line it was written
  for on Windows.

- **`logSafe` is a sanitizer CodeQL can see again.** Rewriting it to bound its
  output instead of its input (previous entry) moved the untrusted rune into a
  variable computed *before* the `unicode.IsControl` guard and overwritten
  inside it — identical at runtime, invisible to taint analysis. CodeQL stopped
  recognising the function as a barrier and re-raised `go/log-injection` on all
  three call sites, which is how it was found.

  The rune is written only inside the guarded branch now. A sanitizer a checker
  cannot see is one that stops being checked the next time someone edits it.

- **`skrog --help` lines up.** The command list was padded to a fixed width of
  10 and `healthcheck` is 11, so that row had no gap at all and `wsl-integrate`
  overflowed by three. The column is derived from the longest name now, with a
  three-space gap, and a test fails if any name collides.

  The settings list in `skrog config --help` had the same problem differently:
  its column was baked into a format string per row, at 22 for some keys and 26
  for others, and **`gpu` and `gpu.vendor` were missing from it entirely** —
  settable, documented nowhere, and therefore absent from `docs/reference.md`
  too. Both lists now render through one helper.

- **A supervisor that adopts a running engine re-applies its settings**
  ([#501](https://github.com/wslkit/skrog/issues/501)). Found on a real machine
  while writing the multi-platform docs for this release: `emulation.platforms`
  was set, `skrog config get` echoed it back, the supervisor log said the
  handlers were registered, and `docker run --platform` still failed with the
  `exec format error` the setting exists to remove. The `binfmt_misc` table was
  empty.

  `binfmt_misc` handlers are **kernel** state, the socket share is a VM-level
  bind, and the agent is a separate process — any of them can be gone while
  dockerd itself is perfectly healthy. `StartEngine` has an already-running
  branch that re-applies exactly this, and its comment says it exists for "a
  supervisor that finds a healthy engine". Nothing reached it: the supervisor
  called `Start` only when the engine was **down**.

  So the exposed paths are the ones with no command behind them — the watchdog
  restarting after a crash, and logon autostart finding an engine still up.
  `skrog start` and `skrog restart --supervisor` were never affected, because
  those commands call `StartEngine` themselves, which is precisely why this was
  invisible to anyone testing by hand.

  Re-applied **once**, on adoption, not every tick: that branch is four `wsl`
  round trips and paying it at the health interval would be the worse bug. A
  table cleared later, under a supervisor that keeps running, is still only
  reported — by `skrog doctor` ([#480](https://github.com/wslkit/skrog/issues/480)) —
  and not repaired.

- **A scheduled prune is part of the supervisor's lifetime**
  ([#423](https://github.com/wslkit/skrog/issues/423)). It ran on a bare
  goroutine that nothing could wait for, cancel or recover from, with three
  consequences:

  Shutdown did not stop it. On `skrog restart`, a logoff or a service stop, the
  reconciler returned while a `docker system prune -a` kept deleting for up to
  thirty minutes, with the process about to exit underneath it. `Run` now waits
  for a prune in flight, bounded at five seconds — long enough for a sweep that
  is finishing to record its clock, short enough that a wedged prune cannot
  hold a logoff open. After that it is abandoned to its own timeout, which is
  the deliberate outcome rather than an error.

  A panic in it took the bridge down. `Prune` is caller-supplied and ran with
  no `recover`, so a bad implementation killed the supervisor and every docker
  command with it. A failed prune is a disk that stays full; a dead supervisor
  is a machine where docker stops working. Recovered and reported now, with the
  guard still cleared so one bad sweep cannot disable pruning until restart.

  An interrupted prune re-ran immediately. Killed before it recorded the clock,
  the old value stood and the next supervisor was instantly due — a prune on
  the first healthy tick after every logon. The clock is written before the
  sweep as well as after, which changes the failure from "prunes too often" to
  "may skip one interval", the safer direction for a feature whose rules are
  about not deleting things unexpectedly.

- **`skrog engine upgrade` no longer races the supervisor**
  ([#486](https://github.com/wslkit/skrog/issues/486)). It failed
  intermittently, with `exit status 1` and no message, on a different file each
  time — and succeeded every time with the supervisor stopped. 0.7.1 shipped it
  as a known issue with "run it again" as the remedy.

  The upgrade already wrote `desired=stopped` before the swap, with a comment
  saying it was to keep the supervisor out of the way. **Honoring that state is
  what caused the failure.** The reconciler, seeing "stopped but up", calls
  `Terminate` — and terminating a WSL distro kills every process in it,
  including the `wsl --exec` copying binaries into that same distro. The exec
  itself boots the distro, dockerd comes up with it, and the next tick sees
  exactly the state that makes it terminate. Both sides agreed on the goal and
  fought over the route.

  "Stop the engine" and "keep your hands off the distro" are different
  instructions, and the second could not be written as a desired state. So
  there is now a maintenance hold: while it is held the reconciler does
  nothing — not even probe, since the probe boots the distro it asks about. It
  carries an expiry, because a crashed holder must not leave a supervisor that
  has silently stopped reconciling forever; that would present as an engine
  that never recovers with nothing in any log to say why.

  Four upgrades in a row on the machine that reproduced it, supervisor running
  throughout, all clean.

- **A restart after `engine upgrade`, `compact`, `relocate` or a snapshot
  restore no longer drops emulation, GPU, proxy and host CA settings**
  ([#490](https://github.com/wslkit/skrog/issues/490)). Found while validating
  the fix above: after an upgrade, `docker run --platform linux/arm64` failed
  with `exec format error` on a machine where it had just worked.

  The supervisor re-reads every per-start setting and has since #83 and #202.
  Every one-shot command that restarts the engine built its own options and
  read no config at all, so the engine came back with emulation off, no GPU
  spec, no proxy and no imported CAs — until something else restarted it.

  Emulation is the one that fails loudly. On a corporate network the others are
  worse: pulls stop working, or fail on a certificate, some time after an
  unrelated maintenance command, with nothing connecting the two.

  There is one list now, used by the supervisor and by all seven one-shot
  paths. The bug was that there were two — the supervisor's, which was right,
  and everyone else's, which did not exist.

- **`skrog doctor` no longer reports WSL's own COM stub as an injected
  third-party module** ([#488](https://github.com/wslkit/skrog/issues/488)).

  ```
  [warn] injected modules: 1 third-party module(s) are loaded into this process
         wslserviceproxystub.dll  (C:\Program Files\WSL\wslserviceproxystub.dll)
       fix: ... ask whoever manages the agent for an exclusion
  ```

  There is no agent and nobody to ask. That DLL is Microsoft-signed, ships with
  WSL, and **Skrog loads it itself**: it talks to `wslservice` over COM (#380),
  and Windows maps the marshalling stub into any process that does. Doctor said
  as much two lines above, in the `WSL service access` check.

  The filter treated only `C:\Windows\...` as the platform's own, and modern
  WSL is serviced separately and installs to `%ProgramFiles%\WSL`. It now
  exempts a directory named `wsl`, matched as a whole path segment so that
  something shipping in `wslhook\` is still reported — the point is to quieten
  one component, not to hand anything WSL-shaped a free pass.

  This fired on every healthy machine, which is the specific way a check whose
  value is that its output means something becomes a check people skim.

## [0.7.1] — 2026-09-21

A patch release for one thing: 0.7.0's headline feature could not reach an
existing install. Everything here was found by installing 0.7.0 on a real
machine and driving it, not by reading the diff.

### Fixed

- **`skrog engine upgrade` now carries the foreign-architecture emulator**
  ([#479](https://github.com/wslkit/skrog/issues/479)). 0.7.0 shipped
  `emulation.platforms` (#462) with its payload in the rootfs, and the
  extractor took one directory — `/usr/local/bin`. The emulator lives in
  `/usr/bin`. So an existing install could move to engine `29.8.1-3`, the
  revision cut to carry it, and not receive it: handler registration then
  failed on `test -x /usr/bin/qemu-aarch64`, and **the feature was unreachable
  for anyone who had not installed fresh**.

  0.7.0's entry for #462 said `skrog engine upgrade` was the fix for an older
  image. It was not, and that line was wrong on the release page as well as
  here. It is now true.

  The extractor takes the interpreter from `/usr/bin` as well, checking the
  directory and the name as a pair — `dockerd` under `/usr/bin` is not the
  engine, and an interpreter under `/usr/local/bin` is not an interpreter. The
  matching `binfmt.d` conf is deliberately not carried: Skrog composes the
  registration itself and this distro runs without systemd, so nothing would
  read it.

- **`skrog upgrade` sees rootfs revisions**
  ([#481](https://github.com/wslkit/skrog/issues/481)). It compared bare
  engine versions, so `29.8.1-1` and `29.8.1-3` both read as "29.8.1" and the
  answer was "current" — on exactly the machines that needed the newer
  revision. It compares refs now, and the engine row names them, so the report
  says `29.8.1-1 -> 29.8.1-3 available` rather than repeating one number. An
  install with no recorded ref still falls back to versions, because an empty
  ref compared against a real one would read as an upgrade forever.

- **A failed engine upgrade says which file it died on**
  ([#483](https://github.com/wslkit/skrog/issues/483)). One copy failed in the
  field with `exit status 1` and no output at all, which read as a truncated
  message and named neither a cause nor a file. Each file is now announced
  before it is copied, so the last name in the transcript is the one that
  failed, and silence is reported as silence — "the command produced no
  output, so it likely never ran" is a clue, where a bare trailing colon is
  not. Rollback already worked correctly and is unchanged.

  That diagnostic paid for itself within the hour: it is what turned the
  failure below from "a flake" into a named file and, from there, into #486.

### Known issues

- **`skrog engine upgrade` fails intermittently while a supervisor is
  running** ([#486](https://github.com/wslkit/skrog/issues/486)). The binary
  copy dies with `exit status 1` and no message, on a different file each
  time; stopping the supervisor first makes it succeed every time. **Rollback
  works** — the engine comes back on the previous revision with images,
  containers and volumes intact — so the cost is a retry, not a broken engine.

  Pre-existing, not introduced here: 0.7.0 does it too. It is called out now
  because this release makes `engine upgrade` the way an existing install gets
  the emulator, so a command that needs retrying matters more than it did.
  **If it fails, run it again.** The guard that was supposed to prevent this
  is already in the code and is evidently not sufficient, which is why the
  issue is open rather than a line in Fixed above.

- **A failed emulation registration is still invisible**
  ([#480](https://github.com/wslkit/skrog/issues/480)) — a `WARN` in
  `supervisor.log` and nothing else; `skrog doctor` has no check for it. Not
  fixed here because a new doctor check is added surface, which belongs in a
  minor. #479 removes its likeliest cause.

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

  > **Two corrections to the two paragraphs above, found validating this
  > release on a real machine.** They are left as written, because this section
  > is the record of what 0.7.0 shipped, and corrected here rather than
  > silently edited.
  >
  > `skrog engine upgrade` was **not** the fix for an older image: it copied
  > only `/usr/local/bin`, so it could move an install to `29.8.1-3` and still
  > not deliver the emulator. In 0.7.0, emulation works **only on a fresh
  > install**. Fixed in 0.7.1 ([#479](https://github.com/wslkit/skrog/issues/479)).
  >
  > `skrog doctor` does **not** report which handlers are live. Its only
  > emulation check is about cross-architecture *builds*, and it reports OK on
  > a machine where runtime registration has failed. Tracked as
  > [#480](https://github.com/wslkit/skrog/issues/480); the failure is a `WARN`
  > in `supervisor.log` and nowhere else.

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

[Unreleased]: https://github.com/wslkit/skrog/compare/v0.8.0...HEAD
[0.8.0]: https://github.com/wslkit/skrog/compare/v0.7.1...v0.8.0
[0.7.1]: https://github.com/wslkit/skrog/compare/v0.7.0...v0.7.1
[0.7.0]: https://github.com/wslkit/skrog/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/wslkit/skrog/compare/v0.5.1...v0.6.0
