// Command skrog runs the upstream Docker Engine on Windows via WSL2:
// a provisioner, a named-pipe bridge, and a supervisor in one binary.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
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
	// exitUnsupported: this machine cannot run what was asked for, and no
	// retry or repair will change that (#388: an arm64 host with an
	// amd64-only engine rootfs). Distinct from exitError so a CI matrix can
	// skip a platform rather than treat it as a broken build -- which is
	// exactly the difference a runner needs and cannot infer from "1".
	exitUnsupported = 4
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

// helpGap is the blank space between a name and its description in every help
// listing. Three, not one: at one space a long name and its summary read as a
// single sentence, and the eye has nothing to run down.
const helpGap = 3

// helpColumn is the width to pad names to so their descriptions line up.
//
// Derived from the longest name rather than hardcoded, which is the bug this
// replaces: the list was padded to a fixed 10 and `healthcheck` is 11, so that
// one row lost its gap entirely and ran its name into its summary. A fixed
// width is wrong the moment someone adds a longer command, and nothing would
// have said so.
func helpColumn(names []string) int {
	widest := 0
	for _, n := range names {
		if len(n) > widest {
			widest = len(n)
		}
	}
	return widest + helpGap
}

// helpList renders aligned "name  description" rows for a help screen.
func helpList(w io.Writer, rows [][2]string) {
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r[0])
	}
	col := helpColumn(names)
	for _, r := range rows {
		fmt.Fprintf(w, "  %-*s%s\n", col, r[0], r[1])
	}
}

func usage(w *os.File) {
	fmt.Fprintf(w, `skrog %s - upstream Docker Engine on Windows via WSL2

usage: skrog <command> [flags]

commands:
`, buildVersion)
	rows := make([][2]string, 0, len(commands()))
	for _, c := range commands() {
		rows = append(rows, [2]string{c.name, c.summary})
	}
	helpList(w, rows)
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

// helpRows renders "name  description" where a description may be several
// lines, indenting the continuation lines to the same column.
//
// The alternative, and what this replaces, is baking the column into a format
// string per row: `  %s   ...` for one key and `  %s  ...` for a longer one,
// with every wrapped line padded by hand. That drifts the moment a key is
// added or renamed, and it had: the settings list had columns at 22 and 26
// depending on the row, and two keys were missing from it entirely because
// adding one meant editing a format string, a variadic argument list and a
// column of spaces in three places.
func helpRows(w io.Writer, rows [][2]string) {
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r[0])
	}
	col := helpColumn(names)
	indent := strings.Repeat(" ", col+2)
	for _, r := range rows {
		for i, line := range strings.Split(r[1], "\n") {
			if i == 0 {
				fmt.Fprintf(w, "  %-*s%s\n", col, r[0], line)
				continue
			}
			fmt.Fprintf(w, "%s%s\n", indent, line)
		}
	}
}
