package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/wslkit/skrog/internal/lockfile"
	"github.com/wslkit/skrog/internal/release"
)

func runLock(args []string) int {
	fs := flag.NewFlagSet("lock", flag.ContinueOnError)
	var (
		engineVersion = fs.String("engine-version", "", "engine version to lock (default: this build's default)")
		output        = fs.String("output", "", "write to this file instead of stdout")
	)
	fs.StringVar(output, "o", "", "shorthand for --output")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog lock [--engine-version <v>] [-o skrog.lock]

Writes a skrog.lock pinning the exact engine this build installs: version,
rootfs URL and SHA-256, and component versions. Check it into a repo and every
developer and CI runner reproduces the same verified engine with:

  skrog install --locked skrog.lock

With no -o, the lock is printed to stdout.

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
	// A lock pins ONE rootfs for THIS architecture (#388). An engine with no
	// build for this host, or one whose release has not been cut, has no
	// verifiable artifact to pin — which is the whole point of a lock — and
	// FromEngine says which of the two it is.
	lock, err := lockfile.FromEngine(engine)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	b, err := lock.Marshal()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	if *output == "" {
		os.Stdout.Write(b)
		return exitOK
	}
	if err := os.WriteFile(*output, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	fmt.Fprintf(os.Stderr, "wrote %s (engine %s)\n", *output, engine.Version)
	return exitOK
}
