package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMain trusts the fixture files every other test in this package writes.
//
// They live in t.TempDir(), which on Windows is owned by the user running the
// tests -- exactly what checkOwner refuses, and correctly so. Without this,
// every existing machine-layer test would be asserting the refusal path rather
// than the behaviour it was written for.
//
// The refusal path has its own tests below, which restore the real check.
func TestMain(m *testing.M) {
	trustMachineFile = func(string) (bool, string) { return true, "" }
	os.Exit(m.Run())
}

// withRealTrustCheck restores the production check for one test.
func withRealTrustCheck(t *testing.T) {
	t.Helper()
	saved := trustMachineFile
	trustMachineFile = checkOwner
	t.Cleanup(func() { trustMachineFile = saved })
}

// A file the user owns is not fleet configuration, whatever it says (#418).
// On Windows a temp file is owned by the user running the test, which is the
// shape of the ProgramData hole: CREATOR OWNER grants Full Control because the
// user owns the object.
func TestMachineFileOwnedByTheUserIsRefused(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("ownership is a Windows question; checkOwner is a stub elsewhere")
	}
	withRealTrustCheck(t)

	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("allow-registries:\n  - evil.example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MachineDirEnv, dir)

	rules, err := LoadMachine()
	if err != nil {
		t.Fatalf("LoadMachine: %v", err)
	}
	if !rules.Empty() {
		t.Errorf("a user-owned machine file was trusted: %+v", rules)
	}

	p := MachineProvenance()
	if p.Trusted {
		t.Error("provenance reports a user-owned file as trusted")
	}
	if p.Why == "" {
		t.Error("a refusal with no reason is not tamper-evidence")
	}
	if !p.Redirected || p.RedirectedTo != dir {
		t.Errorf("provenance does not report the redirection: %+v", p)
	}
}

// The refusal has to be visible even when the file parses and says something
// plausible -- that is the whole point. A silently ignored machine layer and a
// trusted one look identical to a fleet operator otherwise.
func TestProvenanceReportsAnUntrustedFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("ownership is a Windows question")
	}
	withRealTrustCheck(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("require-digest: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MachineDirEnv, dir)

	rules, _, err := LoadLayered(t.TempDir())
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	if rules.RequireDigest {
		t.Error("an untrusted machine layer was merged into the effective rules")
	}
}

// No machine file at all is not a refusal, and must not be reported as one.
func TestProvenanceWithNoMachineFile(t *testing.T) {
	t.Setenv(MachineDirEnv, t.TempDir())
	p := MachineProvenance()
	if p.Path != "" || p.Why != "" {
		t.Errorf("reported something about a machine file that does not exist: %+v", p)
	}
}
