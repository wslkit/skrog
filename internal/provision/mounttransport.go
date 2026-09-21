package provision

import (
	"context"
	"strings"
)

// MountTransport reports what the engine distro actually mounts Windows
// drives over: "virtiofs", "9p", or "" when it could not tell.
//
// Read from /proc/mounts rather than from ~/.wslconfig, because those two
// disagree in the cases that matter. `virtiofs=true` in the config file means
// the user asked, not that they got it:
//
//   - WSL older than 2.9 ignores the key SILENTLY and stays on 9p
//   - the key takes effect only after `wsl --shutdown`, so a machine that set
//     it an hour ago and never restarted is still on 9p
//
// Both of those look identical in the config and are the whole reason someone
// would run doctor about it. docs/vm-sizing.md tells people to confirm with
// exactly this grep; this is that check, done for them.
//
// Host-side and cheap, but it does exec in the distro, so callers must only
// call it when the engine is already up: probing would boot a stopped distro,
// which doctor must never do (#82).
func (p *Provisioner) MountTransport(ctx context.Context, opts Options) string {
	opts = opts.withDefaults()
	out, err := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c",
		"grep ' /mnt/c ' /proc/mounts 2>/dev/null || true")
	if err != nil {
		return ""
	}
	// A /proc/mounts line is "src mountpoint fstype opts ...", so the
	// filesystem type is the third field. Matching on the whole line would
	// find "virtiofs" in a source or option string too.
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		switch fields[2] {
		case "virtiofs", "9p":
			return fields[2]
		}
	}
	return ""
}
