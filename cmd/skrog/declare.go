package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/wslkit/skrog/internal/autostart"
	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/engineconfig"
	"github.com/wslkit/skrog/internal/integrate"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/release"
	"github.com/wslkit/skrog/internal/skrogfile"
	"github.com/wslkit/skrog/internal/wsl"
)

// runInstallFromConfig provisions and converges an install from a skrog.yaml.
// It is idempotent: an existing install skips provisioning and only re-applies
// settings, so the same file drives both first install and later convergence —
// the infrastructure-as-code story (#69). Explicit flags win over file fields.
func runInstallFromConfig(configPath string, opts provision.Options, engineVersion string, noAutostart bool, log *slog.Logger) int {
	f, err := skrogfile.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if opts.Distro == "" {
		opts.Distro = f.Distro
	}
	if opts.DataDir == "" {
		opts.DataDir = f.DataDir
	}
	if engineVersion == "" {
		engineVersion = f.EngineVersion
	}
	opts = optsWithResolvedStateDir(opts)

	ctx, stop := interruptible()
	defer stop()

	p := &provision.Provisioner{Logger: log}
	if _, err := p.ReadManifest(opts); err == nil {
		log.Info("engine already installed; converging settings from config")
	} else {
		rel, err := release.Load()
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		engine, err := rel.Engine(engineVersion)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitUsage
		}
		// Selects this host's architecture and says why when there is none
		// (#388); HostRootfs covers both "never built for this" and "built
		// but not released yet".
		rootfs, err := engine.HostRootfs()
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		opts.RootfsURL = rootfs.URL
		opts.RootfsSHA256 = rootfs.SHA256
		opts.EngineVersion = engine.Version
		if _, err := p.Install(ctx, opts); err != nil {
			var pfe *provision.PreflightError
			if errors.As(err, &pfe) {
				fmt.Fprint(os.Stderr, "skrog: cannot install yet.\n\n")
				for _, pr := range pfe.Report.Problems {
					fmt.Fprintf(os.Stderr, "  %s\n    fix: %s\n\n", pr.Summary, pr.Remedy)
				}
				return exitError
			}
			fmt.Fprintf(os.Stderr, "skrog: install failed: %v\n", err)
			return exitError
		}
		log.Info("engine installed", "distro", opts.Distro, "engine", opts.EngineVersion)
	}

	if err := applySkrogFile(ctx, f, opts, noAutostart, log); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: applying config: %v\n", err)
		return exitError
	}

	// Wire the docker context so `docker` targets the engine; best-effort.
	pipe, _ := pipeproxy.SelectPipeName("")
	if err := (&dockerctx.Manager{}).Ensure(ctx, pipeproxy.DockerHostFor(pipe)); err != nil {
		log.Warn("docker context not wired", "reason", err)
	}

	fmt.Printf("Install converged from %s. Run `skrog start` to bring up the bridge.\n", configPath)
	return exitOK
}

