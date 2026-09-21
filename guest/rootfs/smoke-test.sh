#!/usr/bin/env bash
# Verify a built rootfs before it can become a release asset.
# Runs the engine binaries out of the tarball on the Linux CI host — this is
# not the full acceptance suite (that needs WSL2), just proof the artifact
# isn't broken: right shape, right versions, binaries actually execute.
#   ./smoke-test.sh <out-dir>
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="${1:?usage: smoke-test.sh <out-dir>}"
# shellcheck source=versions.env
. "$here/versions.env"
# shellcheck source=arch.sh
. "$here/arch.sh"

tarball="$out/$rootfs_tarball_name"
sbom="$out/${rootfs_name}.spdx.json"

echo "==> artifacts present"
test -f "$tarball" || { echo "missing $tarball"; exit 1; }
test -f "$tarball.sha256" || { echo "missing checksum"; exit 1; }
test -f "$sbom" || { echo "missing SBOM"; exit 1; }
(cd "$out" && sha256sum -c "$(basename "$tarball").sha256")
python3 -c "import json,sys; json.load(open('$sbom'))" && echo "SBOM parses as JSON"

echo "==> unpack"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
tar -xzf "$tarball" -C "$work"

echo "==> expected files"
# docker-proxy is listed deliberately: dockerd refuses to start without it when
# userland-proxy is enabled, and `dockerd --validate` does not catch that - the
# first rootfs release imported fine and then failed at startup.
for f in usr/local/bin/dockerd usr/local/bin/docker-proxy usr/local/bin/containerd \
         usr/local/bin/runc usr/local/bin/buildkitd usr/local/bin/skrog-agent \
         usr/local/bin/nvidia-cdi-hook \
         etc/docker/daemon.json etc/wsl.conf \
         etc/skrog/engine-version etc/skrog/agent-version etc/skrog/commits; do
    test -e "$work/$f" || { echo "missing $f"; exit 1; }
    echo "  ok $f"
done

echo "==> daemon.json is accepted by the engine, not merely valid JSON"
# `dockerd --validate` parses the config the way the real daemon does and exits.
# The earlier version of this check only asserted the JSON shape, which is why a
# bad log opt ("max-files" instead of "max-file") passed CI and was caught by
# hand in Spike A instead — dockerd refused to start with it. Never again.
python3 - "$work/etc/docker/daemon.json" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
assert cfg["log-opts"]["max-size"], "log rotation not configured"
print("  ok, parses:", cfg)
PY
"$work/usr/local/bin/dockerd" --validate --config-file "$work/etc/docker/daemon.json"

echo "==> binaries execute and report the pinned versions"
"$work/usr/local/bin/dockerd" --version
"$work/usr/local/bin/dockerd" --version | grep -q "$ENGINE_VERSION" \
    || { echo "dockerd version does not match $ENGINE_VERSION"; exit 1; }
"$work/usr/local/bin/containerd" --version
"$work/usr/local/bin/containerd" --version | grep -q "${CONTAINERD_VERSION#v}" \
    || { echo "containerd version does not match $CONTAINERD_VERSION"; exit 1; }
"$work/usr/local/bin/runc" --version
"$work/usr/local/bin/runc" --version | grep -q "${RUNC_VERSION#v}" \
    || { echo "runc version does not match $RUNC_VERSION"; exit 1; }
# Catches the "v0.0.0+unknown" trap: buildkit only reports a real version when
# built with the ldflags in build-engine.sh.
"$work/usr/local/bin/buildkitd" --version
"$work/usr/local/bin/buildkitd" --version | grep -q "${BUILDKIT_VERSION#v}" \
    || { echo "buildkitd version does not match $BUILDKIT_VERSION"; exit 1; }
# The one glibc binary (#139): static, so it runs here on the Debian-based CI
# host exactly as it will on the Alpine engine. It only has to exist and start
# — its presence is what routes `docker run --gpus all` to the CDI spec.
"$work/usr/local/bin/nvidia-cdi-hook" --version

echo "==> skrog-agent identity matches what the rootfs declares"
"$work/usr/local/bin/skrog-agent" -version
test "$("$work/usr/local/bin/skrog-agent" -version)" = "$(cat "$work/etc/skrog/agent-version")" \
    || { echo "agent-version file does not match the binary"; exit 1; }

echo "==> provenance: one pinned commit per component"
cat "$work/etc/skrog/commits"
for c in moby containerd runc buildkit; do
    grep -q "^$c " "$work/etc/skrog/commits" \
        || { echo "no commit recorded for $c"; exit 1; }
done

echo "==> licence text for everything we ship"
# The rootfs shipped eleven third-party binaries and zero licence files
# (#205). Apache-2.0 section 4(a) requires giving recipients a copy, so an
# empty licenses tree is a release blocker, not a cosmetic gap -- and the only
# reason it went unnoticed is that nothing checked.
for c in moby containerd runc buildkit skrog-agent; do
    ls "$work/usr/share/licenses/$c/"* >/dev/null 2>&1 \
        || { echo "no licence shipped for $c"; exit 1; }
    echo "  ok usr/share/licenses/$c"
done
# The Alpine userland has no licence files to copy, so it is covered by a
# pointer to the sources -- which is also the written offer the GPL parts
# (busybox, apk-tools) need.
grep -q "aports" "$work/usr/share/licenses/README" \
    || { echo "licenses/README does not point at the Alpine sources"; exit 1; }
echo "  ok usr/share/licenses/README names the Alpine sources"

echo "==> runtime dependencies the engine and bridge need at boot"
# Absent from the Alpine minirootfs, and their absence only shows up when the
# artifact is booted: no iptables means dockerd cannot build container
# networking, no socat means the pipe relay has nothing to talk to.
# Located by name rather than by path: distributions move binaries between
# /sbin, /usr/sbin and /usr/bin, and hardcoding one of them tests the layout
# instead of the dependency.
for dep in socat iptables ip6tables docker-init nsenter modprobe pigz; do
    found=""
    for d in bin sbin usr/bin usr/sbin usr/local/bin; do
        # -L as well as -e: an absolute symlink inside the rootfs dangles when
        # inspected from outside it, and is still a present dependency.
        if [ -e "$work/$d/$dep" ] || [ -L "$work/$d/$dep" ]; then
            found="$d/$dep"
            break
        fi
    done
    [ -n "$found" ] || { echo "missing runtime dependency: $dep"; exit 1; }
    echo "  ok $found"
done

echo "==> statically linked (no interpreter needed inside the minimal rootfs)"
for b in dockerd containerd runc buildkitd; do
    if file "$work/usr/local/bin/$b" | grep -q "dynamically linked"; then
        echo "  WARN $b is dynamically linked"
    else
        echo "  ok $b static"
    fi
done

echo "smoke test PASSED"
