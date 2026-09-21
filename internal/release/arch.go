package release

import (
	"fmt"
	"runtime"
)

// Host-architecture preflight (#388).
//
// Every rootfs in manifest.json is amd64, and the manifest schema has no
// architecture dimension at all -- an Engine has exactly one rootfs.url.
//
// The BUILD side no longer has that limitation: rootfs.yml runs a native
// matrix over amd64 and arm64, and the scripts name their output
// skrog-rootfs-<version>-<arch>.tar.gz. What is missing is a published arm64
// release and a manifest that can point at one. Until both exist this check
// stays, because what it is really asserting is "the manifest cannot offer
// this host an engine" -- and that is still true on arm64.
//
// Meanwhile skrog.exe itself IS published for arm64: release.yml builds it,
// scripts/install.ps1 auto-selects it, docs/install.md tells Snapdragon and
// Surface owners to download it, and the scoop bucket carries an arm64 URL
// and hash.
//
// So on Windows on ARM the install used to go: download the amd64 rootfs,
// SHA-256 pin PASSES (they are the correct bytes), `wsl --import` SUCCEEDS
// (it is a tarball, nothing inspects the ELFs), and then dockerd cannot exec
// because every binary in it is x86-64. The user gets a failure that never
// mentions architecture, after a multi-hundred-megabyte download, on a
// machine the install script told them was supported.
//
// Refusing early with the reason is the honest behaviour until an arm64
// rootfs exists. Shipping a broken install with a confusing error is not a
// smaller promise than refusing; it is a worse one.

// EngineArch is the architecture every rootfs in the manifest is built for.
const EngineArch = "amd64"

// ErrUnsupportedHostArch is returned when the published engine rootfs cannot
// run on this host.
type ErrUnsupportedHostArch struct {
	Host string // runtime.GOARCH
}

func (e *ErrUnsupportedHostArch) Error() string {
	return fmt.Sprintf(
		"the engine rootfs is %s-only and this machine is %s.\n\n"+
			"skrog.exe itself runs natively on %s — the CLI, the bridge and the\n"+
			"supervisor are all fine. What does not exist yet is an %s build of the\n"+
			"Linux engine image, so there is nothing to import.\n\n"+
			"Emulation is not an answer here: WSL2 runs the guest on the host CPU,\n"+
			"so an x86-64 dockerd cannot start inside an arm64 utility VM.\n\n"+
			"Options today:\n"+
			"  - Use Docker Desktop or Rancher Desktop, both of which ship an arm64\n"+
			"    engine.\n"+
			"  - If you have built an %s rootfs yourself, install it with\n"+
			"    `skrog install --rootfs-url <url> --rootfs-sha256 <sha>`. That path\n"+
			"    is deliberately still open; skrog will not second-guess bytes you\n"+
			"    supplied.\n\n"+
			"Progress on a published arm64 rootfs: "+
			"https://github.com/wslkit/skrog/issues/388",
		EngineArch, e.Host, e.Host, e.Host, e.Host)
}

// CheckHostArch reports whether a published rootfs from the manifest can run
// on this host.
//
// It deliberately does NOT cover a caller-supplied --rootfs-url: those are the
// user's own bytes, and someone who built an arm64 rootfs should be able to
// install it. The check is about what *this manifest* can honestly offer.
func CheckHostArch() error {
	if runtime.GOARCH == EngineArch {
		return nil
	}
	return &ErrUnsupportedHostArch{Host: runtime.GOARCH}
}
