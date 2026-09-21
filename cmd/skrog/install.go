package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/wslkit/skrog/internal/autostart"
	"github.com/wslkit/skrog/internal/bundle"
	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/integrate"
	"github.com/wslkit/skrog/internal/lockfile"
	"github.com/wslkit/skrog/internal/logging"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/release"
	"github.com/wslkit/skrog/internal/wsl"
)

// cliLogger prints progress as plain lines rather than structured logfmt: this
// is a person watching an install, not a log aggregator.
func cliLogger(quiet bool) *slog.Logger {
	level := slog.LevelInfo
	if quiet {
		level = slog.LevelWarn
	}
	return slog.New(newConsoleHandler(os.Stderr, level))
}

// interruptible returns a context cancelled on Ctrl-C, so a long download stops
// promptly and leaves no partial file behind (the provisioner handles cleanup).
func interruptible() (context.Context, func()) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func runInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	var (
		engineVersion = fs.String("engine-version", "", "engine version to install (default: this build's default)")
		distro        = fs.String("distro", "", "WSL distro name (default: "+provision.DefaultDistro+")")
		dataDir       = fs.String("data-dir", "", "where the distro's VHDX lives (default: under the state dir)")
		stateDir      = fs.String("state-dir", "", "override Skrog's state directory")
		headless      = fs.Bool("headless", false, "never prompt; for unattended and CI installs")
		noAutostart   = fs.Bool("no-autostart", false, "do not register the supervisor to start at logon")
		noVerifySig   = fs.Bool("no-verify-signature", false, "skip the rootfs signature check for this install (the SHA-256 pin still applies)")
		rootfsURL     = fs.String("rootfs-url", "", "override the rootfs URL (development)")
		rootfsSHA     = fs.String("rootfs-sha256", "", "expected rootfs SHA-256; required with --rootfs-url")
		asJSON        = fs.Bool("json", false, "emit the resulting manifest as JSON")
		configPath    = fs.String("config", "", "declarative install from a skrog.yaml (see `skrog config export`)")
		locked        = fs.String("locked", "", "install the exact engine pinned in a skrog.lock (see `skrog lock`)")
		offline       = fs.String("offline", "", "install entirely from an air-gap bundle .zip (see `skrog bundle`)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog install [flags]

Provisions the Skrog engine distro: checks the host, downloads and verifies the
rootfs, imports it as a WSL2 distro, and starts the engine.

The rootfs is always checksum-verified. Version pinning is a contract: this
build installs exactly the components in its embedded manifest, and nothing is
fetched as "latest".

Exit codes: 0 ok, %d error, %d usage, %d unsupported platform (the engine
rootfs is amd64-only; see issue #388).

flags:
`, exitError, exitUsage, exitUnsupported)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	log := cliLogger(*asJSON)

	if *configPath != "" {
		return runInstallFromConfig(*configPath, provision.Options{
			Distro:   *distro,
			StateDir: *stateDir,
			DataDir:  *dataDir,
			Headless: *headless,
		}, *engineVersion, *noAutostart, log)
	}

	opts := provision.Options{
		Distro:   *distro,
		StateDir: *stateDir,
		DataDir:  *dataDir,
		Headless: *headless,
	}

	if n := boolCount(*locked != "", *rootfsURL != "", *offline != ""); n > 1 {
		fmt.Fprintln(os.Stderr, "skrog: choose at most one of --locked, --offline, --rootfs-url")
		return exitUsage
	}

	// An explicit URL bypasses the manifest, so it must carry its own digest:
	// there is no code path that imports an unverified rootfs.
	switch {
	case *offline != "":
		b, err := bundle.Open(*offline)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
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
			return exitError
		}
		defer os.RemoveAll(filepath.Dir(extracted))
		opts.RootfsURL = extracted
		opts.RootfsSHA256 = l.Rootfs.SHA256
		opts.EngineVersion = l.EngineVersion
		log.Info("installing offline from bundle", "path", *offline, "engine", l.EngineVersion)
	case *locked != "":
		l, err := lockfile.Load(*locked)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		// The lock's rootfs URL + SHA-256 pin the artifact; the existing verified
		// download refuses on any mismatch, so a tampered lock cannot install
		// something else.
		opts.RootfsURL = l.Rootfs.URL
		opts.RootfsSHA256 = l.Rootfs.SHA256
		opts.EngineVersion = l.EngineVersion
		log.Info("installing from lockfile", "path", *locked, "engine", l.EngineVersion)
	case *rootfsURL != "" && *rootfsSHA == "":
		fmt.Fprintln(os.Stderr, "skrog: --rootfs-url requires --rootfs-sha256")
		return exitUsage
	case *rootfsURL != "":
		opts.RootfsURL = *rootfsURL
		opts.RootfsSHA256 = *rootfsSHA
		opts.EngineVersion = *engineVersion
		log.Warn("using an overridden rootfs; this is a development path, not a release install",
			"url", *rootfsURL)
	default:
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
				return exitUnsupported
			}
			return exitError
		}
		opts.RootfsURL = rootfs.URL
		opts.RootfsSHA256 = rootfs.SHA256
		opts.EngineVersion = engine.Version
	}

	// Signature verification is opt-in (#147) and lives in config so a fleet
	// sets it once. --no-verify-signature turns it off for one install and says
	// so out loud, because an unlogged bypass of a security check is worse than
	// not having the check.
	if c, err := config.Load(optsWithResolvedStateDir(opts).StateDir); err == nil && c.VerifySignature {
		if *noVerifySig {
			log.Warn("skipping the rootfs signature check (--no-verify-signature); " +
				"the SHA-256 pin is still enforced")
		} else {
			opts.VerifySignature = true
		}
	}

	ctx, stop := interruptible()
	defer stop()

	p := &provision.Provisioner{Logger: log}
	manifest, err := p.Install(ctx, opts)
	if err != nil {
		// A preflight failure is the user's to act on, and its remedies are
		// already formatted; anything else is reported plainly.
		var pfe *provision.PreflightError
		if errors.As(err, &pfe) {
			fmt.Fprint(os.Stderr, "skrog: cannot install yet.\n\n")
			for _, p := range pfe.Report.Problems {
				fmt.Fprintf(os.Stderr, "  %s\n    fix: %s\n\n", p.Summary, p.Remedy)
			}
			return exitError
		}
		fmt.Fprintf(os.Stderr, "skrog: install failed: %v\n", err)
		return exitError
	}

	if *asJSON {
		return emitJSON(manifest)
	}

	// The context points at whichever pipe the bridge will serve, decided the
	// same way `skrog proxy` decides it so the two cannot disagree.
	pipe, pipeReason := pipeproxy.SelectPipeName("")
	dockerHost := pipeproxy.DockerHostFor(pipe)

	// Autostart is registered by default: "install once, docker ps works
	// forever" is the headline promise, and a supervisor that only runs when
	// launched by hand does not deliver it. Refusal is honest, not fatal —
	// missing skrogw.exe (a bare go-build binary rather than a release zip)
	// downgrades to a warning with the manual alternative named.
	if !*noAutostart {
		if exe, err := os.Executable(); err == nil {
			if err := autostart.Enable(exe); err != nil {
				log.Warn("autostart not registered", "reason", err,
					"fix", "run `skrog start` after each logon, or `skrog autostart enable` from a release install")
			} else {
				log.Info("supervisor will start at logon", "disable", "skrog autostart disable")
			}
		}
	}

	// Registering the Event Log source needs elevation; cosmetic when absent
	// (entries render with a boilerplate prefix), so best-effort by design.
	if err := logging.RegisterEventSource(); err != nil {
		log.Debug("event log source not registered (needs elevation; cosmetic)", "error", err)
	}

	contextReady := false
	mgr := &dockerctx.Manager{}
	// What the context pointed at before, so repointing an existing one is
	// visible rather than silent: the `skrog` context is a single global
	// object, and on a machine with a second install this is another
	// install's engine being taken over (#217).
	previousHost, _ := mgr.Endpoint(ctx)
	if err := mgr.Ensure(ctx, dockerHost); err != nil {
		// A missing docker CLI is not a reason to fail a working install.
		var noCLI *dockerctx.ErrNoDockerCLI
		if errors.As(err, &noCLI) {
			log.Warn("docker context not created", "reason", err)
		} else {
			log.Warn("could not wire the docker context", "error", err)
		}
	} else {
		contextReady = true
		manifest.DockerContextHost = dockerHost
		if previousHost != "" && previousHost != dockerHost {
			// Another install had it. Say so, and remember where to put it
			// back — uninstalling this one must not leave that install with
			// no context (#217).
			log.Warn("the `skrog` docker context now points at this install",
				"was", previousHost, "now", dockerHost,
				"note", "uninstalling this install restores it")
			manifest.DockerContextPrevious = previousHost
		}
		if err := p.SaveManifest(opts, manifest); err != nil {
			log.Warn("could not record the docker context endpoint", "error", err)
		}
	}

	fmt.Printf(`
Engine installed and running.

  distro   %s
  engine   %s
  data     %s
  pipe     %s  (%s)

`, manifest.Distro, manifest.EngineVersion, manifest.DataDir, pipe, pipeReason)

	fmt.Printf(`Start the always-on bridge:

  skrog start

`)
	if contextReady {
		// Which command to show depends on which pipe we took, and the old
		// message ignored that: it said "use docker normally" and then handed
		// the user `--context skrog`, a flag most of them never need (#272).
		//
		// When the default pipe was free we are serving the endpoint
		// `docker.exe` already talks to, so plain `docker run` works and the
		// context is only there for switching between engines. Showing the
		// flag as the happy path made the product look like it needs ceremony
		// it does not, on exactly the machine it is built for — one without
		// Docker Desktop.
		if pipe == pipeproxy.DefaultPipeName {
			fmt.Printf(`Then use docker normally:

  docker run --rm hello-world

Skrog is serving the pipe docker already talks to, so no flag is needed. The
%q context points here explicitly, for switching between engines:

  docker --context %s run --rm hello-world

`, dockerctx.Name, dockerctx.Name)
		} else {
			fmt.Printf(`Docker Desktop is serving the default pipe, so Skrog took its own. Reach it
with the %q context:

  docker --context %s run --rm hello-world

Or make it the default for every shell:

  docker context use %s

`, dockerctx.Name, dockerctx.Name, dockerctx.Name)
		}
	} else {
		fmt.Printf(`Then point a docker client at it:

  $env:DOCKER_HOST = "%s"
  docker run --rm hello-world

`, dockerHost)
	}
	if !*noAutostart {
		fmt.Println("From your next logon the supervisor starts automatically;")
		fmt.Println("`skrog autostart disable` turns that off.")
	}
	fmt.Println()
	printCLIHintIfMissing()
	return exitOK
}

// boolCount returns how many of its arguments are true.
func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func runUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	var (
		distro   = fs.String("distro", "", "WSL distro name (default: from the install manifest)")
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		yes      = fs.Bool("yes", false, "skip the confirmation prompt")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog uninstall [--yes]

Unregisters the engine distro and removes Skrog's own state. Nothing else on
the system is touched.

This DELETES the distro, and with it every image, container and volume it
holds. Export anything you want to keep first.

Exit codes: 0 ok, %d error.

flags:
`, exitError)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := provision.Options{Distro: *distro, StateDir: *stateDir}
	log := cliLogger(false)
	p := &provision.Provisioner{Logger: log}

	// Say what will actually be destroyed, using the recorded manifest rather
	// than a guess, before asking.
	ownManifest, _ := p.ReadManifest(opts)
	target := opts.Distro
	if ownManifest != nil && ownManifest.Distro != "" {
		target = ownManifest.Distro
	} else if target == "" {
		target = provision.DefaultDistro
	}

	if !*yes {
		fmt.Printf("This will unregister the WSL distro %q and delete all images, "+
			"containers and volumes in it.\nThis cannot be undone. Continue? [y/N] ", target)
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("aborted")
			return exitOK
		}
	}

	ctx, stop := interruptible()
	defer stop()

	// Autostart goes first: a Run entry pointing at a binary that is about to
	// stop having anything to supervise would relaunch a supervisor into an
	// empty install at next logon. Ownership-checked, so uninstalling a
	// secondary install (the e2e suite beside a real one) cannot delete the
	// primary install's entry.
	if exe, err := os.Executable(); err == nil {
		if removed, err := autostart.DisableIfOwned(filepath.Dir(exe)); err != nil {
			log.Warn("could not remove the autostart entry", "error", err)
		} else if removed {
			log.Info("autostart entry removed")
		}
	}
	if err := logging.UnregisterEventSource(); err != nil {
		log.Debug("event log source not unregistered (needs elevation; cosmetic)", "error", err)
	}

	// Unwire every distro wsl-integrate touched: "nothing else on the system
	// was modified" must include profile scripts in the user's own distros.
	(&integrate.Manager{WSL: &wsl.Local{}, StateDir: opts.StateDir, Logger: log}).RemoveAll(ctx)

	// Remove the context before the distro: leaving a context pointed at a
	// pipe nobody serves would make every later docker command fail, which is
	// not "nothing else on the system was modified".
	//
	// But the `skrog` context is a single global object, and a machine can
	// have more than one install — the e2e suite runs one beside a real one.
	// Removing it unconditionally took the first install's context away when
	// the second was uninstalled, and the symptom (`context "skrog": context
	// not found`) looked like Skrog was broken rather than like a cleanup
	// that reached too far (#217). So: only if it is still ours.
	mgr := &dockerctx.Manager{}
	switch ours, why := contextIsOurs(ctx, mgr, ownManifest); {
	case !ours:
		log.Info("leaving the `skrog` docker context alone", "reason", why)

	case ownManifest != nil && ownManifest.DockerContextPrevious != "":
		// This install took the context over from another one. Hand it back
		// instead of deleting it: that install is still running, and its
		// docker commands go through this exact context.
		if err := mgr.Ensure(ctx, ownManifest.DockerContextPrevious); err != nil {
			log.Warn("could not restore the docker context", "error", err,
				"to", ownManifest.DockerContextPrevious)
		} else {
			log.Info("docker context handed back to the install that had it",
				"endpoint", ownManifest.DockerContextPrevious)
		}

	default:
		if err := mgr.Remove(ctx, ""); err != nil {
			var noCLI *dockerctx.ErrNoDockerCLI
			if errors.As(err, &noCLI) {
				log.Debug("no docker CLI, nothing to unwire", "reason", err)
			} else {
				log.Warn("could not remove the docker context", "error", err)
			}
		}
	}

	if err := p.Uninstall(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	fmt.Println("Removed. Nothing else on the system was modified.")
	return exitOK
}

// contextIsOurs decides whether `skrog uninstall` may remove the shared
// `skrog` docker context (#217), and says why when it may not.
//
// The rule: the context is ours if it still points where this install wired
// it. If another install has since repointed it, that install is using it and
// it is not ours to delete — removing it there is how uninstalling a second
// install broke the first one.
//
// An install that predates the recorded endpoint gets the old behaviour. That
// is deliberate: those machines were installed when one install was the only
// possibility, so the context almost certainly is theirs, and leaving a
// context pointed at a pipe nobody serves would break every later docker
// command — the worse of the two failures.
func contextIsOurs(ctx context.Context, mgr *dockerctx.Manager, m *provision.Manifest) (bool, string) {
	if m == nil || m.DockerContextHost == "" {
		return true, ""
	}
	current, err := mgr.Endpoint(ctx)
	if err != nil {
		// No docker CLI, or no context: Remove handles both, and guessing
		// "not ours" here would leave a dangling context behind instead.
		return true, ""
	}
	if current == m.DockerContextHost {
		return true, ""
	}
	return false, fmt.Sprintf(
		"it points at %s, not this install's %s — another install owns it now",
		current, m.DockerContextHost)
}
