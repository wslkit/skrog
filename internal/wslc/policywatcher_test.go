package wslc

import (
	"errors"
	"testing"
)

// newTestWatcher builds a watcher over a caller-controlled read, without
// touching the registry.
func newTestWatcher(initial Policies, read func() (Policies, error)) *PolicyWatcher {
	return &PolicyWatcher{read: read, cur: initial}
}

// TestPolicyGateSeesATightenedPolicy is the #354 regression.
//
// The supervisor runs for months. A policy snapshot taken at logon means an
// allowlist deployed at 10am is not enforced until the machine is rebooted --
// and the direction of that failure is the wrong one: the gate keeps enforcing
// the LOOSER policy while looking correctly configured.
func TestPolicyGateSeesATightenedPolicy(t *testing.T) {
	// No allowlist: this is a machine with no policy deployed yet.
	current := Policies{ContainersAllowed: true, PrivilegedAllowed: true}

	w := newTestWatcher(current, func() (Policies, error) { return current, nil })
	gate := &PolicyGate{Source: w.Policies}

	body := map[string]any{"Image": "docker.io/library/busybox"}
	if _, denied := gate.DenyCreate(body); denied {
		t.Fatal("with no allowlist deployed, the create should be allowed")
	}

	// An administrator deploys an allowlist that does not include Docker Hub.
	// Nothing restarts; the same gate object judges the next request.
	current = Policies{
		ContainersAllowed: true,
		PrivilegedAllowed: true,
		RegistryAllowlist: []string{"contoso.azurecr.io"},
	}

	reason, denied := gate.DenyCreate(body)
	if !denied {
		t.Fatal("the tightened allowlist was not enforced: the gate is still using the startup snapshot (#354)")
	}
	if reason == "" {
		t.Error("a denial should say why")
	}
}

// TestPolicyGateSeesARelaxedPolicy is the same mechanism in the other
// direction, so the fix is a live read rather than a one-way ratchet.
func TestPolicyGateSeesARelaxedPolicy(t *testing.T) {
	current := Policies{
		ContainersAllowed: true,
		PrivilegedAllowed: true,
		RegistryAllowlist: []string{"contoso.azurecr.io"},
	}
	w := newTestWatcher(current, func() (Policies, error) { return current, nil })
	gate := &PolicyGate{Source: w.Policies}

	body := map[string]any{"Image": "docker.io/library/busybox"}
	if _, denied := gate.DenyCreate(body); !denied {
		t.Fatal("the deployed allowlist should deny Docker Hub")
	}

	current = Policies{ContainersAllowed: true, PrivilegedAllowed: true}
	if _, denied := gate.DenyCreate(body); denied {
		t.Error("the allowlist was withdrawn; the gate should stop denying")
	}
}

// TestPrivilegedPolicyIsAlsoLive covers the second check in DenyCreate, which
// reads a different field and would be easy to leave on the snapshot.
func TestPrivilegedPolicyIsAlsoLive(t *testing.T) {
	current := Policies{ContainersAllowed: true, PrivilegedAllowed: true}
	w := newTestWatcher(current, func() (Policies, error) { return current, nil })
	gate := &PolicyGate{Source: w.Policies}

	body := map[string]any{
		"Image":      "busybox",
		"HostConfig": map[string]any{"Privileged": true},
	}
	if _, denied := gate.DenyCreate(body); denied {
		t.Fatal("privileged is allowed by default")
	}

	current = Policies{ContainersAllowed: true, PrivilegedAllowed: false}
	if _, denied := gate.DenyCreate(body); !denied {
		t.Error("AllowWSLContainerPrivileged was deployed and is not being enforced")
	}
}

