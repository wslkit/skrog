#!/usr/bin/env bash
# Emit a minimal SPDX 2.3 SBOM for the rootfs from the pinned versions.
# Deliberately dependency-free: a security team can diff this against
# versions.env by eye. Richer scanning (syft) can layer on later.
#   ./sbom.sh versions.env out.spdx.json
#
# ROOTFS_ARCH selects which architecture's rootfs this describes; it defaults
# to the host's, like everything else here. It belongs in the SBOM because two
# architectures now ship from the same pins, and an inventory that cannot tell
# them apart is one a security team cannot act on.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
versions="${1:?usage: sbom.sh <versions.env> <output.spdx.json>}"
output="${2:?usage: sbom.sh <versions.env> <output.spdx.json>}"
# shellcheck disable=SC1090
. "$versions"
# shellcheck source=arch.sh
. "$here/arch.sh"

sep=""
pkg() { # name version download-location license
  printf '%s    {\n' "$sep"
  cat <<JSON
      "SPDXID": "SPDXRef-Package-$1",
      "name": "$1",
      "versionInfo": "$2",
      "downloadLocation": "$3",
      "filesAnalyzed": false,
      "licenseDeclared": "$4",
      "licenseConcluded": "$4",
      "supplier": "NOASSERTION"
JSON
  printf '    }'
  sep=",
"
}

{
  cat <<JSON
{
  "spdxVersion": "SPDX-2.3",
  "dataLicense": "CC0-1.0",
  "SPDXID": "SPDXRef-DOCUMENT",
  "name": "${rootfs_name}",
  "documentNamespace": "https://github.com/wslkit/skrog/spdx/rootfs-${rootfs_version}-${ROOTFS_ARCH}",
  "creationInfo": {
    "created": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
    "creators": ["Tool: skrog-rootfs-build", "Organization: Skrog"]
  },
  "packages": [
JSON
  pkg "alpine-minirootfs" "$ALPINE_ROOTFS_VERSION" \
      "https://dl-cdn.alpinelinux.org/alpine/$ALPINE_BRANCH/releases/$ARCH_ALPINE/" "MIT AND GPL-2.0-only"
  pkg "moby" "$MOBY_TAG" "https://github.com/moby/moby.git" "Apache-2.0"
  pkg "containerd" "$CONTAINERD_VERSION" "https://github.com/containerd/containerd.git" "Apache-2.0"
  pkg "runc" "$RUNC_VERSION" "https://github.com/opencontainers/runc.git" "Apache-2.0"
  pkg "buildkit" "$BUILDKIT_VERSION" "https://github.com/moby/buildkit.git" "Apache-2.0"
  pkg "nvidia-cdi-hook" "$NVIDIA_CDI_HOOK_VERSION" "https://github.com/NVIDIA/nvidia-container-toolkit.git" "Apache-2.0"
  pkg "go" "$GO_VERSION" "https://go.dev/dl/" "BSD-3-Clause"
  # The agent is versioned by this repository itself; the rootfs revision is
  # its packaging version, and the git remote is its provenance.
  pkg "skrog-agent" "rootfs-r${ROOTFS_REVISION}" "https://github.com/wslkit/skrog.git" "Apache-2.0"
  cat <<'JSON'

  ]
}
JSON
} > "$output"

echo "SBOM: $output"
