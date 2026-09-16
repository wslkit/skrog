package wslc

import (
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/imageref"
	"github.com/wslkit/skrog/internal/pipeproxy"
)

// The gate has to satisfy both pipeproxy interfaces, or the pull and build
// checks are silently skipped — the handler only calls them through a type
// assertion.
func TestPolicyGateImplementsBothInterfaces(t *testing.T) {
	var g any = &PolicyGate{}
	if _, ok := g.(pipeproxy.Gate); !ok {
		t.Error("PolicyGate does not implement pipeproxy.Gate")
	}
	if _, ok := g.(pipeproxy.ImageGate); !ok {
		t.Error("PolicyGate does not implement pipeproxy.ImageGate; " +
			"pulls and builds would pass unjudged")
	}
}

func allowlist(entries ...string) *PolicyGate {
	return &PolicyGate{Policies: Policies{
		ContainersAllowed: true, PrivilegedAllowed: true, RegistryAllowlist: entries,
	}}
}

// TestGateResolvesReferencesLikeDocker checks the wiring: the parsing rule now
// lives in internal/imageref and is tested there, but the gate has to apply it.
func TestGateResolvesReferencesLikeDocker(t *testing.T) {
	g := allowlist("contoso.azurecr.io")

	for _, image := range []string{
		"busybox", "library/busybox", "busybox:latest", // all Docker Hub
		"localhost/app", "localhost:5000/app", "registry:5000/app",
	} {
		if _, denied := g.DenyCreate(map[string]any{"Image": image}); !denied {
			t.Errorf("allowlist=[contoso.azurecr.io] should deny %q", image)
		}
	}
	for _, image := range []string{"contoso.azurecr.io/app", "contoso.azurecr.io/team/app:v1"} {
		if _, denied := g.DenyCreate(map[string]any{"Image": image}); denied {
			t.Errorf("allowlist=[contoso.azurecr.io] should permit %q", image)
		}
	}

	// The discriminating case: a Hub namespace must not be read as a registry,
	// which every denial above would tolerate.
	hub := allowlist(imageref.DockerHub)
	for _, image := range []string{"busybox", "library/busybox", "myuser/app"} {
		if _, denied := hub.DenyCreate(map[string]any{"Image": image}); denied {
			t.Errorf("allowlist=[docker.io] should permit %q (a Hub namespace is not a registry)", image)
		}
	}
}

// TestUppercaseRegistryIsNotDockerHub is #355 at the gate.
//
// Hand-parsing resolved MYREG/img to docker.io, so an allowlist permitting
// Docker Hub also permitted a pull from a single-label host called MYREG: the
// gate judged one registry while the daemon contacted another. Same class as
// the bypasses fixed in #344.
func TestUppercaseRegistryIsNotDockerHub(t *testing.T) {
	hub := allowlist(imageref.DockerHub)

	for _, image := range []string{"MYREG/img", "MyReg/img", "MYREG.example.com/img"} {
		if _, denied := hub.DenyCreate(map[string]any{"Image": image}); !denied {
			t.Errorf("allowlist=[docker.io] must NOT permit %q — Docker resolves it to that host, not to Hub", image)
		}
		if _, denied := hub.DenyPull(image); !denied {
			t.Errorf("pull of %q must be denied for the same reason", image)
		}
	}
}

// TestUnattributableReferenceIsRefused: a reference dockerd itself cannot parse
// must not be guessed at. Refusing matches DenyBuild's precedent -- cannot
// attribute, so refuse -- and is only applied while an allowlist is in force,
// so a machine with no policy is unaffected.
func TestUnattributableReferenceIsRefused(t *testing.T) {
	g := allowlist("contoso.azurecr.io")
	for _, image := range []string{"/leading", "host./img", "user@host/img", "UPPER/UPPER"} {
		if _, denied := g.DenyCreate(map[string]any{"Image": image}); !denied {
			t.Errorf("unparseable reference %q should be refused while an allowlist is active", image)
		}
		if _, denied := g.DenyPull(image); !denied {
			t.Errorf("unparseable reference %q should be refused at pull too", image)
		}
	}

	// With no allowlist there is nothing to enforce, so it passes through and
	// the daemon gives its own error.
	open := &PolicyGate{Policies: Policies{ContainersAllowed: true, PrivilegedAllowed: true}}
	if _, denied := open.DenyCreate(map[string]any{"Image": "user@host/img"}); denied {
		t.Error("with no allowlist deployed, Skrog should not invent a refusal")
	}
}