// applySkrogFile converges the install to match a skrog.yaml: idle timeout,
// lifecycle hooks, engine daemon.json keys, WSL integrations and autostart.
// Each step is idempotent, so re-running against an existing install is safe.
// noAutostart forces autostart off regardless of the file (the --no-autostart
// flag always wins).
func applySkrogFile(ctx context.Context, f skrogfile.File, opts provision.Options, noAutostart bool, log *slog.Logger) error {
	sd := opts.StateDir

	if f.IdleTimeout != "" {
		if err := config.Set(sd, config.KeyIdleTimeout, f.IdleTimeout); err != nil {
			return fmt.Errorf("idle-timeout: %w", err)
		}
		log.Info("applied setting", "idle-timeout", f.IdleTimeout)
	}

	for event, path := range f.Hooks {
		if err := config.Set(sd, "hook."+event, path); err != nil {
			return fmt.Errorf("hook %s: %w", event, err)
		}
		log.Info("applied hook", "event", event, "path", path)
	}

	if len(f.Engine) > 0 {
		m, ok := engineManager(opts)
		if !ok {
			return fmt.Errorf("cannot apply engine settings: no engine installed")
		}
		if _, err := m.SetMany(ctx, f.Engine); err != nil {
			return fmt.Errorf("engine settings: %w", err)
		}
		log.Info("applied engine settings", "keys", len(f.Engine))
	}

	if len(f.Integrations) > 0 {
		p := &provision.Provisioner{Logger: log}
		distro, ok := resolveDistro(p, opts)
		if !ok {
			return fmt.Errorf("cannot wire integrations: no engine installed")
		}
		m := &integrate.Manager{WSL: &wsl.Local{}, StateDir: sd, Logger: log}
		for _, d := range f.Integrations {
			if err := m.Integrate(ctx, d, distro, provision.SharedSocketPath(distro)); err != nil {
				return fmt.Errorf("integrate %s: %w", d, err)
			}
			log.Info("wired integration", "distro", d)
		}
	}

	// Autostart: enabled by default (matching plain install), overridden by the
	// file's explicit value, and forced off by --no-autostart which always wins.
	want := !noAutostart
	if f.Autostart != nil {
		want = *f.Autostart && !noAutostart
	}
	if err := setAutostart(sd, want, log); err != nil {
		return err
	}
	return nil
}

// setAutostart converges the Run entry and records the file's choice (#515),
// so doctor can later tell a file that said "off" from an entry that went
// missing. Recorded even when registration fails, for the reason install
// records it: doctor should say the asked-for autostart is not there.
func setAutostart(stateDir string, want bool, log *slog.Logger) error {
	if want {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := autostart.Enable(exe); err != nil {
			log.Warn("autostart not registered", "reason", err)
			// a bare go-build binary can't self-register; not fatal
		} else {
			log.Info("autostart enabled")
		}
		return recordAutostart(stateDir, true)
	}
	if err := autostart.Disable(); err != nil {
		return err
	}
	log.Info("autostart disabled")
	return recordAutostart(stateDir, false)
}

// exportSkrogFile builds a skrog.yaml from the current install state, so an
// existing setup round-trips into a file that reproduces it.
func exportSkrogFile(opts provision.Options) (skrogfile.File, error) {
	p := &provision.Provisioner{Logger: cliLogger(true)}
	m, err := p.ReadManifest(opts)
	if err != nil {
		return skrogfile.File{}, fmt.Errorf("no engine installed to export (run `skrog install` first): %w", err)
	}

	f := skrogfile.File{
		Distro:        m.Distro,
		DataDir:       m.DataDir,
		EngineVersion: m.EngineVersion,
	}

	if v, err := config.Get(opts.StateDir, config.KeyIdleTimeout); err == nil && v != "" && v != "off" {
		f.IdleTimeout = v
	}

	hooks := map[string]string{}
	for _, k := range config.HookKeys() {
		if v, err := config.Get(opts.StateDir, k); err == nil && v != "" {
			hooks[trimHookPrefix(k)] = v
		}
	}
	if len(hooks) > 0 {
		f.Hooks = hooks
	}

	// Engine daemon.json keys, only the ones actually set.
	if em, ok := engineManager(opts); ok {
		if eng, err := em.List(context.Background()); err == nil {
			set := map[string]string{}
			for full, v := range eng {
				if v != "" {
					set[engineconfig.StripPrefix(full)] = v
				}
			}
			if len(set) > 0 {
				f.Engine = set
			}
		}
	}

	// Integrations.
	im := &integrate.Manager{WSL: &wsl.Local{}, StateDir: opts.StateDir, Logger: cliLogger(true)}
	if wired, err := im.List(); err == nil && len(wired) > 0 {
		f.Integrations = wired
	}

	// Autostart status.
	if on, _, err := autostart.Status(); err == nil {
		f.Autostart = &on
	}

	return f, nil
}

func trimHookPrefix(key string) string {
	const p = "hook."
	if len(key) > len(p) && key[:len(p)] == p {
		return key[len(p):]
	}
	return key
}
