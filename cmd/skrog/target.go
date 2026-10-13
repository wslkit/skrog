package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/wslkit/skrog/internal/autostart"
	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/integrate"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/remote"
	"github.com/wslkit/skrog/internal/skrogfile"
	tgt "github.com/wslkit/skrog/internal/target"
	"github.com/wslkit/skrog/internal/wsl"
)

// runTarget is `skrog target`: per-workspace engines (#539, design in
// docs/design/per-workspace-engine.md).
//
// A target is which engine docker talks to: the default shared engine
// (`local`), a named local engine, or a remote registered with `skrog remote`.
// The two nouns the CLI already has stay as they were: `skrog engine` is the
// dockerd VERSION of the current target, `skrog target` is WHICH engine.
func runTarget(args []string) int {
	fs := flag.NewFlagSet("target", flag.ContinueOnError)
	var (
		stateDir      = fs.String("state-dir", "", "override Skrog's state directory")
		asJSON        = fs.Bool("json", false, "emit machine-readable JSON (list, create)")
		yes           = fs.Bool("yes", false, "skip the confirmation prompt (remove)")
		engineVersion = fs.String("engine-version", "", "dockerd version for create (default: this build's default)")
		locked        = fs.String("locked", "", "create from the exact engine pinned in a skrog.lock")
		offline       = fs.String("offline", "", "create entirely from an air-gap bundle .zip")
		rootfsURL     = fs.String("rootfs-url", "", "override the rootfs URL (development)")
		rootfsSHA     = fs.String("rootfs-sha256", "", "expected rootfs SHA-256; required with --rootfs-url")
		headless      = fs.Bool("headless", false, "never prompt; for unattended use")
		autostartOn   = fs.Bool("autostart", false, "create: also start this engine's supervisor at logon")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog target                          list targets
       skrog target create <name> [flags]    a new, isolated engine
       skrog target remove <name> [--yes]    delete an engine and everything in it

A target is which engine docker talks to: the default shared engine (`+"`local`"+`),
a named local engine, or a remote from `+"`skrog remote`"+`. Switch with `+"`skrog use`"+`.

A named engine is a complete, independent install: its own WSL distro and virtual
disk, its own supervisor and pipe, its own docker context (skrog-<name>), its own
images, containers, volumes, settings and snapshots. A poisoned image pulled in one
cannot be run from another, and each can pin its own dockerd version.

create installs the engine and leaves it stopped: it starts when you `+"`skrog use`"+`
it and stops itself when idle, so many engines cost memory only while one is in
use. They do cost disk (hundreds of MB each before any image is pulled); `+"`list`"+`
shows each engine's disk and the total.

Names: lowercase letters, digits, - and _ (1 to 32). %q and "engine" are reserved,
and a name may not be one of your remotes.

remove DELETES the engine's distro and with it every image, container and volume
in it. The default engine and remotes are never touched.

Flags come before the verb. Exit codes: 0 ok, %d error, %d usage, %d no such target,
%d unsupported platform.

flags:
`, tgt.Local, exitError, exitUsage, exitNotFound, exitUnsupported)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	base := targetBase(*stateDir)
	ctx, stop := interruptible()
	defer stop()
	rest := fs.Args()

	switch {
	case len(rest) == 0 || (rest[0] == "list" && len(rest) == 1):
		return targetList(ctx, base, *asJSON)
	case rest[0] == "create" && len(rest) == 2:
		return targetCreate(ctx, base, rest[1], createFlags{
			src: installSource{
				engineVersion: *engineVersion,
				locked:        *locked,
				offline:       *offline,
				rootfsURL:     *rootfsURL,
				rootfsSHA:     *rootfsSHA,
			},
			headless:  *headless,
			autostart: *autostartOn,
			asJSON:    *asJSON,
		})
	case rest[0] == "remove" && len(rest) == 2:
		return targetRemove(ctx, base, rest[1], *yes)
	default:
		fs.Usage()
		return exitUsage
	}
}

// targetBase is the base state dir, whichever state dir the caller named: an
// engine's own directory resolves to the install it hangs off, so remotes, the
// engine registry and the rootfs cache are found from either.
func targetBase(stateDirFlag string) string {
	return tgt.BaseOf(optsWithResolvedStateDir(provision.Options{StateDir: stateDirFlag}).StateDir)
}

type createFlags struct {
	src       installSource
	headless  bool
	autostart bool
	asJSON    bool
}

// engineOptions are the provisioner options of a named engine.
func engineOptions(base string, l tgt.Layout) provision.Options {
	return provision.Options{
		Distro:   l.Distro,
		StateDir: l.StateDir,
		CacheDir: base,
	}
}

func targetCreate(ctx context.Context, base, name string, f createFlags) int {
	if err := tgt.ValidName(name); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitUsage
	}
	if tgt.Exists(base, name) {
		fmt.Fprintf(os.Stderr, "skrog: engine %q already exists (`skrog use %s` to switch to it, "+
			"`skrog target remove %s` to start over)\n", name, name, name)
		return exitError
	}
	if (&remote.Manager{StateDir: base}).Exists(name) {
		fmt.Fprintf(os.Stderr, "skrog: %q is already a remote; targets share one namespace, pick another name\n", name)
		return exitError
	}

	l := tgt.For(base, name)
	log := cliLogger(f.asJSON)
	opts := engineOptions(base, l)
	opts.Headless = f.headless

	opts, cleanup, code := applyInstallSource(opts, f.src, log)
	defer cleanup()
	if code != exitOK {
		return code
	}

	p := &provision.Provisioner{Logger: log}
	log.Info("creating engine", "name", name, "distro", l.Distro, "state", l.StateDir)
	manifest, err := p.Install(ctx, opts)
	if err != nil {
		var pfe *provision.PreflightError
		if errors.As(err, &pfe) {
			// Nothing was imported, and this directory was created by us a
			// moment ago, so leaving it would only make `target list` show an
			// engine that never existed.
			_ = os.RemoveAll(l.StateDir)
			fmt.Fprint(os.Stderr, "skrog: cannot create the engine yet.\n\n")
			for _, pr := range pfe.Report.Problems {
				fmt.Fprintf(os.Stderr, "  %s\n    fix: %s\n\n", pr.Summary, pr.Remedy)
			}
			return exitError
		}
		fmt.Fprintf(os.Stderr, "skrog: creating engine %q failed: %v\n"+
			"  `skrog target remove %s --yes` cleans up whatever was left\n", name, err, name)
		return exitError
	}

	// Install leaves dockerd running. A created engine has no supervisor yet,
	// so nothing would ever stop it: an engine nobody is using would hold
	// memory until the next `wsl --shutdown`. `skrog use` starts it again.
	if err := p.StopEngine(ctx, opts); err != nil {
		log.Warn("could not stop the new engine; `skrog stop` will", "error", err)
	}

	// Its own context, on its own pipe, so creating an engine can never
	// repoint the default engine's `skrog` context (#217).
	host := pipeproxy.DockerHostFor(l.Pipe)
	ctxReady := false
	if err := (&dockerctx.Manager{}).EnsureNamed(ctx, l.Context, "Skrog engine "+name, host); err != nil {
		var noCLI *dockerctx.ErrNoDockerCLI
		if errors.As(err, &noCLI) {
			log.Warn("docker context not created", "reason", err)
		} else {
			log.Warn("could not wire the docker context", "error", err)
		}
	} else {
		ctxReady = true
		manifest.DockerContextHost = host
		if err := p.SaveManifest(opts, manifest); err != nil {
			log.Warn("could not record the docker context endpoint", "error", err)
		}
	}

	if f.autostart {
		if err := applyEngineAutostart(name, l.StateDir, true); err != nil {
			log.Warn("autostart not registered", "reason", err)
		}
	}

	if f.asJSON {
		return emitJSON(targetCreateJSON{Layout: l, EngineVersion: manifest.EngineVersion, DockerContext: ctxReady})
	}
	fmt.Printf("\nEngine %q created (dockerd %s), stopped.\n\n"+
		"  distro   %s\n  state    %s\n  pipe     %s\n  context  %s\n\n"+
		"Switch to it (this starts it):\n\n  skrog use %s\n\n"+
		"and back with `skrog use %s`. It stops itself when idle; "+
		"`skrog target remove %s` deletes it.\n",
		name, manifest.EngineVersion, l.Distro, l.StateDir, l.Pipe, l.Context, name, tgt.Local, name)
	if !ctxReady {
		fmt.Printf("\nNo docker CLI to hold the context; point a client at it with\n  $env:DOCKER_HOST = %q\n", host)
	}
	return exitOK
}

// applyEngineAutostart registers or removes the logon entry of one engine and
// records the choice in that engine's own settings, the way `skrog autostart`
// does for the default engine (#515).
func applyEngineAutostart(name, stateDir string, on bool) error {
	if on {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := autostart.EnableEngine(exe, name, stateDir); err != nil {
			return err
		}
	} else if err := autostart.DisableEngine(name); err != nil {
		return err
	}
	return recordAutostart(stateDir, on)
}

// gatherTargets reads every target on the machine: the default engine, each
// named engine, and each remote.
func gatherTargets(ctx context.Context, base string) (rows []targetEntryJSON, current string) {
	dc := &dockerctx.Manager{}
	engines, _ := tgt.List(base)
	if cur, err := dc.Current(ctx); err == nil {
		current = tgt.CurrentName(cur, engines)
		if current == "" {
			current = currentRemote(ctx, dc)
		}
	}
	p := &provision.Provisioner{}

	local := targetEntryJSON{Name: tgt.Local, Kind: "local", Context: dockerctx.Name, StateDir: base}
	lopts := provision.Options{StateDir: base}
	if m, err := p.ReadManifest(lopts); err == nil {
		local.Distro, local.EngineVersion = m.Distro, m.EngineVersion
		local.State = engineState(ctx, p, provision.Options{Distro: m.Distro, StateDir: base})
		dataDir := m.DataDir
		if dataDir == "" {
			dataDir = base + `\distro`
		}
		local.DiskBytes = tgt.DiskUsage(dataDir)
	} else {
		local.State = "not-installed"
	}
	local.Current = current == tgt.Local
	rows = append(rows, local)

	for _, n := range engines {
		l := tgt.For(base, n)
		e := targetEntryJSON{
			Name: n, Kind: "engine", Context: l.Context, StateDir: l.StateDir,
			Distro: l.Distro, Pipe: l.Pipe,
			DiskBytes: tgt.DiskUsage(l.StateDir),
			Current:   current == n,
		}
		if m, err := p.ReadManifest(provision.Options{StateDir: l.StateDir}); err == nil {
			e.EngineVersion = m.EngineVersion
			e.State = engineState(ctx, p, provision.Options{Distro: l.Distro, StateDir: l.StateDir})
		} else {
			e.State = "incomplete"
		}
		if on, _, err := autostart.EngineStatus(n); err == nil {
			e.Autostart = &on
		}
		rows = append(rows, e)
	}

	if infos, err := (&remote.Manager{StateDir: base}).List(); err == nil {
		for _, i := range infos {
			rows = append(rows, targetEntryJSON{
				Name: i.Name, Kind: "remote", Context: remote.ContextName(i.Name),
				Host: i.Host, Current: current == i.Name,
			})
		}
	}
	return rows, current
}

// engineState is running or stopped for an installed engine, and incomplete
// when its distro is not registered (a create that died half way, or a distro
// removed by hand). A listing only: it never boots the distro it asks about.
func engineState(ctx context.Context, p *provision.Provisioner, opts provision.Options) string {
	running, err := p.DistroRunning(ctx, opts)
	switch {
	case err != nil:
		return "incomplete"
	case running:
		return "running"
	default:
		return "stopped"
	}
}

func targetList(ctx context.Context, base string, asJSON bool) int {
	rows, current := gatherTargets(ctx, base)

	var total int64
	for _, r := range rows {
		total += r.DiskBytes
	}

	if asJSON {
		return emitJSON(targetListJSON{Current: current, Targets: rows, TotalDiskBytes: total})
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, " \tNAME\tKIND\tSTATE\tENGINE\tDISK\tWHERE")
	for _, r := range rows {
		mark := " "
		if r.Current {
			mark = "*"
		}
		disk, where := "", r.StateDir
		if r.Kind != "remote" {
			disk = humanBytes(r.DiskBytes)
		} else {
			where = r.Host
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			mark, r.Name, r.Kind, r.State, r.EngineVersion, disk, where)
	}
	tw.Flush()
	fmt.Printf("\ntotal disk across local engines: %s\n", humanBytes(total))
	if current == "" {
		fmt.Println("(docker currently targets a context that is not one of these)")
	}
	return exitOK
}

func targetRemove(ctx context.Context, base, name string, yes bool) int {
	if err := tgt.ValidName(name); err != nil || !tgt.Exists(base, name) {
		fmt.Fprintf(os.Stderr, "skrog: no such engine %q (`skrog target` lists them)\n", name)
		return exitNotFound
	}
	l := tgt.For(base, name)
	log := cliLogger(false)
	p := &provision.Provisioner{Logger: log}
	opts := engineOptions(base, l)

	if !yes {
		fmt.Printf("This will unregister the WSL distro %q and delete all images, containers and "+
			"volumes in engine %q.\nThis cannot be undone. Continue? [y/N] ", l.Distro, name)
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("aborted")
			return exitOK
		}
	}

	// Logon entry first, for the reason uninstall gives: a Run entry that
	// outlives the engine would relaunch a supervisor into nothing.
	if err := autostart.DisableEngine(name); err != nil {
		log.Warn("could not remove the logon entry", "error", err)
	}
	if err := stopSupervisor(l.StateDir); err != nil {
		log.Warn("continuing with the supervisor still running", "error", err)
	}
	(&integrate.Manager{WSL: &wsl.Local{}, StateDir: l.StateDir, Logger: log}).RemoveAll(ctx)

	// The context, which only this engine uses. If docker is on it, move to the
	// default engine's first: a context pointing at a pipe nobody serves would
	// break every later docker command.
	dc := &dockerctx.Manager{}
	restore := "default"
	if ok, _ := dc.ExistsNamed(ctx, dockerctx.Name); ok {
		restore = dockerctx.Name
	}
	if err := dc.RemoveNamed(ctx, l.Context, restore); err != nil {
		var noCLI *dockerctx.ErrNoDockerCLI
		if !errors.As(err, &noCLI) {
			log.Warn("could not remove the docker context", "error", err)
		}
	}

	// Distro, VHDX and manifest; then the engine's directory, which holds only
	// this engine's logs, settings, snapshots and the state files around them.
	if err := p.Uninstall(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if err := os.RemoveAll(l.StateDir); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: removing %s: %v\n", l.StateDir, err)
		return exitError
	}
	fmt.Printf("Removed engine %q. Nothing else on the system was modified.\n", name)
	return exitOK
}

// runUse is `skrog use`: make docker, and everything that follows the docker
// context (VS Code Dev Containers, compose), talk to a target.
func runUse(args []string) int {
	fs := flag.NewFlagSet("use", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "override Skrog's state directory")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog use <name>|%s

Points docker at a target: the default shared engine (%s), a named engine from
`+"`skrog target create`"+`, or a remote from `+"`skrog remote add`"+`. Switching to a local
engine starts it first, so the next docker command finds it ready.

It switches the docker CONTEXT and nothing else. It does not export DOCKER_HOST
(a process cannot set a variable in its parent shell, and DOCKER_HOST overrides the
context, which would leave the CLI and your editor on different engines); use
DOCKER_CONTEXT for a script that must name one explicitly.

Exit codes: 0 ok, %d error, %d usage, %d no such target.

flags:
`, tgt.Local, tgt.Local, exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	ctx, stop := interruptible()
	defer stop()
	return useTarget(ctx, targetBase(*stateDir), fs.Arg(0))
}

// useTarget switches docker to a target. Shared by `skrog use`,
// `skrog remote use` (kept as an alias so nothing documented breaks) and the
// skrog.yaml `target:` key.
func useTarget(ctx context.Context, base, name string) int {
	dc := &dockerctx.Manager{}
	rm := &remote.Manager{StateDir: base}

	switch {
	case name == tgt.Local:
		return remoteUse(ctx, rm, dc, tgt.Local)

	case tgt.ValidName(name) == nil && tgt.Exists(base, name):
		return useEngine(ctx, base, name, dc)

	case rm.Exists(name):
		return remoteUse(ctx, rm, dc, name)
	}
	fmt.Fprintf(os.Stderr, "skrog: no such target %q (`skrog target` lists them)\n", name)
	return exitNotFound
}

// useEngine starts a named engine and points docker at it.
//
// Started BEFORE the switch: a `docker` that follows `use` should find a ready
// engine, and a start that fails must not leave docker aimed at one that is
// down. The context is ensured here as well as by the supervisor, so the
// switch does not depend on how quickly a freshly spawned supervisor gets to
// wire it.
func useEngine(ctx context.Context, base, name string, dc *dockerctx.Manager) int {
	l := tgt.For(base, name)
	p := &provision.Provisioner{}
	if _, err := p.ReadManifest(provision.Options{StateDir: l.StateDir}); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: engine %q is incomplete (no install recorded); "+
			"`skrog target remove %s --yes` and create it again\n", name, name)
		return exitError
	}

	host := pipeproxy.DockerHostFor(l.Pipe)
	if err := dc.EnsureNamed(ctx, l.Context, "Skrog engine "+name, host); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if code := runStart([]string{"--state-dir", l.StateDir}); code != exitOK {
		return code
	}
	if err := dc.UseNamed(ctx, l.Context); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	fmt.Printf("docker now targets engine %q (context %s)\n", name, l.Context)
	return exitOK
}

