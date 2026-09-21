package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/wslkit/skrog/internal/integrate"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/wsl"
)

func runWSLIntegrate(args []string) int {
	fs := flag.NewFlagSet("wsl-integrate", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		remove   = fs.Bool("remove", false, "unwire the distro instead")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog wsl-integrate <distro>       wire a distro to the engine
       skrog wsl-integrate --remove <distro>
       skrog wsl-integrate                without arguments: list wired distros

Points docker inside one of your own WSL distros at the Skrog engine, by
writing %s there (DOCKER_HOST to the engine socket shared at /mnt/wsl). Takes
effect in new login shells. Never touches a distro you did not name, and
`+"`skrog uninstall`"+` unwires everything it wired.

Note: with an idle-timeout configured, in-flight work over the shared socket
(a build or pull from the integrated distro) holds the engine up — it is never
stopped mid-operation. But between operations the engine can still idle-stop,
and a shared-socket client cannot wake a stopped engine on its own; run
`+"`skrog start`"+` first (or keep idle-timeout off) for long in-distro sessions.

Exit codes: 0 ok, %d error, %d usage.
`, integrate.ProfilePath, exitError, exitUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	log := cliLogger(false)
	m := &integrate.Manager{WSL: &wsl.Local{}, StateDir: opts.StateDir, Logger: log}

	rest := fs.Args()
	if len(rest) == 0 && !*remove {
		wired, err := m.List()
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		if len(wired) == 0 {
			fmt.Println("no distros wired; `skrog wsl-integrate <distro>` wires one")
			return exitOK
		}
		for _, d := range wired {
			fmt.Println(d)
		}
		return exitOK
	}
	if len(rest) != 1 {
		fs.Usage()
		return exitUsage
	}
	target := rest[0]
	ctx := context.Background()

	if *remove {
		if err := m.Remove(ctx, target); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		fmt.Printf("%s unwired; open a new shell there for it to take effect\n", target)
		return exitOK
	}

	p := &provision.Provisioner{Logger: log}
	engineDistro, ok := requireDistroInstall(p, opts)
	if !ok {
		return exitNotFound
	}
	if err := m.Integrate(ctx, target, engineDistro,
		provision.SharedSocketPath(engineDistro)); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	fmt.Printf(`%s wired to the Skrog engine.

Open a new shell in it and docker just works:

  wsl -d %s
  docker ps

Undo anytime with: skrog wsl-integrate --remove %s
`, target, target, target)
	return exitOK
}
