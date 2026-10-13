package tray

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/supervise"
)

func TestStatusToState(t *testing.T) {
	cases := []struct {
		s    Status
		want State
	}{
		{Status{Installed: true, Engine: "running"}, StateRunning},
		{Status{Installed: true, Engine: "idle"}, StateIdle},
		{Status{Installed: true, Engine: "stopped"}, StateStopped},
		{Status{Installed: false}, StateNotInstalled},
		{Status{Installed: true, Engine: "wat"}, StateUnknown},
	}
	for _, c := range cases {
		if got := c.s.State(); got != c.want {
			t.Errorf("state(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestHealthy(t *testing.T) {
	// Idle is healthy: the engine is one docker command away, by design.
	for _, st := range []State{StateRunning, StateIdle} {
		if !Healthy(st) {
			t.Errorf("Healthy(%q) = false, want true", st)
		}
	}
	for _, st := range []State{StateStopped, StateNotInstalled, StateUnknown} {
		if Healthy(st) {
			t.Errorf("Healthy(%q) = true, want false", st)
		}
	}
}

func TestTooltipCoversEveryState(t *testing.T) {
	for _, st := range []State{StateRunning, StateIdle, StateStopped, StateNotInstalled, StateUnknown} {
		if Tooltip(st) == "" {
			t.Errorf("no tooltip for state %q", st)
		}
	}
}

func TestActionsAreCLICalls(t *testing.T) {
	// The tray holds no logic: every lifecycle action is a bare CLI verb.
	want := map[string]string{
		"Start engine":   "start",
		"Stop engine":    "stop",
		"Restart engine": "restart",
	}
	if len(Actions) != len(want) {
		t.Fatalf("got %d actions, want %d", len(Actions), len(want))
	}
	for _, a := range Actions {
		verb, ok := want[a.Label]
		if !ok {
			t.Errorf("unexpected action %q", a.Label)
			continue
		}
		if len(a.Args) != 1 || a.Args[0] != verb {
			t.Errorf("action %q runs %v, want [%s]", a.Label, a.Args, verb)
		}
	}
}

func TestPollPublishedReadsTheSupervisorsReading(t *testing.T) {
	// The point of #192: no process spawn. A fresh published reading is the
	// answer, mapped through the same Status.State() the CLI path uses.
	dir := t.TempDir()
	if err := supervise.WriteStats(dir, supervise.Stats{Engine: "running"}); err != nil {
		t.Fatal(err)
	}
	got, ok := PollPublished(dir)
	if !ok {
		t.Fatal("a fresh reading should be usable")
	}
	if got != StateRunning {
		t.Errorf("state = %q, want %q", got, StateRunning)
	}
}

func TestPollPublishedMapsIdleAndStopped(t *testing.T) {
	for engine, want := range map[string]State{
		"idle":    StateIdle,
		"stopped": StateStopped,
	} {
		dir := t.TempDir()
		if err := supervise.WriteStats(dir, supervise.Stats{Engine: engine}); err != nil {
			t.Fatal(err)
		}
		got, ok := PollPublished(dir)
		if !ok || got != want {
			t.Errorf("engine %q -> %q (ok=%v), want %q", engine, got, ok, want)
		}
	}
}

func TestPollPublishedRefusesWhatItCannotTrust(t *testing.T) {
	t.Run("no state dir", func(t *testing.T) {
		if _, ok := PollPublished(""); ok {
			t.Error("an empty state dir must fall back to the CLI")
		}
	})

	t.Run("no supervisor has ever run", func(t *testing.T) {
		if _, ok := PollPublished(t.TempDir()); ok {
			t.Error("a missing stats file must fall back to the CLI")
		}
	})

	t.Run("reading predates the Engine field", func(t *testing.T) {
		// An older supervisor still running after an upgrade writes stats
		// without the field. Guessing from the other counters would be the
		// tray inventing engine logic; fall back instead.
		dir := t.TempDir()
		if err := supervise.WriteStats(dir, supervise.Stats{}); err != nil {
			t.Fatal(err)
		}
		if _, ok := PollPublished(dir); ok {
			t.Error("a reading with no Engine must fall back to the CLI")
		}
	})

	t.Run("stale reading", func(t *testing.T) {
		// A supervisor that died leaves its last reading behind. Showing it
		// forever would be a status light that lies.
		dir := t.TempDir()
		stale := supervise.Stats{Engine: "running", UpdatedAt: time.Now().Add(-5 * time.Minute)}
		b, err := json.Marshal(stale)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "supervisor-stats.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := PollPublished(dir); ok {
			t.Error("a stale reading must fall back to the CLI")
		}
	})
}

// With several engines the dot follows the one docker is on, so the tooltip
// names a named engine and the default engine's tooltip is exactly what it was
// (#539).
func TestTooltipNamesTheTarget(t *testing.T) {
	if got, want := TooltipFor(StateRunning, ""), Tooltip(StateRunning); got != want {
		t.Errorf("the default engine's tooltip changed: %q", got)
	}
	if got := TooltipFor(StateRunning, "proj-a"); !strings.Contains(got, "proj-a") || !strings.Contains(got, "running") {
		t.Errorf("tooltip = %q, want the state and the engine's name", got)
	}
}

// A lifecycle click must act on the engine the dot is showing, not the default.
func TestActionArgsAimAtTheShownEngine(t *testing.T) {
	a := Action{Label: "Stop engine", Args: []string{"stop"}}
	if got := ActionArgs(a, ""); !reflect.DeepEqual(got, []string{"stop"}) {
		t.Errorf("no engine named: args = %v", got)
	}
	got := ActionArgs(a, `C:\s\engines\proj-a`)
	if want := []string{"stop", "--state-dir", `C:\s\engines\proj-a`}; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	if len(a.Args) != 1 {
		t.Errorf("ActionArgs mutated the shared action table: %v", a.Args)
	}
}
