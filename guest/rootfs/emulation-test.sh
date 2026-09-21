#!/usr/bin/env bash
# Prove the shipped rootfs can actually run a foreign-architecture container
# under emulation (#462).
#
#   ./emulation-test.sh <out-dir>
#
# This is the test the Windows side cannot do. skrog's e2e suite runs on
# windows-latest, which is amd64, and hosted arm64 Windows runners cannot
# start WSL2 at all (no nested virtualization, measured in #388). But the
# rootfs builds natively on BOTH architectures here, so this job proves both
# directions for real:
#
#   on ubuntu-24.04      amd64 rootfs runs an arm64 container
#   on ubuntu-24.04-arm  arm64 rootfs runs an amd64 container
#
# Nothing here is emulated twice over or approximated: each run is the real
# rootfs, the real dockerd started out of it, the real qemu binary it ships,
# and a real image for the other architecture.
#
# What this does NOT cover, and must not be read as covering: registration
# inside a WSL2 utility VM, surviving `wsl --shutdown`, or skrog's supervisor
# doing the registering. Those are Windows-side and belong in test/e2e.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="${1:?usage: emulation-test.sh <out-dir>}"
# shellcheck source=versions.env
. "$here/versions.env"
# shellcheck source=arch.sh
. "$here/arch.sh"

# Pinned by digest, not tag. This image is the measuring instrument: if it
# moved, a failure here would look like an emulation bug and cost an
# afternoon. alpine:3.21, multi-arch, ~4 MB per platform.
test_image="alpine@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507"

tarball="$out/$rootfs_tarball_name"
test -f "$tarball" || { echo "missing $tarball"; exit 1; }

reg="$here/binfmt-$ARCH_EMULATE.reg"
test -f "$reg" || { echo "missing $reg"; exit 1; }

image="skrog-emulation-test:${ENGINE_VERSION}"
name="skrog-emulation-test-$$"

# The engine is driven from THIS host's docker CLI over a shared socket,
# because the rootfs deliberately contains no docker CLI -- PLAN §04 keeps the
# client a Windows-side concern, bundled beside skrog.exe rather than inside
# the Linux image. `docker exec <engine> docker ...` is therefore exit 127,
# which is how the first version of this script failed.
sockdir="$(mktemp -d)"
engine() { docker -H "unix://$sockdir/docker.sock" "$@"; }

# Deregistering is part of cleanup, and it is not optional politeness.
#
# MEASURED, 2026-09-21: a binfmt_misc registration made inside a privileged
# container is NOT scoped to that container. It lands in the kernel, and on
# WSL2 that kernel is shared by every distro in the utility VM -- so
# registering here was visible from an unrelated Ubuntu distro, and it
# survived the container exiting. The F flag makes that outlive the container
# usefully rather than harmlessly: the interpreter is loaded into the kernel
# at registration time, so it keeps working after the binary that provided it
# is gone.
#
# This script runs on ephemeral CI runners, where leaking would not matter,
# and on developer machines, where it would. Clean up on both.
dereg() {
    docker exec "$name" sh -c \
        'for h in /proc/sys/fs/binfmt_misc/qemu-*; do [ -e "$h" ] && echo -1 > "$h"; done' \
        >/dev/null 2>&1 || true
}
cleanup() {
    dereg
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker rmi -f "$image" >/dev/null 2>&1 || true
    rm -rf "$sockdir"
}
trap cleanup EXIT

echo "==> host is $ROOTFS_ARCH; this rootfs should be able to emulate $ARCH_EMULATE"

docker import "$tarball" "$image" >/dev/null

echo "==> the emulator ships and runs"
docker run --rm "$image" "$QEMU_BIN" -version | head -1 | sed 's/^/  /'

echo "==> and nothing is registered out of the box"
# Emulation is opt-in. A rootfs that registered an interpreter by itself would
# be making a performance decision for the user, silently. This is what keeps
# "opt-in" true rather than merely intended.
if docker run --rm --privileged "$image" \
       sh -c 'ls /proc/sys/fs/binfmt_misc/ 2>/dev/null' | grep -q qemu; then
    echo "FATAL: the rootfs ships a live binfmt registration; emulation must be opt-in" >&2
    exit 1
