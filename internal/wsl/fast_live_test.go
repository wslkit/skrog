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

// TestLiveTerminateOverCOM drives the new slots against the real service.
//
// Offline tests cannot catch a wrong vtable slot: they exercise the fallback
// against a fake, and a bad slot number would call a DIFFERENT method on the
// live object — which is the one failure mode this whole approach has to be
// checked against. So this terminates a real distro and confirms the state
// changed, rather than only confirming the call returned S_OK.
func TestLiveTerminateOverCOM(t *testing.T) {
	const distro = "skrog-engine"
	ctx := context.Background()

	f := NewFast()
	defer f.Close()
	if ok, why := f.Accelerated(); !ok {
		t.Skipf("no COM fast path on this host: %s", why)
	}
	local := NewLocal()
	if !registered(ctx, t, local, distro) {
		t.Skipf("%s is not registered here", distro)
	}

	// Start it so there is something to stop.
	if _, err := local.Exec(ctx, distro, "root", "/bin/true"); err != nil {
		t.Skipf("could not start %s: %v", distro, err)
	}
	if !runningNow(ctx, t, f, distro) {
		t.Fatalf("%s did not come up, so the terminate below would prove nothing", distro)
	}

	if err := f.Terminate(ctx, distro); err != nil {
		t.Fatalf("Terminate over COM: %v", err)
	}
	// The state has to have actually changed. A call that returns S_OK while
	// hitting the wrong slot would pass a weaker assertion than this.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !runningNow(ctx, t, f, distro) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("Terminate returned success but the distro is still running")
}

// TestLiveTerminateUnknownDistroIsAnError checks the error path resolves, which
// is where GetDistributionId is actually exercised.
func TestLiveTerminateUnknownDistroIsAnError(t *testing.T) {
	f := NewFast()
	defer f.Close()
	if ok, why := f.Accelerated(); !ok {
		t.Skipf("no COM fast path on this host: %s", why)
	}
	// A name nothing will have registered.
	err := f.Terminate(context.Background(), "skrog-no-such-distro-8f3a1c")
	if err == nil {
		t.Fatal("terminating a distro that does not exist reported success")
	}
	t.Logf("unknown distro error: %v", err)
}

func registered(ctx context.Context, t *testing.T, w WSL, name string) bool {
	t.Helper()
	ds, err := w.List(ctx)
	if err != nil {
		return false
	}
	for _, d := range ds {
		if d.Name == name {
			return true
		}
	}
	return false
}

func runningNow(ctx context.Context, t *testing.T, w WSL, name string) bool {
	t.Helper()
	ds, err := w.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, d := range ds {
		if d.Name == name {
			return d.Running()
		}
	}
	return false
}

// TestLiveTerminateLatency records COM against the CLI on this host, which is
// the measurement #356 asks for. Not an assertion -- a number in the log, the
// same shape as the List figures in the package comment.
func TestLiveTerminateLatency(t *testing.T) {
	const distro = "skrog-engine"
	ctx := context.Background()

	f := NewFast()
	defer f.Close()
	if ok, why := f.Accelerated(); !ok {
		t.Skipf("no COM fast path on this host: %s", why)
	}
	local := NewLocal()
	if !registered(ctx, t, local, distro) {
		t.Skipf("%s is not registered here", distro)
	}

	timeOne := func(stop func() error) time.Duration {
		// Start it first so each measurement terminates something real.
		if _, err := local.Exec(ctx, distro, "root", "/bin/true"); err != nil {
			t.Skipf("could not start %s: %v", distro, err)
		}
		start := time.Now()
		if err := stop(); err != nil {
			t.Fatalf("terminate: %v", err)
		}
		return time.Since(start)
	}

	const n = 3
	var com, cli time.Duration
	for i := 0; i < n; i++ {
		com += timeOne(func() error { return f.Terminate(ctx, distro) })
		cli += timeOne(func() error { return local.Terminate(ctx, distro) })
	}
	t.Logf("Terminate over %d runs: COM %.1f ms, CLI %.1f ms",
		n, float64(com.Microseconds())/float64(n)/1000, float64(cli.Microseconds())/float64(n)/1000)
}

// TestLiveTerminateSplit attributes the 10.5 ms: is it RPC overhead, which
// caching the GUID would remove, or the service doing real work, which nothing
// here can?
func TestLiveTerminateSplit(t *testing.T) {
	const distro = "skrog-engine"
	ctx := context.Background()

	f := NewFast()
	defer f.Close()
	if ok, why := f.Accelerated(); !ok {
		t.Skipf("no COM fast path on this host: %s", why)
	}
	s := f.session()
	if s == nil {
		t.Skip("no COM session")
	}

	// GetDistributionId alone, repeated: a pure lookup, no side effect.
	const n = 10
	start := time.Now()
	for i := 0; i < n; i++ {
		var err error
		if derr := s.do(ctx, func() {
			_, err = s.distributionIDLocked(distro)
		}); derr != nil {
			t.Fatalf("do: %v", derr)
		}
		if err != nil {
			t.Skipf("GetDistributionId: %v", err)
		}
	}
	lookup := time.Since(start) / n

	local := NewLocal()
	if _, err := local.Exec(ctx, distro, "root", "/bin/true"); err != nil {
		t.Skipf("could not start %s: %v", distro, err)
	}
	start = time.Now()
	if err := f.Terminate(ctx, distro); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	whole := time.Since(start)

	t.Logf("GetDistributionId alone: %.2f ms (mean of %d)", float64(lookup.Microseconds())/1000, n)
	t.Logf("Terminate (lookup + terminate): %.2f ms", float64(whole.Microseconds())/1000)
	t.Logf("=> attributable to the terminate itself: %.2f ms",
		float64((whole-lookup).Microseconds())/1000)
}
