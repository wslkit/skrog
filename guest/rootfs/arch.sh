# shellcheck shell=bash
# Architecture naming for the rootfs, shared by build.sh and the four scripts
# that check its output (#388).
#
# Source this AFTER versions.env: rootfs_name is built from ENGINE_VERSION and
# ROOTFS_REVISION.
#
# Why one file rather than a case statement in each script: the tarball's name
# has to be derived identically in five places, and the three ecosystems
# involved spell architectures three different ways. Alpine and Docker's static
# bundles say x86_64/aarch64, Go and Docker's platform strings say amd64/arm64,
# and `uname -m` agrees with Alpine. A copy of that mapping per script is a
# silent name mismatch waiting to happen -- the failure mode is a check that
# says "missing skrog-rootfs-...tar.gz" about a file that was built under a
# slightly different name.
#
# ROOTFS_ARCH is the Go spelling, because that is what `skrog install` will
# compare against runtime.GOARCH, and it ends up in the filename.

if [ -z "${ROOTFS_ARCH:-}" ]; then
    # Derived from the machine, not passed in. Every component is compiled
    # natively (the build runs upstream sources through docker on this host),
    # so the host IS the target; a flag would only add a way for the name and
    # the bytes to disagree. Set ROOTFS_ARCH explicitly if that ever stops
    # being true.
    case "$(uname -m)" in
        x86_64)        ROOTFS_ARCH=amd64 ;;
        aarch64|arm64) ROOTFS_ARCH=arm64 ;;
        *)
            echo "unsupported build architecture: $(uname -m)" >&2
            echo "  the rootfs is built natively, so this script has to run on" >&2
            echo "  an x86_64 or aarch64 Linux host." >&2
            exit 1
            ;;
    esac
fi

case "$ROOTFS_ARCH" in
    amd64) ARCH_ALPINE=x86_64  ;;
    arm64) ARCH_ALPINE=aarch64 ;;
    *)
        echo "ROOTFS_ARCH=$ROOTFS_ARCH is not one of amd64, arm64" >&2
        exit 1
        ;;
esac
# Docker's static bundles use Alpine's spelling too
# (download.docker.com/linux/static/stable/<arch>/).
# shellcheck disable=SC2034  # read by the scripts that source this file
ARCH_DOCKER_STATIC="$ARCH_ALPINE"

rootfs_version="${ENGINE_VERSION}-${ROOTFS_REVISION}"

# The architecture is IN the filename, for both architectures, from here on.
#
# Releases up to and including rootfs-v29.8.1-1 are named without it and stay
# that way -- a published asset's name is a fact, and internal/release/
# manifest.json points at those bytes. But an unsuffixed name that silently
# means amd64 is the exact ambiguity #388 is about: it let an amd64 rootfs be
# offered to an arm64 machine with nothing in the name to contradict it.
rootfs_name="skrog-rootfs-${rootfs_version}-${ROOTFS_ARCH}"
# shellcheck disable=SC2034  # read by the scripts that source this file
rootfs_tarball_name="${rootfs_name}.tar.gz"