fi
echo "  nothing registered, as intended"

echo "==> starting dockerd out of the rootfs"
docker run -d --name "$name" --privileged -v "$sockdir:/shared" "$image" \
    /usr/local/bin/dockerd -H unix:///shared/docker.sock >/dev/null
for _ in $(seq 1 60); do
    [ -S "$sockdir/docker.sock" ] && break
    sleep 1
done
[ -S "$sockdir/docker.sock" ] || {
    echo "dockerd did not create its socket. Log:" >&2
    docker logs "$name" 2>&1 | tail -30 >&2
    exit 1
}
# dockerd makes the socket root-only; this script is not necessarily root.
docker exec "$name" chmod 666 /shared/docker.sock
engine version --format '  engine {{.Server.Version}} via {{.Server.Os}}/{{.Server.Arch}}'

# Pulled as its own step so a network failure is a network failure, and not
# mistaken for the negative control below succeeding.
echo "==> pulling a $ARCH_EMULATE image into that engine"
engine pull -q --platform "linux/$ARCH_EMULATE" "$test_image" >/dev/null
engine image inspect "$test_image" --format '  pulled architecture: {{.Architecture}}'

echo "==> it does NOT run yet"
# The negative control, and the reason this test is worth anything. Without
# it, green could mean "the rootfs made emulation work" or "the kernel already
# had a handler and the rootfs contributed nothing".
if engine run --rm --platform "linux/$ARCH_EMULATE" "$test_image" /bin/true 2>/dev/null; then
    echo "FATAL: a $ARCH_EMULATE container ran with no interpreter registered." >&2
    echo "  Either this kernel already had one -- in which case this test proves" >&2
    echo "  nothing about the rootfs -- or the image is not really $ARCH_EMULATE." >&2
    exit 1
fi
echo "  exec format error, as it should be"

echo "==> registering $QEMU_BIN"
# The registration line is piped in as bytes rather than interpolated through
# a shell. It is full of backslash escapes that binfmt_misc parses itself, and
# every layer of quoting between here and the kernel is a chance to eat one.
#
# The F flag is the substance of it: it loads the interpreter into the kernel
# at registration time, so it still resolves inside a container whose mount
# namespace has no /usr/bin/qemu-*. Without F the registration appears to
# succeed and every emulated exec then fails with ENOENT -- the most common
# way to get this wrong, and one that looks like a qemu bug.
docker exec "$name" sh -c \
    'grep -q binfmt_misc /proc/mounts || mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc'
docker exec -i "$name" sh -c 'cat > /proc/sys/fs/binfmt_misc/register' < "$reg"
docker exec "$name" sh -c 'cat /proc/sys/fs/binfmt_misc/qemu-*' | sed 's/^/  /'

echo "==> the same container now runs"
got=$(engine run --rm --platform "linux/$ARCH_EMULATE" "$test_image" uname -m)
echo "  uname -m in a linux/$ARCH_EMULATE container: $got"
case "$ARCH_EMULATE:$got" in
    arm64:aarch64|amd64:x86_64) ;;
    *) echo "FATAL: expected a $ARCH_EMULATE machine, got '$got'" >&2; exit 1 ;;
esac

echo "==> and a native container is still native"
# The half everyone forgets. A registration whose mask also matched the host's
# own ELFs would route every native binary through the emulator, and a test
# that only checked the foreign case would still be green.
engine pull -q "$test_image" >/dev/null
native=$(engine run --rm "$test_image" uname -m)
echo "  uname -m in a default container: $native"
case "$ROOTFS_ARCH:$native" in
    amd64:x86_64|arm64:aarch64) ;;
    *)
        echo "FATAL: the default platform is no longer native ($native on $ROOTFS_ARCH)." >&2
        echo "  A binfmt handler is intercepting binaries the CPU runs itself." >&2
        exit 1
        ;;
esac

echo "emulation test PASSED: $ROOTFS_ARCH rootfs runs $ARCH_EMULATE containers, natives unaffected"
