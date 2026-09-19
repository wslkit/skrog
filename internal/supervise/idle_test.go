package supervise_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/supervise"
)

// fakeActivity is a scriptable supervise.Activity.
type fakeActivity struct {
	mu    sync.Mutex
	conns int
	last  time.Time
}

func (f *fakeActivity) ActiveConns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func (f *fakeActivity) LastActivity() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeActivity) set(conns int, last time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns, f.last = conns, last
}

// idleSup builds a supervisor primed for idle testing: engine up, desired
// running, idle timeout tiny, everything quiet since long ago.
func idleSup(t *testing.T) (*supervise.Supervisor, *fakeEngine, *fakeActivity, string) {
	t.Helper()
	e := &fakeEngine{running: true}
	act := &fakeActivity{last: time.Now().Add(-time.Hour)}
	s, dir := newSup(t, e, time.Hour)
	s.Activity = act
	s.IdleTimeout = func() time.Duration { return 50 * time.Millisecond }
	s.Busy = func(context.Context) (bool, error) { return false, nil }
	return s, e, act, dir
}

// runTicks pumps the reconciler enough times, with a pause so the idle clock
// (anchored at upSince on the first healthy tick) can age past the timeout.
func runTicks(s *supervise.Supervisor, n int, pause time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < n; i++ {
		s.TickForTest(ctx)
		time.Sleep(pause)
	}
}

func TestIdleStopsQuietEngine(t *testing.T) {
	s, e, _, dir := idleSup(t)

	runTicks(s, 3, 60*time.Millisecond)

	if _, stops := e.counts(); stops != 1 {
		t.Fatalf("engine stopped %d times, want 1 idle stop", stops)
	}
	if supervise.ReadEngineState(dir) != supervise.EngineIdle {
		t.Error("idle stop did not record the idle state")
	}

	// And it STAYS down: the reconciler must not treat its own idle stop as a
	// crash to repair.
	runTicks(s, 3, 10*time.Millisecond)
	if starts, _ := e.counts(); starts != 0 {
		t.Errorf("idle-stopped engine restarted %d times by the loop", starts)
	}
}

func TestIdleVetoedByActiveConns(t *testing.T) {
	s, e, act, _ := idleSup(t)
	act.set(1, time.Now().Add(-time.Hour)) // a long-lived quiet connection (docker events)

	runTicks(s, 3, 60*time.Millisecond)
	if _, stops := e.counts(); stops != 0 {
		t.Error("engine idled while a client connection was open")
	}
}

func TestIdleVetoedByRunningContainers(t *testing.T) {
	s, e, _, _ := idleSup(t)
	s.Busy = func(context.Context) (bool, error) { return true, nil }

	runTicks(s, 3, 60*time.Millisecond)
	if _, stops := e.counts(); stops != 0 {
		t.Error("engine idled with containers running; that kills them")
	}
}

func TestIdleVetoedWhenBusyUnknown(t *testing.T) {
	s, e, _, _ := idleSup(t)
	s.Busy = func(context.Context) (bool, error) { return false, errors.New("probe down") }

	runTicks(s, 3, 60*time.Millisecond)
	if _, stops := e.counts(); stops != 0 {
		t.Error("engine idled although the container probe failed; a stop needs a definite no")
	}

	s.Busy = nil
	runTicks(s, 3, 60*time.Millisecond)
	if _, stops := e.counts(); stops != 0 {
		t.Error("engine idled with no container probe wired at all")
	}
}

func TestIdleRespectsRecentTraffic(t *testing.T) {
	s, e, act, _ := idleSup(t)
	act.set(0, time.Now()) // a connection just closed

	runTicks(s, 2, 5*time.Millisecond)
	if _, stops := e.counts(); stops != 0 {
		t.Error("engine idled before the timeout elapsed after the last connection")
	}
}

func TestDemandColdStartsIdleEngine(t *testing.T) {
	s, e, _, dir := idleSup(t)
	runTicks(s, 3, 60*time.Millisecond) // idle it
	if engineUp(e) {
		t.Fatal("precondition: engine should be idle-stopped")
	}

	if err := s.Demand(context.Background()); err != nil {
		t.Fatalf("Demand: %v", err)
	}
	if !engineUp(e) {
		t.Error("Demand did not start the engine")
	}
	if supervise.ReadEngineState(dir) != supervise.EngineActive {
		t.Error("Demand left the idle marker behind")
	}

	// Demand on a non-idle engine is a no-op, not a redundant start.
	starts, _ := e.counts()
	s.Demand(context.Background())
	if s2, _ := e.counts(); s2 != starts {
		t.Error("Demand started an engine that was not idle")
	}
}

func TestPokeWakesIdleEngine(t *testing.T) {
	// `skrog start` deletes the engine-state file; the next tick must treat
	// that as demand and restart.
	s, e, _, dir := idleSup(t)
	runTicks(s, 3, 60*time.Millisecond) // idle it

	if err := supervise.WriteEngineState(dir, supervise.EngineActive); err != nil {
		t.Fatal(err)
	}
	runTicks(s, 1, 0)
	if !engineUp(e) {
		t.Error("deleting the idle marker did not wake the engine")
	}
}

func TestSupervisorRestartAdoptsIdleState(t *testing.T) {
	// A new supervisor process finding engine-state=idle must not "repair"
	// the down engine.
	e := &fakeEngine{running: false}
	s, dir := newSup(t, e, time.Hour)
	if err := supervise.WriteEngineState(dir, supervise.EngineIdle); err != nil {
		t.Fatal(err)
	}

	runTicks(s, 3, 5*time.Millisecond)
	if starts, _ := e.counts(); starts != 0 {
		t.Errorf("restarted supervisor started an idle-stopped engine %d times", starts)
	}
}

