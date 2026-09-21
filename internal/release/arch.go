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
// The engine rootfs did not follow, and the result was the failure this
// selection exists to prevent: download the amd64 rootfs, SHA-256 pin PASSES
// (they are the correct bytes), `wsl --import` SUCCEEDS (it is a tarball,
// nothing inspects the ELFs), and then dockerd cannot exec because every
// binary in it is x86-64. A failure that never mentioned architecture, after
// a multi-hundred-megabyte download, on a machine the install script said was
// supported.
//
// Both architectures are published now, so the common path is a lookup. This
// error is what is left: an architecture the manifest has no entry for at
// all. That is rarer than it was and matters more when it happens, because
// the answer is never "wait" -- there is no build coming for a host nobody
// builds for.

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
			"so a rootfs built for another architecture cannot start inside this\n"+
			"machine's utility VM.\n\n"+
			"Options today:\n"+
			"  - Use Docker Desktop or Rancher Desktop, if either supports %s.\n"+
			"  - If you have built an %s rootfs yourself, install it with\n"+
			"    `skrog install --rootfs-url <url> --rootfs-sha256 <sha>`. That path\n"+
			"    is deliberately still open; skrog will not second-guess bytes you\n"+
			"    supplied.\n\n"+
			"If %s is a platform Skrog should support, say so: "+
			"https://github.com/wslkit/skrog/issues",
		e.Host, have, e.Host, e.Host, e.Host, e.Host, e.Host)
}

// HostArch is this build's GOARCH, so callers need not import runtime just to
// name the architecture in a message.
func HostArch() string { return runtime.GOARCH }
