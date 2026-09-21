package main

import (
	"context"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/supervise"
)

// fakeDocker answers `docker context inspect` from the test, so the ownership
// rule can be exercised without a docker CLI on the machine.
type fakeDocker struct {
	endpoint string
	err      error
}

func (f fakeDocker) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.endpoint + "\n"), nil
}

func managerAt(endpoint string, err error) *dockerctx.Manager {
	return &dockerctx.Manager{Runner: fakeDocker{endpoint: endpoint, err: err}}
}

func TestContextIsOursWhenItStillPointsAtUs(t *testing.T) {
	m := &provision.Manifest{DockerContextHost: "npipe:////./pipe/docker_engine"}
	ours, why := contextIsOurs(context.Background(),
		managerAt("npipe:////./pipe/docker_engine", nil), m, t.TempDir())
	if !ours {
		t.Errorf("ours = false (%s), want true", why)
	}
}

func TestContextIsNotOursWhenAnotherInstallTookIt(t *testing.T) {
	// The bug: uninstalling the second install removed the first one's
	// context, and `docker --context skrog` then failed for an install that
	// was still running perfectly.
	m := &provision.Manifest{DockerContextHost: "npipe:////./pipe/skrog_engine"}
	ours, why := contextIsOurs(context.Background(),
		managerAt("npipe:////./pipe/docker_engine", nil), m, t.TempDir())
	if ours {
		t.Fatal("ours = true; a context pointing at another install must not be removed")
	}
	for _, want := range []string{"docker_engine", "skrog_engine", "another install"} {
		if !strings.Contains(why, want) {
			t.Errorf("reason %q should mention %q", why, want)
		}
	}
}

func TestContextIsOursOnAnInstallThatPredatesTheField(t *testing.T) {
	// Those machines were installed when one install was the only
	// possibility, so the context almost certainly is theirs — and leaving a
	// context pointed at a pipe nobody serves breaks every later docker
	// command, which is the worse of the two failures.
	ours, _ := contextIsOurs(context.Background(),
		managerAt("npipe:////./pipe/docker_engine", nil), &provision.Manifest{}, t.TempDir())
	if !ours {
		t.Error("ours = false for a manifest with no recorded endpoint")
	}
}

func TestContextIsOursWhenThereIsNoManifest(t *testing.T) {
	ours, _ := contextIsOurs(context.Background(),
		managerAt("npipe:////./pipe/docker_engine", nil), nil, t.TempDir())
	if !ours {
		t.Error("ours = false with no manifest at all")
	}
}

func TestContextIsOursWhenTheEndpointCannotBeRead(t *testing.T) {
	// No docker CLI, or no context to inspect. Remove handles both, and
	// guessing "not ours" would leave a dangling context behind instead.
	m := &provision.Manifest{DockerContextHost: "npipe:////./pipe/skrog_engine"}
	ours, _ := contextIsOurs(context.Background(),
		managerAt("", &dockerctx.ErrNoDockerCLI{}), m, t.TempDir())
	if !ours {
		t.Error("ours = false when the endpoint could not be read")
	}
}

func TestUninstallRestoresRatherThanRemovesWhenItTookTheContextOver(t *testing.T) {
	// The scenario the bug report describes, at the level the decision is
	// made: this install recorded that it took the context from another, so
	// the correct cleanup is to hand it back, not to delete it.
	m := &provision.Manifest{
		DockerContextHost:     "npipe:////./pipe/skrog_engine",
		DockerContextPrevious: "npipe:////./pipe/docker_engine",
	}
	ours, _ := contextIsOurs(context.Background(),
		managerAt("npipe:////./pipe/skrog_engine", nil), m, t.TempDir())
	if !ours {
		t.Fatal("ours = false; the context does still point at this install")
	}
	if m.DockerContextPrevious == "" {
		t.Fatal("no previous endpoint recorded, so uninstall would delete instead of restore")
	}
}

func TestAnInstallThatCreatedTheContextRecordsNoPrevious(t *testing.T) {
	// The single-install machine, which is almost everyone: nothing to hand
	// back, so uninstall removes the context and leaves nothing dangling.
	m := &provision.Manifest{DockerContextHost: "npipe:////./pipe/docker_engine"}
	if m.DockerContextPrevious != "" {
		t.Error("a fresh install should record no previous endpoint")
	}
	ours, _ := contextIsOurs(context.Background(),
		managerAt("npipe:////./pipe/docker_engine", nil), m, t.TempDir())
	if !ours {
		t.Error("ours = false for a context this install created and still owns")
	}
}

// The case the acceptance suite caught (#217 reaching too far the other way).
//
// `skrog install` records what IT wired -- normally docker_engine. A
// supervisor started with `--pipe custom` then re-points the same shared
// context at its own endpoint, and nothing writes that back to the manifest.
// Uninstall compared the two, concluded another install owned the context,
// and left one pointing at a pipe it was about to delete. Every later
// `docker --context skrog` then failed on a machine that had just
// uninstalled Skrog.
//
// served-pipe is the durable record of what this install last asked to serve
// (#429): written at supervisor start, deliberately not cleared on exit,
// which is what makes it readable here after the supervisor has gone.
func TestContextIsOursWhenOurSupervisorRepointedIt(t *testing.T) {
	dir := t.TempDir()
	if err := supervise.WriteServedPipe(dir, `\\.\pipe\skrog-e2e-suite`); err != nil {
		t.Fatal(err)
	}
	m := &provision.Manifest{DockerContextHost: "npipe:////./pipe/docker_engine"}

	ours, why := contextIsOurs(context.Background(),
		managerAt("npipe:////./pipe/skrog-e2e-suite", nil), m, dir)
	if !ours {
		t.Errorf("ours = false (%s).\n"+
			"  This install's own supervisor set that endpoint. Disowning it leaves a "+
			"`skrog` context pointing at a pipe uninstall is about to delete.", why)
	}
}

// And the protection #217 added must survive: a context pointing somewhere
// neither this install nor its supervisor ever set still belongs to someone
// else, and removing it breaks a working install.
func TestContextIsNotOursEvenWithAServedPipeRecord(t *testing.T) {
	dir := t.TempDir()
	if err := supervise.WriteServedPipe(dir, `\\.\pipe\skrog-e2e-suite`); err != nil {
		t.Fatal(err)
	}
	m := &provision.Manifest{DockerContextHost: "npipe:////./pipe/skrog_engine"}

	ours, _ := contextIsOurs(context.Background(),
		managerAt("npipe:////./pipe/somebody_else", nil), m, dir)
	if ours {
		t.Error("ours = true for an endpoint neither the install nor its supervisor set; " +
			"#217 comes back and uninstalling one install breaks another")
	}
}
