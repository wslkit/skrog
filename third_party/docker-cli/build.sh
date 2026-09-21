#!/usr/bin/env bash
# Build a Windows arm64 docker.exe from docker/cli source (#450).
#
#   ./build.sh [output-dir]     default: ./out
#
# Output: docker-<version>-windows-arm64.exe and its .sha256.
#
# Runs anywhere Docker runs; the compile itself happens in the pinned golang
# image so the toolchain is the same in CI and on a laptop. docker/cli is pure
# Go with everything vendored, so this cross-compiles from x86-64 with no ARM
# hardware and no extra toolchain.
#
# # Why only arm64
#
# amd64 keeps coming from download.docker.com, and that asymmetry is a choice:
#
#   - Those bytes are Docker's. Anyone can re-download the zip and check it
#     against the sha256 pinned in internal/dockercli/manifest.json, with no
#     reference to Skrog at all. A binary we build can only be checked against
#     us. Replacing a verifiable-by-anyone artifact with a trust-us one, for
#     users who do not need it, is a real loss.
#   - Building it ourselves would make a bad build wrong for every user rather
#     than for the arm64 minority who have no alternative.
#   - An upstream bump stays a URL and a hash rather than a rebuild.
#
# It is not about code signing. Docker's published Windows CLI is NOT
# Authenticode-signed -- measured on the binary `skrog cli install` places --
# so there is no signature being preserved on one side and lost on the other.
#
# The asymmetry ends by itself: when upstream publishes a Windows arm64
# docker.exe, the manifest points at it and this directory is deleted.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="${1:-$here/out}"
# shellcheck source=versions.env
. "$here/versions.env"

mkdir -p "$out"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

target="docker-${DOCKER_CLI_VERSION}-windows-arm64.exe"

echo "==> building docker/cli ${DOCKER_CLI_TAG} for windows/arm64"

# HOST_UID/GID: the build runs as root in the container but writes into a host
# directory, so it hands ownership back before exiting -- otherwise the
# checksum step and cleanup hit permission errors on a CI runner.
docker run --rm \
  -v "$work:/out" \
  -e "CLI_TAG=$DOCKER_CLI_TAG" \
  -e "CLI_VERSION=$DOCKER_CLI_VERSION" \
  -e "CLI_SHA=${DOCKER_CLI_SHA:-}" \
  -e "TARGET=$target" \
  -e "HOST_UID=$(id -u)" \
  -e "HOST_GID=$(id -g)" \
  "golang:${GO_VERSION}-alpine" sh -euc '
    apk add --no-cache git >/dev/null

    git -c advice.detachedHead=false clone --depth 1 --branch "$CLI_TAG" \
      https://github.com/docker/cli.git /src >/dev/null 2>&1
    cd /src

    sha="$(git rev-parse HEAD)"
    if [ -n "$CLI_SHA" ] && [ "$CLI_SHA" != "$sha" ]; then
      echo "FATAL: docker/cli $CLI_TAG resolved to $sha, expected $CLI_SHA" >&2
      echo "  (a moved tag or a compromised source; refusing to build)" >&2
      exit 1
    fi
    [ -n "$CLI_SHA" ] || echo "    WARNING: no expected SHA pinned; resolved $sha" >&2

    # docker/cli ships vendor.mod and vendor.sum rather than go.mod and go.sum,
    # deliberately, so the repository cannot be consumed as a Go library. Their
    # own build scripts rename them; so must we, or the build cannot resolve a
    # single import.
    cp vendor.mod go.mod
    cp vendor.sum go.sum

    # Version and commit, so `docker version` reports something true. Without
    # them it says "dev" with an empty commit, which is worse than useless on
    # a binary the user did not download from Docker: it makes an unfamiliar
    # build look broken as well as unfamiliar.
    #
    # PlatformName is deliberately NOT set. Upstream stamps it "Docker Engine
    # - Community", and this build is not that -- it is docker/cli source
    # compiled here because upstream ships no Windows arm64 binary. Claiming
    # their build string would be the one genuinely misleading thing this
    # script could do. Leaving it empty costs a cosmetic line in
    # `docker version` and keeps the output honest.
    #
    # No BuildTime either: a timestamp is the one input that makes two builds
    # of the same commit differ, and a reproducible binary is worth more than
    # a date nobody reads.
    pkg=github.com/docker/cli/cli/version
    GOOS=windows GOARCH=arm64 CGO_ENABLED=0 \
      go build -mod=vendor -trimpath \
        -ldflags "-s -w -X ${pkg}.Version=${CLI_VERSION} -X ${pkg}.GitCommit=${sha}" \
        -o "/out/$TARGET" ./cmd/docker

    [ -s "/out/$TARGET" ] || { echo "the build produced no binary" >&2; exit 1; }
    printf "docker/cli %s %s\n" "$CLI_TAG" "$sha" > /out/commit.txt
    chown -R "$HOST_UID:$HOST_GID" /out'

cp "$work/$target" "$out/$target"
cp "$work/commit.txt" "$out/docker-${DOCKER_CLI_VERSION}-windows-arm64.commit.txt"

# The architecture is asserted, not assumed. A cross-compile that silently
# produced an x86-64 binary would sail through every other check here and fail
# only on a user's ARM machine, which is the exact failure #388 was about --
# and the one thing a build on an x86-64 runner genuinely cannot feel.
#
# PE: "PE\0\0" at the offset stored in the 4 bytes at 0x3c, then a 2-byte
# little-endian machine type. 0xaa64 is ARM64, 0x8664 is x86-64.
pe_off=$(od -An -tu4 -j60 -N4 "$out/$target" | tr -d ' ')
machine=$(od -An -tx1 -j"$((pe_off + 4))" -N2 "$out/$target" | tr -d ' ')
case "$machine" in
    64aa) arch="ARM64" ;;   # 0xaa64, little-endian
    6486) arch="x86-64" ;;  # 0x8664
    *)    arch="UNKNOWN" ;;
esac
echo "==> PE machine type: $machine ($arch)"
if [ "$machine" != "64aa" ]; then
    echo "FATAL: built a $arch binary, expected ARM64" >&2
    exit 1
fi

(cd "$out" && sha256sum "$target" > "$target.sha256")

ls -la "$out"
echo "OK"
