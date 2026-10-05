package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/wslconfig"
)

// runWSLConfig is `skrog wsl-config`: propagate the VM sizing recorded in
// Skrog's own settings into the global ~/.wslconfig, with consent (#148).
//
// The two-step shape is deliberate. `skrog config set wsl.memory 4GB` records
// an intention in Skrog's state; this command is the only thing that touches
// a file every WSL2 distro on the machine shares, and it shows the diff first.
func runWSLConfig(args []string) int {
	if len(args) == 0 {
		return runWSLConfigShow(nil)
	}
	switch args[0] {
	case "show":
		return runWSLConfigShow(args[1:])
	case "apply":
		return runWSLConfigApply(args[1:])
	case "-h", "--help", "help":
		wslConfigUsage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "skrog wsl-config: unknown subcommand %q\n\n", args[0])
		wslConfigUsage(os.Stderr)
		return exitUsage
	}
}

func wslConfigUsage(w *os.File) {
	fmt.Fprintf(w, `usage: skrog wsl-config [show|apply] [--yes] [--json]

Right-sizes the WSL2 VM the engine runs in: memory, processors, swap and
autoMemoryReclaim, plus virtiofs for how Windows drives are mounted. Those
live in %%USERPROFILE%%\.wslconfig, which is GLOBAL — every WSL2 distro on this
machine shares it, Docker Desktop's included — so Skrog records what you asked
for and writes it only when you say so:

  skrog config set wsl.memory 4GB
  skrog config set wsl.processors 2
  skrog config set wsl.virtiofs true   # faster /mnt/c; needs WSL 2.9+ (stable: 3.0)
  skrog wsl-config apply            # shows the diff, asks, then writes

  show     the effective limits and any pending changes (default)
  apply    write the pending changes to ~/.wslconfig

--yes skips the prompt, for a runner where the owner has already decided;
re-running it changes nothing.

Sizing takes effect when the WSL VM next starts. Skrog will not restart it:
the only way is `+"`wsl --shutdown`"+`, which stops every distro on the machine.

Exit codes: 0 ok, %d error, %d usage.
`, exitError, exitUsage)
}

// desiredFromConfig maps Skrog's wsl.* settings onto .wslconfig key names,
// skipping the ones with no recorded intention.
func desiredFromConfig(stateDir string) (map[string]string, error) {
	out := map[string]string{}
	for skrogKey, wslKey := range config.WSLKeys {
		v, err := config.Get(stateDir, skrogKey)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(v) != "" {
			out[wslKey] = v
		}
	}
	return out, nil
}

