# Engine upgrades and rollback

An engine security patch should not have to wait for a Skrog release, and
taking one should not cost you your images. `skrog engine` does both:

```
skrog engine list                 # what this build can install, and what is installed
skrog engine upgrade              # move to the manifest's default engine
skrog engine upgrade --to 29.8.0  # a specific one
skrog engine rollback             # back to the engine installed before the last upgrade
```

Every step is checksum-mandatory and reversible, and none of it touches your
data.

## What an upgrade actually does

It replaces the **engine binaries** — `dockerd`, `containerd`,
`containerd-shim-runc-v2`, `ctr`, `runc`, `buildkitd`, `buildctl`,
`docker-proxy`, `docker-init`, `nvidia-cdi-hook`, and Skrog's own
`skrog-agent` — out of a rootfs tarball verified against its published
SHA-256, and leaves the filesystem they live on alone.

**And the QEMU emulators**, `qemu-aarch64` and `qemu-x86_64`, which live in
`/usr/bin` rather than beside the engine binaries. They are listed separately
because leaving them out is not hypothetical: the extractor filtered on one
directory, so for every existing install an upgrade delivered no emulator and
[foreign-architecture containers](docker-cli.md#running-a-foreign-architecture-container)
stayed unreachable ([#479](https://github.com/wslkit/skrog/issues/479)). A
tarball that carries no emulator — anything before `29.8.1-3` — simply has that
step skipped.

That is the whole trick: **`/var/lib/docker` never moves.** Images, containers,
volumes and build cache are not exported, re-imported or migrated, because
nothing asks them to be.

```
$ skrog engine upgrade --to 29.8.0
engine upgraded: 29.8.1-3 -> 29.8.0-2 (dockerd 29.8.0)
  11 binaries replaced; images, containers and volumes untouched
  `skrog engine rollback` returns to 29.8.1-3
```

The count is what was actually replaced, so it moves with the tarball: the
agent has had two names across the rename ([#1](https://github.com/wslkit/skrog/issues/1))
and the extractor takes whichever is present, so an older image replaces one
fewer.

The **rootfs revision** is the unit of upgrade (`29.7.2-4`, not `29.7.2`): two
revisions can carry the same dockerd and differ in everything else around it —
revision 4, for instance, is where `nvidia-cdi-hook` arrived (#139).

### Why not swap the whole distro

Importing the new rootfs side by side and switching to it reads better and
loses your data: a fresh rootfs has an empty `/var/lib/docker`. Keeping the
data would mean either exporting and re-importing the engine's entire data set
(tens of gigabytes, every upgrade) or moving `/var/lib/docker` onto a separate
virtual disk — a change to the install layout, which deserves its own decision
rather than arriving as a side effect of an upgrade command.

### What it deliberately does not touch

Only the binaries listed above. The rootfs also carries Alpine's own userland
(`socat`, `iptables`, `ca-certificates`…), and an upgrade that quietly replaced
those would be a distro upgrade wearing an upgrade's clothes. It also does not
*remove* anything: rolling back to a revision that predates a file leaves that
file in place, which is why a downgrade from revision 4 keeps
`nvidia-cdi-hook` sitting there — inert unless a CDI spec references it.

## Failure leaves you with a working engine

If the new engine does not start, or starts and never answers, the previous
binaries go back in and the engine is started again **before** the failure is
reported:

```
$ skrog engine upgrade --rootfs-url file:///C:/tmp/broken.tar.gz --rootfs-sha256 …
skrog: engineupgrade: the new engine did not start: dockerd did not create
/var/run/docker.sock within 1m0s (see `skrog logs --source dockerd`); rolled back to
29.7.2-4, which is running
```

Exit code 1, engine up, data intact. The install manifest still records the
engine that is actually in the distro — a failed upgrade does not leave a
manifest describing an engine you do not have.

## Rollback

`skrog engine rollback` returns to the ref recorded before the last upgrade.
It is the same swap in the other direction, from the same verified source:

```
$ skrog engine rollback
engine rolled back: 29.7.2-3 -> 29.7.2-4 (dockerd 29.7.2)
  11 binaries replaced; images, containers and volumes untouched
```

Rootfs tarballs stay in the state directory's `rootfs/` cache, so a rollback
normally re-extracts rather than re-downloads — useful on a runner with no
outbound access at the moment you need it. If the cache was cleared, the
download is the same checksum-mandatory path as an install, so a rollback does
need the network then.

There is exactly **one** rollback point: the engine you were on before the last
upgrade. For anything further back, `skrog engine upgrade --to <ref>` names a
version directly, and `skrog snapshot` covers the case where you want the
whole engine *state* back too ([snapshots.md](snapshots.md)).

## Only the tested matrix

`--to` accepts what this build's manifest lists, and nothing else — that set
*is* the tested matrix. An engine nobody tested against this Skrog is not an
upgrade, it is an experiment, and `skrog engine list` shows exactly what is on
offer:

```
$ skrog engine list
engines this build can install:
  29.8.1-3       dockerd 29.8.1  (default, installed)
  29.8.0-2       dockerd 29.8.0
  29.7.2         dockerd 29.7.2
rollback target: 29.8.0-2 (`skrog engine rollback`)
```

For development there is `--rootfs-url` with `--rootfs-sha256` (both, always —
there is no path that installs an unverified rootfs), which is how a rootfs you
built yourself gets in.

## `--dry-run`

```
$ skrog engine upgrade --to 29.8.0 --dry-run
would move the engine from 29.8.1-3 to 29.8.0-2:
  fetch and verify https://github.com/…/skrog-rootfs-29.8.0-2.tar.gz
  extract 12 engine binaries and the emulator, if the image carries one
  stop the engine
  replace them in skrog-engine:/usr/local/bin and /usr/bin
  start the engine and confirm it answers
  on failure: restore 29.8.1-3 and start the engine again
```

## See also

- [snapshots.md](snapshots.md) — engine *state* save/restore, which is a
  different axis from the engine *version*
- [housekeeping.md](housekeeping.md) — `skrog prune` and `skrog compact`
- [security.md](security.md) — how the rootfs is verified, and how to check a
  download's signature yourself
