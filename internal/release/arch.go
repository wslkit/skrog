package release

import (
	"fmt"
	"runtime"
	"strings"
)

// Host-architecture selection (#388).
//
// skrog.exe is published for arm64: release.yml builds it, scripts/install.ps1
// auto-selects it, docs/install.md tells Snapdragon and Surface owners to
// download it, and the scoop bucket carries an arm64 URL and hash.
//
// The engine rootfs did not follow. So on Windows on ARM the install went:
// download the amd64 rootfs, SHA-256 pin PASSES (they are the correct bytes),
// `wsl --import` SUCCEEDS (it is a tarball, nothing inspects the ELFs), and
// then dockerd cannot exec because every binary in it is x86-64. The user got
// a failure that never mentioned architecture, after a multi-hundred-megabyte
// download, on a machine the install script told them was supported.
//
// The manifest now carries a rootfs per architecture, so this is a lookup
// rather than a blanket refusal -- but the refusal has to stay good, because
// an architecture with no build is still the common case for anything except
// amd64, and it is the case where a clear message is worth the most.

// ErrUnsupportedHostArch is returned when no rootfs in the manifest is built
// for this host.
type ErrUnsupportedHostArch struct {
	Host      string   // runtime.GOARCH
	Available []string // GOARCH values this engine does have
}

func (e *ErrUnsupportedHostArch) Error() string {
	have := "none"
	if len(e.Available) > 0 {
		have = strings.Join(e.Available, ", ")
	}
	return fmt.Sprintf(
		"this machine is %s and the engine rootfs is built for %s.\n\n"+
			"skrog.exe itself runs natively on %s — the CLI, the bridge and the\n"+
			"supervisor are all fine. What does not exist is an %s build of the\n"+
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
		e.Host, have, e.Host, e.Host, e.Host)
}

// HostArch is this build's GOARCH, so callers need not import runtime just to
// name the architecture in a message.
func HostArch() string { return runtime.GOARCH }
