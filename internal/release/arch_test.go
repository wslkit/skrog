package release_test

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/release"
)

// A tripwire on the premise CheckHostArch rests on: an Engine has exactly one
// rootfs and no architecture dimension anywhere — not in the schema, not even
// in the published filenames, which are `skrog-rootfs-<version>.tar.gz` with
// no arch in them at all. That is why a blanket refusal is the right shape
// today: there is nothing to select between.
//
// The day an arm64 rootfs is published (#388), this has to change — an arch
// key in the schema, or arch in the asset name — and one of the assertions
// below will fail and lead whoever does it to CheckHostArch, which must then
// select rather than refuse.
func TestTheManifestStillHasNoArchitectureDimension(t *testing.T) {
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Engines) == 0 {
		t.Fatal("the manifest has no engines; this test proves nothing")
	}
	if release.EngineArch != "amd64" {
		t.Errorf("EngineArch is %q; every published rootfs is built x86_64-only "+
			"(rootfs.yml has one build job and no matrix)", release.EngineArch)
	}
	for _, e := range m.Engines {
		if e.Rootfs.URL == "" {
			continue // placeholder entry; Published() already covers it
		}
		for _, other := range []string{"arm64", "aarch64"} {
			if strings.Contains(e.Rootfs.URL, other) {
				t.Errorf("engine %s points at what looks like a %s rootfs (%s), but "+
					"CheckHostArch refuses every non-%s host outright. Teach it to "+
					"select before publishing this.", e.Version, other, e.Rootfs.URL, release.EngineArch)
			}
		}
	}
}

func TestCheckHostArchOnThisHost(t *testing.T) {
	err := release.CheckHostArch()
	if runtime.GOARCH == "amd64" {
		if err != nil {
			t.Errorf("refused on amd64, where the rootfs works: %v", err)
		}
		return
	}

	// On arm64 -- which CI runs (#389) -- it must refuse, and the message has
	// to carry the three things a user needs: what is wrong, that their skrog
	// binary is fine, and what to do instead. "unsupported" on its own sends
	// people to the issue tracker to ask.
	if err == nil {
		t.Fatalf("allowed a manifest install on %s; the rootfs is %s-only",
			runtime.GOARCH, release.EngineArch)
	}
	var unsupported *release.ErrUnsupportedHostArch
	if !errors.As(err, &unsupported) {
		t.Fatalf("error is not *ErrUnsupportedHostArch, so callers cannot branch on it: %T", err)
	}
	if unsupported.Host != runtime.GOARCH {
		t.Errorf("Host = %q, want %q", unsupported.Host, runtime.GOARCH)
	}
	for _, want := range []string{release.EngineArch, runtime.GOARCH, "--rootfs-url", "issues/388"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never mentions %q:\n%s", want, err)
		}
	}
}

// The message is the whole deliverable here -- the bug was never a crash, it
// was a failure that did not say "architecture". Check it on both hosts.
func TestUnsupportedHostArchMessageIsActionable(t *testing.T) {
	err := &release.ErrUnsupportedHostArch{Host: "arm64"}
	msg := err.Error()

	for _, want := range []string{
		"amd64-only",     // what
		"arm64",          // and on what
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
