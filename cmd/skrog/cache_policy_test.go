package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/policy"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/prune"
	"github.com/wslkit/skrog/internal/regcache"
)

func writePolicy(t *testing.T, dir, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, policy.FileName), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorDeniedByPolicy(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "allow-registries:\n  - docker.io\n")
	opts := provision.Options{StateDir: dir}

	if _, denied := mirrorDeniedByPolicy(opts, "https://docker.io"); denied {
		t.Error("an allowed registry was refused as a mirror")
	}
	if _, denied := mirrorDeniedByPolicy(opts, "https://evil.example.com"); !denied {
		t.Error("a registry outside the allowlist was accepted as a mirror")
	}
	// The default upstream is Docker Hub itself and is not a user choice.
	if _, denied := mirrorDeniedByPolicy(opts, ""); denied {
		t.Error("the default upstream was refused")
	}
}

// A rule set that cannot be read must not block the command: this is a
// registry allowlist, not an authentication boundary, and `skrog policy show`
// already reports the parse error.
func TestMirrorNotDeniedWhenPolicyIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "this: is: not: valid: yaml:\n  - [\n")
	if _, denied := mirrorDeniedByPolicy(provision.Options{StateDir: dir}, "https://evil.example.com"); denied {
		t.Error("an unparsable policy blocked the command")
	}
}

// The wiring, not the helper: `cache enable` has to consult policy BEFORE it
// pulls anything or starts a container.
//
// The exit CODE cannot carry this test. With the check deleted the command
// runs on and fails at docker, which also returns exitError — the first
// version of this test asserted the code, passed with the check removed, and
// proved nothing. So the assertion is the reason on stderr, which only the
// policy refusal produces.
func TestCacheEnableRefusesADeniedMirrorBeforeTouchingDocker(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "allow-registries:\n  - docker.io\n")

	code, errOut := cacheEnableCapturingStderr(t, dir, "https://evil.example.com", false)

	if code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errOut, "evil.example.com") || !strings.Contains(errOut, "mirror") {
		t.Errorf("stderr does not carry the policy refusal, so the check did not run:\n%s", errOut)
	}
}

// The http:// refusal is wired to the same place, and has its own exit code,
// so this one can assert both.
func TestCacheEnableRefusesPlainHTTPBeforeTouchingDocker(t *testing.T) {
	dir := t.TempDir()
	code, errOut := cacheEnableCapturingStderr(t, dir, "http://mirror.internal", false)

	if code != exitUsage {
		t.Errorf("exit = %d, want %d for an http:// upstream without --insecure", code, exitUsage)
	}
	if !strings.Contains(errOut, "--insecure") {
		t.Errorf("stderr should name the way out:\n%s", errOut)
	}
}

// cacheEnableCapturingStderr runs cacheEnable with stderr redirected, so a test
// can tell WHICH refusal happened rather than only that something did.
func cacheEnableCapturingStderr(t *testing.T, stateDir, upstream string, insecure bool) (int, string) {
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

	code := cacheEnable(context.Background(), prune.DockerRunner{},
		provision.Options{StateDir: stateDir},
		regcache.Options{Upstream: upstream}, insecure)

	w.Close()
	os.Stderr = old
	return code, <-done
}
