package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/engineupgrade"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/release"
	"github.com/wslkit/skrog/internal/supervise"
	"github.com/wslkit/skrog/internal/wsl"
)

// runEngine is `skrog engine`: what engine is installed, and moving between
// engines reversibly (#65).
func runEngine(args []string) int {
	if len(args) == 0 {
		engineUsage(os.Stderr)
		return exitUsage
	}
	switch args[0] {
	case "list":
		return runEngineList(args[1:])
	case "upgrade":
		return runEngineUpgrade(args[1:], false)
	case "rollback":
		return runEngineUpgrade(args[1:], true)
	case "-h", "--help", "help":
		engineUsage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "skrog engine: unknown subcommand %q\n\n", args[0])
		engineUsage(os.Stderr)
		return exitUsage
	}
}

func engineUsage(w *os.File) {
	fmt.Fprintf(w, `usage: skrog engine list|upgrade|rollback

Engine security patches should not have to wait for an app release, and taking
one should not cost you your images.

  list       engines this build can install, and which one is installed
  upgrade    move to another engine, reversibly (--to <ref>)
  rollback   go back to the engine installed before the last upgrade

An upgrade swaps the engine binaries (dockerd, containerd, runc, buildkitd…)
out of a checksum-verified rootfs and leaves the filesystem alone, so
/var/lib/docker — every image, container and volume — is untouched. If the new
engine does not come back, the previous binaries are restored and the engine is
started again before the failure is reported.

Exit codes: 0 ok, %d error, %d usage, %d not installed.
`, exitError, exitUsage, exitNotFound)
}

// engineRef is the revisioned label for an engine: the rootfs revision is the
// unit of upgrade, so "29.7.2-4" and not "29.7.2". Derived from the rootfs file
// name, which carries it, with the plain version as the fallback for a
// hand-passed URL that does not.
func engineRef(version, rootfsURL string) string {
	base := path.Base(rootfsURL)
	base = strings.TrimSuffix(base, ".tar.gz")
	// The architecture suffix is trimmed (#388). A ref names an engine BUILD,
	// and it is recorded in the install manifest and compared against later.
	// Leaving the architecture in would make the same engine release report a
	// different ref on an arm64 machine than on an amd64 one, and would print
	// "-amd64" on every row of `skrog engine list` on a machine that has no
	// other choice. Neither tells anyone anything.
	for _, arch := range []string{"-amd64", "-arm64"} {
		base = strings.TrimSuffix(base, arch)
	}
	if rest, ok := strings.CutPrefix(base, "skrog-rootfs-"); ok && rest != "" {
		return rest
	}
	return version
}

// engineRefURL picks the rootfs URL a ref is derived from.
//
// This host's, when there is one. When there is not -- an engine with no
// build for this architecture -- any of them still identifies the engine, and
// `skrog engine list` has to be able to print a row for an entry it cannot
// install. Architectures() is sorted, so the choice is deterministic rather
// than whatever the map iterates first.
func engineRefURL(e release.Engine) string {
	if r, err := e.HostRootfs(); err == nil {
		return r.URL
	}
	for _, a := range e.Architectures() {
		return e.Rootfs[a].URL
	}
	return ""
}

