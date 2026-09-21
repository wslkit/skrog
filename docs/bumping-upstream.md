# Bumping upstream: the engine and the docker CLI

Skrog pins every upstream byte it ships. Nothing is ever fetched as "latest" —
that is the determinism promise in PLAN §04, and breaking it once costs the
wedge market. So a version bump is a reviewed commit, and this is what one
involves.

There are **two independent streams**. They are often confused because both
carry a Docker version number, and they can legitimately differ:

| stream | what it is | pinned in |
| --- | --- | --- |
| **engine** | `dockerd` and friends, built from source into the rootfs | `guest/rootfs/versions.env` + `internal/release/manifest.json` |
| **docker CLI** | the `docker.exe` `skrog cli install` puts on PATH, plus compose, buildx, the credential helper | `internal/dockercli/manifest.json` |

A newer CLI talking to an older daemon is supported — the API negotiates — but
a large gap is worth closing, because the acceptance suite only validates the
combination that is pinned.

---

## The engine

### 1. Find the values

The tag and its **commit** are both pinned (#88): a git tag can be moved and a
source repo compromised, and the result runs as root in every user's engine VM.
`build-engine.sh` fails the build on mismatch.

```sh
gh api repos/moby/moby/releases --jq '[.[] | select(.prerelease==false)][0].tag_name'
gh api repos/moby/moby/commits/docker-v29.8.0 --jq .sha
```

**Verify the method before trusting it.** Resolve the tag that is *already*
pinned and confirm it reproduces the `MOBY_SHA` in the file. If it does not,
the query is wrong — `git/ref/tags/<tag>` returns the annotated tag object's
SHA, which is *not* the commit, and pinning it would fail every build.

### 2. Check the Go floor, do not assume it

```sh
gh api "repos/moby/moby/contents/go.mod?ref=docker-v29.8.0" --jq .content | base64 -d | grep '^go '
```

`GO_VERSION` in `versions.env` must be at or above that. It is pinned because
the toolchain changes the output bytes, so raise it deliberately, not reflexively.

### 3. Decide about containerd, runc and buildkit — deliberately

moby's own `Dockerfile` carries `ARG CONTAINERD_VERSION` / `ARG RUNC_VERSION`.
**These are informational, not authoritative.** They describe moby's dev
container and CI, and Skrog has never simply mirrored them: at 29.7.2, moby's
Dockerfile said containerd `v2.3.3` while Skrog shipped `v2.1.4` — on purpose.

The rule in `versions.env` is the one that governs: *the pinned set is a tested
combination*, and `skrog engine upgrade` refuses combinations outside the
tested matrix. So:

- Bumping moby alone is normal and is usually the right change.
- Moving containerd/runc/buildkit is a **separate decision** with its own
  justification (a CVE, a bug you need fixed, a moby release that genuinely
  requires it) — never "GitHub lists a newer tag".
- Any component you do move needs its `*_SHA` updated in the same commit.

### 4. Edit `versions.env`

```
ENGINE_VERSION=29.8.0
MOBY_TAG=docker-v29.8.0
MOBY_SHA=<the commit from step 1>
ROOTFS_REVISION=1     # reset to 1 on an engine bump; incremented for a
                      # rootfs-only content change
```

### 5. Edit `internal/release/manifest.json` in the same commit

`TestManifestAgreesWithVersionsEnv` asserts that the **default** engine entry
matches `versions.env` — engine version and every component. A `versions.env`
bump on its own does not compile past the tests, by design.

Add the new engine as `"default": true`, with the URL the release *will* have,
and `"sha256": ""`. **Keep the previous engine listed** with its checksum
intact: that is what `skrog engine list` offers and what `skrog engine
rollback` returns to. Dropping it would strand anyone who needs to go back.

The empty checksum is deliberate and temporary. While it is empty, `skrog
install` refuses and tells the user to pass `--rootfs-url` / `--rootfs-sha256`
rather than installing something unverified — there is no code path that
installs an unverified rootfs.

### 6. Open the PR and let CI build it

`rootfs.yml` runs on any pull request touching `guest/rootfs/**`. It builds the
engine from source and runs `smoke-test.sh`, `boot-test.sh` and
`reference-diff.sh`. Those last two exist because a rootfs whose engine could
not start reached a published release **twice** — a green build alone has
already proven insufficient here, so treat the boot test as the real gate.

It does all of that **twice**, once per architecture, on native amd64 and
arm64 runners (#388). So a bump has to build, boot and match Docker's
reference bundle on both. Read both jobs: an upstream tag that compiles on
x86-64 and not on aarch64 is a real possibility, and `fail-fast: false` is set
so the passing one still tells you which it is.

### 7. Release, then fill in the checksum

Follow [RELEASING.md](../RELEASING.md): tag `rootfs-v<engine>-<revision>` (here
`rootfs-v29.8.0-1`), let the workflow publish, then copy the value from the
published `.sha256` asset into `manifest.json` and merge that as a normal PR.

Finally, exercise `skrog engine upgrade` from the previous version and
`skrog engine rollback` back to it, confirming data survives both.

---

## The docker CLI

`internal/dockercli/manifest.json` pins the CLI, compose, buildx and the
credential helper — each by version, per-arch URL and sha256.

Where the checksums come from differs per project, and the file's own `comment`
block is the reference:

- **compose** — `checksums.txt` on the release
- **buildx** — `checksums-signed.txt` (the one that covers Windows)
- **wincred** — `checksums.txt`
- **docker CLI** — the exception: `download.docker.com` publishes no checksum
  at all, so the amd64 digest is computed from the official download and pinned
  here. Every install still verifies exact bytes.

Windows **arm64** has no upstream static `docker.exe`. Its entry stays a
placeholder with an empty sha256; `Published()` reads that as "not available on
this platform" and the installer skips it with an honest message. Do not invent
a URL to fill the hole.

To compute the CLI digest:

```sh
curl -fsSLO https://download.docker.com/win/static/stable/x86_64/docker-29.8.0.zip
sha256sum docker-29.8.0.zip
```

The CLI can be bumped on its own — it is a separate stream — but if it moves
well ahead of the engine, open an issue to bring the engine along rather than
letting the two drift silently.

---

## Checklist

- [ ] Tag and commit SHA resolved, and the method verified against the existing pin
- [ ] Go floor checked against moby's `go.mod`
- [ ] containerd / runc / buildkit: moved with a stated reason, or deliberately left alone
- [ ] `versions.env` updated, `ROOTFS_REVISION` reset to 1
- [ ] `manifest.json`: new default entry with empty sha256, previous engine retained
- [ ] `go test ./internal/release/` passes
- [ ] PR green, including `rootfs.yml`'s boot test and reference diff
- [ ] Rootfs release tagged and published
- [ ] Published checksum copied into `manifest.json` and merged
- [ ] `engine upgrade` and `rollback` verified with real data