// scopedStateDir is the state dir a command acts on when --state-dir is not
// given: the current target's, so `skrog engine upgrade` and `skrog config set
// engine.*` change the engine docker is talking to and not a different one.
//
// Costs nothing on a machine with no named engines (the common case): the
// registry is checked first, and docker is only asked which context is current
// when there is more than one engine to be confused between. Any failure falls
// back to the default engine, which is what these commands always did.
func scopedStateDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	base := optsWithResolvedStateDir(provision.Options{}).StateDir
	if names, _ := tgt.List(base); len(names) == 0 {
		return flagVal
	}
	cur, err := (&dockerctx.Manager{}).Current(context.Background())
	if err != nil {
		return flagVal
	}
	if name := tgt.CurrentName(cur, mustList(base)); name != "" && name != tgt.Local {
		return tgt.For(base, name).StateDir
	}
	return flagVal
}

func mustList(base string) []string {
	names, _ := tgt.List(base)
	return names
}

// autostartEngine is `skrog autostart --engine <name> enable|disable|status`.
func autostartEngine(name, verb string) int {
	base := targetBase("")
	if err := tgt.ValidName(name); err != nil || !tgt.Exists(base, name) {
		fmt.Fprintf(os.Stderr, "skrog: no such engine %q (`skrog target` lists them)\n", name)
		return exitNotFound
	}
	l := tgt.For(base, name)
	switch verb {
	case "enable", "disable":
		if err := applyEngineAutostart(name, l.StateDir, verb == "enable"); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		if verb == "enable" {
			fmt.Printf("autostart enabled for engine %q: its supervisor starts at your next logon\n", name)
		} else {
			fmt.Printf("autostart disabled for engine %q (a running supervisor is not stopped; use `skrog stop --state-dir %q`)\n", name, l.StateDir)
		}
		return exitOK
	case "status", "":
		enabled, cmd, err := autostart.EngineStatus(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		if enabled {
			fmt.Printf("enabled: %s\n", cmd)
			return exitOK
		}
		fmt.Println("disabled")
		return exitNotFound
	default:
		fmt.Fprintln(os.Stderr, "usage: skrog autostart --engine <name> enable|disable|status")
		return exitUsage
	}
}

// installTargetFromConfig is `skrog install --config` for a skrog.yaml that
// names a `target:` (#539): create the engine when it is missing, converge the
// file's settings onto it, and point docker at it. Idempotent, like the
// default-engine path, so the same file drives first use and later convergence.
func installTargetFromConfig(configPath string, f skrogfile.File, opts provision.Options, engineVersion string, noAutostart bool, log *slog.Logger) int {
	if opts.Distro != "" || opts.DataDir != "" || opts.StateDir != "" {
		fmt.Fprintln(os.Stderr, "skrog: --distro, --data-dir and --state-dir do not apply to a skrog.yaml with a target:; "+
			"a named engine's locations are derived from its name")
		return exitUsage
	}
	if engineVersion == "" {
		engineVersion = f.EngineVersion
	}
	base := targetBase("")
	l := tgt.For(base, f.Target)
	ctx, stop := interruptible()
	defer stop()

	if tgt.Exists(base, f.Target) {
		log.Info("engine already exists; converging settings from config", "target", f.Target)
	} else if code := targetCreate(ctx, base, f.Target, createFlags{
		src:      installSource{engineVersion: engineVersion},
		headless: opts.Headless,
	}); code != exitOK {
		return code
	}

	eopts := engineOptions(base, l)
	if err := applySkrogFile(ctx, f, eopts, noAutostart, log); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: applying config: %v\n", err)
		return exitError
	}
	if code := useTarget(ctx, base, f.Target); code != exitOK {
		return code
	}
	fmt.Printf("Engine %q converged from %s and selected.\n", f.Target, configPath)
	return exitOK
}

// engineWiring is what a supervisor for stateDir must serve and wire: the
// named engine's own pipe and docker context, or ok=false for the default
// engine, whose pipe selection and `skrog` context are unchanged.
//
// Derived from the state dir, rather than passed in, so every way a supervisor
// gets started -- `skrog start`, the logon entry, the watchdog restarting it --
// agrees without each having to be told, and cannot disagree after a crash.
func engineWiring(stateDir string) (name string, l tgt.Layout, ok bool) {
	name, ok = tgt.NameOf(stateDir)
	if !ok {
		return "", tgt.Layout{}, false
	}
	return name, tgt.For(tgt.BaseOf(stateDir), name), true
}