// TestDenyBuildAndDenyPullAreLive covers the other two gate entry points.
func TestDenyBuildAndDenyPullAreLive(t *testing.T) {
	current := Policies{ContainersAllowed: true, PrivilegedAllowed: true}
	w := newTestWatcher(current, func() (Policies, error) { return current, nil })
	gate := &PolicyGate{Source: w.Policies}

	if _, denied := gate.DenyBuild(); denied {
		t.Fatal("no allowlist, so a build is fine")
	}
	if _, denied := gate.DenyPull("docker.io/library/busybox"); denied {
		t.Fatal("no allowlist, so a pull is fine")
	}

	current = Policies{
		ContainersAllowed: true,
		PrivilegedAllowed: true,
		RegistryAllowlist: []string{"contoso.azurecr.io"},
	}
	if _, denied := gate.DenyBuild(); !denied {
		t.Error("an allowlist is in force; the build should be refused")
	}
	if _, denied := gate.DenyPull("docker.io/library/busybox"); !denied {
		t.Error("an allowlist is in force; the pull should be refused")
	}
}

// TestReadFailureKeepsTheLastPolicy is the fail-safe direction.
//
// A transient registry failure must never widen what is enforced. Falling back
// to the zero value would turn a blip into an open gate, which is exactly the
// bypass shape #322 was about.
func TestReadFailureKeepsTheLastPolicy(t *testing.T) {
	restrictive := Policies{
		ContainersAllowed: true,
		PrivilegedAllowed: false,
		RegistryAllowlist: []string{"contoso.azurecr.io"},
	}
	fail := true
	w := newTestWatcher(restrictive, func() (Policies, error) {
		if fail {
			return Policies{}, errors.New("registry unavailable")
		}
		return restrictive, nil
	})

	got := w.Policies()
	if len(got.RegistryAllowlist) != 1 || got.RegistryAllowlist[0] != "contoso.azurecr.io" {
		t.Errorf("a failed re-read discarded the allowlist: %+v", got)
	}
	if got.PrivilegedAllowed {
		t.Error("a failed re-read re-allowed privileged containers")
	}

	// And a gate over it still denies.
	gate := &PolicyGate{Source: w.Policies}
	if _, denied := gate.DenyCreate(map[string]any{"Image": "busybox"}); !denied {
		t.Error("the gate opened while the policy could not be read")
	}

	// Recovery works.
	fail = false
	if got := w.Policies(); len(got.RegistryAllowlist) != 1 {
		t.Errorf("after recovery the policy should be readable again: %+v", got)
	}
}

// TestReadErrorIsReportedOnce keeps a persistently broken key from drowning the
// log of a supervisor that runs for months.
func TestReadErrorIsReportedOnce(t *testing.T) {
	w := newTestWatcher(Policies{}, func() (Policies, error) {
		return Policies{}, errors.New("same failure every time")
	})

	w.Policies()
	if w.lastErr == "" {
		t.Fatal("the first failure should be recorded")
	}
	first := w.lastErr
	w.Policies()
	if w.lastErr != first {
		t.Error("a repeated identical failure should not be re-recorded")
	}
}

// TestPolicyGateWithoutSourceUsesTheField keeps the existing construction
// working, which is what every other test in this package relies on.
func TestPolicyGateWithoutSourceUsesTheField(t *testing.T) {
	gate := &PolicyGate{Policies: Policies{
		ContainersAllowed: true,
		PrivilegedAllowed: true,
		RegistryAllowlist: []string{"contoso.azurecr.io"},
	}}
	if _, denied := gate.DenyCreate(map[string]any{"Image": "busybox"}); !denied {
		t.Error("a gate built with Policies and no Source must still enforce it")
	}
}

func TestSamePolicies(t *testing.T) {
	base := Policies{ContainersAllowed: true, PrivilegedAllowed: true, RegistryAllowlist: []string{"a", "b"}}
	for _, tc := range []struct {
		name string
		b    Policies
		want bool
	}{
		{"identical", base, true},
		{"allowlist order", Policies{true, true, []string{"b", "a"}}, false},
		{"allowlist length", Policies{true, true, []string{"a"}}, false},
		{"privileged", Policies{true, false, []string{"a", "b"}}, false},
		{"containers", Policies{false, true, []string{"a", "b"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := samePolicies(base, tc.b); got != tc.want {
				t.Errorf("samePolicies = %v, want %v", got, tc.want)
			}
		})
	}
}
