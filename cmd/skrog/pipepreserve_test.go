package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/supervise"
)

// `restart --supervisor` rebuilds the supervisor's arguments rather than
// forwarding them, and the served pipe was not among what it carried (#429).
// A supervisor started with --pipe <custom> came back on the default, so
// DOCKER_HOST stopped working with an error naming a missing file rather than
// a moved pipe. The watchdog relaunch went through the same path.
//
// The two exclusions are the interesting part, and they are what keep this
// from breaking the ordinary install.
func TestCustomPipeToPreserve(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string
		want     string
		why      string
	}{
		{
			name:     "a pipe someone asked for by name is kept",
			recorded: `\\.\pipe\skrog-e2e-suite`,
			want:     `\\.\pipe\skrog-e2e-suite`,
			why:      "this is the case that was being lost",
		},
		{
			// DefaultPipeName and FallbackPipeName are already full
			// `\\.\pipe\...` paths. Concatenating another prefix is how the
			// first draft of this test silently passed nothing to compare.
			name:     "the default is NOT pinned",
			recorded: pipeproxy.DefaultPipeName,
			want:     "",
			why: "normal selection takes it again when free and falls back when " +
				"Docker Desktop has it; pinning would turn that fallback into a failure to bind",
		},
		{
			name:     "the fallback is NOT pinned",
			recorded: pipeproxy.FallbackPipeName,
			want:     "",
			why: "selection re-derives it, and pinning would make the fallback sticky — " +
				"a machine that stopped running Desktop would never take the default back",
		},
		{
			name:     "case does not matter",
			recorded: `\\.\pipe\DOCKER_ENGINE`,
			want:     "",
			why:      "Windows pipe names are case-insensitive",
		},
		{
			// A bare name, in case anything ever records one: the comparison
			// must not depend on the prefix being present.
			name:     "a bare default name is still the default",
			recorded: "docker_engine",
			want:     "",
			why:      "pipeEq trims the prefix on both sides",
		},
		{
			name:     "no record means choose normally",
			recorded: "",
			want:     "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.recorded != "" {
				if err := supervise.WriteEndpoint(dir, supervise.Endpoint{Pipe: tc.recorded}); err != nil {
					t.Fatal(err)
				}
			}
			if got := customPipeToPreserve(dir); got != tc.want {
				t.Errorf("customPipeToPreserve = %q, want %q\n  %s", got, tc.want, tc.why)
			}
		})
	}
}

// pipeArg returns the value after --pipe, or "" when there is none.
func pipeArg(args []string) string {
	i := slices.Index(args, "--pipe")
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// The RESTART sequence, which is the one that was broken (#429).
//
// The first attempt at this fix read the recorded endpoint inside
// spawnSupervisor. That passed a test which wrote an endpoint and called
// supervisorCommand directly — and still failed in the product, because
// runSupervise DELETES its endpoint record on a clean exit, and
// `restart --supervisor` exits it cleanly before spawning the replacement.
// The test asserted the right thing about the wrong scenario.
//
// So this models the ordering: record present, supervisor exits and clears it,
// THEN the replacement is built.
func TestRestartSequencePreservesTheCustomPipe(t *testing.T) {
	self := filepath.Join(t.TempDir(), "skrog.exe")
	dir := t.TempDir()
	const custom = `\\.\pipe\skrog-e2e-suite`

	// 1. A supervisor is serving a custom pipe.
	if err := supervise.WriteEndpoint(dir, supervise.Endpoint{Pipe: custom}); err != nil {
		t.Fatal(err)
	}
	// 2. restart --supervisor captures it before tearing anything down.
	captured := customPipeToPreserve(dir)

	// 3. The old supervisor exits cleanly, which clears the record.
	if err := supervise.ClearEndpoint(dir); err != nil {
		t.Fatal(err)
	}
	if got := customPipeToPreserve(dir); got != "" {
		t.Fatalf("the record survived a clean exit (%q); this test no longer models the bug", got)
	}

	// 4. The replacement is built. Reading the record here finds nothing,
	//    which is exactly why the captured value has to be carried.
	_, args := supervisorCommand(self, dir, captured)
	if got := pipeArg(args); got != custom {
		t.Errorf("args %v carry pipe %q, want %q — the replacement will re-select and "+
			"DOCKER_HOST stops working (#429)", args, got, custom)
	}
}

// The crash path: a supervisor killed hard runs no cleanup, so its record
// survives and is the only thing that remembers the pipe. No captured value is
// available there, so the recorded fallback has to work.
func TestCrashRestartUsesTheRecordedPipe(t *testing.T) {
	self := filepath.Join(t.TempDir(), "skrog.exe")
	dir := t.TempDir()
	const custom = `\\.\pipe\skrog-e2e-suite`
	if err := supervise.WriteEndpoint(dir, supervise.Endpoint{Pipe: custom}); err != nil {
		t.Fatal(err)
	}
	_, args := supervisorCommand(self, dir, "") // nothing captured
	if got := pipeArg(args); got != custom {
		t.Errorf("args %v carry pipe %q, want the recorded %q", args, got, custom)
	}
}

func TestSupervisorCommandLeavesTheOrdinaryCaseAlone(t *testing.T) {
	self := filepath.Join(t.TempDir(), "skrog.exe")

	t.Run("the default pipe is not forwarded", func(t *testing.T) {
		dir := t.TempDir()
		if err := supervise.WriteEndpoint(dir, supervise.Endpoint{Pipe: pipeproxy.DefaultPipeName}); err != nil {
			t.Fatal(err)
		}
		if _, args := supervisorCommand(self, dir, ""); slices.Contains(args, "--pipe") {
			t.Errorf("args %v pin the default pipe; normal selection must stay free to fall back", args)
		}
	})

	t.Run("no endpoint means no --pipe", func(t *testing.T) {
		if _, args := supervisorCommand(self, t.TempDir(), ""); slices.Contains(args, "--pipe") {
			t.Errorf("args %v carry --pipe with nothing recorded", args)
		}
	})

	// The state dir must always survive, whatever happens to the pipe.
	t.Run("the state dir is always passed", func(t *testing.T) {
		dir := t.TempDir()
		if _, args := supervisorCommand(self, dir, ""); !slices.Contains(args, "--state-dir") {
			t.Errorf("args %v lost --state-dir", args)
		}
	})
}