func TestManualStartClearsStaleIdleMarker(t *testing.T) {
	// Engine running + file says idle = someone started it around the
	// supervisor; the marker is stale and must go.
	e := &fakeEngine{running: true}
	s, dir := newSup(t, e, time.Hour)
	if err := supervise.WriteEngineState(dir, supervise.EngineIdle); err != nil {
		t.Fatal(err)
	}

	runTicks(s, 1, 0)
	if supervise.ReadEngineState(dir) != supervise.EngineActive {
		t.Error("stale idle marker survived a healthy tick")
	}
}

func TestExplicitStopClearsIdle(t *testing.T) {
	s, e, _, dir := idleSup(t)
	runTicks(s, 3, 60*time.Millisecond) // idle it
	e.setRunning(true)                  // engine got started somehow

	if err := supervise.WriteDesired(dir, supervise.DesiredStopped); err != nil {
		t.Fatal(err)
	}
	runTicks(s, 1, 0)
	if supervise.ReadEngineState(dir) != supervise.EngineActive {
		t.Error("explicit stop left the idle marker; status would lie")
	}
}

func TestDemandRefusesWhenDesiredStopped(t *testing.T) {
	// #80: an explicitly stopped engine is never resurrected by traffic. The
	// nasty path is idle-stop THEN `skrog stop` while the engine is already
	// down: the stop clears the file but not this process's memory.
	s, e, _, dir := idleSup(t)
	runTicks(s, 3, 60*time.Millisecond) // idle-stop it
	if engineUp(e) {
		t.Fatal("precondition: engine should be idle-stopped")
	}

	// What runStop does when the engine is already down: desired=stopped,
	// marker cleared. The in-memory idleStopped flag is the trap.
	if err := supervise.WriteDesired(dir, supervise.DesiredStopped); err != nil {
		t.Fatal(err)
	}
	if err := supervise.WriteEngineState(dir, supervise.EngineActive); err != nil {
		t.Fatal(err)
	}

	if err := s.Demand(context.Background()); err == nil {
		t.Fatal("Demand woke an explicitly stopped engine")
	}
	if engineUp(e) {
		t.Fatal("engine started despite desired=stopped")
	}
	if starts, _ := e.counts(); starts != 0 {
		t.Errorf("engine start attempted %d times, want 0", starts)
	}

	// And ticks agree: nothing restarts it, nothing flaps.
	runTicks(s, 3, 10*time.Millisecond)
	if starts, _ := e.counts(); starts != 0 {
		t.Errorf("tick started a stopped engine %d times", starts)
	}

	// `skrog start` (desired=running, marker poked) revives it normally.
	if err := supervise.WriteDesired(dir, supervise.DesiredRunning); err != nil {
		t.Fatal(err)
	}
	runTicks(s, 1, 0)
	if !engineUp(e) {
		t.Error("engine did not start after skrog start's desired=running")
	}
}

func TestTickClearsIdleFlagOnStoppedAndDown(t *testing.T) {
	s, e, _, dir := idleSup(t)
	runTicks(s, 3, 60*time.Millisecond) // idle-stop

	supervise.WriteDesired(dir, supervise.DesiredStopped)
	supervise.WriteEngineState(dir, supervise.EngineActive)
	runTicks(s, 1, 0) // the stopped-and-down branch clears the flag

	// Back to running: with the flag cleared, the reconciler itself (not just
	// Demand) must bring the engine up.
	supervise.WriteDesired(dir, supervise.DesiredRunning)
	runTicks(s, 1, 0)
	if !engineUp(e) {
		t.Error("stale idleStopped flag still suppressing the reconciler")
	}
}

func TestDemandBacksOffAfterFailedColdStart(t *testing.T) {
	// #89: a failing engine must not be hammered by every queued connection.
	s, e, _, _ := idleSup(t)
	runTicks(s, 3, 60*time.Millisecond) // idle-stop
	e.mu.Lock()
	e.startErr = errors.New("VHDX locked")
	e.mu.Unlock()

	if err := s.Demand(context.Background()); err == nil {
		t.Fatal("Demand succeeded with a failing engine")
	}
	starts, _ := e.counts()
	if starts != 1 {
		t.Fatalf("first Demand made %d start attempts, want 1", starts)
	}

	// Within the backoff window, further Demands fail fast without another
	// start attempt.
	for i := 0; i < 4; i++ {
		if err := s.Demand(context.Background()); err == nil {
			t.Fatal("Demand succeeded during backoff")
		}
	}
	if s2, _ := e.counts(); s2 != starts {
		t.Errorf("Demands during backoff made %d extra start attempts", s2-starts)
	}
}

func TestIdleVetoedByBusySignal(t *testing.T) {
	// #72: the Busy func now folds in "an integrated distro is using the
	// engine socket". A true from Busy (for any reason) must veto the idle
	// stop, so work over the /mnt/wsl share is never killed mid-flight.
	s, e, _, _ := idleSup(t)
	s.Busy = func(context.Context) (bool, error) { return true, nil }

	runTicks(s, 3, 60*time.Millisecond)
	if _, stops := e.counts(); stops != 0 {
		t.Error("engine idled while Busy reported in-flight work (shared socket / containers)")
	}
}

// engineUp is the boolean half of Engine.Running, for assertions that only
// care whether the engine ended up running. The error half is exercised
// directly in the probe tests (#437).
func engineUp(e supervise.Engine) bool {
	up, _ := e.Running(context.Background())
	return up
}
