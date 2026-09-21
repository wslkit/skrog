#!/usr/bin/env bash
# Compare the binaries we ship against Docker's own static bundle.
#
# Skrog reimplements the packaging that Docker already does, so Docker's
# bundle is the reference for what a working engine needs. Comparing against it
# is how `docker-init` was found — after `docker-proxy` had already shipped
# missing and broken the first release. Discovering these one crash at a time is
# avoidable: the reference is published and stable.
#
# The bundle is downloaded for comparison only. Skrog builds its binaries from
# source (PLAN §04); nothing here ends up in the artifact.
#
#   ./reference-diff.sh <out-dir>
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="${1:?usage: reference-diff.sh <out-dir>}"
# shellcheck source=versions.env
. "$here/versions.env"
# shellcheck source=arch.sh
. "$here/arch.sh"

tarball="$out/$rootfs_tarball_name"
test -f "$tarball" || { echo "missing $tarball"; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "==> what Docker ships in its ${ENGINE_VERSION} ${ARCH_DOCKER_STATIC} static bundle"
# Per architecture: Docker's aarch64 bundle is not guaranteed to hold the same
# set of binaries as the x86_64 one, and "the reference for what a working
# engine needs" is only a reference if it is the reference for THIS engine.
bundle_url="https://download.docker.com/linux/static/stable/${ARCH_DOCKER_STATIC}/docker-${ENGINE_VERSION}.tgz"
if ! curl -fsSL "$bundle_url" -o "$work/bundle.tgz"; then
    # A pinned engine version with no published bundle is possible (a release
    # candidate, say). Skip rather than fail: this check is a safety net, not a
    # gate on Docker's publishing schedule.
    echo "  no static bundle published for ${ENGINE_VERSION}; skipping the comparison"
    exit 0
fi
tar -tzf "$work/bundle.tgz" | sed 's|^docker/||' | grep -v '^$' | sort > "$work/reference.txt"
sed 's|^|  |' "$work/reference.txt"

echo "==> what we ship"
mkdir -p "$work/rootfs"
tar -xzf "$tarball" -C "$work/rootfs" ./usr/local/bin 2>/dev/null || \
    tar -xzf "$tarball" -C "$work/rootfs"
find "$work/rootfs/usr/local/bin" -maxdepth 1 -type f -printf '%f\n' | sort > "$work/ours.txt"
sed 's|^|  |' "$work/ours.txt"

echo "==> binaries Docker ships that we do not"
# `docker` itself is deliberately absent: the CLI is a Windows-side concern,
# bundled next to skrog.exe rather than inside the Linux rootfs (PLAN §04).
missing=""
while read -r b; do
    case "$b" in
        docker) continue ;;
    esac
    if ! grep -qx "$b" "$work/ours.txt"; then
        missing="$missing $b"
    fi
done < "$work/reference.txt"

if [ -n "$missing" ]; then
    echo "  MISSING:$missing"
    echo ""
    echo "Docker ships these and we do not. Each omission has already cost a"
    echo "broken release once (docker-proxy, docker-init), so this is a failure"
    echo "rather than a warning. If an omission is deliberate, add it to the"
    echo "exclusion list above with the reason."
    exit 1
fi
echo "  none - we ship everything Docker does (plus buildkit)"

echo "reference diff PASSED"
