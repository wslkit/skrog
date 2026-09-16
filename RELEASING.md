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

## What is not in place yet

- **Code signing.** No longer tied to a release number. The SignPath
  Foundation's free programme declined for now (it wants an established user
  base) and invited a reapplication as visibility grows; paying for a
  certificate is the other route and waits on nobody. So this is a decision to
  take, not a milestone to schedule — [#77](https://github.com/wslkit/skrog/issues/77).
  Until then binaries are unsigned and SmartScreen warns. `SHA256SUMS`, SLSA
  provenance and the cosign bundle are published so a download can be verified,
  which is not a substitute.
- **winget / scoop / choco manifests.** Behind the same decision: an unsigned
  installer that asks for elevation is worse than a zip.
- No release currently updates `manifest.json` automatically; step 2 above is
  deliberately a reviewed commit, because it changes what every install fetches.
