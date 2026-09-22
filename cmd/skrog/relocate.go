package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/relocate"
	"github.com/wslkit/skrog/internal/supervise"
	"github.com/wslkit/skrog/internal/wsl"
)

// exitNoSpace is its own code because it is the failure a script can act on —
// free some room, or pick another drive — rather than merely report.
const exitNoSpace = 12

// runRelocate is `skrog relocate`: move the engine's data directory, and
// everything in it, to another drive (#64).
func runRelocate(args []string) int {
	fs := flag.NewFlagSet("relocate", flag.ContinueOnError)
	var (
		stateDir    = fs.String("state-dir", "", "override Skrog's state directory")
		distro      = fs.String("distro", "", "WSL distro (default: from the install manifest)")
		dryRun      = fs.Bool("dry-run", false, "print the steps and change nothing")
		restart     = fs.Bool("restart", false, "start the engine again afterwards")
		keepArchive = fs.Bool("keep-archive", false, "keep the transfer archive instead of deleting it")
		asJSON      = fs.Bool("json", false, "emit machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog relocate <new-directory> [--restart] [--dry-run] [--keep-archive] [--json]

Moves the engine's data directory — the virtual disk, and every image,
container and volume in it — to another drive. "Move it off C:" is what this
is for.

The engine is exported to a checksummed archive, the old distro is
unregistered, and the archive is imported at the new location. The archive is
written to the TARGET drive, because the reason to move is usually that the
current one is full. It is deleted once the new location is proven, and %s
keeps it.

Nothing is destroyed before the archive exists and its checksum has been
verified, so an interrupted move leaves your data recoverable — and if the
import fails, the error prints the exact command that restores it.

Because the archive and the new disk briefly coexist, the target needs roughly
twice the current disk's size free. That is checked before anything is touched.

  skrog relocate D:\skrog --dry-run      # what it would do, and what it needs
  skrog relocate D:\skrog --restart      # move it, then bring the engine back

Exit codes: 0 ok, %d error, %d usage, %d not installed, %d not enough space.

flags:
`, "`--keep-archive`", exitError, exitUsage, exitNotFound, exitNoSpace)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// Go's flag package stops at the first non-flag token, so a documented
	// invocation like `skrog relocate D:\skrog --dry-run` would otherwise
	// leave --dry-run unparsed and fail as a usage error. Take the directory
	// and re-parse whatever followed it, so flags work on either side.
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return exitUsage
	}
	target := rest[0]
	if len(rest) > 1 {
		if err := fs.Parse(rest[1:]); err != nil {
			return exitUsage
		}
		if fs.NArg() != 0 {
			fs.Usage()
			return exitUsage
		}
	}

	ctx, stop := interruptible()
	defer stop()

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir, Distro: *distro})
	p := &provision.Provisioner{Logger: cliLogger(false)}
	name, ok := requireDistroInstall(p, opts)
	if !ok {
		return exitNotFound
	}
	opts.Distro = name

	m, err := p.ReadManifest(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: reading the install manifest: %v\n", err)
		return exitNotFound
	}
	from := m.DataDir
	if from == "" {
		from = filepath.Join(opts.StateDir, "distro")
	}

	// compact does the same: a manifest can outlive the disk it names, and a
	// move that starts without a source is a confusing way to find that out.
	if _, err := os.Stat(filepath.Join(from, relocate.DiskName)); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: no virtual disk at %s: %v\n", filepath.Join(from, relocate.DiskName), err)
		return exitNotFound
	}

	abs, err := filepath.Abs(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %s: %v\n", target, err)
		return exitUsage
	}

	r := &relocate.Runner{
		WSL:    wsl.NewLocal(),
		Volume: relocate.RealVolume{},
		Logger: cliLogger(false),
		Stop: func(ctx context.Context) error {
			// Record the desired state too, or a running supervisor restarts
			// the engine in the middle of the export.
			if err := supervise.WriteDesired(opts.StateDir, supervise.DesiredStopped); err != nil {
				return err
			}
			supervise.WriteEngineState(opts.StateDir, supervise.EngineActive)
			return p.StopEngine(ctx, opts)
		},
		Start: func(ctx context.Context) error {
			if err := supervise.WriteDesired(opts.StateDir, supervise.DesiredRunning); err != nil {
				return err
			}
			return p.StartEngine(ctx, startOptions(ctx, opts))
		},
		Commit: func(dir string) error {
			m.DataDir = dir
			return p.SaveManifest(opts, m)
		},
	}

	rep, err := r.Run(ctx, relocate.Options{
		Distro:      name,
		From:        from,
		To:          abs,
		DryRun:      *dryRun,
		Restart:     *restart,
		KeepArchive: *keepArchive,
	})

	if *asJSON {
		emitJSON(relocateJSON(rep))
	}
	if err != nil {
		return reportRelocateError(err, *asJSON)
	}
	if !*asJSON {
		printRelocateReport(rep)
	}
	return exitOK
}

// reportRelocateError prints the failure in the shape the user can act on.
func reportRelocateError(err error, asJSON bool) int {
	var short *relocate.ErrNotEnoughSpace
	var orphan *relocate.ErrOrphaned
	switch {
	case errors.As(err, &short):
		if !asJSON {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", short)
			fmt.Fprintln(os.Stderr, "  Free some space on that drive, or pick another one. Nothing was changed.")
		}
		return exitNoSpace
	case errors.As(err, &orphan):
		// The one case where the user must act to get their data back, so it
		// gets the whole message rather than a one-line summary.
		if !asJSON {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", orphan)
		}
		return exitError
	}
	if !asJSON {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
	}
	return exitError
}

func printRelocateReport(rep relocate.Report) {
	if rep.DryRun {
		fmt.Printf("would move %s from %s to %s:\n", rep.Distro, rep.From, rep.To)
		for _, s := range rep.Steps {
			fmt.Printf("  %s\n", s)
		}
		fmt.Printf("\n%s needs about %s free at the peak; it has %s.\n",
			rep.To, relocate.HumanBytes(rep.NeedBytes), relocate.HumanBytes(rep.FreeBytes))
		return
	}
	fmt.Printf("%s: moved to %s (%s transferred)\n", rep.Distro, rep.To, relocate.HumanBytes(uint64(rep.MovedBytes)))
	if rep.ArchiveKept != "" {
		fmt.Printf("  archive kept at %s — delete it once you are satisfied\n", rep.ArchiveKept)
	}
	if !rep.Restarted {
		fmt.Println("  the engine is stopped; `skrog start` brings it back")
	}
}

// relocateJSON maps the report onto the pinned JSON shape.
func relocateJSON(rep relocate.Report) relocateJSONShape {
	return relocateJSONShape{
		Distro:      rep.Distro,
		From:        rep.From,
		To:          rep.To,
		MovedBytes:  rep.MovedBytes,
		SHA256:      rep.SHA256,
		NeedBytes:   rep.NeedBytes,
		FreeBytes:   rep.FreeBytes,
		Steps:       rep.Steps,
		DryRun:      rep.DryRun,
		Restarted:   rep.Restarted,
		ArchiveKept: rep.ArchiveKept,
	}
}
