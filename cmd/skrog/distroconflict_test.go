package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/provision"
)

// runStderr runs a command's entry point and returns its exit code and stderr.
func runStderr(t *testing.T, run func([]string) int, args []string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	code := run(args)
	w.Close()
	os.Stderr = old
	return code, <-done
}

// `compact --distro X` on a machine whose engine is Y compacted Y (#561): the
// manifest wins wherever a distro is resolved. Every command that takes
// --distro must refuse the conflict before doing anything at all.
//
// Neither distro exists, so a command that ignored the conflict would fail
// against nothing -- but each one also runs only as far as its own refusal
// once fixed, which is what this asserts.
func TestDistroConflictIsRefusedBeforeAnything(t *testing.T) {
	cases := []struct {
		name string
		run  func([]string) int
		args []string
	}{
		{"compact", runCompact, nil},
		{"enable-gpu", runEnableGPU, nil},
		{"relocate", runRelocate, []string{`Z:\skrog-561-nowhere`, "--dry-run"}},
		{"serve", runServe, []string{"--tcp", "127.0.0.1:0"}},
		{"proxy", runProxy, nil},
		{"supervise", runSupervise, nil},
		{"uninstall", runUninstall, []string{"--yes"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := writeManifest(t, provision.Manifest{Distro: "skrog-561-recorded"})
			before, _ := os.ReadDir(opts.StateDir)
			args := append([]string{"--distro", "skrog-561-other", "--state-dir", opts.StateDir}, c.args...)

			code, stderr := runStderr(t, c.run, args)
			if code != exitUsage {
				t.Errorf("exit = %d, want %d (usage)", code, exitUsage)
			}
			for _, want := range []string{"skrog-561-other", "skrog-561-recorded", "--state-dir"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr does not mention %q:\n%s", want, stderr)
				}
			}
			after, _ := os.ReadDir(opts.StateDir)
			if len(after) != len(before) {
				t.Errorf("the refused command changed the state dir: %d entries, then %d", len(before), len(after))
			}
		})
	}
}

// The same name, in any case, is not a conflict: WSL distro names are
// case-insensitive, and the e2e suite passes --distro with the manifest's own.
func TestDistroMatchingTheManifestIsNoConflict(t *testing.T) {
	opts := writeManifest(t, provision.Manifest{Distro: "skrog-engine"})
	for _, d := range []string{"", "skrog-engine", "Skrog-Engine"} {
		opts.Distro = d
		if msg := distroConflict(&provision.Provisioner{}, opts); msg != "" {
			t.Errorf("--distro %q: %s", d, msg)
		}
	}
	// With no install, --distro is how a distro is chosen at all.
	none := provision.Options{StateDir: t.TempDir(), Distro: "hand-rolled"}
	if msg := distroConflict(&provision.Provisioner{}, none); msg != "" {
		t.Errorf("no manifest: %s", msg)
	}
}
