package release_test

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/release"
)

// The default engine must be installable on BOTH architectures (#388).
//
// This replaces a tripwire that asserted the opposite -- that no arm64 rootfs
// was published yet -- and which fired, as designed, the moment one was. It
// is kept pointing the other way because the interesting failure is no longer
// "arm64 appeared unannounced" but "arm64 quietly disappeared": an engine
// bump that publishes only amd64 would otherwise silently drop Windows on ARM
// back to a refusal, and nothing on an amd64 developer machine or an amd64 CI
// runner would notice.
//
// Older engines are exempt. 29.8.0 and 29.7.2 were cut before the rootfs
// build had an architecture matrix, and no arm64 tarball for them exists to
// point at. They stay amd64-only, which is why this checks the default rather
// than every entry.
func TestTheDefaultEngineShipsBothArchitectures(t *testing.T) {
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def, err := m.Engine("")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if _, err := def.RootfsFor(arch); err != nil {
			t.Errorf("default engine %s cannot be installed on %s: %v",
				def.Version, arch, err)
		}
	}
}

// Every engine must have an amd64 ENTRY, which is the architecture the
// product is actually shipped and tested on. An entry that went missing would
// be a typo in a hand-edited JSON file that nothing else catches until an
// install.
//
// Deliberately tolerates an empty checksum. That is the documented interim
// between cutting a rootfs release and copying its digests in (RELEASING.md
// step 2), and main sits in it for as long as that takes. Asserting
// installability here would make a normal, intended state look like a broken
// repository -- and worse, would train someone to ignore a red main during
// every release. What must never be empty is a checksum in a RELEASE build,
// and release.yml gates that, where it is unambiguously true.
func TestEveryEngineHasAnAmd64Entry(t *testing.T) {
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range m.Engines {
		_, err := e.RootfsFor("amd64")
		var unsupported *release.ErrUnsupportedHostArch
		if errors.As(err, &unsupported) {
			t.Errorf("engine %s lists no amd64 rootfs at all (architectures: %v)",
				e.Version, e.Architectures())
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

// The default engine resolves on whatever host the tests are running on.
//
// This is the one that runs on both CI runners, and since arm64 shipped it
// asserts the same thing on each: `skrog install` finds a rootfs. It used to
// encode "amd64 works, arm64 refuses", which meant the arm64 runner was
// asserting the product was broken there -- useful while that was true, and
// exactly the sort of test that keeps passing after it stops being true.
func TestTheDefaultEngineResolvesOnThisHost(t *testing.T) {
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def, err := m.Engine("")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	r, err := def.HostRootfs()
	if err != nil {
		t.Fatalf("the default engine has no rootfs for %s: %v", runtime.GOARCH, err)
	}
	// The URL must actually name this architecture. Selecting the wrong entry
	// would pass the check above and fail at `wsl --import`, which is the
	// original bug wearing a different hat.
	if !strings.Contains(r.URL, runtime.GOARCH) {
		t.Errorf("on %s the selected rootfs is %q, which does not name this architecture",
			runtime.GOARCH, r.URL)
	}
}

// An architecture nobody builds for still has to refuse well. This is the
// path ErrUnsupportedHostArch is left serving now that amd64 and arm64 both
// resolve.
func TestAnUnbuiltArchitectureIsRefusedWithAReason(t *testing.T) {
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def, _ := m.Engine("")
	_, err = def.RootfsFor("riscv64")
	var unsupported *release.ErrUnsupportedHostArch
	if !errors.As(err, &unsupported) {
		t.Fatalf("want *ErrUnsupportedHostArch so callers can branch, got %T: %v", err, err)
	}
	if unsupported.Host != "riscv64" {
		t.Errorf("Host = %q, want riscv64", unsupported.Host)
	}
	for _, want := range []string{"riscv64", "amd64", "arm64", "--rootfs-url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never mentions %q:\n%s", want, err)
		}
	}
}

// The message is the whole deliverable here -- the bug was never a crash, it
// was a failure that did not say "architecture".
func TestUnsupportedHostArchMessageIsActionable(t *testing.T) {
	err := &release.ErrUnsupportedHostArch{
		Host:      "riscv64",
		Available: []string{"amd64", "arm64"},
	}
	msg := err.Error()

	for _, want := range []string{
		"riscv64",        // this machine
		"amd64, arm64",   // what there is instead
		"runs natively",  // your skrog binary is not the problem
		"Docker Desktop", // a real alternative, where one exists
		"--rootfs-url",   // the escape hatch, still open
		"issues",         // where to say this platform matters
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
