package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/dockercli"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/release"
	"github.com/wslkit/skrog/internal/selfexe"
	"github.com/wslkit/skrog/internal/supervise"
	"github.com/wslkit/skrog/internal/upgrade"
)

// runUpgrade is `skrog upgrade`: one answer to "am I current?" across the
// app, the engine and the bundled docker CLI (#191).
func runUpgrade(args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	var (
		check    = fs.Bool("check", false, "report only; change nothing")
		apply    = fs.Bool("apply", false, "also replace skrog.exe itself with the newer release")
		dryRun   = fs.Bool("dry-run", false, "print what would be applied, and apply nothing")
		yes      = fs.Bool("yes", false, "skip the confirmation prompt (for runners)")
		offline  = fs.Bool("offline", false, "skip the network check for the app version")
		asJSON   = fs.Bool("json", false, "emit machine-readable JSON (implies --check)")
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		timeout  = fs.Duration("timeout", 15*time.Second, "how long to wait for the releases API")
		force    = fs.Bool("force", false, "replace the binary even when a package manager owns this install")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog upgrade [--check|--dry-run] [--yes] [--offline] [--json]

Reports whether the app, the engine and the bundled docker CLI are current,
then brings forward the two it owns — after showing you what it will do.

  skrog upgrade            everything
  skrog engine upgrade     just the engine

The app is reported and not applied by default, because replacing the running
binary restarts the supervisor and briefly drops the docker pipe. `+"`--apply`"+`
does it: the release zip is downloaded and checked against that release's
SHA256SUMS BEFORE anything on disk is touched, the binaries are moved aside
rather than overwritten, and a failure at any point leaves the install exactly
as it was.

  --check     report only; change nothing
  --apply     replace skrog.exe too, not just the engine and the CLI
  --dry-run   print exactly what would be applied, and apply nothing
  --yes       do not ask (runners)
  --force     replace the binary even when a package manager owns this install

--apply replaces the files. skrog.exe takes effect immediately, because the
supervisor is recycled onto the new one; skrogw.exe and skrogtray.exe take
effect when those processes next start, which for most people is the next
logon. The command says which is which rather than implying it all swapped.

Why this is not just a convenience: the engines `+"`skrog engine upgrade`"+` can
reach are pinned in THIS binary's manifest. A newer engine can therefore need
a newer skrog first — so "engine: current" is only ever true of the build you
are running, and this command says so when it matters.

Nothing here auto-updates or polls in the background. It runs when you ask,
and makes exactly one outbound request — to the releases API, for the app
version. The engine and CLI answers are local (both manifests are compiled
in), so `+"`--offline`"+` still reports those. Air-gapped installs should use it.

The CLI is applied before the engine: it is a file copy that costs no
downtime, where an engine upgrade stops and restarts the engine. A failed
engine upgrade therefore leaves the CLI already current rather than nothing
done, and `+"`skrog engine rollback`"+` reverses the engine half on its own.

Exit codes: 0 nothing to do or everything applied, %d error, %d usage,
%d something can be upgraded (--check and --dry-run only).

flags:
`, exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// --json is a report format, and there is no way to ask a question in
	// JSON — so it never applies anything, the same rule `skrog wsl-config`
	// follows.
	reportOnly := *check || *dryRun || *asJSON

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})

	c := &upgrade.Checker{
		App:             buildVersion,
		EngineLatest:    newestPublishedEngine(),
		EngineLatestRef: newestPublishedEngineRef(),
		CLILatest:       bundledCLIVersion(),
		Installed: upgrade.Installed{
			EngineVersion: installedEngineVersion(opts),
			EngineRef:     installedEngineRef(opts),
			CLIVersion:    installedCLIVersion(opts),
		},
	}
	if !*offline {
		c.Releases = &upgrade.GitHubReleases{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	rep := c.Check(ctx)

	if *asJSON {
		if code := emitJSON(rep); code != exitOK {
			return code
		}
	} else {
		if err := rep.WriteText(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
	}

	plan := rep.Plan()
	if reportOnly {
		if *dryRun && len(plan) > 0 {
			fmt.Println("\nwould run:")
			printUpgradePlan(plan)
			fmt.Println("\nnothing was changed (--dry-run)")
		}
		// Exit 3 for "there is something to install", matching
		// `skrog cli status` so a script can gate on either the same way.
		if rep.Available() {
			return exitNotFound
		}
		return exitOK
	}

	appBehind := rep.AppStream().Status == upgrade.StatusAvailable

	if len(plan) == 0 && !(appBehind && *apply) {
		// The app being behind is not something this command can act on
		// without --apply, and saying "nothing to do" without that
		// qualification would read as "you are current".
		if rep.Available() {
			fmt.Println("\nnothing here is Skrog's to apply — see above")
			if appBehind {
				fmt.Println("`skrog upgrade --apply` will replace skrog.exe itself")
			}
		}
		return exitOK
	}

	if !*yes {
		fmt.Println("\nwill run:")
		printUpgradePlan(plan)
		if appBehind && *apply {
			fmt.Printf("  %-16s %s -> %s  (replaces skrog.exe; the supervisor restarts)\n",
				"app", buildVersion, rep.AppStream().Latest)
		}
		if !confirm("\nproceed?") {
			fmt.Println("nothing was changed")
			return exitOK
		}
	}
	if code := applyUpgradePlan(plan, *stateDir); code != exitOK {
		return code
	}
	// The app last: it is the only step that restarts the supervisor, so a
	// failure in the engine or CLI half leaves the running binary the one that
	// produced the log the user is about to read.
	if appBehind && *apply {
		return applyApp(rep.AppStream().Latest, opts.StateDir, *force)
	}
	return exitOK
}

// printUpgradePlan shows the commands, not a summary of them: the reader can
// then run any of them by hand, and can see that nothing else is happening.
func printUpgradePlan(plan []upgrade.Action) {
	for _, a := range plan {
		fmt.Printf("  skrog %-16s %s -> %s\n", joinArgs(a.Args), orUnset(a.From), a.To)
	}
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

func orUnset(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// applyUpgradePlan runs each action in order, stopping at the first failure.
//
// Stopping rather than continuing is deliberate: the actions are independent,
// so what already succeeded stands and needs no unwinding, and pressing on
// after an engine upgrade failed would stack a second change on top of a
// machine whose state nobody has looked at yet.
func applyUpgradePlan(plan []upgrade.Action, stateDir string) int {
	for _, a := range plan {
		args := append([]string(nil), a.Args[1:]...)
		if stateDir != "" {
			args = append(args, "--state-dir", stateDir)
		}
		fmt.Printf("\n== %s: %s -> %s\n", a.Stream, orUnset(a.From), a.To)

		var code int
		switch a.Args[0] {
		case "cli":
			code = runCLI(args)
		case "engine":
			code = runEngine(args)
		default:
			fmt.Fprintf(os.Stderr, "skrog: no way to apply %q\n", a.Stream)
			return exitError
		}
		if code != exitOK {
			fmt.Fprintf(os.Stderr,
				"\nskrog: %s upgrade failed (exit %d); stopping before anything else\n",
				a.Stream, code)
			if a.Stream == "engine" {
				fmt.Fprintln(os.Stderr,
					"the engine upgrade is reversible: `skrog engine rollback`")
			}
			return code
		}
	}
	fmt.Println("\nup to date")
	return exitOK
}

// newestPublishedEngine is the newest engine this build can actually install.
//
// Published, not merely listed: an entry without a checksum is a placeholder
// for a rootfs release that has not been cut, and offering it as an upgrade
// would point `skrog engine upgrade` at something it will refuse.
func newestPublishedEngine() string {
	m, err := release.Load()
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range m.Engines {
		if !e.Published() {
			continue
		}
		if best == "" || upgrade.Compare(e.Version, best) > 0 {
			best = e.Version
		}
	}
	return best
}

// newestPublishedEngineRef is newestPublishedEngine as a ref -- the version
// with its rootfs revision (#481). Two engines can share a version and differ
// in what they carry, so the revision is the part that answers "is there
// anything to do".
func newestPublishedEngineRef() string {
	m, err := release.Load()
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range m.Engines {
		if !e.Published() {
			continue
		}
		ref := engineRef(e.Version, engineRefURL(e))
		if best == "" || upgrade.Compare(ref, best) > 0 {
			best = ref
		}
	}
	return best
}

// installedEngineRef is what this machine actually has, revision included.
// Empty when the install predates the recorded ref and the URL does not yield
// one -- the caller falls back to the bare version rather than guessing.
func installedEngineRef(opts provision.Options) string {
	p := &provision.Provisioner{}
	m, err := p.ReadManifest(opts)
	if err != nil {
		return ""
	}
	return m.EngineRefOrDerived()
}

// bundledCLIVersion is the docker CLI version this build ships, taken from the
// `docker` component itself rather than from compose or buildx.
func bundledCLIVersion() string {
	m, err := dockercli.Load()
	if err != nil {
		return ""
	}
	for _, c := range m.Components {
		if c.Role == dockercli.RoleCLI {
			return c.Version
		}
	}
	return ""
}

// installedEngineVersion reads what the install manifest recorded. Empty means
// no engine is installed, which is a different answer from "unknown".
func installedEngineVersion(opts provision.Options) string {
	p := &provision.Provisioner{}
	m, err := p.ReadManifest(opts)
	if err != nil {
		return ""
	}
	return m.EngineVersion
}

// dockerVersionLine matches `docker --version` output: "Docker version
// 29.8.0, build 1234567".
var dockerVersionLine = regexp.MustCompile(`(?i)version\s+([0-9][0-9.\-]*)`)

// installedCLIVersion asks the installed docker.exe what it is.
//
// Asking the binary rather than trusting the file's presence is the point:
// `skrog cli status` reports a stale docker.exe as "installed", which is
// true and unhelpful — the question here is whether it is the version this
// build ships, and only the binary knows that.
func installedCLIVersion(opts provision.Options) string {
	exe := filepath.Join(cliBinDir(opts.StateDir), "docker.exe")
	if _, err := os.Stat(exe); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, exe, "--version").Output()
	if err != nil {
		return ""
	}
	if m := dockerVersionLine.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// applyApp replaces the installed binaries with a newer release (#309).
//
// The order is the whole safety argument: download, verify, and only then
// touch the install directory. Nothing here can leave a half-upgraded install
// -- SwapBinaries rolls back if any file cannot be replaced, and a download
// that fails verification never reaches the swap at all.
func applyApp(version, stateDir string, force bool) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: locating the running binary: %v\n", err)
		return exitError
	}

	// Refuse to fight a package manager (#370).
	//
	// The check needs the RESOLVED path, because a winget install is reached
	// through an alias symlink and only the target is inside the package
	// directory (#360). The swap below deliberately uses the UNRESOLVED one:
	// they are different questions, and conflating them is what #360 was.
	if resolved, rerr := selfexe.Path(); rerr == nil {
		if owner, owned := upgrade.OwnedBy(resolved); owned && !force {
			fmt.Fprintf(os.Stderr, "skrog: this install is managed by %s, so `skrog upgrade --apply` "+
				"would overwrite files it owns and leave it reporting a version you no longer have.\n\n"+
				"  upgrade with:  %s\n\n"+
				"Pass --force to replace the binary anyway.\n", owner.Name, owner.Command)
			return exitError
		}
	}

	// Deliberately NOT resolved through symlinks (#360). Everywhere else that
	// derives a sibling uses internal/selfexe, but replacing a binary is the
	// one case where following a link would be wrong.
	dir := filepath.Dir(exe)

	// Clear leftovers from a previous upgrade before making new ones, so the
	// directory never accumulates two generations of .old.
	if cleaned := upgrade.CleanOld(dir); len(cleaned) > 0 {
		fmt.Printf("  cleaned up from the last upgrade: %s\n", strings.Join(cleaned, ", "))
	}

	fmt.Printf("\n== app: %s -> %s\n", buildVersion, version)

	staged, err := os.MkdirTemp("", "skrog-upgrade-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	defer os.RemoveAll(staged)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Printf("  downloading %s\n", upgrade.AssetName(version, runtime.GOARCH))
	if err := (&upgrade.Fetcher{}).Stage(ctx, version, runtime.GOARCH, staged); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		fmt.Fprintln(os.Stderr, "nothing was changed")
		return exitError
	}
	fmt.Println("  verified against the release's SHA256SUMS")

	res, err := upgrade.SwapBinaries(dir, staged)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		fmt.Fprintln(os.Stderr, "the install was rolled back and is unchanged")
		return exitError
	}
	fmt.Printf("  replaced %s in %s\n", strings.Join(res.Replaced, ", "), dir)

	// Recycle the supervisor so the new skrog.exe is the one serving. skrogw
	// re-spawns it by path, so this is all it takes.
	if supervise.Held(stateDir) {
		fmt.Println("  restarting the supervisor onto the new binary")
		if code := recycleSupervisor(stateDir); code != exitOK {
			fmt.Fprintln(os.Stderr,
				"skrog: the binaries are new but the supervisor did not come back; run `skrog start`")
			return code
		}
	}

	// Stated rather than probed. Which processes are still on the old image is
	// fixed by design -- skrog.exe is live now via the recycle above, the other
	// two at their next start -- and the obvious probe was wrong twice over
	// (see upgrade.SwapResult).
	fmt.Println("\nnote: skrogw.exe and skrogtray.exe pick up the new build when they next\n" +
		"      start — the tray on relaunch, the watchdog at your next logon.")
	fmt.Printf("\nskrog %s is installed. `skrog version` confirms it.\n", version)
	return exitOK
}
