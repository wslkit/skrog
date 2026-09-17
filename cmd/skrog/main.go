// Command skrog runs the upstream Docker Engine on Windows via WSL2:
// a provisioner, a named-pipe bridge, and a supervisor in one binary.
package main

import (
	"fmt"
	"os"
)

// buildVersion is stamped by the release build (-ldflags "-X main.buildVersion=...").
var buildVersion = "dev"

// exit codes are part of the CLI contract: CI scripts branch on them, so they
// are assigned deliberately rather than by accident (PLAN §03).
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitNotFound = 3 // asked about something that is not installed
)

type command struct {
	name    string
	summary string
	run     func(args []string) int
}

func commands() []command {
	return []command{
		{"audit", "print the container-affecting API audit log (`audit tail`)", runAudit},
		{"autostart", "start the supervisor at logon: enable, disable, status", runAutostart},
		{"bundle", "pack the engine into a .zip for an air-gapped `install --offline`", runBundle},
		{"cache", "pull-through registry cache on the engine: enable, disable, status", runCache},
		{"cli", "install the bundled docker CLI + compose + buildx (ditch Docker Desktop)", runCLI},
		{"compact", "shrink the engine's virtual disk: fstrim + CompactVirtualDisk", runCompact},
		{"config", "list, get, or set Skrog settings (idle-timeout)", runConfig},
		{"doctor", "diagnose the host and engine; --fix applies safe remedies", runDoctor},
		{"enable-gpu", "install the NVIDIA CDI spec so containers can use the GPU", runEnableGPU},
		{"engine", "engine list, upgrade and rollback — pinned and reversible", runEngine},
		{"healthcheck", "readiness probe: exit 0 when a docker command would succeed (--wait)", runHealthcheck},
		{"install", "provision the engine distro and start it", runInstall},
		{"lock", "write a skrog.lock pinning the exact engine (reproducible installs)", runLock},
		{"logs", "supervisor, dockerd, or audit log; --follow, --json for shippers", runLogs},
		{"migrate", "copy images and volumes from Docker Desktop into the engine", runMigrate},
		{"prewarm", "pull a pinned image list ahead of need (runner warm-up, golden images)", runPrewarm},
		{"policy", "local admission control for the docker API: show, check, test", runPolicy},
		{"profile", "save and switch named settings profiles (work/home)", runProfile},
		{"proxy", "serve the docker pipe in the foreground (debug mode)", runProxy},
		{"prune", "reclaim disk: stopped containers, unused images, build cache", runPrune},
		{"relocate", "move the engine data dir to another drive", runRelocate},
		{"remote", "register and switch to a remote engine served over mutual TLS", runRemote},
		{"reset", "reset the engine to a snapshot, unconditionally (runner clean slate)", runReset},
		{"restart", "stop the engine, then start it", runRestart},
		{"runner", "runner check: verify auto-logon, autostart, supervisor and engine on an unattended host", runRunner},
		{"serve", "expose the engine over the network with mutual TLS (`serve cert`)", runServe},
		{"snapshot", "save/restore/list the engine state (images, containers, volumes)", runSnapshot},
		{"start", "ensure the supervisor and engine are running", runStart},
		{"status", "report supervisor, engine and desired state", runStatus},
		{"stop", "stop the engine; it stays stopped until start", runStop},
		{"supervise", "serve the pipe and keep the engine alive (the always-on layer)", runSupervise},
		{"uninstall", "remove the engine distro and Skrog's state", runUninstall},
		{"upgrade", "am I current? app, engine and bundled CLI in one answer", runUpgrade},
		{"wsl-integrate", "point docker inside your own WSL distros at the engine", runWSLIntegrate},
		{"wsl-config", "right-size the WSL2 VM: show and apply ~/.wslconfig sizing, with consent", runWSLConfig},
		{"version", "report every component version and which docker.exe is active", runVersion},
	}
}

func usage(w *os.File) {
	fmt.Fprintf(w, `skrog %s - upstream Docker Engine on Windows via WSL2

usage: skrog <command> [flags]

commands:
`, buildVersion)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, `
Commands still in development are tracked at
https://github.com/wslkit/skrog/issues

run `+"`skrog <command> --help`"+` for a command's flags
`)
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(exitUsage)
	}

	name := os.Args[1]
	switch name {
	case "-h", "--help", "help":
		// `skrog help --json` is the command index as data, for the
		// reference generator (#209). Kept here rather than as a visible
		// command: a `help-dump` entry in the command list would be clutter
		// for every user, to serve one script.
		if len(os.Args) > 2 && os.Args[2] == "--json" {
			os.Exit(emitJSON(helpIndex()))
		}
		usage(os.Stdout)
		os.Exit(exitOK)
	case "-v", "--version":
		// Convenience alias; `skrog version` is the real command.
		name = "version"
	}

	for _, c := range commands() {
		if c.name == name {
			os.Exit(c.run(os.Args[2:]))
		}
	}

	fmt.Fprintf(os.Stderr, "skrog: unknown command %q\n\n", name)
	usage(os.Stderr)
	os.Exit(exitUsage)
}

// helpEntry is one command in the machine-readable index.
type helpEntry struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Subs are the command's subcommands, absent when it has none. They feed
	// the shell completions generated from this index (#392).
	Subs []string `json:"subcommands,omitempty"`
}

// subcommands maps a command to the words that may follow it.
//
// Declared as data rather than parsed out of each command's usage text: that
// text is prose written for a human, and reformatting a sentence would
// silently break completion. It lives here rather than in the command table so
// the table stays one line per command, and a missing entry is one line to add.
//
// The lists come from each command's dispatch switch, not from its usage
// string, because the two disagree: `profile delete`, `remote remove` and the
// explicit `list` forms are all accepted by the binary and documented in
// neither. Completion should offer what the CLI takes, not what it advertises.
//
// A command whose bare form does something (`skrog config` lists, `skrog
// profile` lists) still appears here: completion offers the subcommands, and
// the user is free to press enter instead.
var subcommands = map[string][]string{
	"audit":      {"tail", "trace"},
	"autostart":  {"enable", "disable", "status"},
	"cache":      {"enable", "disable", "status"},
	"cli":        {"install", "status", "uninstall"},
	"config":     {"get", "set", "export"},
	"engine":     {"list", "upgrade", "rollback"},
	"policy":     {"show", "check", "test"},
	"profile":    {"list", "create", "switch", "show", "delete"},
	"remote":     {"list", "add", "use", "test", "remove"},
	"runner":     {"check"},
	"serve":      {"cert"},
	"snapshot":   {"list", "save", "restore", "delete"},
	"wsl-config": {"show", "apply"},
}

// helpIndex is the command list as data, so the reference generator does not
// have to scrape the human-readable usage text and re-break every time its
// column widths change.
func helpIndex() []helpEntry {
	cmds := commands()
	out := make([]helpEntry, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, helpEntry{Name: c.name, Summary: c.summary, Subs: subcommands[c.name]})
	}
	return out
}
