package policy

import (
	"path/filepath"
	"strings"
	"testing"
)

// The default has to stay "allow and say so". #334 decided that deliberately,
// and a rule that silently started refusing builds would break working setups
// on upgrade.
func TestBuildsAllowedByDefault(t *testing.T) {
	for _, r := range []Rules{
		{},
		{AllowRegistries: []string{"registry.example.com"}},
		{DenyPrivileged: true, AllowRegistries: []string{"registry.example.com"}},
	} {
		if _, denied := r.DenyBuild(); denied {
			t.Errorf("Rules%+v refused a build without being asked to", r)
		}
	}
}

// The rule needs an allowlist to bite: refusing builds on a machine with no
// registry restriction would close a hole that is not open.
func TestDenyUnattributableBuildsNeedsAnAllowlist(t *testing.T) {
	if _, denied := (Rules{DenyUnattributableBuilds: true}).DenyBuild(); denied {
		t.Error("builds refused with no allow-registries; the rule should be inert")
	}
}

func TestDenyUnattributableBuildsRefusesWithAnAllowlist(t *testing.T) {
	r := Rules{
		DenyUnattributableBuilds: true,
		AllowRegistries:          []string{"registry.example.com", "*.internal"},
	}
	reason, denied := r.DenyBuild()
	if !denied {
		t.Fatal("builds were allowed with the rule set and an allowlist active")
	}
	// The message has to say what is in force and how to proceed, or it reads
	// as an outage rather than a policy.
	for _, want := range []string{"registry.example.com", "deny-unattributable-builds", "Pull the image"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason does not mention %q: %q", want, reason)
		}
	}
}

// A file that sets only this rule is a configured file. Reporting it as "no
// policy" would be the kind of lie this package exists to avoid.
func TestDenyUnattributableBuildsCountsAsPolicy(t *testing.T) {
	if (Rules{DenyUnattributableBuilds: true}).Empty() {
		t.Error("a rule set with deny-unattributable-builds reported itself empty")
	}
}

// It is a deny rule, so the user layer must not be able to drop it (#386).
func TestDenyUnattributableBuildsSurvivesMerge(t *testing.T) {
	machine := Rules{DenyUnattributableBuilds: true, AllowRegistries: []string{"registry.example.com"}}
	if _, denied := Merge(machine, Rules{}).DenyBuild(); !denied {
		t.Error("an empty user layer dropped the machine's build rule")
	}
	// And the machine can set the rule while the user supplies the allowlist
	// that makes it bite -- which is the correct outcome, not an accident.
	m := Merge(Rules{DenyUnattributableBuilds: true}, Rules{AllowRegistries: []string{"only.example.com"}})
	if _, denied := m.DenyBuild(); !denied {
		t.Error("machine rule + user allowlist did not combine into a refusal")
	}
}

// Parsing: the YAML key is part of the contract.
func TestDenyUnattributableBuildsParses(t *testing.T) {
	r, err := Parse([]byte("deny-unattributable-builds: true\nallow-registries:\n  - r.example.com\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !r.DenyUnattributableBuilds {
		t.Error("the key did not parse")
	}
	if _, denied := r.DenyBuild(); !denied {
		t.Error("parsed rules did not refuse a build")
	}
}

// The Watcher is what the bridge installs as its gate -- Rules is not. This
// whole rule shipped as a no-op because every test called Rules.DenyBuild()
// and nothing called the method the product actually reaches.
func TestWatcherDenyBuildConsultsTheRules(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(MachineDirEnv, t.TempDir())
	write(t, filepath.Join(dir, FileName),
		"allow-registries:\n  - registry.example.com\ndeny-unattributable-builds: true\n")

	w := NewWatcher(dir)
	reason, denied := w.DenyBuild()
	if !denied {
		t.Fatal("Watcher.DenyBuild allowed a build with the rule set; the gate is a no-op")
	}
	if !strings.Contains(reason, "registry.example.com") {
		t.Errorf("reason does not name the allowlist in force: %q", reason)
	}
}

// ...and still allows by default, which is the shipped behaviour #334 chose.
func TestWatcherDenyBuildAllowsByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(MachineDirEnv, t.TempDir())
	write(t, filepath.Join(dir, FileName), "allow-registries:\n  - registry.example.com\n")

	if _, denied := NewWatcher(dir).DenyBuild(); denied {
		t.Error("Watcher.DenyBuild refused without deny-unattributable-builds")
	}
}

// A machine file that has never parsed must REFUSE, not fall through to the
// user's rules. The user layer already did this (#254); the machine half did
// not, which is worse -- a typo in an Intune deployment meant every machine
// that received it ran unenforced.
func TestWatcherFailsClosedOnAnUnreadableMachineFile(t *testing.T) {
	machineDir := t.TempDir()
	t.Setenv(MachineDirEnv, machineDir)
	// KnownFields(true) makes a misspelled rule a hard parse error, which is
	// the realistic deployment typo.
	write(t, filepath.Join(machineDir, FileName), "deny-priviledged: true\n")

	w := NewWatcher(t.TempDir())
	body := map[string]any{"HostConfig": map[string]any{"Privileged": true}}

	reason, denied := w.DenyCreate(body)
	if !denied {
		t.Fatal("a broken machine policy allowed --privileged; the fleet layer failed OPEN")
	}
	if !strings.Contains(reason, "machine-wide") {
		t.Errorf("the refusal does not say which layer is broken: %q", reason)
	}
	if err := w.Unavailable(); err == nil {
		t.Error("Unavailable() reported healthy with an unparseable machine file")
	}
}