func runWSLConfigShow(args []string) int {
	fs := flag.NewFlagSet("wsl-config show", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "override Skrog's state directory")
	file := fs.String("file", "", "the .wslconfig to read (default: the one in your user profile)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: skrog wsl-config show [--json]\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	path, f, desired, code := loadWSLConfig(opts.StateDir, *file)
	if code != exitOK {
		return code
	}
	plan := f.Plan(desired)

	if *asJSON {
		out := wslConfigJSON{Path: path, Exists: f.Existed(), Effective: f.All(), Desired: desired}
		for _, c := range plan {
			out.Pending = append(out.Pending, wslConfigChangeJSON{
				Key: c.Key, Old: c.Old, New: c.New, Added: c.Added,
			})
		}
		return emitJSON(out)
	}

	fmt.Printf("%s%s\n", path, map[bool]string{true: "", false: "  (does not exist yet)"}[f.Existed()])
	eff := f.All()
	if len(eff) == 0 {
		fmt.Println("  no sizing set; WSL's defaults apply (memory: 50% of host RAM, processors: all)")
	}
	for _, k := range wslconfig.Managed() {
		if v, ok := eff[k]; ok {
			fmt.Printf("  %-18s %s\n", k, v)
		}
	}
	if len(plan) == 0 {
		if len(desired) > 0 {
			fmt.Println("\nnothing pending: ~/.wslconfig already matches Skrog's settings")
		} else {
			fmt.Println("\nnothing requested: `skrog config set wsl.memory 4GB` records an intention")
		}
		return exitOK
	}
	fmt.Println("\npending changes (`skrog wsl-config apply` writes them):")
	printWSLPlan(plan)
	return exitOK
}

func runWSLConfigApply(args []string) int {
	fs := flag.NewFlagSet("wsl-config apply", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		file     = fs.String("file", "", "the .wslconfig to write (default: the one in your user profile)")
		yes      = fs.Bool("yes", false, "apply without asking (headless runners)")
		asJSON   = fs.Bool("json", false, "emit machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: skrog wsl-config apply [--yes] [--json]\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	path, f, desired, code := loadWSLConfig(opts.StateDir, *file)
	if code != exitOK {
		return code
	}
	plan := f.Plan(desired)

	if len(plan) == 0 {
		if *asJSON {
			return emitJSON(wslConfigJSON{Path: path, Exists: f.Existed(),
				Effective: f.All(), Desired: desired, Applied: false})
		}
		if len(desired) == 0 {
			fmt.Println("nothing requested; `skrog config set wsl.memory 4GB` records an intention")
		} else {
			fmt.Printf("%s already matches Skrog's settings; nothing to do\n", path)
		}
		return exitOK
	}

	if !*yes {
		if *asJSON {
			fmt.Fprintln(os.Stderr, "skrog: --json needs --yes (there is no way to ask a question in JSON)")
			return exitUsage
		}
		fmt.Printf("%s is shared by every WSL2 distro on this machine.\n", path)
		if !f.Existed() {
			fmt.Println("It does not exist yet and would be created.")
		}
		fmt.Println("\nSkrog would change:")
		printWSLPlan(plan)
		if !confirm("\napply these changes?") {
			fmt.Println("nothing was written")
			return exitOK
		}
	}

	if err := f.Apply(plan); err != nil {
		if !*asJSON {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		}
		return exitError
	}

	if *asJSON {
		out := wslConfigJSON{Path: path, Exists: true, Effective: f.All(), Desired: desired, Applied: true}
		for _, c := range plan {
			out.Pending = append(out.Pending, wslConfigChangeJSON{
				Key: c.Key, Old: c.Old, New: c.New, Added: c.Added,
			})
		}
		return emitJSON(out)
	}
	fmt.Printf("wrote %d change(s) to %s\n", len(plan), path)
	// Reported, never performed: forcing it means `wsl --shutdown`, which stops
	// every distro on the machine — someone else's containers included.
	fmt.Println("  the new sizing takes effect when the WSL VM next starts.")
	fmt.Println("  `skrog stop` does not do that (it stops only Skrog's distro); a reboot,")
	fmt.Println("  or `wsl --shutdown` when you are sure nothing else is running, does.")
	return exitOK
}

// loadWSLConfig reads the .wslconfig to act on. override is the --file flag:
// normally empty, and useful for previewing a change into a copy before
// committing to a file every distro on the machine reads.
func loadWSLConfig(stateDir, override string) (string, *wslconfig.File, map[string]string, int) {
	path := override
	if path == "" {
		p, err := wslconfig.Path()
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return "", nil, nil, exitError
		}
		path = p
	}
	f, err := wslconfig.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return "", nil, nil, exitError
	}
	desired, err := desiredFromConfig(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return "", nil, nil, exitError
	}
	return path, f, desired, exitOK
}

func printWSLPlan(plan []wslconfig.Change) {
	for _, c := range plan {
		for _, line := range strings.Split(c.String(), "\n") {
			fmt.Printf("  %s\n", line)
		}
	}
}

// confirm asks a yes/no question on stdin. A non-interactive stdin answers no,
// so a script that forgot --yes changes nothing rather than hanging.
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		fmt.Println()
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
