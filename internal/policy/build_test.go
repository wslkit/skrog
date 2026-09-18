package policy

import (
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
