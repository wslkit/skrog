package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wslkit/skrog/internal/bundle"
	"github.com/wslkit/skrog/internal/lockfile"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/release"
)

func runBundle(args []string) int {
	fs := flag.NewFlagSet("bundle", flag.ContinueOnError)
	var (
		engineVersion = fs.String("engine-version", "", "engine version to bundle (default: this build's default)")
		output        = fs.String("output", "", "bundle path (default: skrog-bundle-<version>.zip)")
		stateDir      = fs.String("state-dir", "", "override Skrog's state directory (rootfs download cache)")
	)
	fs.StringVar(output, "o", "", "shorthand for --output")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog bundle [--engine-version <v>] [-o skrog-bundle.zip]

Packs the engine rootfs and a skrog.lock into one .zip for an air-gapped
install. Run this on a connected machine; the rootfs is downloaded and
checksum-verified, then copied into the bundle. On the isolated machine:

  skrog install --offline skrog-bundle.zip

installs entirely from the file — any network access on that step is a bug.

Exit codes: 0 ok, %d error, %d usage.

flags:
`, exitError, exitUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	m, err := release.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	engine, err := m.Engine(*engineVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitUsage
	}
	if !engine.Published() {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", &release.ErrNotPublished{Version: engine.Version})
		return exitError
	}

	log := cliLogger(false)
	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	p := &provision.Provisioner{Logger: log}

	ctx, stop := interruptible()
	defer stop()

	// A bundle is for THIS architecture. It carries one rootfs and a lock that
	// pins it, so building one on amd64 for an arm64 machine would mean
	// shipping bytes this machine never verified (#388). Someone who needs an
	// arm64 bundle builds it on an arm64 machine, which is the rule air-gap
	// transfer follows anyway.
	rootfs, err := engine.HostRootfs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	// Download + verify into the normal rootfs cache, so a later real install
	// reuses it and this does not re-fetch what is already present.
	cached := filepath.Join(opts.StateDir, "rootfs", filepath.Base(rootfs.URL))
	log.Info("fetching rootfs for bundle", "version", engine.Version, "arch", release.HostArch())
	if err := p.FetchRootfs(ctx, rootfs.URL, rootfs.SHA256, cached); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	dest := *output
	if dest == "" {
		dest = fmt.Sprintf("skrog-bundle-%s.zip", engine.Version)
	}
	lock, err := lockfile.FromEngine(engine)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if err := bundle.Create(dest, lock, cached); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	info, _ := os.Stat(dest)
	fmt.Fprintf(os.Stderr, "wrote %s", dest)
	if info != nil {
		fmt.Fprintf(os.Stderr, " (%.1f MB, engine %s)", float64(info.Size())/(1<<20), engine.Version)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "install it on an isolated machine with:\n  skrog install --offline %s\n", dest)
	return exitOK
}
