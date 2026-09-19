package supervise_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/supervise"
)

type hookRec struct {
	mu     sync.Mutex
	events []string
}

func (h *hookRec) fire(e string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, e)
}

func (h *hookRec) has(e string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, x := range h.events {
		if x == e {
			return true
		}
	}
	return false
}

// idleActivity looks permanently quiet, so maybeIdleStop only waits out the
// timeout, not real traffic.
type idleActivity struct{}

func (idleActivity) ActiveConns() int        { return 0 }
func (idleActivity) LastActivity() time.Time { return time.Time{} }

func TestHookPostStartAndPreStop(t *testing.T) {
	e := &fakeEngine{}
	sup, dir := newSup(t, e, 20*time.Millisecond)
	rec := &hookRec{}
	sup.Hook = rec.fire

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	waitFor(t, 3*time.Second, func() bool { return rec.has(supervise.HookPostStart) },
		"post-start hook did not fire after the engine started")

	if err := supervise.WriteDesired(dir, supervise.DesiredStopped); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return rec.has(supervise.HookPreStop) },
		"pre-stop hook did not fire on desired=stopped")
}

func TestHookOnIdleStop(t *testing.T) {
	e := &fakeEngine{running: true}
	sup, _ := newSup(t, e, 20*time.Millisecond)
	rec := &hookRec{}
	sup.Hook = rec.fire
	sup.Activity = idleActivity{}
	sup.IdleTimeout = func() time.Duration { return time.Millisecond }
	sup.Busy = func(context.Context) (bool, error) { return false, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	waitFor(t, 3*time.Second, func() bool { return rec.has(supervise.HookOnIdleStop) },
		"on-idle-stop hook did not fire")
	if engineUp(e) {
		t.Error("engine should be idle-stopped")
	}
}

func TestHookOnWake(t *testing.T) {
	e := &fakeEngine{running: false}
	sup, dir := newSup(t, e, time.Hour) // no ticks; drive Demand directly
	rec := &hookRec{}
	sup.Hook = rec.fire

	// Present as idle-stopped so Demand cold-starts rather than refusing.
	if err := supervise.WriteEngineState(dir, supervise.EngineIdle); err != nil {
		t.Fatal(err)
	}

	if err := sup.Demand(context.Background()); err != nil {
		t.Fatalf("Demand: %v", err)
	}
	if !rec.has(supervise.HookOnWake) {
		t.Error("on-wake hook did not fire after a cold start")
	}
}

func TestNilHookIsNoOp(t *testing.T) {
	e := &fakeEngine{}
	sup, _ := newSup(t, e, 20*time.Millisecond) // Hook left nil

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	waitFor(t, 3*time.Second, func() bool { s, _ := e.counts(); return s >= 1 },
		"engine should still start with no hook wired")
}
