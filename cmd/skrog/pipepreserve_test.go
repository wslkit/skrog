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

// The wiring, not just the helper.
//
// The first version of this fix had the --pipe append inline in
// spawnSupervisor, and TestCustomPipeToPreserve passed with that append
// deleted -- a correct helper that nothing called. Three other defects in this
// release had exactly that shape, so the argument list itself gets asserted.
func TestSupervisorCommandForwardsACustomPipe(t *testing.T) {
	self := filepath.Join(t.TempDir(), "skrog.exe")

	t.Run("a custom pipe is forwarded", func(t *testing.T) {
		dir := t.TempDir()
		const custom = `\\.\pipe\skrog-e2e-suite`
		if err := supervise.WriteEndpoint(dir, supervise.Endpoint{Pipe: custom}); err != nil {
			t.Fatal(err)
		}
		_, args := supervisorCommand(self, dir)
		if !slices.Contains(args, "--pipe") {
			t.Fatalf("args %v carry no --pipe; the replacement supervisor would re-select and "+
				"DOCKER_HOST would stop working (#429)", args)
		}
		i := slices.Index(args, "--pipe")
		if i+1 >= len(args) || args[i+1] != custom {
			t.Errorf("args %v: --pipe does not carry the recorded pipe %q", args, custom)
		}
	})

	t.Run("the default pipe is not forwarded", func(t *testing.T) {
		dir := t.TempDir()
		if err := supervise.WriteEndpoint(dir, supervise.Endpoint{Pipe: pipeproxy.DefaultPipeName}); err != nil {
			t.Fatal(err)
		}
		if _, args := supervisorCommand(self, dir); slices.Contains(args, "--pipe") {
			t.Errorf("args %v pin the default pipe; normal selection must stay free to fall back", args)
		}
	})

	t.Run("no endpoint means no --pipe", func(t *testing.T) {
		if _, args := supervisorCommand(self, t.TempDir()); slices.Contains(args, "--pipe") {
			t.Errorf("args %v carry --pipe with nothing recorded", args)
		}
	})

	// The state dir must always survive, whatever happens to the pipe.
	t.Run("the state dir is always passed", func(t *testing.T) {
		dir := t.TempDir()
		if _, args := supervisorCommand(self, dir); !slices.Contains(args, "--state-dir") {
			t.Errorf("args %v lost --state-dir", args)
		}
	})
}
