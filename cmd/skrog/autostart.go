package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/wslkit/skrog/internal/autostart"
	"github.com/wslkit/skrog/internal/provision"
)

func runAutostart(args []string) int {
	fs := flag.NewFlagSet("autostart", flag.ContinueOnError)
	engine := fs.String("engine", "", "act on this named engine's logon entry instead of the default engine's")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: skrog autostart [--engine <name>] enable|disable|status

Controls whether the supervisor starts at logon, via a per-user Run entry
(visible and switchable in Task Manager's Startup tab; no admin needed). The
entry runs skrogw.exe, the windowless launcher, so nothing flashes at logon.

install registers this by default; uninstall removes it.

enable and disable also record the choice, exactly as `+"`skrog config set\nautostart on|off`"+` does, so `+"`skrog doctor`"+` can tell an entry turned off on
purpose from one that went missing, and put a missing one back with --fix.

--engine <name> acts on a named engine's own logon entry (skrog target create
--autostart does the same at creation) and leaves the default engine's alone.
A named engine starts at logon only when you ask: by default it starts when you
`+"`skrog use`"+` it, and stops itself when idle.
`)
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *engine != "" {
		return autostartEngine(*engine, fs.Arg(0))
	}

	// Both verbs record the choice as well as changing the Run entry (#515),
	// so doctor can tell an entry turned off on purpose from one that went
	// missing. The same path `skrog config set autostart` takes.
	stateDir := optsWithResolvedStateDir(provision.Options{}).StateDir

	switch fs.Arg(0) {
	case "enable":
		if err := applyAutostart(stateDir, true); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		fmt.Println("autostart enabled: the supervisor starts at your next logon")
		return exitOK

	case "disable":
		if err := applyAutostart(stateDir, false); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		fmt.Println("autostart disabled (a running supervisor is not stopped; use `skrog stop`)")
		return exitOK

	case "status", "":
		enabled, cmd, err := autostart.Status()
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
		fs.Usage()
		return exitUsage
	}
}
