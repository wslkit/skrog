package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/wslkit/skrog/internal/autostart"
	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/selfexe"
	"github.com/wslkit/skrog/internal/version"
	"github.com/wslkit/skrog/internal/wslc"
)

// runInstallWslc records an install that serves a WSL container session
// instead of a distro Skrog owns (#335).
//
// There is nothing to download and nothing to import: Microsoft ships the
// engine, and every session already has it. So this "install" is a check that
// the machine can do it, a proof that it actually works end to end, and a
// manifest saying which backend this machine uses — after which `skrog start`
// serves the pipe exactly as it does for a distro.
//
// It proves rather than assumes on purpose. Writing a manifest and discovering
// at first `docker ps` that wslc is unusable would move the failure to the
// least helpful moment; the session boot costs a few seconds once.
func runInstallWslc(ctx context.Context, opts provision.Options, agentPath string, noAutostart, asJSON bool, log *slog.Logger) int {
	l := wslc.New()

	ver, err := l.Version(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: this machine cannot run WSL containers: %v\n\n"+
			"  wslc.exe ships with WSL 2.9.3 and newer. `wsl --update` installs it.\n", err)
		return exitError
	}
	log.Info("wslc is available", "version", ver)

	// The agent has to exist before anything else: it is the one artifact this
	// backend needs and the one thing a source build may be missing.
	agent, err := loadGuestAgent(ctx, agentPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	sd := optsWithResolvedStateDir(opts).StateDir
	secret, err := wslcSecret(sd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	session, err := l.ResolveSession(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	log.Info("using wslc session", "session", session)

	if err := l.Bootstrap(ctx, session, agent, secret); err != nil {
		if !errors.Is(err, wslc.ErrNoPortForwarding) {
			fmt.Fprintf(os.Stderr, "skrog: could not place the agent in session %q: %v\n", session, err)
			return exitError
		}
		log.Warn("published ports will not reach Windows on this agent")
	}
	log.Info("agent placed in the session", "bytes", len(agent))

	p := &provision.Provisioner{Logger: log}
	manifest := &provision.Manifest{
		Backend:     provision.BackendWslc,
		InstalledAt: time.Now().UTC(),
		WSLVersion:  ver,
	}

	// The pipe a wslc INSTALL serves is the default one, unlike an ad-hoc
	// `skrog proxy --engine wslc` beside a distro install (#337). The rule is
	// not "which backend" but "is this the machine's engine": if it is, plain
	// `docker` should reach it with no flag and no context.
	pipe, pipeReason := pipeproxy.SelectPipeName("")
	dockerHost := pipeproxy.DockerHostFor(pipe)

	mgr := &dockerctx.Manager{}
	previousHost, _ := mgr.Endpoint(ctx)
	contextReady := false
	if err := mgr.Ensure(ctx, dockerHost); err != nil {
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
			log.Warn("the `skrog` docker context now points at this install",
				"was", previousHost, "now", dockerHost)
			manifest.DockerContextPrevious = previousHost
		}
	}

	if err := p.SaveManifest(opts, manifest); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: could not record the install: %v\n", err)
		return exitError
	}

	if asJSON {
		return emitJSON(manifest)
	}

	if !noAutostart {
		if exe, err := os.Executable(); err == nil {
			if err := autostart.Enable(exe); err != nil {
				log.Warn("autostart not registered", "reason", err,
					"fix", "run `skrog start` after each logon")
			} else {
				log.Info("supervisor will start at logon", "disable", "skrog autostart disable")
			}
		}
	}

	fmt.Printf(`
Installed, using a WSL container session as the engine (experimental).

  backend  wslc
  session  %s
  engine   Microsoft's, shipped with WSL %s
  pipe     %s  (%s)

`, session, ver, pipe, pipeReason)

	fmt.Printf("Start the always-on bridge:\n\n  skrog start\n\n")

	if contextReady && pipe == pipeproxy.DefaultPipeName {
		fmt.Printf("Then use docker normally:\n\n  docker run --rm hello-world\n\n")
	} else {
		fmt.Printf("Then point docker at it:\n\n  $env:DOCKER_HOST = %q\n\n", dockerHost)
	}

	fmt.Printf(`This backend cannot pin the engine: ` + "`wsl --update`" + ` moves it, so
` + "`skrog lock`" + `, ` + "`engine upgrade`" + `, ` + "`compact`" + `, ` + "`snapshot`" + ` and ` + "`gpu`" + ` do not apply.
See docs/wslc-backend.md for what does and does not work.

`)
	printCLIHintIfMissing()
	return exitOK
}

// printCLIHintIfMissing tells the user how to get a `docker` command when they
// have none.
//
// `skrog install` provisions the ENGINE; the CLI is a separate, opt-in `skrog
// cli install`. On a machine with Docker Desktop that is invisible, because its
// docker.exe is already on PATH. On a clean machine it is the whole difference
// between "installed" and "usable", and the install output said nothing about
// it -- so the next step was `docker run` and "command not found".
func printCLIHintIfMissing() {
	// selfexe, not os.Executable: a winget portable install runs through a
	// symlink, and the bundled CLI sits beside the real binary (#360).
	if len(version.FindDockerBinaries(version.Env{SkrogBin: selfexe.Dir()})) > 0 {
		return
	}
	fmt.Printf(`No ` + "`docker`" + ` command found on PATH. Skrog runs the engine; the CLI is
separate, and installs the upstream tools (docker, compose, buildx):

  skrog cli install

`)
}
