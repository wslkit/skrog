package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/wslkit/skrog/internal/bundle"
	"github.com/wslkit/skrog/internal/lockfile"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/release"
)

// installSource is the "which engine, from where" half of an install's flags.
// `skrog install` and `skrog target create` take the same ones, so the choice
// is made in one place: two copies of "an explicit URL must carry its own
// digest" is how one of them stops being true.
type installSource struct {
	engineVersion string
	locked        string // --locked: a skrog.lock
	offline       string // --offline: an air-gap bundle
	rootfsURL     string // --rootfs-url, with rootfsSHA
	rootfsSHA     string
}

// applyInstallSource fills the rootfs fields of opts from src. The returned
// cleanup must be called when the install is over (it removes an extracted
// offline rootfs); it is never nil. The int is exitOK when the install should
// go on, and the exit code to return otherwise, with the message already
// printed.
func applyInstallSource(opts provision.Options, src installSource, log *slog.Logger) (provision.Options, func(), int) {
	cleanup := func() {}

	if n := boolCount(src.locked != "", src.rootfsURL != "", src.offline != ""); n > 1 {
		fmt.Fprintln(os.Stderr, "skrog: choose at most one of --locked, --offline, --rootfs-url")
		return opts, cleanup, exitUsage
	}

	// An explicit URL bypasses the manifest, so it must carry its own digest:
	// there is no code path that imports an unverified rootfs.
	switch {
	case src.offline != "":
		b, err := bundle.Open(src.offline)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return opts, cleanup, exitError
		}
		defer b.Close()
		l := b.Lock()
		// Extract the bundled rootfs beside the state dir; the install below then
		// copies it into the cache and verifies its SHA-256 — the same verified
		// path a networked install uses, so "offline" adds no unchecked import.
		sd := optsWithResolvedStateDir(opts).StateDir
		extracted := filepath.Join(sd, "offline", bundle.ExtractedRootfsName)
		if err := b.ExtractRootfs(extracted); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return opts, cleanup, exitError
		}
		defer os.RemoveAll(filepath.Dir(extracted))
		opts.RootfsURL = extracted
		opts.RootfsSHA256 = l.Rootfs.SHA256
		opts.EngineVersion = l.EngineVersion
		log.Info("installing offline from bundle", "path", src.offline, "engine", l.EngineVersion)
	case src.locked != "":
		l, err := lockfile.Load(src.locked)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return opts, cleanup, exitError
		}
		// The lock's rootfs URL + SHA-256 pin the artifact; the existing verified
		// download refuses on any mismatch, so a tampered lock cannot install
		// something else.
		opts.RootfsURL = l.Rootfs.URL
		opts.RootfsSHA256 = l.Rootfs.SHA256
		opts.EngineVersion = l.EngineVersion
		log.Info("installing from lockfile", "path", src.locked, "engine", l.EngineVersion)
	case src.rootfsURL != "" && src.rootfsSHA == "":
		fmt.Fprintln(os.Stderr, "skrog: --rootfs-url requires --rootfs-sha256")
		return opts, cleanup, exitUsage
	case src.rootfsURL != "":
		opts.RootfsURL = src.rootfsURL
		opts.RootfsSHA256 = src.rootfsSHA
		opts.EngineVersion = src.engineVersion
		log.Warn("using an overridden rootfs; this is a development path, not a release install",
			"url", src.rootfsURL)
	default:
		m, err := release.Load()
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return opts, cleanup, exitError
		}
		engine, err := m.Engine(src.engineVersion)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return opts, cleanup, exitUsage
		}
		// Pick the rootfs for this host's architecture, and refuse HERE rather
		// than after a 200 MB download, a passing SHA-256 and a successful
		// `wsl --import` that leaves the user with a distro whose every binary
		// is the wrong ISA (#388).
		//
		// Only this branch selects: --rootfs-url, --offline and --locked all
		// carry bytes the caller chose, and skrog does not second-guess those.
		rootfs, err := engine.HostRootfs()
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			var unsupported *release.ErrUnsupportedHostArch
			if errors.As(err, &unsupported) {
				return opts, cleanup, exitUnsupported
			}
			return opts, cleanup, exitError
		}
		opts.RootfsURL = rootfs.URL
		opts.RootfsSHA256 = rootfs.SHA256
		opts.EngineVersion = engine.Version
	}

	return opts, cleanup, exitOK
}