func runEngineList(args []string) int {
	fs := flag.NewFlagSet("engine list", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "override Skrog's state directory")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: skrog engine list [--json]\n\nflags:\n")
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
	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	p := &provision.Provisioner{Logger: cliLogger(false)}
	installed, previous := "", ""
	if im, err := p.ReadManifest(opts); err == nil {
		installed = im.EngineRef
		if installed == "" {
			installed = engineRef(im.EngineVersion, im.RootfsURL)
		}
		previous = im.PreviousEngineRef
	}

	if *asJSON {
		out := engineListJSON{Installed: installed, Previous: previous}
		for _, e := range m.Engines {
			out.Available = append(out.Available, engineEntryJSON{
				Ref:       engineRef(e.Version, engineRefURL(e)),
				Version:   e.Version,
				Default:   e.Default,
				Published: e.Published(),
			})
		}
		return emitJSON(out)
	}

	fmt.Println("engines this build can install:")
	for _, e := range m.Engines {
		ref := engineRef(e.Version, engineRefURL(e))
		marks := []string{}
		if e.Default {
			marks = append(marks, "default")
		}
		if ref == installed {
			marks = append(marks, "installed")
		}
		// Published() is host-relative since #388, so this row says why THIS
		// machine cannot install it -- "not built for arm64" and "built but
		// not released yet" are different sentences, and RootfsFor already
		// writes both.
		if _, err := e.HostRootfs(); err != nil {
			var unsupported *release.ErrUnsupportedHostArch
			if errors.As(err, &unsupported) {
				marks = append(marks, "no "+release.HostArch()+" build — not installable here")
			} else {
				marks = append(marks, "no published checksum — not installable")
			}
		}
		suffix := ""
		if len(marks) > 0 {
			suffix = "  (" + strings.Join(marks, ", ") + ")"
		}
		fmt.Printf("  %-14s dockerd %s%s\n", ref, e.Version, suffix)
	}
	if installed != "" && !containsRef(m.Engines, installed) {
		fmt.Printf("\ninstalled: %s (not in this build's manifest — installed by a different build,\n"+
			"or with an explicit --rootfs-url)\n", installed)
	}
	if previous != "" {
		fmt.Printf("rollback target: %s (`skrog engine rollback`)\n", previous)
	}
	return exitOK
}

func containsRef(engines []release.Engine, ref string) bool {
	for _, e := range engines {
		if engineRef(e.Version, engineRefURL(e)) == ref {
			return true
		}
	}
	return false
}

