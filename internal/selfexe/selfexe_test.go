package selfexe

import (
	"os"
	"path/filepath"
	"testing"
)

func withExecutable(t *testing.T, p string, err error) {
	t.Helper()
	prev := executable
	executable = func() (string, error) { return p, err }
	t.Cleanup(func() { executable = prev })
}

// The bug this package exists for: launched through a symlink, os.Executable
// returns the shim, and Dir() then points at a directory with no siblings in
// it. winget's portable installer type puts exactly such a symlink on PATH.
func TestPathFollowsASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real", "app")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		// Windows needs Developer Mode or elevation for this; skip rather than
		// fail on a runner that has neither.
		t.Skipf("cannot create a symlink here: %v", err)
	}
	withExecutable(t, link, nil)

	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	resolvedTarget, _ := filepath.EvalSymlinks(target)
	if got != resolvedTarget {
		t.Errorf("Path() = %q, want the symlink target %q", got, resolvedTarget)
	}
	if Dir() != filepath.Dir(resolvedTarget) {
		t.Errorf("Dir() = %q, want the directory holding the real binary", Dir())
	}
}

// A path that is not a symlink must come back unchanged — the ordinary case,
// and the one every existing install uses.
func TestPathLeavesARealPathAlone(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "app")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	withExecutable(t, exe, nil)

	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	// EvalSymlinks also normalises (8.3 names, case); compare against it.
	want, _ := filepath.EvalSymlinks(exe)
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

// A path that cannot be resolved (deleted, or a filesystem that will not say)
// must still yield the unresolved answer rather than failing: it is the best
// available, and a broken sibling lookup reports itself more clearly than a
// startup error would.
func TestPathFallsBackWhenResolutionFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone", "app")
	withExecutable(t, missing, nil)

	got, err := Path()
	if err != nil {
		t.Fatalf("Path() errored on an unresolvable path: %v", err)
	}
	if got != missing {
		t.Errorf("Path() = %q, want the unresolved path %q", got, missing)
	}
}

// Only a genuine os.Executable failure is an error.
func TestPathPropagatesALookupFailure(t *testing.T) {
	withExecutable(t, "", os.ErrNotExist)
	if _, err := Path(); err == nil {
		t.Error("want an error when the executable cannot be located")
	}
	if Dir() != "" {
		t.Errorf("Dir() = %q, want empty when the executable is unknown", Dir())
	}
}
