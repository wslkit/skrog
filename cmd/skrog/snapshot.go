package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/snapshot"
	"github.com/wslkit/skrog/internal/supervise"
	"github.com/wslkit/skrog/internal/wsl"
)

func runSnapshot(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		force    = fs.Bool("force", false, "proceed even if containers are running")
		yes      = fs.Bool("yes", false, "skip the confirmation prompt (required for restore)")
		asJSON   = fs.Bool("json", false, "emit machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog snapshot save <name>       capture the engine state
       skrog snapshot list                list snapshots
       skrog snapshot restore <name>      replace the engine with a snapshot
       skrog snapshot delete <name>

Saves and restores the whole engine state — every image, container and volume —
as a named, checksummed archive. "Set up a dev environment, snapshot it, trash
it during a risky test, restore it in seconds." Only Skrog's own distro is ever
touched.

save/restore refuse while containers are running (pass --force to override);
restore is destructive and needs --yes.

Exit codes: 0 ok, %d error, %d usage, %d no such snapshot / not installed.

flags:
`, exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	// Flags on either side of the verb: `snapshot restore golden --yes` is what
	// the docs show and what people type (#244).
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	log := cliLogger(false)
	p := &provision.Provisioner{Logger: log}
	m, merr := p.ReadManifest(opts)
	if merr != nil {
		fmt.Fprintln(os.Stderr, "skrog: no install found. Run `skrog install` first.")
		return exitNotFound
	}
	opts.Distro = m.Distro
	opts.DataDir = m.DataDir

	mgr := &snapshot.Manager{
		WSL:      &wsl.Local{},
		StateDir: opts.StateDir,
		Distro:   m.Distro,
		DataDir:  m.DataDir,
		Logger:   log,
	}

	switch {
	case len(rest) == 0 || (rest[0] == "list" && len(rest) == 1):
		return snapshotList(mgr, *asJSON)
	case rest[0] == "save" && len(rest) == 2:
		return snapshotSave(mgr, p, opts, m.EngineVersion, rest[1], *force, *asJSON)
	case rest[0] == "restore" && len(rest) == 2:
		return snapshotRestore(mgr, p, opts, rest[1], *force, *yes, *asJSON)
	case rest[0] == "delete" && len(rest) == 2:
		return snapshotDelete(mgr, rest[1], *asJSON)
	default:
		fs.Usage()
		return exitUsage
	}
}

func snapshotList(mgr *snapshot.Manager, asJSON bool) int {
	snaps, err := mgr.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if asJSON {
		// Always an array, never null: an empty list is a normal answer.
		out := []snapshot.Meta{}
		for _, s := range snaps {
			out = append(out, snapshot.Meta{
				Name: s.Name, Created: s.Created, EngineVersion: s.EngineVersion,
				Distro: s.Distro, SHA256: s.SHA256, SizeBytes: s.SizeBytes,
			})
		}
		return emitJSON(out)
	}
	if len(snaps) == 0 {
		fmt.Println("no snapshots; `skrog snapshot save <name>` captures the engine state")
		return exitOK
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCREATED\tENGINE\tSIZE")
	for _, s := range snaps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name,
			s.Created.Local().Format("2006-01-02 15:04"), orDash(s.EngineVersion), humanBytes(s.SizeBytes))
	}
	tw.Flush()
	return exitOK
}

func snapshotSave(mgr *snapshot.Manager, p *provision.Provisioner, opts provision.Options, engineVersion, name string, force, asJSON bool) int {
	if err := snapshot.ValidName(name); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitUsage
	}
	if busy, why := containersRunning(p, opts); busy && !force {
		fmt.Fprintf(os.Stderr, "skrog: %s; stop them or pass --force\n", why)
		return exitError
	}
	// `wsl --export` terminates the distro for a consistent image; the
	// supervisor brings the engine back afterward.
	meta, err := mgr.Save(context.Background(), name, engineVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if asJSON {
		return emitJSON(meta)
	}
	fmt.Printf("saved snapshot %q (%s)\n", meta.Name, humanBytes(meta.SizeBytes))
	return exitOK
}

func snapshotRestore(mgr *snapshot.Manager, p *provision.Provisioner, opts provision.Options, name string, force, yes, asJSON bool) int {
	if !mgr.Exists(name) {
		fmt.Fprintf(os.Stderr, "skrog: no such snapshot %q\n", name)
		return exitNotFound
	}
	if busy, why := containersRunning(p, opts); busy && !force {
		fmt.Fprintf(os.Stderr, "skrog: %s; stop them or pass --force\n", why)
		return exitError
	}
	if !yes {
		fmt.Fprintf(os.Stderr, "skrog: restoring %q replaces the current engine — every image, "+
			"container and volume not in the snapshot is lost. Re-run with --yes to confirm.\n", name)
		return exitError
	}

	ctx := context.Background()
	if err := restoreWithEngine(ctx, mgr, p, opts, name); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if asJSON {
		return emitJSON(snapshotRestoredJSON{Restored: name})
	}
	fmt.Printf("restored snapshot %q; the engine is back on that state\n", name)
	return exitOK
}

func snapshotDelete(mgr *snapshot.Manager, name string, asJSON bool) int {
	if err := mgr.Delete(name); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitNotFound
	}
	if asJSON {
		return emitJSON(snapshotDeletedJSON{Deleted: name})
	}
	fmt.Printf("deleted snapshot %q\n", name)
	return exitOK
}

// restoreWithEngine pauses the engine (cooperating with the supervisor, like
// the engine-config bounce), replaces the distro from the snapshot, then brings
// it back on the restored state.
func restoreWithEngine(ctx context.Context, mgr *snapshot.Manager, p *provision.Provisioner, opts provision.Options, name string) error {
	const settle = 120 * time.Second
	held := supervise.Held(opts.StateDir)

	if held {
		if err := supervise.WriteDesired(opts.StateDir, supervise.DesiredStopped); err != nil {
			return err
		}
		if !waitFor(ctx, settle, func() bool { return !p.EngineRunning(ctx, opts) }) {
			return fmt.Errorf("engine did not stop within %s", settle)
		}
	} else if err := p.StopEngine(ctx, opts); err != nil {
		return err
	}

	if err := mgr.Restore(ctx, name); err != nil {
		// Best-effort: bring the engine back up whatever happened.
		if held {
			supervise.WriteDesired(opts.StateDir, supervise.DesiredRunning)
		} else {
			p.StartEngine(ctx, opts)
		}
		return err
	}

	if held {
		if err := supervise.WriteDesired(opts.StateDir, supervise.DesiredRunning); err != nil {
			return err
		}
		if !waitFor(ctx, settle, func() bool { return p.EngineRunning(ctx, opts) }) {
			return fmt.Errorf("restored engine did not come back within %s", settle)
		}
		return nil
	}
	return p.StartEngine(ctx, opts)
}

// containersRunning reports whether the engine has running containers, reusing
// the idle busy-probe. A probe error is treated as "assume busy" so a snapshot
// is never taken over an engine in an unknown state.
func containersRunning(p *provision.Provisioner, opts provision.Options) (bool, string) {
	if !p.EngineRunning(context.Background(), opts) {
		return false, "" // down: nothing running
	}
	dialer := engineDialer(opts.Distro, "", opts.StateDir, cliLogger(true))
	busy, err := busyProbe(dialer, cliLogger(true))(context.Background())
	if err != nil {
		return true, "could not confirm the engine is idle"
	}
	if busy {
		return true, "containers are running"
	}
	return false, ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// humanBytes formats a byte count with a binary unit suffix.
//
// Generic over both widths deliberately. The engine reports sizes as uint64
// and prune reports them as int64, and every call site used to bridge that
// with humanBytes(int64(x)) -- which wraps to a negative number for anything
// above 2^63 and prints it as "-8.0 EiB". Those values come out of the
// engine's own JSON, so the bound is not ours to assume (CodeQL
// go/incorrect-integer-conversion). Taking the width the caller already has
// removes the conversion rather than range-checking it.
func humanBytes[T int64 | uint64](b T) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := T(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
