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
