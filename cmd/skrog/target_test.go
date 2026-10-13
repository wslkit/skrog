package main

import (
	"path/filepath"
	"testing"

	"github.com/wslkit/skrog/internal/provision"
	tgt "github.com/wslkit/skrog/internal/target"
)

// The supervisor for a named engine must serve that engine's own pipe and wire
// that engine's own context, and the default engine's supervisor must be left
// exactly as it was (#539). Both are derived from the state dir.
func TestEngineWiring(t *testing.T) {
	base := filepath.Join(t.TempDir(), "Skrog")

	name, l, ok := engineWiring(tgt.For(base, "proj-a").StateDir)
	if !ok || name != "proj-a" {
		t.Fatalf("a named engine's state dir was not recognised: %q %v", name, ok)
	}
	if l.Pipe != `\\.\pipe\skrog-proj-a` || l.Context != "skrog-proj-a" {
		t.Errorf("wiring = %+v", l)
	}
	if l.Context == "skrog" {
		t.Error("a named engine must never wire the default engine's `skrog` context")
	}

	if _, _, ok := engineWiring(base); ok {
		t.Error("the default engine's state dir was treated as a named engine")
	}
}

// A new engine downloads nothing the machine already has: the rootfs cache is
// the base state dir's, while everything else is the engine's own.
func TestEngineOptionsShareTheRootfsCache(t *testing.T) {
	base := filepath.Join(t.TempDir(), "Skrog")
	l := tgt.For(base, "proj-a")
	o := engineOptions(base, l)
	if o.CacheDir != base {
		t.Errorf("CacheDir = %q, want the shared base %q", o.CacheDir, base)
	}
	if o.StateDir != l.StateDir || o.Distro != "skrog-proj-a" {
		t.Errorf("options = %+v", o)
	}
	if o.StateDir == base {
		t.Error("an engine must not share the default engine's state dir")
	}
}

// With --state-dir given, a command acts there and nothing is looked up; and
// with no named engines there is nothing to disambiguate, so no docker call is
// made and the default engine is used, as before.
func TestScopedStateDirIsInertWithoutEngines(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if got := scopedStateDir(`C:\explicit`); got != `C:\explicit` {
		t.Errorf("an explicit --state-dir was overridden: %q", got)
	}
	if got := scopedStateDir(""); got != "" {
		t.Errorf("with no engines the default must stay the default, got %q", got)
	}
}

// targetBase resolves an engine's own state dir back to the install it hangs
// off, so remotes and the engine registry are found from either.
func TestTargetBaseResolvesAnEnginesDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), "Skrog")
	if got := targetBase(tgt.For(base, "proj-a").StateDir); got != base {
		t.Errorf("targetBase(engine dir) = %q, want %q", got, base)
	}
	if got := targetBase(base); got != base {
		t.Errorf("targetBase(base) = %q", got)
	}
}

// Creating, listing and using work from an engine's own dir as well as from the
// base: the base is the registry.
func TestNamedEngineDirsAreTheRegistry(t *testing.T) {
	base := t.TempDir()
	opts := provision.Options{StateDir: tgt.For(base, "a").StateDir}
	if got := tgt.BaseOf(opts.StateDir); got != base {
		t.Errorf("BaseOf = %q, want %q", got, base)
	}
}

func TestTargetShapes(t *testing.T) {
	m := roundTrip(t, targetListJSON{Current: "local", Targets: []targetEntryJSON{}})
	requireKeys(t, m, "current", "targets", "totalDiskBytes")
	if a, ok := m["targets"].([]any); !ok || a == nil {
		t.Errorf("targets must be an array even when empty, got %v", m["targets"])
	}

	e := roundTrip(t, targetEntryJSON{Name: "a", Kind: "engine", Context: "skrog-a"})
	requireKeys(t, e, "name", "kind", "current", "context")
	for _, k := range []string{"host", "autostart", "pipe"} {
		if _, ok := e[k]; ok {
			t.Errorf("%q should be omitted when unset", k)
		}
	}

	c := roundTrip(t, targetCreateJSON{Layout: tgt.For(`C:\s`, "a")})
	requireKeys(t, c, "name", "stateDir", "distro", "pipe", "context", "engineVersion", "dockerContext")
}
