package supervise_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/supervise"
)

// distroEngine is fakeEngine plus the distro's own state (#518), which is what
// lets the supervisor tell `wsl --shutdown` from a dockerd crash. The distro
// follows the engine unless a test says otherwise: Start boots it, Stop
// terminates it, and a test stops only dockerd with crashDaemon.
type distroEngine struct {
	*fakeEngine
	dmu      sync.Mutex
	distroUp bool
	probeErr error
	probes   int
}

func newDistroEngine(running bool) *distroEngine {
	return &distroEngine{fakeEngine: &fakeEngine{running: running}, distroUp: running}
}

func (d *distroEngine) DistroRunning(context.Context) (bool, error) {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	d.probes++
	return d.distroUp, d.probeErr
}

func (d *distroEngine) Start(ctx context.Context) error {
	err := d.fakeEngine.Start(ctx)
	if err == nil {
		d.setDistro(true)
	}
	return err
}

func (d *distroEngine) Stop(ctx context.Context) error {
	d.setDistro(false)
	return d.fakeEngine.Stop(ctx)
}

func (d *distroEngine) setDistro(v bool) {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	d.distroUp = v
}

// shutdownVM is `wsl --shutdown`: the daemon and its distro go together.
func (d *distroEngine) shutdownVM() {
	d.setRunning(false)
	d.setDistro(false)
}

// crashDaemon is dockerd dying while the distro keeps running.
func (d *distroEngine) crashDaemon() { d.setRunning(false) }

func tickN(s *supervise.Supervisor, n int) {
	for i := 0; i < n; i++ {
		s.TickForTest(context.Background())
	}
}

// The point of #518: `wsl --shutdown` sticks. The supervisor that watched the
// engine run does not start it again; it records it as idle, so `skrog status`
// says so and the next docker command wakes it.
func TestOutsideShutdownLeavesTheEngineDown(t *testing.T) {
	e := newDistroEngine(true)
	s, dir := newSup(t, e, time.Hour)

	s.TickForTest(context.Background()) // watches it run
	e.shutdownVM()
	tickN(s, 5)

	if starts, _ := e.counts(); starts != 0 {
		t.Fatalf("started %d times after wsl --shutdown, want 0", starts)
	}
	if got := supervise.ReadEngineState(dir); got != supervise.EngineIdle {
		t.Errorf("engine state = %q, want idle: status must not call it stopped", got)
	}
	if got := s.EngineStatus(); got != "idle" {
		t.Errorf("EngineStatus = %q, want idle", got)
	}

	// And the next docker command brings it back, through the path an idle
	// stop already uses.
	if err := s.Demand(context.Background()); err != nil {
		t.Fatalf("Demand: %v", err)
	}
	if starts, _ := e.counts(); starts != 1 {
		t.Errorf("Demand started the engine %d times, want 1", starts)
	}
}

// A dead daemon in a live distro is a crash, and it is restarted as before.
func TestDaemonCrashWithTheDistroUpIsRestarted(t *testing.T) {
	e := newDistroEngine(true)
	s, _ := newSup(t, e, time.Hour)

	s.TickForTest(context.Background())
	e.crashDaemon()
	s.TickForTest(context.Background())

	if starts, _ := e.counts(); starts != 1 {
		t.Errorf("started %d times after a crash, want 1", starts)
	}
}

// A supervisor that never saw the engine run has no change to judge: after a
// reboot, logon autostart, `restart --supervisor` or the watchdog, a stopped
// distro is simply an engine to start.
func TestFreshSupervisorStartsAStoppedDistro(t *testing.T) {
	e := newDistroEngine(false)
	s, _ := newSup(t, e, time.Hour)

	s.TickForTest(context.Background())
	if starts, _ := e.counts(); starts != 1 {
		t.Errorf("a fresh supervisor started a stopped distro %d times, want 1", starts)
	}
}

