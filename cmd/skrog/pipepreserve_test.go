package main

import (
	"os"
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

// The exact sequence the acceptance suite runs, which is the one BOTH earlier
// attempts failed (#429).
//
// Attempt one read the endpoint record inside spawnSupervisor, after the old
// supervisor had cleared it on the way out. Attempt two captured it earlier,
// which fixed that and still failed — because stageSupervisorRestart DELETES
// endpoint.json before restarting, deliberately, so that finding a record
// afterwards proves the replacement wrote its own (#273) rather than
// inheriting one.
//
// There is therefore no moment at which endpoint.json can answer this. A
// record that is correctly deleted cannot also be a handoff channel, and that
// is why the fix is a separate served-pipe record with a different lifetime.
//
// Both unit tests before this one passed while the product failed, because
// each modelled a scenario in which endpoint.json still existed.
func TestRestartSurvivesTheSuiteDeletingTheEndpointRecord(t *testing.T) {
	self := filepath.Join(t.TempDir(), "skrog.exe")
	dir := t.TempDir()
	const custom = `\\.\pipe\skrog-e2e-suite`

	// 1. A supervisor starts, asked for a custom pipe. It records both.
	if err := supervise.WriteServedPipe(dir, custom); err != nil {
		t.Fatal(err)
	}
	if err := supervise.WriteEndpoint(dir, supervise.Endpoint{Pipe: custom}); err != nil {
		t.Fatal(err)
	}

	// 2. The suite deletes the endpoint record before restarting.
	if err := os.Remove(filepath.Join(dir, "endpoint.json")); err != nil {
		t.Fatal(err)
	}

	// 3. `skrog restart --supervisor` captures what to keep.
	captured := customPipeToPreserve(dir)

	// 4. The old supervisor exits cleanly, clearing the endpoint record it no
	//    longer has. The served-pipe record is NOT cleared -- that is its job.
	if err := supervise.ClearEndpoint(dir); err != nil {
		t.Fatal(err)
	}

	// 5. The replacement is built.
	_, args := supervisorCommand(self, dir, captured)
	if got := pipeArg(args); got != custom {
		t.Errorf("args %v carry pipe %q, want %q.\n"+
			"  With no endpoint record anywhere in this sequence, the replacement "+
			"re-selects from scratch and DOCKER_HOST stops working (#429).",
			args, got, custom)
	}
}

// The clean-exit path, with the endpoint record present up to the exit. This
// is what attempt two fixed and must keep working.
func TestRestartSequencePreservesTheCustomPipe(t *testing.T) {
	self := filepath.Join(t.TempDir(), "skrog.exe")
	dir := t.TempDir()
	const custom = `\\.\pipe\skrog-e2e-suite`

	if err := supervise.WriteEndpoint(dir, supervise.Endpoint{Pipe: custom}); err != nil {
		t.Fatal(err)
	}
	captured := customPipeToPreserve(dir)

	if err := supervise.ClearEndpoint(dir); err != nil {
		t.Fatal(err)
	}
	if got := customPipeToPreserve(dir); got != "" {
		t.Fatalf("the record survived a clean exit (%q); this test no longer models the bug", got)
	}

	_, args := supervisorCommand(self, dir, captured)
	if got := pipeArg(args); got != custom {
		t.Errorf("args %v carry pipe %q, want %q — the replacement will re-select and "+
			"DOCKER_HOST stops working (#429)", args, got, custom)
	}
}

// Starting a supervisor with no --pipe must ERASE a previous run's
// preference, not inherit it. Otherwise one `skrog supervise --pipe custom`
// pins that pipe for every future supervisor on the machine, and the only way
// back is deleting a file nobody documented.
func TestAServedPipeIsForgottenWhenNoneIsRequested(t *testing.T) {
	dir := t.TempDir()
	const custom = `\\.\pipe\skrog-e2e-suite`

	if err := supervise.WriteServedPipe(dir, custom); err != nil {
		t.Fatal(err)
	}
	if got := supervise.ReadServedPipe(dir); got != custom {
		t.Fatalf("ReadServedPipe = %q, want %q", got, custom)
	}

	// The next supervisor is started without --pipe.
	if err := supervise.WriteServedPipe(dir, ""); err != nil {
		t.Fatal(err)
	}
	if got := supervise.ReadServedPipe(dir); got != "" {
		t.Errorf("ReadServedPipe = %q after a supervisor asked for no pipe; "+
			"the old preference is sticky and the default can never come back", got)
	}
	if got := customPipeToPreserve(dir); got != "" {
		t.Errorf("customPipeToPreserve = %q, want none", got)
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
