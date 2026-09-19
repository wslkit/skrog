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

## [0.6.0] — unreleased

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

[Unreleased]: https://github.com/wslkit/skrog/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/wslkit/skrog/compare/v0.5.1...v0.6.0
