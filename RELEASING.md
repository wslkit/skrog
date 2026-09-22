# Releasing

Three independent release streams, deliberately kept apart:

| stream | tag | what it publishes | workflow |
|---|---|---|---|
| **rootfs** | `rootfs-vX.Y.Z-N` | one engine tarball **per architecture**, each with its `.sha256` and an SPDX SBOM | `rootfs.yml` |
| **app** | `vX.Y.Z` | `skrog.exe` for amd64 and arm64, zipped, plus `SHA256SUMS` | `release.yml` |
| **docker CLI** | `dockercli-vX.Y.Z` | the Windows **arm64** `docker.exe` upstream does not publish (#450), with its `.sha256` | `dockercli.yml` |

They are separate because they version independently: an engine security patch
should not require an app release, a docker CLI bump is upstream's schedule
rather than ours, and vice versa (PLAN §04, `skrog engine upgrade`).

**Each workflow tests for its own prefix, positively.** They used to exclude
each other by name — "not `rootfs-`" — which is an open list: adding a third
namespace would silently have attached `skrog.exe` to a docker CLI release
because nothing said no. The guards now ask "is this mine", which the next
namespace cannot break.

Deciding *what* to bump — which moby tag, whether containerd/runc/buildkit move
with it, where each checksum comes from — is a separate job from cutting the
release. That is [docs/bumping-upstream.md](docs/bumping-upstream.md); this file
covers publishing once the versions are settled.

## `git describe` sees every stream

The tag namespaces share one repository, and rootfs releases are cut far
more often than app releases — so `git describe --tags` usually resolves to a
**rootfs** tag, not an app version:

```
$ git describe --tags
rootfs-v29.8.0-1-7-g650c25e        # not an app version

$ git describe --tags --match 'v*'
v0.3.0-50-g650c25e                 # what you wanted
```

Nothing in CI is affected — `release.yml` takes the version from the release
tag and never asks git. But any local script or build that stamps a version
from a bare `git describe` gets the wrong stream, silently. Always pass
`--match 'v*'` for an app version, or `--match 'rootfs-v*'` for the engine.

## The ordering that matters

An app release embeds the rootfs checksum in
[`internal/release/manifest.json`](internal/release/manifest.json). So the rootfs
has to exist first:

1. **Cut the rootfs release.** Tag `rootfs-v<engine-version>-<revision>` —
   e.g. `rootfs-v29.7.2-2`, matching `ENGINE_VERSION` and `ROOTFS_REVISION` in
   `guest/rootfs/versions.env`. The revision exists because the tarball
   carries more than the engine (skrog-agent, config, the Alpine userland):
   it can change while the engine version stays put, and a published release's
   assets must never be replaced — skrog builds already in the wild pin its
   checksum. Bump the revision for a content change, reset it to 1 on an
   engine bump. Publishing the tag triggers `rootfs.yml`, which rebuilds from
   source (or restores the cached binaries), runs the smoke test, and attaches
   the assets. (The first release predates the scheme and is tagged bare
   `rootfs-v29.7.2`.)

   **Two architectures, one release (#388).** `rootfs.yml` builds amd64 and
   arm64 on native runners and both sets of assets go onto the same tag:

   ```
   skrog-rootfs-<version>-<rev>-amd64.tar.gz{,.sha256,.cosign.bundle}
   skrog-rootfs-<version>-<rev>-arm64.tar.gz{,.sha256,.cosign.bundle}
   skrog-rootfs-<version>-<rev>-{amd64,arm64}.spdx.json
   ```

   The publish job waits for both builds, so a release is never half an
   architecture. The matrix does **not** fail fast: if arm64 breaks, the amd64
   result is what tells you whether the cause is the change or the
   architecture. A tag published while one architecture is red attaches
   nothing, which is the intended outcome — fix it and re-run.

   Assets through `rootfs-v29.8.1-1` carry no `-<arch>` suffix and are amd64.
   They are not renamed: `manifest.json` pins those exact bytes.

   **Always pass `--latest=false`:**

   ```
   gh release edit rootfs-vX.Y.Z-N --prerelease=false --latest=false
   ```

   Rootfs releases are cut far more often than app releases, so a rootfs tag
   is almost always the newest thing in the repository. Publish one as a
   normal release without pinning `--latest=false` and **it takes the repo's
   "Latest release" badge** — a visitor lands on an engine tarball instead of
   skrog. Neither installer is affected (both filter by `^v\d+\.\d+\.\d+$` and
   never touch `/releases/latest`), so this is purely about what a human sees
   first, which is reason enough.

   Releases through `rootfs-v29.8.0-2` are still flagged pre-release, from
   before this file described a normal release at all. Harmless — the assets
   are what matter and they are immutable — and not worth rewriting.
2. **Copy the published checksums into the manifest.** `manifest.json` is
   schema 2: `engines[].rootfs` is a map keyed by GOARCH. Take each
   architecture's `.sha256` asset and add its entry, confirming each `url`
   matches the tag you used.

   ```json
   "rootfs": {
     "amd64": { "url": "...-amd64.tar.gz", "sha256": "..." },
     "arm64": { "url": "...-arm64.tar.gz", "sha256": "..." }
   }
   ```

   Merge that as a normal PR — a test asserts the manifest agrees with
   `versions.env`, per architecture.

   **Absent and empty mean different things.** An architecture that is not in
   the map was never built for that engine; one that is present with an empty
   `sha256` is built but not released. `skrog install` says something
   different for each, because the user's next move differs — nothing they
   wait for fixes the first. Do not add an empty entry for an architecture
   that has no build.

   Adding the first arm64 entry is what turns arm64 on. There is no code
   change: `HostRootfs()` selects by `runtime.GOARCH`, and a test
   (`TestNoArm64RootfsIsPublishedYet`) fails on purpose the moment one
   appears, to send you at `docs/install.md` and `internal/release/arch.go`,
   which still describe arm64 as unsupported.
3. **Cut the app release.** Tag `v<app-version>`. `release.yml` builds both
   architectures, asserts the version stamp actually took, packages, and
   attaches the zips with `SHA256SUMS`.

Until step 2 lands, `skrog install` refuses with a message telling the user to
pass `--rootfs-url` and `--rootfs-sha256` explicitly. That is intentional: there
is no code path that installs an unverified rootfs.

## Cutting a docker CLI release

Only needed on Windows arm64, and only until upstream publishes one
([#450](https://github.com/wslkit/skrog/issues/450)). The shape mirrors the
rootfs stream, one step shorter because there is a single artifact.

1. **Pin the version.** `third_party/docker-cli/versions.env` carries
   `DOCKER_CLI_VERSION`, `DOCKER_CLI_TAG` and `DOCKER_CLI_SHA` — the tag's
   dereferenced commit, which `build.sh` refuses to build without matching:

   ```
   git ls-remote https://github.com/docker/cli 'refs/tags/vX.Y.Z^{}'
   ```

   It must equal the `docker` component's `version` in
   `internal/dockercli/manifest.json`; a test asserts that, because the same
   binary is installed on both architectures and two versions is a support
   trap.

2. **Cut the release.** Tag `dockercli-v<version>` — matching
   `DOCKER_CLI_VERSION`, no revision suffix, because the artifact is one
   binary from one upstream commit and there is nothing else in it to revise.
   Publishing triggers `dockercli.yml`, which builds, asserts the PE header is
   really ARM64, attests, signs the checksum and attaches both files.

   **`--latest=false`, as always:**

   ```
   gh release edit dockercli-vX.Y.Z --prerelease=false --latest=false
   ```

3. **Point the manifest at it.** Put the published `.sha256` and URL into
   `internal/dockercli/manifest.json` under
   `components[docker].arch.arm64`. Merge as a normal PR.

   amd64 is **not** touched — it stays Docker's own published zip. That
   asymmetry is deliberate and argued in `docs/docker-cli.md`; a test fails if
   the amd64 URL ever stops pointing at `download.docker.com`, so changing it
   is a decision rather than a slip.

## Dry runs

Both workflows can be exercised without publishing:

- `release.yml` — run it from the Actions tab (`workflow_dispatch`) with a
  version string. It builds, verifies the stamp, packages, and uploads to the
  workflow's own artifacts, but attaches nothing to any release.
- `rootfs.yml` — `workflow_dispatch` builds and smoke-tests without publishing.

Worth doing before the first real release of either stream, since release
plumbing is the kind of thing you want to have already debugged.

## Pre-releases

Use a normal semver pre-release tag (`v0.1.0-preview.1`) and tick **"Set as a
pre-release"** so it does not become `latest`. Everything else behaves the same.
Pre-releases are the right way to shake out the pipeline; the version stamp
check means a preview that reports `dev` fails the build rather than shipping.

## A normal release

This section did not exist until v0.6.0, which is why **every release up to
v0.5.1 — all sixteen, app and rootfs — was flagged as a pre-release**,
including the plain `vX.Y.Z` ones. That was never a decision; it was the
absence of one. The document described only the preview case, so the preview
case is what happened every time.

A normal release is the same pipeline with three differences:

1. **Do not tick "Set as a pre-release."** It becomes `latest`, which is what
   `scripts/install.ps1` prefers and what most people will get.
2. **Write the CHANGELOG entry first**, in the same PR as the last code
   change, not afterwards from the git log. See [CHANGELOG.md](CHANGELOG.md);
   the release notes are that section, and nothing else has to be authored
   twice.
3. **Update the downstream channels**, which the tag does not touch:

   | channel | what to do |
   |---|---|
   | [scoop-skrog](https://github.com/wslkit/scoop-skrog) | bump `version`, both URLs and both hashes in `bucket/skrog.json`. `checkver`/`autoupdate` are configured, so `scoop bucket` tooling can do it, but **nothing runs automatically** — cut a release and forget this and the bucket silently serves the previous version. **Check the change reached `main`:** the manifest sat on an unmerged branch from 0.5.1 to 0.8.0, so the bucket served *nothing* for three releases while local checkouts on that branch looked current |
   | [winget](#publishing-to-winget) | new manifest directory under `manifests/w/wslkit/skrog/<version>/` |
   | [setup-skrog](https://github.com/wslkit/setup-skrog) | only if the action pins a version |
   | [skrog-vscode](https://github.com/wslkit/skrog-vscode) | only if it pins one |

4. **Bump the pinned versions in the docs' own examples.** `docs/ci-runners.md`
   and `docs/install.md` show a concrete version to pin, and nothing checks
   them — they sat at `0.6.0` through two releases, so anyone copying the
   GitHub Actions or GitLab snippet pinned a version two behind the one they
   had just read about. Grep for the previous version across `docs/` before
   tagging:

   ```powershell
   Select-String -Path docs\*.md,README.md -Pattern '<previous version>'
   ```

   Examples that name a version are the only kind that goes stale silently, so
   they are the only kind worth a checklist line.

### Before dropping the pre-release flag for the first time

Dropping the flag is a claim that the front page is true, which is a different
and larger claim than "the build works". Before a release that is not a
preview:

- **The README's measurable claims are measured.** Latency and cold-start
  numbers must point at an issue with the measurement in it, or not be there.
  This cost v0.6.0 two false numbers that had been on the front page for
  months ([#326](https://github.com/wslkit/skrog/issues/326),
  [#398](https://github.com/wslkit/skrog/issues/398)).
- **Every advertised platform works, or says it does not.** An install that
  downloads, verifies and imports before failing is worse than a refusal
  ([#388](https://github.com/wslkit/skrog/issues/388)).
- **`test/e2e` has run on this commit** — `gh workflow run e2e.yml` — and
  **every failure is triaged**, with none of them a regression introduced by
  this release. It is not a per-PR gate because it provisions a real engine,
  so it is the release manager's job to fire it and read it.

  "Green" is the goal and not the bar, and the distinction is deliberate
  rather than a loophole. The suite drives 45 stages against real Windows,
  real WSL2 and the public internet; some stages skip when an optional tool is
  absent, and a pre-existing bug in a rarely-used path should not hold a
  release hostage. **What is not acceptable is a red run nobody looked at.**
  Each failure gets an issue, a provenance call — regression or pre-existing —
  and a line in the changelog's Known issues. 0.6.0 shipped that way for
  [#429](https://github.com/wslkit/skrog/issues/429), and the reasoning is
  written in that issue so the next person can judge whether it was right.

  A failure the release *did* introduce blocks, with no judgement call
  available.
- **Code scanning is clean**: no open CodeQL alerts, `govulncheck` green.

### Semver, while we are pre-1.0

`0.x` means the surface can still move. What each bump promises:

- **Patch** (`0.6.1`) — fixes only. No new flags, no new config keys, no
  change to what an existing command does on success.
- **Minor** (`0.6.0`) — new commands, flags and config keys, and **behaviour
  changes are allowed here**, including ones that turn a previously working
  call into a refusal. v0.6.0 had several: policy now judges pull and push,
  the machine layer can only be tightened, and `upgrade --apply` refuses
  inside a package-manager directory. Each needs a CHANGELOG line under
  **Changed** or **Removed**, not **Added**.
- **Major** — reserved for 1.0, which additionally commits to a stable CLI
  surface. [docs/cli-json.md](docs/cli-json.md) is the only compatibility
  contract that exists today, and it covers the JSON and the exit codes, not
  the flags or the on-disk state.

## Publishing to winget

winget does **not** wait on code signing, which this file claimed for a long
time and which was simply wrong. The `portable` installer type unpacks a zip
per-user under `%LOCALAPPDATA%\Microsoft\WinGet\Packages\` and drops an alias
symlink in `...\WinGet\Links\` (which is on PATH). No elevation, no MSI, no
Authenticode. ripgrep and fzf ship exactly this way, unsigned. SmartScreen still
warns on first run of the exe — that part is real, and is [#77](https://github.com/wslkit/skrog/issues/77).

Manifests live in [`microsoft/winget-pkgs`](https://github.com/microsoft/winget-pkgs)
under `manifests/w/wslkit/skrog/<version>/`, three files: `wslkit.skrog.yaml`
(version), `wslkit.skrog.locale.en-US.yaml` (metadata), and
`wslkit.skrog.installer.yaml` (URLs and SHA256 per architecture). Each release
is a new PR adding a new version directory; existing versions are never edited.

Per release, after the app tag has published its assets:

1. Take the amd64 and arm64 hashes from the release's `SHA256SUMS` and put them
   in the installer manifest, along with the new `InstallerUrl`s, `PackageVersion`,
   `ReleaseDate` and `ReleaseNotesUrl`.
2. `winget validate --manifest <dir>` — catches schema errors only.
3. **Install it and drive it**, which validation does not do:
   ```powershell
   winget settings --enable LocalManifestFiles   # once, elevated
   winget install --manifest <dir> --scope user
   ```
   Then run the binary **through the alias symlink**, not the package directory,
   because that is what a user gets and it is a different code path:
   `skrog version`, and `skrog autostart enable` — the latter derives
   `skrogw.exe` as a sibling and is the one that broke under winget
   ([#360](https://github.com/wslkit/skrog/issues/360)). Uninstall afterwards.
4. Open the PR against `microsoft/winget-pkgs`. Automated validation runs on it;
   review is typically one to two weeks.

Step 3 is not optional ceremony. 0.5.0 passed `winget validate` cleanly and was
still broken under a real winget install — the fix for #360 had landed one
commit *after* the tag, so the artifact being packaged predated it. Validation
checks the manifest; only installing checks the product.

## What is not in place yet

- **Code signing.** No longer tied to a release number. The SignPath
  Foundation's free programme declined for now (it wants an established user
  base) and invited a reapplication as visibility grows; paying for a
  certificate is the other route and waits on nobody. So this is a decision to
  take, not a milestone to schedule — [#77](https://github.com/wslkit/skrog/issues/77).
  Until then binaries are unsigned and SmartScreen warns. `SHA256SUMS`, SLSA
  provenance and the cosign bundle are published so a download can be verified,
  which is not a substitute.
- **MSI / choco packaging.** These install machine-wide and ask for elevation,
  where an unsigned installer really is a worse experience than a zip, so they
  do wait on the signing decision.
- No release currently updates `manifest.json` automatically; step 2 above is
  deliberately a reviewed commit, because it changes what every install fetches.
