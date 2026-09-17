package supervise

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// pokeEngine counts Start calls and reports whatever Running should say.
type pokeEngine struct {
	running atomic.Bool
	starts  atomic.Int32
	probes  atomic.Int32
}

func (e *pokeEngine) Running(context.Context) bool { e.probes.Add(1); return e.running.Load() }
func (e *pokeEngine) Start(context.Context) error  { e.starts.Add(1); e.running.Store(true); return nil }
func (e *pokeEngine) Stop(context.Context) error   { e.running.Store(false); return nil }

// The point of #398: the supervisor must notice `skrog start` in well under
// the health interval, because up to a full interval of a 6.6-9.4 s start was
// being spent waiting for a tick that did nothing.
func TestSupervisorReactsToStartFasterThanTheHealthInterval(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesired(dir, DesiredStopped); err != nil {
		t.Fatal(err)
	}
	eng := &pokeEngine{}
	s := &Supervisor{
		Engine: eng,
		// A health interval long enough that reacting on it would fail this
		// test outright: any pass has to come from the poke path.
		Config: Config{StateDir: dir, Interval: 30 * time.Second},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// Let the initial reconcile settle with the engine deliberately stopped.
	time.Sleep(200 * time.Millisecond)
	if got := eng.starts.Load(); got != 0 {
		t.Fatalf("engine started %d times while desired=stopped", got)
	}

	// What `skrog start` does.
	if err := WriteDesired(dir, DesiredRunning); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if eng.starts.Load() > 0 {
			return // reacted well inside the 30s health interval
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the supervisor never noticed desired=running; it is still waiting on the health tick")
}

// `skrog start` on an IDLE engine leaves desired at "running" and deletes the
// engine-state marker instead. Watching only the desired file would miss it —
// and that is exactly the case that most needs waking.
func TestSupervisorReactsToTheIdleWakePoke(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesired(dir, DesiredRunning); err != nil {
		t.Fatal(err)
	}
	if err := WriteEngineState(dir, EngineIdle); err != nil {
		t.Fatal(err)
	}
	eng := &pokeEngine{}
	s := &Supervisor{Engine: eng, Config: Config{StateDir: dir, Interval: 30 * time.Second}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	time.Sleep(200 * time.Millisecond)
	if got := eng.starts.Load(); got != 0 {
		t.Fatalf("an idle-stopped engine was started %d times without being asked", got)
	}

	// The poke: remove the idle marker, desired unchanged throughout.
	if err := WriteEngineState(dir, EngineActive); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if eng.starts.Load() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("removing the idle marker did not wake the engine inside the health interval")
}

// The fast loop must not turn into the health loop. On a machine where nothing
// changes, the engine is probed at the health interval and no more.
func TestPokeLoopDoesNotProbeTheEngine(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesired(dir, DesiredRunning); err != nil {
		t.Fatal(err)
	}
	eng := &pokeEngine{}
	eng.running.Store(true)
	s := &Supervisor{Engine: eng, Config: Config{StateDir: dir, Interval: 30 * time.Second}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// Long enough for ~8 poke intervals with a 30s health interval, so the
	// only probe that may have happened is the initial reconcile.
	time.Sleep(2 * time.Second)

	if got := eng.probes.Load(); got > 1 {
		t.Errorf("engine probed %d times in 2s with nothing changing; the poke loop is "+
			"running the health check (that is %d wsl execs a user did not ask for)", got, got)
	}
}