// runEngineUpgrade implements both `upgrade` and `rollback`: the same swap,
// differing only in how the target is chosen.
func runEngineUpgrade(args []string, rollback bool) int {
	name := "engine upgrade"
	if rollback {
		name = "engine rollback"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var (
		stateDir   = fs.String("state-dir", "", "override Skrog's state directory")
		to         = fs.String("to", "", "engine version to move to (default: the manifest's default engine)")
		rootfsURL  = fs.String("rootfs-url", "", "override the rootfs URL (development)")
		rootfsSHA  = fs.String("rootfs-sha256", "", "expected rootfs SHA-256; required with --rootfs-url")
		dryRun     = fs.Bool("dry-run", false, "print the plan and change nothing")
		asJSON     = fs.Bool("json", false, "emit machine-readable JSON")
		waitEngine = fs.Duration("timeout", 2*time.Minute, "how long to wait for the engine after the swap")
	)
	fs.Usage = func() {
		if rollback {
			fmt.Fprintf(os.Stderr, `usage: skrog engine rollback [--dry-run] [--json]

Restores the engine installed before the last `+"`skrog engine upgrade`"+`, from its
checksum-verified rootfs. Images, containers and volumes are untouched: only
the engine binaries move.

flags:
`)
		} else {
			fmt.Fprintf(os.Stderr, `usage: skrog engine upgrade [--to <version>] [--dry-run] [--json]

Moves the engine to another version from this build's manifest (`+"`skrog engine list`"+`),
reversibly. The rootfs is verified against its published checksum, only the
engine binaries are replaced, and /var/lib/docker is never touched — so images,
containers and volumes survive. If the new engine does not come back, the
previous binaries are restored and the engine started again before the failure
is reported.

  skrog engine upgrade --dry-run        # what it would do
  skrog engine upgrade --to 29.7.3      # a specific engine
  skrog engine rollback                 # back to the previous one

flags:
`)
		}
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(fs.Args()) != 0 {
		fs.Usage()
		return exitUsage
	}
	if (*rootfsURL == "") != (*rootfsSHA == "") {
		fmt.Fprintln(os.Stderr, "skrog: --rootfs-url and --rootfs-sha256 go together")
		return exitUsage
	}
	if rollback && (*to != "" || *rootfsURL != "") {
		fmt.Fprintln(os.Stderr, "skrog: rollback takes its target from the install manifest, not --to")
		return exitUsage
	}

	ctx, stop := interruptible()
	defer stop()

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	p := &provision.Provisioner{Logger: cliLogger(false)}
	distro, ok := requireDistroInstall(p, opts)
	if !ok {
		return exitNotFound
	}
	opts.Distro = distro
	im, err := p.ReadManifest(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: reading the install manifest: %v\n", err)
		return exitError
	}
	current := im.EngineRef
	if current == "" {
		current = engineRef(im.EngineVersion, im.RootfsURL)
	}

	target, code := resolveTarget(rollback, *to, *rootfsURL, *rootfsSHA, im, current)
	if code != exitOK {
		return code
	}

	r := &engineupgrade.Runner{
		WSL:     wsl.NewLocal(),
		Fetcher: p,
		Stop: func(ctx context.Context) error {
			// Desired state follows, so a running supervisor does not restart
			// the engine in the middle of the swap.
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
			return p.StartEngine(ctx, opts)
		},
		Healthy: func(ctx context.Context) bool {
			deadline := time.Now().Add(*waitEngine)
			for time.Now().Before(deadline) {
				if p.EngineRunning(ctx, opts) {
					return true
				}
				time.Sleep(time.Second)
			}
			return false
		},
	}
	// Restoring is an upgrade in the other direction, so it reuses everything
	// above rather than being a second code path that can rot.
	r.Restore = func(ctx context.Context, previous string) error {
		prev, err := targetForRef(previous, im)
		if err != nil {
			return err
		}
		_, err = (&engineupgrade.Runner{
			WSL: r.WSL, Fetcher: r.Fetcher, Stop: r.Stop, Start: r.Start, Healthy: r.Healthy,
		}).Run(ctx, engineupgrade.Options{
			StateDir: opts.StateDir, Distro: distro, Target: prev,
		})
		return err
	}

	rep, err := r.Run(ctx, engineupgrade.Options{
		StateDir: opts.StateDir,
		Distro:   distro,
		Target:   target,
		From:     current,
		DryRun:   *dryRun,
	})

	// Record what is installed now, even on a rollback-after-failure: the
	// manifest must describe the engine that is actually in the distro.
	//
	// `err == nil` used to guard this, which excluded the one case the sentence
	// above names. When the new engine failed to start AND the self-heal also
	// failed, the distro carried the target's binaries while the manifest still
	// said the old ref with the *pre-upgrade* previous — so the
	// `skrog engine rollback` that the failure message tells the user to run
	// went two versions down, or reported "no previous engine is recorded" to
	// someone who had just been told to run it (#241).
	//
	// rep.Replaced is what distinguishes "the swap happened" from "we failed
	// before touching anything": a download or staging failure leaves the old
	// engine in place, and recording the target then would be a different lie.
	swapped := !*dryRun && len(rep.Replaced) > 0
	switch {
	case *dryRun:
		// Nothing is installed by a dry run.
	case err == nil, swapped && !rep.RolledBack:
		im.EngineRef = target.Ref
		im.PreviousEngineRef = current
		im.RootfsURL, im.RootfsSHA256 = target.URL, target.SHA256
		im.UpgradedAt = time.Now().UTC()
		// Only when it was confirmed. On the failure path the engine did not
		// answer, so the old value would describe an engine that is no longer
		// in the distro — a manifest naming two different engines at once.
		im.EngineVersion = rep.EngineVersion
		if serr := p.SaveManifest(opts, im); serr != nil {
			fmt.Fprintf(os.Stderr, "skrog: the engine was upgraded but recording it failed: %v\n", serr)
			return exitError
		}
	default:
		// Either nothing was swapped, or the rollback put the previous engine
		// back — both leave the manifest already describing what is installed.
	}

	if *asJSON {
		emitJSON(engineUpgradeJSON{
			From: rep.From, To: rep.To, Replaced: rep.Replaced,
			EngineVersion: rep.EngineVersion, RolledBack: rep.RolledBack,
			DryRun: rep.DryRun, Steps: rep.Steps,
		})
	}
	if err != nil {
		var same *engineupgrade.ErrSameVersion
		if errors.As(err, &same) {
			if !*asJSON {
				fmt.Printf("%v; nothing to do\n", same)
			}
			return exitOK
		}
		if !*asJSON {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		}
		return exitError
	}
	if !*asJSON {
		printEngineReport(rep, rollback)
	}
	return exitOK
}

// resolveTarget picks the engine to move to: the recorded previous one for a
// rollback, an explicit rootfs for development, or a manifest entry.
func resolveTarget(rollback bool, to, url, sha string, im *provision.Manifest, current string) (engineupgrade.Engine, int) {
	if rollback {
		if im.PreviousEngineRef == "" {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", &engineupgrade.ErrNoPrevious{})
			return engineupgrade.Engine{}, exitError
		}
		t, err := targetForRef(im.PreviousEngineRef, im)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return engineupgrade.Engine{}, exitError
		}
		return t, exitOK
	}
	if url != "" {
		return engineupgrade.Engine{Ref: engineRef("", url), URL: url, SHA256: sha}, exitOK
	}

	m, err := release.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return engineupgrade.Engine{}, exitError
	}
	// Only what the manifest lists: that set IS the tested matrix (PLAN §04),
	// and an engine nobody tested with this build is not an upgrade.
	e, err := m.Engine(to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return engineupgrade.Engine{}, exitUsage
	}
	// Refuse before a multi-hundred-megabyte download rather than after it
	// (#388). An explicit --url above is exempt, as it is in `skrog install`:
	// those are bytes the caller chose.
	r, err := e.HostRootfs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		var unsupported *release.ErrUnsupportedHostArch
		if errors.As(err, &unsupported) {
			return engineupgrade.Engine{}, exitUnsupported
		}
		return engineupgrade.Engine{}, exitError
	}
	return engineupgrade.Engine{
		Ref: engineRef(e.Version, r.URL), URL: r.URL, SHA256: r.SHA256,
	}, exitOK
}

