//go:build wslcom && windows

// Live checks against the real wslservice on this machine. Behind a build tag
// so `go test ./...` stays host-independent — CI runs on Windows but has no WSL
// installed, and a test that needs a registered distro would fail there for a
// reason that has nothing to do with the change under test:
//
//	go test -tags wslcom ./internal/wsl/
//
// These exist because the offline tests validate the fallback logic against a
// fake, which cannot catch the COM surface itself moving, the struct layout
// being wrong, or the two backends disagreeing about what is running.
package wsl

import (
	"context"
	"testing"
	"time"
)

func TestLiveFastAgreesWithLocal(t *testing.T) {
	ctx := context.Background()
	f := NewFast()
	defer f.Close()

	if ok, why := f.Accelerated(); !ok {
		t.Skipf("COM fast path unavailable on this host: %s", why)
	}

	fast, err := f.List(ctx)
	if err != nil {
		t.Fatalf("Fast.List: %v", err)
	}
	local, err := NewLocal().List(ctx)
	if err != nil {
		t.Fatalf("Local.List: %v", err)
	}

	// Agreement is the whole point. Two backends behind one interface are only
	// safe if callers cannot tell which one answered.
	if len(fast) != len(local) {
		t.Fatalf("different distro counts: COM %d, CLI %d\n COM=%+v\n CLI=%+v",
			len(fast), len(local), fast, local)
	}
	byName := map[string]Distro{}
	for _, d := range fast {
		byName[d.Name] = d
	}
	for _, w := range local {
		g, ok := byName[w.Name]
		if !ok {
			t.Errorf("%q is listed by the CLI and missing from COM", w.Name)
			continue
		}
		// Running() is the only thing anything in Skrog actually asks, so it is
		// what has to match -- not the raw State string, which the COM path
		// reports from an enum rather than a localised table.
		if g.Running() != w.Running() {
			t.Errorf("%q: COM running=%v, CLI running=%v", w.Name, g.Running(), w.Running())
		}
		if g.Version != w.Version {
			t.Errorf("%q: COM version=%d, CLI version=%d", w.Name, g.Version, w.Version)
		}
	}
}

// The reason this backend exists. Not an assertion about a specific ratio --
// that is a property of the host -- but it should not be SLOWER, and if it is
// then something is wrong with the premise.
func TestLiveFastIsFasterThanTheCLI(t *testing.T) {
	ctx := context.Background()
	f := NewFast()
	defer f.Close()
	if ok, why := f.Accelerated(); !ok {
		t.Skipf("COM fast path unavailable on this host: %s", why)
	}
	l := NewLocal()

	const n = 10
	// Warm-up discarded from both: the first call pays for the apartment on one
	// side and for loading wsl.exe on the other, and neither is what a steady
	// state poll costs.
	f.List(ctx)
	l.List(ctx)

	t0 := time.Now()
	for i := 0; i < n; i++ {
		if _, err := f.List(ctx); err != nil {
			t.Fatalf("Fast.List: %v", err)
		}
	}
	fast := time.Since(t0) / n

	t0 = time.Now()
	for i := 0; i < n; i++ {
		if _, err := l.List(ctx); err != nil {
			t.Fatalf("Local.List: %v", err)
		}
	}
	cli := time.Since(t0) / n

	t.Logf("COM %v per call, CLI %v per call (%.0fx)", fast, cli, float64(cli)/float64(fast))
	if fast >= cli {
		t.Errorf("the COM path is not faster (COM %v, CLI %v); the reason for this backend does not hold here", fast, cli)
	}
}

// Repeated construction must not leak an apartment thread or an interface
// reference, because `skrog status` and `doctor` each build one per run.
func TestLiveRepeatedSessionsClose(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		f := NewFast()
		if ok, why := f.Accelerated(); !ok {
			f.Close()
			t.Skipf("COM fast path unavailable on this host: %s", why)
		}
		if _, err := f.List(ctx); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		f.Close()
		// Closing twice must be safe: Fast.Close and comSession.Close are both
		// reachable from a defer and from an explicit call.
		f.Close()
	}
}