// Gating container creation alone would let a blocked image be fetched onto the
// machine and merely not run. WSL applies the allowlist at the pull, so this
// has to as well.
func TestDenyPullEnforcesTheAllowlist(t *testing.T) {
	g := allowlist("contoso.azurecr.io")

	if _, denied := g.DenyPull("contoso.azurecr.io/app:v1"); denied {
		t.Error("an allowed registry was refused")
	}
	reason, denied := g.DenyPull("busybox")
	if !denied {
		t.Fatal("docker.io was permitted by an allowlist that does not name it")
	}
	// The message has to name what was blocked and what is allowed, or the
	// user cannot act on it.
	for _, want := range []string{"docker.io", "busybox", "contoso.azurecr.io"} {
		if !strings.Contains(reason, want) {
			t.Errorf("denial %q does not mention %q", reason, want)
		}
	}
}

func TestDenyCreateEnforcesTheAllowlist(t *testing.T) {
	g := allowlist("contoso.azurecr.io")

	if _, denied := g.DenyCreate(map[string]any{"Image": "contoso.azurecr.io/app"}); denied {
		t.Error("an allowed registry was refused")
	}
	if _, denied := g.DenyCreate(map[string]any{"Image": "busybox"}); !denied {
		t.Error("a blocked registry was permitted")
	}
}

// A build can pull from anywhere, so it cannot be attributed to an allowed
// registry. WSL refuses `wslc image build` on exactly this reasoning; allowing
// it here would be the easiest way around the allowlist.
func TestDenyBuildWhenTheAllowlistIsActive(t *testing.T) {
	if _, denied := (&PolicyGate{Policies: Policies{ContainersAllowed: true, PrivilegedAllowed: true}}).DenyBuild(); denied {
		t.Error("a build was refused with no allowlist configured")
	}
	reason, denied := allowlist("contoso.azurecr.io").DenyBuild()
	if !denied {
		t.Fatal("a build was allowed while an allowlist was in force")
	}
	if !strings.Contains(reason, "WSLContainerRegistryAllowlist") {
		t.Errorf("denial %q does not name the policy responsible", reason)
	}
}

func TestPrivilegedPolicy(t *testing.T) {
	privBody := map[string]any{
		"Image":      "busybox",
		"HostConfig": map[string]any{"Privileged": true},
	}

	allowed := &PolicyGate{Policies: Policies{ContainersAllowed: true, PrivilegedAllowed: true}}
	if _, denied := allowed.DenyCreate(privBody); denied {
		t.Error("privileged was refused with no policy forbidding it")
	}

	denied := &PolicyGate{Policies: Policies{ContainersAllowed: true, PrivilegedAllowed: false}}
	reason, no := denied.DenyCreate(privBody)
	if !no {
		t.Fatal("privileged was allowed despite AllowWSLContainerPrivileged")
	}
	if !strings.Contains(reason, AllowPrivilegedValue) {
		t.Errorf("denial %q does not name the policy responsible", reason)
	}

	// An unprivileged container must still pass.
	if _, no := denied.DenyCreate(map[string]any{"Image": "busybox"}); no {
		t.Error("an unprivileged container was refused")
	}
}

// With nothing deployed the gate must be invisible: every ordinary install has
// no policy, and a stand-in that denies by default would break all of them.
func TestUnconfiguredPolicyDeniesNothing(t *testing.T) {
	g := &PolicyGate{Policies: Policies{ContainersAllowed: true, PrivilegedAllowed: true}}

	if _, denied := g.DenyPull("anything/at/all:latest"); denied {
		t.Error("a pull was refused with no policy deployed")
	}
	if _, denied := g.DenyBuild(); denied {
		t.Error("a build was refused with no policy deployed")
	}
	if _, denied := g.DenyCreate(map[string]any{
		"Image":      "busybox",
		"HostConfig": map[string]any{"Privileged": true},
	}); denied {
		t.Error("a container was refused with no policy deployed")
	}
}
