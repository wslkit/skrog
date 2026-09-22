package main

import (
	"context"
	"fmt"
	"os"

	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/hostca"
	"github.com/wslkit/skrog/internal/provision"
)

// withStartSettings folds the settings that are applied at ENGINE START into
// the options a StartEngine call is about to use (#490).
//
// These are the settings that live in the kernel or in the distro rather than
// on disk, so a distro that was terminated comes back without them unless the
// starter puts them back: the binfmt handlers, the GPU CDI spec, dockerd's
// proxy environment and the imported host CA bundle.
//
// The supervisor learned to re-read them in #202 and #83. Every other path
// that restarts the engine -- `engine upgrade`, `compact`, `relocate`, the
// snapshot restore -- built its own options and read no config at all, so the
// engine came back with emulation off, no GPU, no proxy and no host CAs. The
// emulation half is the only one that fails loudly; on a corporate network the
// others present as pulls that simply stop working some time after an
// unrelated maintenance command.
//
// One list, because the bug was that there were two: the supervisor's, which
// was right, and everyone else's, which did not exist. A field added to
// provision.Options for a future per-start setting has exactly one place to be
// wired in.
//
// hostCAs is a provider rather than a value so the supervisor can keep caching
// the certificate store it reads once per process, while a one-shot command
// reads it when it needs it and not otherwise. It is only consulted when the
// setting is on.
func withStartSettings(opts provision.Options, c config.Config, hostCAs func() []byte) provision.Options {
	opts.GPUEnabled = c.GPU
	opts.GPUVendor = c.GPUVendor
	opts.EmulationPlatforms = c.EmulationPlatforms
	opts.Network = provision.NetConfig{Proxy: c.Proxy, NoProxy: c.NoProxy}
	if c.ImportHostCAs && hostCAs != nil {
		opts.Network.HostCAPEM = hostCAs()
	}
	return opts
}

// startOptions is withStartSettings for a one-shot command: it loads the
// config itself and reads the host CA store directly.
//
// A config that cannot be read leaves the options alone rather than failing
// the command. Refusing to restart an engine because a settings file is
// unreadable would turn a cosmetic problem into an outage, and the engine
// still comes up -- just as it did before any of this existed.
func startOptions(ctx context.Context, opts provision.Options) provision.Options {
	c, err := config.Load(opts.StateDir)
	if err != nil {
		return opts
	}
	return withStartSettings(opts, c, func() []byte {
		pem, err := hostca.HostRootCAs(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"skrog: host CA import is on but the store could not be read: %v\n", err)
			return nil
		}
		return pem
	})
}