// targetForRef finds a rootfs for a recorded engine ref: the manifest first,
// then the install's own record (which is where an engine installed with an
// explicit --rootfs-url lives).
func targetForRef(ref string, im *provision.Manifest) (engineupgrade.Engine, error) {
	if m, err := release.Load(); err == nil {
		for _, e := range m.Engines {
			r, rerr := e.HostRootfs()
			if rerr != nil {
				continue
			}
			if engineRef(e.Version, r.URL) == ref {
				return engineupgrade.Engine{Ref: ref, URL: r.URL, SHA256: r.SHA256}, nil
			}
		}
	}
	if im != nil && im.RootfsURL != "" && engineRef(im.EngineVersion, im.RootfsURL) == ref {
		return engineupgrade.Engine{Ref: ref, URL: im.RootfsURL, SHA256: im.RootfsSHA256}, nil
	}
	return engineupgrade.Engine{}, fmt.Errorf("no rootfs known for engine %s: it is not in this "+
		"build's manifest and not the install's own rootfs. Re-run with --rootfs-url and "+
		"--rootfs-sha256, or reinstall", ref)
}

func printEngineReport(rep engineupgrade.Report, rollback bool) {
	verb := "upgraded"
	if rollback {
		verb = "rolled back"
	}
	if rep.DryRun {
		fmt.Printf("would move the engine from %s to %s:\n", orNone(rep.From), rep.To)
		for _, s := range rep.Steps {
			fmt.Printf("  %s\n", s)
		}
		return
	}
	fmt.Printf("engine %s: %s -> %s", verb, orNone(rep.From), rep.To)
	if rep.EngineVersion != "" {
		fmt.Printf(" (dockerd %s)", rep.EngineVersion)
	}
	fmt.Println()
	fmt.Printf("  %d binaries replaced; images, containers and volumes untouched\n", len(rep.Replaced))
	if !rollback {
		fmt.Printf("  `skrog engine rollback` returns to %s\n", orNone(rep.From))
	}
}

func orNone(s string) string {
	if s == "" {
		return "(unrecorded)"
	}
	return s
}
