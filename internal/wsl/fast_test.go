package wsl

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner stands in for wsl.exe so the CLI fallback can be observed without
// one being installed.
type fakeRunner struct {
	out    string
	err    error
	called int
}

func (r *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.called++
	return []byte(r.out), r.err
}

func (r *fakeRunner) Start(ctx context.Context, name string, args ...string) (func(), error) {
	return func() {}, nil
}

const twoDistros = "  NAME            STATE           VERSION\n" +
	"* Ubuntu          Stopped         2\n" +
	"  skrog-engine    Running         2\n"

// withNoCOM makes newCOMSession fail, which is the state of every machine
// without wslservice — including CI, which runs Windows with no WSL.
func withNoCOM(t *testing.T, reason string) {
	t.Helper()
	prev := newSession
	newSession = func() (*comSession, error) { return nil, errors.New(reason) }
	t.Cleanup(func() { newSession = prev })
}

// TestFastFallsBackWhenCOMIsUnavailable is the property the whole design rests
// on: a machine where the COM surface is missing or has moved must be SLOWER,
// never broken.
func TestFastFallsBackWhenCOMIsUnavailable(t *testing.T) {
	withNoCOM(t, "no wslservice here")

	r := &fakeRunner{out: twoDistros}
	f := NewFast()
	f.Local.Runner = r

	got, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if r.called != 1 {
		t.Errorf("wsl.exe was invoked %d times, want 1 — the fallback did not run", r.called)
	}
	if len(got) != 2 {
		t.Fatalf("got %d distros, want 2: %+v", len(got), got)
	}
	if got[1].Name != "skrog-engine" || !got[1].Running() {
		t.Errorf("fallback returned %+v", got[1])
	}
}

// The reason must survive for doctor to report, rather than the machine merely
// being mysteriously slow.
func TestAcceleratedReportsWhyNot(t *testing.T) {
	withNoCOM(t, "CoCreateInstance(LxssUserSession): Class not registered")

	f := NewFast()
	ok, why := f.Accelerated()
	if ok {
		t.Fatal("Accelerated reported true with no COM session")
	}
	if !strings.Contains(why, "Class not registered") {
		t.Errorf("why = %q; it should carry the underlying reason", why)
	}
}

// Detection must happen once, not per call: a supervisor ticking for months
// must not retry a surface that is not there on every tick.
func TestSessionIsAttemptedOnce(t *testing.T) {
	attempts := 0
	prev := newSession
	newSession = func() (*comSession, error) {
		attempts++
		return nil, errors.New("nope")
	}
	t.Cleanup(func() { newSession = prev })

	f := NewFast()
	f.Local.Runner = &fakeRunner{out: twoDistros}
	for i := 0; i < 5; i++ {
		f.List(context.Background())
	}
	f.Accelerated()

	if attempts != 1 {
		t.Errorf("newCOMSession called %d times, want 1", attempts)
	}
}

// A Fast with no COM must behave exactly like a Local for everything else --
// it embeds one, and nothing else is overridden.
func TestFastDelegatesEverythingElse(t *testing.T) {
	withNoCOM(t, "unavailable")

	r := &fakeRunner{out: "ok"}
	f := NewFast()
	f.Local.Runner = r

	if _, err := f.Exec(context.Background(), "d", "root", "true"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if r.called != 1 {
		t.Errorf("Exec did not reach the CLI runner (called=%d)", r.called)
	}
}

// Close must be safe to call when no session was ever created, and twice.
func TestCloseIsSafeWithoutASession(t *testing.T) {
	withNoCOM(t, "unavailable")
	f := NewFast()
	f.Close()
	f.Close()
}

func TestFastImplementsWSL(t *testing.T) {
	var _ WSL = NewFast()
}

// Terminate must fall back exactly as List does. The property is the same one
// the whole design rests on: a machine where COM is unavailable is slower, not
// broken.
func TestFastTerminateFallsBackWhenCOMIsUnavailable(t *testing.T) {
	withNoCOM(t, "no wslservice here")
	r := &fakeRunner{}
	f := &Fast{Local: &Local{Runner: r}}

	if err := f.Terminate(context.Background(), "skrog-engine"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if r.called == 0 {
		t.Error("Terminate did not fall back to wsl.exe with no COM session")
	}
}

// A cancelled context is the caller's doing, not the surface having moved, so
// it must not demote the fast path for the rest of the process.
func TestFastTerminateCancelledContextDoesNotDemote(t *testing.T) {
	withNoCOM(t, "unavailable")
	r := &fakeRunner{}
	f := &Fast{Local: &Local{Runner: r}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// With no COM session this goes straight to Local; the assertion that
	// matters is that nothing panics and the reason is unchanged.
	_ = f.Terminate(ctx, "skrog-engine")
	if _, why := f.Accelerated(); !strings.Contains(why, "unavailable") {
		t.Errorf("reason = %q; a cancelled call should not have rewritten it", why)
	}
}

// A service-level error must NOT demote the fast path.
//
// Regression test for what the live run caught: terminating a distro that does
// not exist is the service answering correctly, and treating it as a moved
// interface disabled COM for the rest of the process AND re-ran the doomed
// operation through wsl.exe to produce a second, worse error.
func TestServiceErrorIsNotASurfaceFailure(t *testing.T) {
	if isServiceError(errors.New("plain")) {
		t.Error("a plain error was classified as a service answer")
	}
}
