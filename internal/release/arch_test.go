package release_test

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/release"
)

// The manifest now has an architecture dimension, so selection replaces the
// blanket refusal that was here before (#388). What is still true, and what
// this pins, is that no arm64 rootfs has been PUBLISHED: the build produces
// one but no release carries it yet.
//
// When that changes, this fails and leads whoever cut the release to the
// install-side tests below, which stop describing the arm64 machine as
// unsupported.
func TestNoArm64RootfsIsPublishedYet(t *testing.T) {
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Engines) == 0 {
		t.Fatal("the manifest has no engines; this test proves nothing")
	}
	for _, e := range m.Engines {
		if _, err := e.RootfsFor("arm64"); err == nil {
			t.Errorf("engine %s now has a published arm64 rootfs. Good — but the "+
				"tests below still describe arm64 as unsupported, and docs/install.md "+
				"and internal/release/arch.go say so too. Update them.", e.Version)
		}
	}
}

// Every engine must offer amd64, which is the architecture the product is
// actually shipped and tested on. An entry that lost it would be a typo in a
// hand-edited JSON file that nothing else would catch until an install.
func TestEveryEngineHasAnAmd64Rootfs(t *testing.T) {
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range m.Engines {
		if _, err := e.RootfsFor("amd64"); err != nil {
			t.Errorf("engine %s has no usable amd64 rootfs: %v", e.Version, err)
		}
	}
}

// RootfsFor's two failure modes must stay distinguishable, because the exit
// code and the advice differ: "never built for your machine" is permanent,
// "release not cut" is not.
func TestRootfsForDistinguishesItsTwoFailures(t *testing.T) {
	e := release.Engine{
		Version: "1.2.3",
		Rootfs: map[string]release.Rootfs{
			"amd64": {URL: "https://x/y.tar.gz", SHA256: strings.Repeat("a", 64)},
			"arm64": {}, // built, release not cut
		},
	}

	if _, err := e.RootfsFor("amd64"); err != nil {
		t.Errorf("a complete entry was rejected: %v", err)
	}

	var notPublished *release.ErrNotPublished
	if _, err := e.RootfsFor("arm64"); !errors.As(err, &notPublished) {
		t.Errorf("an entry with no checksum gave %T, want *ErrNotPublished: %v", err, err)
	} else if !strings.Contains(err.Error(), "arm64") {
		t.Errorf("the message does not name the architecture:\n%s", err)
	}

	var unsupported *release.ErrUnsupportedHostArch
	if _, err := e.RootfsFor("riscv64"); !errors.As(err, &unsupported) {
		t.Errorf("a missing architecture gave %T, want *ErrUnsupportedHostArch: %v", err, err)
	} else if got := unsupported.Available; len(got) != 2 || got[0] != "amd64" || got[1] != "arm64" {
		t.Errorf("Available = %v, want the sorted keys [amd64 arm64]", got)
	}
}

// Published() is host-relative since #388: `skrog engine list` offers what
// THIS machine can install, and an amd64-only engine is not a rollback target
// on an arm64 box.
func TestPublishedIsHostRelative(t *testing.T) {
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	sha := strings.Repeat("a", 64)

	mine := release.Engine{Rootfs: map[string]release.Rootfs{
		runtime.GOARCH: {URL: "https://x/y.tar.gz", SHA256: sha},
	}}
	if !mine.Published() {
		t.Errorf("an engine built for %s reports unpublished on %s", runtime.GOARCH, runtime.GOARCH)
	}

	theirs := release.Engine{Rootfs: map[string]release.Rootfs{
		other: {URL: "https://x/y.tar.gz", SHA256: sha},
	}}
	if theirs.Published() {
		t.Errorf("an engine built only for %s reports installable on %s", other, runtime.GOARCH)
	}
}

// The default engine must be installable on this host, or a plain
// `skrog install` fails on a machine the release supports.
//
// On the arm64 CI runner (#389) this is expected to refuse, and the refusal
// has to carry the three things a user needs: what is wrong, that their skrog
// binary is fine, and what to do instead. "unsupported" on its own sends
// people to the issue tracker to ask.
func TestTheDefaultEngineOnThisHost(t *testing.T) {
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def, err := m.Engine("")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	_, err = def.HostRootfs()

	if runtime.GOARCH == "amd64" {
		if err != nil {
			t.Errorf("refused on amd64, where the rootfs works: %v", err)
		}
		return
	}

	if err == nil {
		t.Fatalf("allowed a manifest install on %s; no %s rootfs is published",
			runtime.GOARCH, runtime.GOARCH)
	}
	var unsupported *release.ErrUnsupportedHostArch
	if !errors.As(err, &unsupported) {
		t.Fatalf("error is not *ErrUnsupportedHostArch, so callers cannot branch on it: %T", err)
	}
	if unsupported.Host != runtime.GOARCH {
		t.Errorf("Host = %q, want %q", unsupported.Host, runtime.GOARCH)
	}
	for _, want := range []string{"amd64", runtime.GOARCH, "--rootfs-url", "issues/388"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never mentions %q:\n%s", want, err)
		}
	}
}

// The message is the whole deliverable here -- the bug was never a crash, it
// was a failure that did not say "architecture". Check it on both hosts.
func TestUnsupportedHostArchMessageIsActionable(t *testing.T) {
	err := &release.ErrUnsupportedHostArch{Host: "arm64", Available: []string{"amd64"}}
	msg := err.Error()

	for _, want := range []string{
		"arm64",          // this machine
		"amd64",          // what there is instead
		"runs natively",  // your skrog binary is not the problem
		"Docker Desktop", // a real alternative today
		"--rootfs-url",   // the escape hatch, still open
		"issues/388",     // where this is going
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n%s", want, msg)
		}
	}
	// A wall of text nobody reads is its own failure.
	if n := strings.Count(msg, "\n"); n > 20 {
		t.Errorf("message is %d lines; keep it skimmable", n+1)
	}
}

// An engine with no architectures at all is a hand-edit gone wrong, and the
// most likely one: a schema-1 manifest decodes its single rootfs object into
// an empty map. Load must reject that rather than let every install blame the
// user's machine.
func TestLoadRejectsAnEngineWithNoArchitectures(t *testing.T) {
	// Covered through the real manifest by TestEveryEngineHasAnAmd64Rootfs;
	// this asserts the guard itself is reachable and worded about the
	// manifest rather than the host.
	e := release.Engine{Version: "1.2.3"}
	_, err := e.HostRootfs()
	if err == nil {
		t.Fatal("an engine with no rootfs entries resolved one")
	}
	var unsupported *release.ErrUnsupportedHostArch
	if !errors.As(err, &unsupported) || len(unsupported.Available) != 0 {
		t.Errorf("want ErrUnsupportedHostArch with no alternatives, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "built for none") {
		t.Errorf("with nothing available the message should say so:\n%s", err)
	}
}