// Stop then start -- `skrog restart`, a daemon.json bounce, a snapshot
// restore under a supervisor -- ends with the distro stopped and desired
// running. That is a start request, not an outside stop.
func TestStartAfterARequestedStopIsAStart(t *testing.T) {
	e := newDistroEngine(true)
	s, dir := newSup(t, e, time.Hour)

	s.TickForTest(context.Background())
	if err := supervise.WriteDesired(dir, supervise.DesiredStopped); err != nil {
		t.Fatal(err)
	}
	s.TickForTest(context.Background()) // the supervisor stops it
	if _, stops := e.counts(); stops != 1 {
		t.Fatalf("stopped %d times on desired=stopped, want 1", stops)
	}
	if err := supervise.WriteDesired(dir, supervise.DesiredRunning); err != nil {
		t.Fatal(err)
	}
	s.TickForTest(context.Background())
	if starts, _ := e.counts(); starts != 1 {
		t.Errorf("started %d times after stop-then-start, want 1", starts)
	}
}

// Whatever a maintenance hold's owner does to the distro is its business, so a
// down engine after the hold is restarted -- which is what keeps `snapshot
// save`, if `wsl --export` really terminates the distro, coming back as it did.
func TestAStopDuringAHoldIsNotAnOutsideStop(t *testing.T) {
	e := newDistroEngine(true)
	s, dir := newSup(t, e, time.Hour)

	s.TickForTest(context.Background())
	release, err := supervise.Hold(dir, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	s.TickForTest(context.Background()) // held
	e.shutdownVM()
	release()
	s.TickForTest(context.Background())

	if starts, _ := e.counts(); starts != 1 {
		t.Errorf("started %d times after a held stop, want 1", starts)
	}
}

// "Cannot tell" is a crash, not a choice: guessing "stopped on purpose" wrongly
// would leave a crashed engine down.
func TestUnknownDistroStateRestarts(t *testing.T) {
	e := newDistroEngine(true)
	s, _ := newSup(t, e, time.Hour)

	s.TickForTest(context.Background())
	e.shutdownVM()
	e.dmu.Lock()
	e.probeErr = errors.New("wslservice not answering")
	e.dmu.Unlock()
	s.TickForTest(context.Background())

	if starts, _ := e.counts(); starts != 1 {
		t.Errorf("started %d times with the distro state unknown, want 1", starts)
	}
}

// The verdict is latched at the first down observation. A crash whose restart
// is backing off can outlive the distro -- WSL reaps a distro nothing keeps
// alive -- and that later stop must not be read as someone's `wsl --shutdown`.
func TestACrashInBackoffIsNotLaterReadAsAShutdown(t *testing.T) {
	e := newDistroEngine(true)
	s, dir := newSup(t, e, time.Hour)

	s.TickForTest(context.Background())
	e.fakeEngine.mu.Lock()
	e.fakeEngine.startErr = errors.New("dockerd would not start")
	e.fakeEngine.mu.Unlock()
	e.crashDaemon()
	s.TickForTest(context.Background()) // crash: a start attempt, which fails
	if starts, _ := e.counts(); starts != 1 {
		t.Fatalf("started %d times after the crash, want 1", starts)
	}

	e.setDistro(false) // WSL reaps the distro while we back off
	time.Sleep(2100 * time.Millisecond)
	s.TickForTest(context.Background())

	if starts, _ := e.counts(); starts != 2 {
		t.Errorf("started %d times, want 2: a crash in backoff must keep being retried", starts)
	}
	if got := supervise.ReadEngineState(dir); got == supervise.EngineIdle {
		t.Error("a crash was later recorded as idle")
	}
}

// An engine without the distro probe keeps the pre-#518 behaviour exactly.
func TestEngineWithoutDistroProbeStillRestarts(t *testing.T) {
	e := &fakeEngine{running: true}
	s, _ := newSup(t, e, time.Hour)

	s.TickForTest(context.Background())
	e.setRunning(false)
	s.TickForTest(context.Background())

	if starts, _ := e.counts(); starts != 1 {
		t.Errorf("started %d times, want 1", starts)
	}
}
