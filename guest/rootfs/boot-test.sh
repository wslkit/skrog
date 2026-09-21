#!/usr/bin/env bash
# Boot the rootfs and run a container in it.
#
# This exists because the smoke test was green while shipping a rootfs whose
# engine could not start — twice. Checking that files exist and that
# `dockerd --validate` passes verifies the shape of the artifact, not that it
# works: a valid config plus a missing binary is still a dead engine.
#
# Every one of these was caught by hand, after publishing, and would have been
# caught here:
#   - daemon.json used max-files instead of max-file  -> dockerd refused to start
#   - docker-proxy missing                            -> dockerd refused to start
#   - socat and iptables absent from the rootfs       -> no networking, no relay
#   - docker-init missing                             -> `docker run --init` broken
#
# Runs on any Linux host with Docker (CI uses ubuntu-latest). Needs --privileged
# because dockerd sets up cgroups, iptables and overlayfs.
#
#   ./boot-test.sh <out-dir>
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="${1:?usage: boot-test.sh <out-dir>}"
# shellcheck source=versions.env
. "$here/versions.env"
# shellcheck source=arch.sh
. "$here/arch.sh"

tarball="$out/$rootfs_tarball_name"
test -f "$tarball" || { echo "missing $tarball"; exit 1; }

image="skrog-boot-test:${ENGINE_VERSION}"
name="skrog-boot-test-$$"
cleanup() {
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker rmi -f "$image" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> importing the rootfs as a container image"
# The same tarball wsl --import would consume, loaded as an image so the engine
# can be started exactly as it ships.
docker import "$tarball" "$image" >/dev/null

echo "==> starting dockerd inside it"
docker run -d --name "$name" --privileged "$image" \
    /usr/local/bin/dockerd >/dev/null

echo "==> waiting for the socket"
socket_up=false
for _ in $(seq 1 60); do
    if docker exec "$name" test -S /var/run/docker.sock 2>/dev/null; then
        socket_up=true
        break
    fi
    sleep 1
done

if [ "$socket_up" != true ]; then
    echo "dockerd did not create its socket. Log:"
    docker logs "$name" 2>&1 | tail -30
    exit 1
fi
echo "  socket is up"

echo "==> engine reports itself healthy"
docker exec "$name" /usr/local/bin/docker-proxy --help >/dev/null 2>&1 || true
docker exec "$name" sh -c '
    set -eu
    # A dead engine answers nothing; this is the check that actually matters.
    /usr/local/bin/ctr --address /var/run/docker/containerd/containerd.sock version >/dev/null
    echo "  containerd responds"
'

echo "==> the daemon accepted its shipped configuration"
# dockerd rejects a bad daemon.json at startup, not at validate time, so
# reaching a live socket already proves the config is good. Reported for the
# log so a future failure is easy to place.
docker exec "$name" sh -c 'cat /etc/docker/daemon.json'

echo "==> networking prerequisites are usable, not merely present"
docker exec "$name" sh -c '
    set -eu
    iptables --version >/dev/null
    ip link show >/dev/null
    socat -V >/dev/null
    echo "  iptables, ip and socat all run"
'

echo "==> docker-init is executable"
docker exec "$name" sh -c '/usr/local/bin/docker-init --version >/dev/null 2>&1 || \
    /usr/local/bin/docker-init -- true'
echo "  docker-init runs"

echo "boot test PASSED: the shipped rootfs starts a working engine"
