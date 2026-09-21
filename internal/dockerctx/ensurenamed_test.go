package dockerctx_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/dockerctx"
)

// otherContext is a name that is deliberately NOT dockerctx.Name. These tests
// are about EnsureNamed's three behaviours -- create, update, no-op -- and the
// rule that it never touches a context it was not asked about. A second name
// is how that last one is observable at all.
//
// It named a second backend's context while there was one (#335); the
// behaviour it pins outlived that backend (#451).
const otherContext = "skrog-other"

func TestEnsureNamedCreatesTheNamedContext(t *testing.T) {
	f := newFakeDocker().
		on("context ls", "default\nskrog", nil).
		on("context create", "", nil)
	m := &dockerctx.Manager{Runner: f}

	err := m.EnsureNamed(context.Background(), otherContext,
		"Some other engine", "npipe:////./pipe/other_engine")
	if err != nil {
		t.Fatal(err)
	}

	c := f.callWith("context create " + otherContext)
	if c == nil {
		t.Fatalf("expected `context create %s`, calls: %v", otherContext, f.calls)
	}
	if joined := strings.Join(c, " "); !strings.Contains(joined, "host=npipe:////./pipe/other_engine") {
		t.Errorf("create args do not carry the endpoint: %s", joined)
	}
	// The skrog context already exists in the listing above and must be left
	// exactly as it was.
	if f.called("context update " + dockerctx.Name) {
		t.Error("a context nobody asked about was updated")
	}
}

func TestEnsureNamedUpdatesItsOwnContextOnly(t *testing.T) {
	f := newFakeDocker().
		on("context ls", "default\nskrog\n"+otherContext, nil).
		on("context inspect", "npipe:////./pipe/stale", nil).
		on("context update", "", nil)
	m := &dockerctx.Manager{Runner: f}

	if err := m.EnsureNamed(context.Background(), otherContext, "d",
		"npipe:////./pipe/other_engine"); err != nil {
		t.Fatal(err)
	}
	c := f.callWith("context update " + otherContext)
	if c == nil {
		t.Fatalf("expected `context update %s`, calls: %v", otherContext, f.calls)
	}
	if f.called("context create") {
		t.Error("an existing context must be updated, not recreated")
	}
}

// An endpoint that already matches must not be rewritten: Ensure runs on every
// bridge start, and a no-op start should not touch the user's docker config.
func TestEnsureNamedIsANoOpWhenAlreadyCorrect(t *testing.T) {
	f := newFakeDocker().
		on("context ls", "default\n"+otherContext, nil).
		on("context inspect", "npipe:////./pipe/other_engine", nil)
	m := &dockerctx.Manager{Runner: f}

	if err := m.EnsureNamed(context.Background(), otherContext, "d",
		"npipe:////./pipe/other_engine"); err != nil {
		t.Fatal(err)
	}
	if f.called("context update") || f.called("context create") {
		t.Errorf("a correct context was rewritten; calls: %v", f.calls)
	}
}

// Ensure is EnsureNamed with the engine's identity, and that must not drift:
// every existing install depends on this exact name and description.
func TestEnsureStillTargetsTheDistroContext(t *testing.T) {
	f := newFakeDocker().
		on("context ls", "default", nil).
		on("context create", "", nil)
	m := &dockerctx.Manager{Runner: f}

	if err := m.Ensure(context.Background(), "npipe:////./pipe/docker_engine"); err != nil {
		t.Fatal(err)
	}
	c := f.callWith("context create " + dockerctx.Name)
	if c == nil {
		t.Fatalf("Ensure no longer creates %q; calls: %v", dockerctx.Name, f.calls)
	}
	if joined := strings.Join(c, " "); !strings.Contains(joined, "Skrog engine (WSL2)") {
		t.Errorf("the distro context description changed: %s", joined)
	}
}

func TestEnsureNamedRejectsAnEmptyName(t *testing.T) {
	m := &dockerctx.Manager{Runner: newFakeDocker()}
	if err := m.EnsureNamed(context.Background(), "", "d", "npipe://x"); err == nil {
		t.Fatal("want an error for an empty context name")
	}
}
