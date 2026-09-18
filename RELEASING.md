# Releasing

Two independent release streams, deliberately kept apart:

| stream | tag | what it publishes | workflow |
|---|---|---|---|
| **rootfs** | `rootfs-vX.Y.Z` | the engine tarball, its `.sha256`, and an SPDX SBOM | `rootfs.yml` |
| **app** | `vX.Y.Z` | `skrog.exe` for amd64 and arm64, zipped, plus `SHA256SUMS` | `release.yml` |

They are separate because the engine and the app version independently: an engine
security patch should not require an app release, and vice versa (PLAN §04,
`skrog engine upgrade`). Both workflows check the tag prefix, so a rootfs
release never gets app binaries attached and an app release never gets a rootfs.

Deciding *what* to bump — which moby tag, whether containerd/runc/buildkit move
with it, where each checksum comes from — is a separate job from cutting the
release. That is [docs/bumping-upstream.md](docs/bumping-upstream.md); this file
covers publishing once the versions are settled.

## `git describe` sees both streams

The two tag namespaces share one repository, and rootfs releases are cut far
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
   the three assets. (The first release predates the scheme and is tagged bare
   `rootfs-v29.7.2`.)
2. **Copy the published checksum into the manifest.** Take the value from the
   uploaded `.sha256` and put it in `manifest.json` under
   `engines[].rootfs.sha256`, confirming the `url` matches the tag you used.
   Merge that as a normal PR — a test asserts the manifest agrees with
   `versions.env`.
3. **Cut the app release.** Tag `v<app-version>`. `release.yml` builds both
   architectures, asserts the version stamp actually took, packages, and
   attaches the zips with `SHA256SUMS`.

Until step 2 lands, `skrog install` refuses with a message telling the user to
pass `--rootfs-url` and `--rootfs-sha256` explicitly. That is intentional: there
is no code path that installs an unverified rootfs.

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
   | [scoop-skrog](https://github.com/wslkit/scoop-skrog) | bump `version`, both URLs and both hashes in `bucket/skrog.json`. `checkver`/`autoupdate` are configured, so `scoop bucket` tooling can do it, but **nothing runs automatically** — cut a release and forget this and the bucket silently serves the previous version |
   | [winget](#publishing-to-winget) | new manifest directory under `manifests/w/wslkit/skrog/<version>/` |
   | [setup-skrog](https://github.com/wslkit/setup-skrog) | only if the action pins a version |
   | [skrog-vscode](https://github.com/wslkit/skrog-vscode) | only if it pins one |

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
- **`test/e2e` has run green** on this commit — `gh workflow run e2e.yml`.
  It is not a per-PR gate because it provisions a real engine, so it is the
  release manager's job to fire it and read it.
- **Code scanning is clean**: no open CodeQL alerts, `govulncheck` green.

### Semver, while we are pre-1.0

`0.x` means the surface can still move. What each bump promises:

- **Patch** (`0.6.1`) — fixes only. No new flags, no new config keys, no
  change to what an existing command does on success.
- **Minor** (`0.6.0`) — new commands, flags and config keys, and **behaviour
  changes are allowed here**, including ones that turn a previously working
  call into a refusal. v0.6.0 has several: policy now judges pull and push,
  the machine layer can only be tightened, `upgrade --apply` refuses inside a
  package-manager directory, and wslc refuses to serve with WSL plugins
  registered. Each needs a CHANGELOG line under **Changed** or **Removed**,
  not **Added**.
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
