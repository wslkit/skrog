package provision

import (
	"context"
	"fmt"
	"strings"

	"github.com/wslkit/skrog/internal/emulation"
)

// applyEmulation registers the QEMU interpreters this install asked for, so
// `docker run --platform` can start a foreign-architecture container (#462).
//
// Runs on every engine start, like applyGPU and applyNetwork, and for a
// sharper reason than either: binfmt_misc is KERNEL state. `wsl --shutdown`
// takes the whole utility VM with it, and the handlers go too. Registering
// once at install would work until the first reboot and then quietly stop.
//
// Costs nothing when emulation is off, which is the default and will be the
// overwhelming majority of installs: no configured platforms means no exec at
// all. That matters because each `wsl` round trip is ~165 ms on a warm distro
// and engine start is already the subject of #398.
func (p *Provisioner) applyEmulation(ctx context.Context, opts Options) {
	if strings.TrimSpace(opts.EmulationPlatforms) == "" {
		return
	}

	handlers, err := emulation.Parse(opts.EmulationPlatforms)
	if err != nil {
		// Not fatal. `skrog config set` validates this key, so reaching here
		// means a hand-edited settings file -- and refusing to start the
		// engine over an optional feature would be a worse answer than
		// starting without it and saying so.
		p.logger().Warn("emulation.platforms is not usable; starting without emulation",
			"error", err, "value", opts.EmulationPlatforms)
		return
	}
	if len(handlers) == 0 {
		return
	}

	// One exec for the whole set. Three round trips to register two handlers
	// would be most of a second on a path that is already too slow (#398).
	script := emulationScript(handlers)
	out, err := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c", script)
	if err != nil {
		p.logger().Warn("could not register emulation handlers; foreign-architecture "+
			"containers will fail with exec format error",
			"error", err, "output", strings.TrimSpace(out))
		return
	}

	archs := make([]string, 0, len(handlers))
	for _, h := range handlers {
		archs = append(archs, h.Arch)
	}
	// Said at INFO, every start, on purpose. This changes how every distro in
	// the utility VM executes foreign binaries, not just Skrog's -- a user
	// reading the log to work out why their Ubuntu behaves differently should
	// find it here rather than deduce it.
	p.logger().Info("emulation handlers registered (shared by every WSL2 distro)",
		"platforms", strings.Join(archs, ","))
}

// emulationScript builds the shell that registers a set of handlers.
//
// Idempotent, because it runs on every start and the handlers usually survive
// between them: an entry that is already present is removed and rewritten
// rather than skipped, so a changed interpreter path or a stale registration
// from an older rootfs cannot persist. Writing over an existing name fails
// with EEXIST, which is why the removal is not optional.
func emulationScript(handlers []emulation.Handler) string {
	var b strings.Builder
	// binfmt_misc is not mounted in a fresh WSL2 distro until something asks.
	b.WriteString("set -e\n")
	b.WriteString("grep -q binfmt_misc /proc/mounts || " +
		"mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc\n")
	for _, h := range handlers {
		// The interpreter has to exist before registering. With the F flag the
		// kernel opens it at registration time, so a missing file is an error
		// here -- which is better than the alternative, but the message the
		// kernel gives is ENOENT with no subject.
		fmt.Fprintf(&b, "test -x %s || { echo \"missing interpreter %s\" >&2; exit 1; }\n",
			h.Interpreter, h.Interpreter)
		fmt.Fprintf(&b, "[ -e /proc/sys/fs/binfmt_misc/%s ] && echo -1 > /proc/sys/fs/binfmt_misc/%s\n",
			h.Name, h.Name)
		// The registration line is single-quoted and contains no single
		// quotes, so it reaches the kernel byte for byte. It is nothing but
		// backslash escapes, and every layer of shell between here and
		// /proc is a chance to eat one.
		fmt.Fprintf(&b, "printf '%%s' '%s' > /proc/sys/fs/binfmt_misc/register\n",
			strings.TrimRight(h.Registration, "\n"))
	}
	return b.String()
}

// RemoveEmulation deregisters every handler Skrog may have registered.
//
// Called from `skrog stop` and from uninstall. It is not politeness: these
// handlers are kernel-wide, so leaving them behind means Skrog changed how an
// unrelated distro executes binaries and then stopped being the thing that
// could undo it. "Nothing left behind" has to include this.
//
// Best effort throughout. A distro that is already down has no handlers to
// remove, and failing a stop over it would be absurd.
func (p *Provisioner) RemoveEmulation(ctx context.Context, opts Options) {
	opts = opts.withDefaults()
	if opts.Distro == "" {
		return
	}
	names := make([]string, 0, len(emulation.Supported()))
	for _, arch := range emulation.Supported() {
		if h, ok := emulation.HandlerFor(arch); ok {
			names = append(names, h.Name)
		}
	}
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "[ -e /proc/sys/fs/binfmt_misc/%s ] && echo -1 > /proc/sys/fs/binfmt_misc/%s\n",
			n, n)
	}
	b.WriteString("exit 0\n")
	p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c", b.String())
}
