// Package tray holds the logic behind the Skrog system-tray status light,
// separate from the GUI shell so it can be tested without a desktop.
//
// The tray is a status light, not a control panel (PLAN §03): a fixed, tiny
// menu, and every item shells out to the `skrog` CLI — the tray itself holds
// no engine logic and makes no decisions the CLI would not.
package tray

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/supervise"
)

// State is the engine state the dot reflects, mirroring `skrog status`.
type State string

const (
	StateRunning      State = "running"
	StateIdle         State = "idle"
	StateStopped      State = "stopped"
	StateNotInstalled State = "not-installed"
	StateUnknown      State = "unknown"
)

// Status is the subset of `skrog status --json` the tray renders.
type Status struct {
	Installed  bool   `json:"installed"`
	Engine     string `json:"engine"`
	Supervisor string `json:"supervisor"`
	Desired    string `json:"desired"`
	// StateDir is where the supervisor publishes its stats. Learning it once
	// is what lets the tray stop shelling out for every refresh (#192).
	StateDir string `json:"stateDir"`
	// Target is the named engine the dot describes (#539); empty for the default
	// shared engine. The dot follows the engine docker is on, so the tooltip
	// has to say which one that is.
	Target string `json:"target"`
}

// State collapses a Status into the dot's State.
func (s Status) State() State {
	if !s.Installed {
		return StateNotInstalled
	}
	switch s.Engine {
	case "running":
		return StateRunning
	case "idle":
		return StateIdle
	case "stopped":
		return StateStopped
	default:
		return StateUnknown
	}
}

// Tooltip is the one-line hover text for a state.
func Tooltip(st State) string {
	switch st {
	case StateRunning:
		return "Skrog — engine running"
	case StateIdle:
		return "Skrog — engine idle (starts on demand)"
	case StateStopped:
		return "Skrog — engine stopped"
	case StateNotInstalled:
		return "Skrog — not installed"
	default:
		return "Skrog — engine state unknown"
	}
}

// TooltipFor is Tooltip naming the engine when it is a named one, so with
// several engines the dot never leaves you guessing which it describes.
func TooltipFor(st State, target string) string {
	t := Tooltip(st)
	if target == "" {
		return t
	}
	return t + " (" + target + ")"
}

// ActionArgs are the CLI arguments for an action aimed at the engine whose
// state dir is stateDir. The lifecycle verbs act on the default engine when not
// told otherwise, and the dot may be describing a named one: a menu that shows
// engine B's state and acts on engine A would be the worst kind of wrong.
func ActionArgs(a Action, stateDir string) []string {
	args := append([]string(nil), a.Args...)
	if stateDir != "" {
		args = append(args, "--state-dir", stateDir)
	}
	return args
}

// Healthy reports whether the state should read as green: running or idle are
// both "working as intended" (idle wakes on the next docker command).
func Healthy(st State) bool { return st == StateRunning || st == StateIdle }

// CLI runs the skrog binary for the tray. Exe is the resolved skrog.exe path.
type CLI struct {
	Exe string
}

// Poll asks the CLI for the current status. Any failure — CLI missing, not
// installed, timeout — resolves to a definite State rather than an error, so
// the dot always shows something honest.
func (c CLI) Poll(ctx context.Context) Status {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := hideWindow(exec.CommandContext(ctx, c.Exe, "status", "--json")).Output()
	if err != nil {
		// `skrog status` exits non-zero when the engine is not running, but
		// still prints valid JSON on stdout; parse it before giving up.
		if len(out) == 0 {
			return Status{}
		}
	}
	var st Status
	if json.Unmarshal(out, &st) != nil {
		return Status{}
	}
	return st
}

// State is Poll collapsed to the dot state.
func (c CLI) State(ctx context.Context) State { return c.Poll(ctx).State() }

// Action is one menu command: a label and the CLI arguments it runs.
type Action struct {
	Label string
	Args  []string
}

// Actions are the lifecycle items, each a thin CLI call. Kept as data so the
// GUI shell only wires labels to Run and the mapping stays testable.
var Actions = []Action{
	{Label: "Start engine", Args: []string{"start"}},
	{Label: "Stop engine", Args: []string{"stop"}},
	{Label: "Restart engine", Args: []string{"restart"}},
}

// Run executes an action through the CLI and returns its combined output. The
// tray shows failures as a notification rather than swallowing them.
func (c CLI) Run(ctx context.Context, a Action) (string, error) {
	return c.RunOn(ctx, a, "")
}

// RunOn is Run against a specific engine's state dir; empty is the default.
func (c CLI) RunOn(ctx context.Context, a Action, stateDir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := hideWindow(exec.CommandContext(ctx, c.Exe, ActionArgs(a, stateDir)...)).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// PollPublished reads the state the supervisor already publishes, instead of
// spawning `skrog status --json` to ask for it.
//
// This is the whole point of #192: the reconciler probes the engine every few
// seconds anyway and writes the answer down, so the tray was paying 285 ms and
// a process spawn -- roughly 7% of a core, continuously -- to recompute
// something already on disk. Reading it costs a file read.
//
// Returns ok=false when there is no supervisor, when its reading is stale, or
// when the file predates the Engine field. The caller falls back to the CLI,
// which is correct but expensive, so it should do so sparingly.
func PollPublished(stateDir string) (State, bool) {
	if stateDir == "" {
		return StateUnknown, false
	}
	st, found, err := supervise.ReadStats(stateDir)
	if err != nil || !found || !st.Fresh() || st.Engine == "" {
		return StateUnknown, false
	}
	// A supervisor is writing, so Skrog is installed; reuse the existing
	// mapping rather than growing a second copy of it in the tray.
	return Status{Installed: true, Engine: st.Engine}.State(), true
}
