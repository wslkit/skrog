package provision

import (
	"context"
	"sort"
	"strings"
)

// BinfmtHandlers lists the foreign-architecture handlers registered in the
// kernel the engine runs on (#384), so `skrog doctor` can say whether the
// DEFAULT buildx driver can build for another architecture.
//
// Two things make this worth reading carefully.
//
// The default `docker` driver builds inside dockerd's own BuildKit, which has
// no bundled emulator, so a foreign RUN needs a binfmt_misc handler on the
// host. A `docker-container` builder does not: official BuildKit images carry
// their own qemu, which is why `buildx create --driver docker-container` cross-
// builds on an engine whose handler table is empty.
//
// And binfmt_misc is kernel state in the WSL2 UTILITY VM, which every distro on
// the machine shares — the same kernel and the same boot_id — so what this
// reports is not a property of the engine distro but of the whole machine, and
// entries here may well have been registered by someone's Ubuntu.
//
// Host-side and cheap, but it does exec in the distro, so callers must only
// call it when the engine is already up: probing would boot a stopped distro,
// which doctor must never do (#82).
func (p *Provisioner) BinfmtHandlers(ctx context.Context, opts Options) []string {
	opts = opts.withDefaults()
	out, err := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c",
		"ls /proc/sys/fs/binfmt_misc/ 2>/dev/null")
	if err != nil {
		return nil
	}
	var handlers []string
	for _, line := range strings.Fields(out) {
		name := strings.TrimSpace(line)
		// A WHITELIST, not a blacklist. binfmt_misc holds far more than
		// architecture emulators: this table on a developer machine also
		// carries WSLInterop (Windows binaries) and, observed in testing, a
		// `python3.14` entry registered by the Ubuntu next door. Excluding the
		// names we happened to know made doctor report "the default builder
		// can emulate: python3.14", which is both false and confidently said.
		//
		// Every emulator that matters here is named qemu-<arch> by convention:
		// tonistiigi/binfmt, Alpine's qemu-openrc and Docker Desktop all use
		// it, and BuildKit's in-container ones are buildkit-qemu-<arch>.
		if !strings.HasPrefix(name, "qemu-") && !strings.HasPrefix(name, "buildkit-qemu-") {
			continue
		}
		handlers = append(handlers, name)
	}
	sort.Strings(handlers)
	return handlers
}
