package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wslkit/skrog/internal/audit"
	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/logging"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/policy"
	"github.com/wslkit/skrog/internal/provision"
)

func runProxy(args []string) int {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	var (
		distro     = fs.String("distro", "", "WSL distro to relay to (default: from the install manifest)")
		stateDir   = fs.String("state-dir", "", "override Skrog's state directory")
		pipeName   = fs.String("pipe", "", "pipe to serve (default: "+pipeproxy.DefaultPipeName+", or Skrog's own if that is taken)")
		socketPath = fs.String("socket", provision.EngineSocket, "engine socket inside the distro")
		noContext  = fs.Bool("no-context", false, "do not create or update the skrog docker context")
		noRewrite  = fs.Bool("no-path-translation", false, "relay bytes verbatim, without translating Windows bind paths")
		// "interactive users" here was the PRE-#79 behaviour. The default has
		// granted the owning user, not IU, since v0.3 -- see defaultSDDL and
		// the dacl test that asserts ";IU)" never appears. This string is
		// published verbatim into docs/reference.md, so it was telling every
		// reader of the command reference that the pipe is open to every
		// interactive account on the machine: the exact vulnerability
		// docs/security.md describes as fixed.
		sddl = fs.String("sddl", "", "security descriptor for the pipe (advanced; default restricts to SYSTEM, administrators and the owning user)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog proxy [flags]

Serves the Windows named pipe that stock docker.exe connects to, relaying it to
the engine inside the WSL2 distro. Runs in the foreground until interrupted.

This is the debug bridge. For everyday use, "skrog supervise" is the always-on
layer: it serves the same pipe and also keeps the engine alive across crashes
(a "wsl --shutdown" is left alone until the next docker command wakes the
engine). "skrog install" registers it to start at logon.

A Windows service that needs no logged-on session is not coming: issue #3
spiked it and the answer was no. WSL2 cannot start from session 0, so the
supervisor runs as you. For an unattended machine, configure auto-logon —
docs/auto-logon-runner.md.

Exit codes: 0 clean shutdown, %d error, %d usage, %d no engine installed.

flags:
`, exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	log := cliLogger(false)
	opts := provision.Options{Distro: *distro, StateDir: *stateDir}

	// The manifest knows which distro this machine actually has, which matters
	// when it was installed under a custom name.
	p := &provision.Provisioner{Logger: log}
	targetDistro := opts.Distro
	if m, err := p.ReadManifest(opts); err == nil && m.Distro != "" {
		targetDistro = m.Distro
	} else if targetDistro == "" {
		fmt.Fprintf(os.Stderr,
			"skrog: no install found. Run `skrog install` first, "+
				"or pass --distro to relay to an existing distro.\n")
		return exitNotFound
	}

	// Make sure the engine is actually up before serving the pipe.
	//
	// A distro shuts down when its last process exits, so the dockerd that
	// `skrog install` started does not outlive the install. Until the v0.2
	// supervisor exists, the proxy is the long-lived process, so starting the
	// engine is its job — otherwise every client sees an EOF and the bridge
	// looks broken when the engine is merely absent.
	startOpts := opts
	startOpts.Distro = targetDistro
	if err := p.StartEngine(interruptCtx(), startOptions(interruptCtx(), startOpts)); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	selected, reason := pipeproxy.SelectPipeName(*pipeName)
	dockerHost := pipeproxy.DockerHostFor(selected)

	listener, err := pipeproxy.Listen(selected, *sddl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	defer listener.Close()

	log.Info("serving pipe", "pipe", selected, "reason", reason)
	log.Info("relaying to engine", "distro", targetDistro, "socket", *socketPath)

	// Wiring the context is a convenience, not a precondition: a missing docker
	// CLI must not stop the bridge from running.
	if !*noContext {
		mgr := &dockerctx.Manager{}
		if err := mgr.Ensure(interruptCtx(), dockerHost); err != nil {
			var noCLI *dockerctx.ErrNoDockerCLI
			if errors.As(err, &noCLI) {
				log.Warn("skipping docker context", "reason", err)
			} else {
				log.Warn("could not wire the docker context", "error", err)
			}
		} else {
			log.Info("docker context ready", "context", dockerctx.Name, "host", dockerHost)
		}
	}

	dialer := engineDialer(targetDistro, *socketPath, opts.StateDir, log)
	srv := &pipeproxy.Server{
		Dialer: dialer,
		Logger: log,
	}
	if !*noRewrite {
		// Guarded here too. This is the debug bridge, so the case is weaker
		// than `skrog serve` — but it served the same engine with the owner's
		// rules unenforced and its calls unaudited, which is not something a
		// debug flag should quietly buy you (#257).
		//
		// --no-rewrite still turns the whole HTTP layer off, gate included:
		// that flag exists to take Skrog out of the request path entirely.
		// Resolved, not the raw flag: --state-dir is optional, and policy.Path("")
		// would look for policy.yaml in the current directory.
		sd := optsWithResolvedStateDir(opts).StateDir
		settings := config.NewWatcher(sd)
		watcher := policy.NewWatcher(sd)
		watcher.OnError = func(err error) {
			log.Error("policy file is not valid", "error", err, "path", policy.Path(sd))
		}
		auditor := &audit.Switch{
			Enabled: func() bool { return settings.Config().Audit },
			Open: func() (io.WriteCloser, error) {
				return logging.NewRotatingWriter(filepath.Join(sd, "audit.log"), 0, 0)
			},
		}
		defer auditor.Close()
		// Provenance too, for the same reason the gate is here: this bridge
		// serves the same engine, so a rule that stopped at the supervisor's
		// pipe would be one `skrog proxy` away from not applying (#343).
		prov := &imageProvenance{stateDir: sd, dialer: dialer, log: log}
		go prov.SeedExisting(interruptCtx())
		srv.Handler = pipeproxy.RewriteBindsProvenanced(auditor, watcher, prov)
	}

	fmt.Fprintf(os.Stderr, `
Bridge is up. In another shell:

  docker --context %s ps
  $env:DOCKER_HOST = "%s"; docker ps

Ctrl-C to stop.

`, dockerctx.Name, dockerHost)

	ctx, stop := interruptible()
	defer stop()

	if err := srv.Serve(ctx, listener); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	log.Info("bridge stopped")
	return exitOK
}
