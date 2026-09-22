package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMain trusts the fixture files every other test in this package writes.
//
// They live in t.TempDir(), whose owner depends on the host: a developer's box
// makes them user-owned, and a CI runner that runs elevated makes them
// Administrators-owned. Without this, half the machine-layer tests would
// assert the refusal path on one and the acceptance path on the other.
//
// The refusal path has its own tests below, which drive it deterministically.
func TestMain(m *testing.M) {
	trustMachineFile = func(string) (bool, string) { return true, "" }
	os.Exit(m.Run())
}

// withTrust forces a verdict for one test, so the plumbing can be exercised
// without depending on who happens to own a temp directory.
func withTrust(t *testing.T, trusted bool, why string) {
	t.Helper()
	saved := trustMachineFile
	trustMachineFile = func(string) (bool, string) { return trusted, why }
	t.Cleanup(func() { trustMachineFile = saved })
}

func writeMachinePolicy(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MachineDirEnv, dir)
	return dir
}

// An untrusted machine file is not fleet configuration, whatever it says
// (#418). It must not reach the rules, and the refusal must be visible.
func TestUntrustedMachineFileIsRefusedAndReported(t *testing.T) {
	dir := writeMachinePolicy(t, "allow-registries:\n  - evil.example.com\n")
	withTrust(t, false, "it is owned by CONTOSO\\alice, not by Administrators or SYSTEM")

	rules, err := LoadMachine()
	if err != nil {
		t.Fatalf("LoadMachine: %v", err)
	}
	if !rules.Empty() {
		t.Errorf("an untrusted machine file was trusted: %+v", rules)
	}

	p := MachineProvenance()
	if p.Trusted {
		t.Error("provenance reports an untrusted file as trusted")
	}
	if p.Why == "" {
		t.Error("a refusal with no reason is not tamper-evidence")
	}
	if !p.Redirected || p.RedirectedTo != dir {
		t.Errorf("provenance does not report the redirection: %+v", p)
	}
}

// And it must not be merged into the effective rules, nor listed as a
// contributing layer -- `policy show` printed the refusal and then "deployed
// machine-wide" about the same file until Source stopped listing it.
func TestUntrustedMachineLayerIsNotMergedOrListed(t *testing.T) {
	writeMachinePolicy(t, "require-digest: true\n")
	withTrust(t, false, "not owned by an administrator")

	rules, src, err := LoadLayered(t.TempDir())
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	if rules.RequireDigest {
		t.Error("an untrusted machine layer was merged into the effective rules")
	}
	if src.MachinePath != "" {
		t.Errorf("an untrusted file is listed as a contributing layer: %q", src.MachinePath)
	}
}

// The accepting path still works, or the check would be a way to disable fleet
// policy rather than to verify it.
func TestTrustedMachineLayerIsMergedAndListed(t *testing.T) {
	writeMachinePolicy(t, "require-digest: true\n")
	withTrust(t, true, "")

	rules, src, err := LoadLayered(t.TempDir())
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	if !rules.RequireDigest {
		t.Error("a trusted machine layer was not merged")
	}
	if src.MachinePath == "" {
		t.Error("a trusted machine layer is not listed as contributing")
	}
}

// No machine file at all is not a refusal, and must not be reported as one.
func TestProvenanceWithNoMachineFile(t *testing.T) {
	t.Setenv(MachineDirEnv, t.TempDir())
	p := MachineProvenance()
	if p.Path != "" || p.Why != "" {
		t.Errorf("reported something about a machine file that does not exist: %+v", p)
	}
	if !p.Redirected {
		t.Error("a redirect to a directory with no policy file is the documented bypass; it must be reported")
	}
}

// checkOwner against whatever this host actually produces.
//
// The verdict is asserted against the file's REAL owner rather than against an
// assumption about it: a developer's box owns temp files as the user, and an
// elevated CI runner owns them as Administrators. An earlier version of this
// test assumed the first and failed only on the runner.
func TestCheckOwnerMatchesTheRealOwner(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("ownership is a Windows question; checkOwner is a stub elsewhere")
	}
	path := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	trusted, why := checkOwner(path)
	admin, err := ownedByAdminOrSystem(path)
	if err != nil {
		t.Skipf("could not read the owner independently: %v", err)
	}

	if trusted != admin {
		t.Errorf("checkOwner = %v (%s), but the file's owner is admin/SYSTEM = %v", trusted, why, admin)
	}
	if !trusted && why == "" {
		t.Error("a refusal must carry a reason")
	}
	t.Logf("this host creates temp files owned by admin/SYSTEM = %v; checkOwner agreed", admin)
}
