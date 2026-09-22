package supervise_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/supervise"
)

// fakeEngine is a scriptable engine with call counting.
type fakeEngine struct {
	mu       sync.Mutex
	running  bool
	startErr error
	// probeErr makes the health probe fail rather than answer -- the "cannot
	// tell" case the reconciler must not read as "down" (#437).
	probeErr error
	// probeEntered is signalled when Running starts, and probeGate blocks it
	// there -- together they let a test hold a probe in flight and check what
	// else can still make progress (#437).
	probeEntered chan struct{}
	probeGate    chan struct{}
	starts       int
	stops        int
	probes       int
}

func (f *fakeEngine) Running(context.Context) (bool, error) {
	f.mu.Lock()
	f.probes++
	entered, gate, running, perr := f.probeEntered, f.probeGate, f.running, f.probeErr
	f.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	if perr != nil {
		return false, perr
	}
	return running, nil
}

func (f *fakeEngine) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	return nil
}

func (f *fakeEngine) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	f.running = false
	return nil
}

func (f *fakeEngine) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

func (f *fakeEngine) setRunning(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = v
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newSup(t *testing.T, e supervise.Engine, interval time.Duration) (*supervise.Supervisor, string) {
	t.Helper()
	dir := t.TempDir()
	return &supervise.Supervisor{
		Engine: e,
		Config: supervise.Config{StateDir: dir, Interval: interval},
		Log:    quiet(),
	}, dir
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestStartsDownEngine(t *testing.T) {
	e := &fakeEngine{}
	sup, _ := newSup(t, e, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	waitFor(t, 3*time.Second, func() bool { s, _ := e.counts(); return s >= 1 && engineUp(e) },
		"engine was never started")
}

func TestRestartsAfterCrash(t *testing.T) {
	// The wsl --shutdown / crash / resume shape: engine was healthy, drops,
	// must come back without any user action (PLAN §05 v0.2 exit criteria).
	e := &fakeEngine{running: true}
	sup, _ := newSup(t, e, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	time.Sleep(100 * time.Millisecond) // a few healthy ticks
	e.setRunning(false)                // crash

	waitFor(t, 3*time.Second, func() bool { return engineUp(e) },
		"engine was not restarted after a crash")
}

func TestHonorsDesiredStopped(t *testing.T) {
	// `skrog stop` must mean *stays* stopped: without honoring recorded
	// intent, the health loop would restart the engine the user just stopped.
	e := &fakeEngine{running: true}
	sup, dir := newSup(t, e, 20*time.Millisecond)
	if err := supervise.WriteDesired(dir, supervise.DesiredStopped); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	waitFor(t, 3*time.Second, func() bool { return !engineUp(e) },
		"engine was not stopped despite desired=stopped")

	// And it must STAY stopped across many ticks.
	time.Sleep(200 * time.Millisecond)
	if starts, _ := e.counts(); starts != 0 {
		t.Errorf("engine was started %d times while desired=stopped", starts)
	}
}

func TestStopThenStartRoundTrip(t *testing.T) {
	e := &fakeEngine{running: true}
	sup, dir := newSup(t, e, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	if err := supervise.WriteDesired(dir, supervise.DesiredStopped); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return !engineUp(e) }, "did not stop")

	if err := supervise.WriteDesired(dir, supervise.DesiredRunning); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return engineUp(e) }, "did not start again")
}

func TestBackoffLimitsStartAttempts(t *testing.T) {
	// A crash loop must not hammer WSL: with starts failing continuously,
	// attempts should be spaced out, not one per tick.
	e := &fakeEngine{startErr: errors.New("engine keeps dying")}
	sup, _ := newSup(t, e, 10*time.Millisecond)
	sup.Config.BackoffMax = time.Hour // make the cap irrelevant here

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	sup.Run(ctx)

	starts, _ := e.counts()
	// 500ms of 10ms ticks is ~50 opportunities; with 2s-and-up backoff only
	// the first attempt (plus at most one race) should have happened.
	if starts > 2 {
		t.Errorf("engine start attempted %d times in 500ms; backoff is not holding", starts)
	}
}

func TestBackoffResetsOnRecovery(t *testing.T) {
	e := &fakeEngine{startErr: errors.New("still broken")}
	sup, _ := newSup(t, e, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	waitFor(t, time.Second, func() bool { s, _ := e.counts(); return s >= 1 },
		"no start attempt")

	// Repair the engine out of band (as a human or WSL itself might); once the
	// loop sees it healthy, backoff state must clear so the NEXT failure is
	// retried promptly rather than inheriting the old penalty.
	e.mu.Lock()
	e.startErr = nil
	e.running = true
	e.mu.Unlock()

	time.Sleep(100 * time.Millisecond) // healthy ticks observe it

	e.setRunning(false) // fresh crash
	waitFor(t, 3*time.Second, func() bool { return engineUp(e) },
		"fresh crash after recovery was not repaired promptly")
}

func TestDesiredStateDefaultsToRunning(t *testing.T) {
	if got := supervise.ReadDesired(t.TempDir()); got != supervise.DesiredRunning {
		t.Errorf("default desired = %q, want running", got)
	}
}

func TestDesiredStateRoundTrips(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []supervise.Desired{supervise.DesiredStopped, supervise.DesiredRunning} {
		if err := supervise.WriteDesired(dir, d); err != nil {
			t.Fatal(err)
		}
		if got := supervise.ReadDesired(dir); got != d {
			t.Errorf("read %q after writing %q", got, d)
		}
	}
}

func TestGarbageDesiredStateReadsAsRunning(t *testing.T) {
	// A corrupted file must fail toward the default, not toward stopped:
	// silently keeping the engine down would read as "Skrog is broken".
	dir := t.TempDir()
	if err := supervise.WriteDesired(dir, "garbage-value"); err != nil {
		t.Fatal(err)
	}
	if got := supervise.ReadDesired(dir); got != supervise.DesiredRunning {
		t.Errorf("garbage state read as %q, want running", got)
	}
}

// A probe that FAILS must not be read as a stopped engine (#437).
//
// Before Running grew an error, both real implementations collapsed a failed
// probe into false — and the reconciler's response to false is Engine.Start.
// So a wedged wslservice, a WSL update mid-flight, or any transient probe
// failure provoked a start of an engine that was very likely running.
//
// This is also why the bounded probe could not be added first: bounding a
// two-valued Running converts "slow" into "down", which is the same bug with
// a timer attached.
func TestProbeFailureIsNotTreatedAsAStoppedEngine(t *testing.T) {
	dir := t.TempDir()
	if err := supervise.WriteDesired(dir, supervise.DesiredRunning); err != nil {
		t.Fatal(err)
	}
	e := &fakeEngine{running: true, probeErr: errors.New("wslservice is not answering")}
	s := &supervise.Supervisor{
		Engine: e,
		Config: supervise.Config{StateDir: dir, Interval: 10 * time.Millisecond},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	e.mu.Lock()
	probes, starts, stops := e.probes, e.starts, e.stops
	e.mu.Unlock()

	if probes == 0 {
		t.Fatal("the engine was never probed; the test proves nothing")
	}
	if starts != 0 {
		t.Errorf("Start called %d time(s) after a FAILED probe: a probe that could not "+
			"answer was read as a stopped engine", starts)
	}
	if stops != 0 {
		t.Errorf("Stop called %d time(s) on an unknown reading", stops)
	}
}

// ...and a probe that genuinely reports "down" must still start the engine, or
// the fix above would have bought safety by disabling the supervisor.
func TestDefiniteDownStillStartsTheEngine(t *testing.T) {
	dir := t.TempDir()
	if err := supervise.WriteDesired(dir, supervise.DesiredRunning); err != nil {
		t.Fatal(err)
	}
	e := &fakeEngine{running: false} // definite: (false, nil)
	s := &supervise.Supervisor{
		Engine: e,
		Config: supervise.Config{StateDir: dir, Interval: 10 * time.Millisecond},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	e.mu.Lock()
	starts := e.starts
	e.mu.Unlock()
	if starts == 0 {
		t.Error("a definite (false, nil) did not start the engine")
	}
}

// A probe in flight must not block Demand (#437).
//
// tick used to hold s.mu across Engine.Running with the supervisor's
// process-lifetime context. A wslservice that stopped answering therefore
// parked the reconciler INSIDE the lock, and Demand takes the same lock — so
// every docker connection through demandDialer hung rather than failing,
// LifecycleSnapshot froze so the tray could not show the supervisor was stuck,
// and the poke loop never ran again.
//
// The probe now runs outside the lock. This holds one in flight and checks
// that Demand still gets the mutex.
func TestDemandIsNotBlockedByAProbeInFlight(t *testing.T) {
	dir := t.TempDir()
	if err := supervise.WriteDesired(dir, supervise.DesiredRunning); err != nil {
		t.Fatal(err)
	}
	e := &fakeEngine{
		running:      true,
		probeEntered: make(chan struct{}, 1),
		probeGate:    make(chan struct{}),
	}
	s := &supervise.Supervisor{
		Engine: e,
		Config: supervise.Config{StateDir: dir, Interval: 10 * time.Millisecond},
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); s.Run(ctx) }()

	// Wait for a probe to be in flight, then leave it wedged.
	select {
	case <-e.probeEntered:
	case <-time.After(5 * time.Second):
		cancel()
		<-runDone
		t.Fatal("no probe started; the test proves nothing")
	}

	// Demand must not wait behind it. The engine is not idle, so Demand does
	// nothing but take the lock and return — which is exactly the assertion:
	// it could take the lock at all.
	demanded := make(chan error, 1)
	go func() { demanded <- s.Demand(context.Background()) }()

	select {
	case <-demanded:
	case <-time.After(5 * time.Second):
		close(e.probeGate)
		cancel()
		<-runDone
		t.Fatal("Demand blocked behind a probe in flight: the reconciler is holding " +
			"s.mu across Engine.Running, so a wedged wslservice hangs every docker command (#437)")
	}

	close(e.probeGate)
	cancel()
	<-runDone
}

// The reconciler must do NOTHING while a maintenance hold is in place (#486).
//
// This is the wiring, not the primitive: Hold/HoldActive are unit-tested next
// door, and a hold nothing consults is exactly the "correct helper nobody
// calls" defect this repo keeps finding.
//
// The setup is the one that bit: desired=stopped with the engine up, which is
// precisely when the reconciler terminates the distro -- and during an upgrade
// that distro has a `wsl --exec` inside it copying binaries.
func TestTickDoesNothingWhileHeld(t *testing.T) {
	e := &fakeEngine{running: true}
	s, dir := newSup(t, e, time.Hour)
	if err := supervise.WriteDesired(dir, supervise.DesiredStopped); err != nil {
		t.Fatal(err)
	}
	release, err := supervise.Hold(dir, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	s.TickForTest(context.Background())

	if got := stopsOf(e); got != 0 {
		t.Errorf("engine stopped %d time(s) under a maintenance hold; "+
			"that terminate is what kills the copy an upgrade is running", got)
	}
	// Not even probed: the probe boots the distro it asks about, and booting
	// the distro is half of what the holder is controlling.
	if got := probesOf(e); got != 0 {
		t.Errorf("engine probed %d time(s) under a maintenance hold", got)
	}

	// And it resumes once released, or the fix trades a race for a wedge.
	release()
	s.TickForTest(context.Background())
	if got := stopsOf(e); got == 0 {
		t.Error("engine was never stopped after the hold was released")
	}
}

// stopsOf and probesOf read the counters under the fake's own lock, so the
// hold test does not race the reconciler it just ticked.
func stopsOf(e *fakeEngine) int {
	_, stops := e.counts()
	return stops
}

func probesOf(e *fakeEngine) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.probes
}

// Shutdown waits for a prune in flight (#423). Nothing could before: Run
// returned on ctx.Done() while a `docker system prune -a` kept deleting, with
// the process about to exit underneath it.
func TestRunWaitsForAPruneInFlight(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})

	s, dir := prunableSup(t)
	s.Prune = func(context.Context, supervise.PrunePolicy) (uint64, error) {
		<-release
		close(finished)
		return 0, nil
	}
	duePrune(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	runReturned := make(chan struct{})
	go func() { s.Run(ctx); close(runReturned) }()

	waitFor(t, 5*time.Second, s.PruningForTest, "prune never started")
	cancel()

	select {
	case <-runReturned:
		t.Fatal("Run returned while a prune was still running")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	<-finished
	select {
	case <-runReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the prune finished")
	}
}

// ...but not forever. A wedged prune must not hold a logoff or a `restart
// --supervisor` open; it has its own timeout and is abandoned.
func TestRunAbandonsAWedgedPrune(t *testing.T) {
	wedged := make(chan struct{})
	s, dir := prunableSup(t)
	s.Prune = func(context.Context, supervise.PrunePolicy) (uint64, error) {
		<-wedged
		return 0, nil
	}
	duePrune(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	runReturned := make(chan struct{})
	go func() { s.Run(ctx); close(runReturned) }()
	waitFor(t, 5*time.Second, s.PruningForTest, "prune never started")
	cancel()

	select {
	case <-runReturned:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned; a wedged prune is holding shutdown open")
	}

	// Run abandoned it, which is the point — but the TEST must not. Release it
	// and wait, or the goroutine writes `last-prune` into t.TempDir() while
	// cleanup is removing it:
	//
	//	TempDir RemoveAll cleanup: ...\001: The directory is not empty.
	//
	// which is the exact failure #423 was filed from, reproduced here by a
	// test asserting the fix for it. The guard clears after the clock write, so
	// waiting on it waits for the write.
	close(wedged)
	waitFor(t, 10*time.Second, func() bool { return !s.PruningForTest() },
		"the abandoned prune never finished, so its state-dir writes race cleanup")
}

// A panic in the caller-supplied Prune must not take the supervisor with it: a
// failed prune is a full disk, a dead supervisor is a machine where docker
// stops working.
func TestAPanickingPruneDoesNotKillTheSupervisor(t *testing.T) {
	s, dir := prunableSup(t)
	s.Prune = func(context.Context, supervise.PrunePolicy) (uint64, error) {
		panic("a bad prune implementation")
	}
	duePrune(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runReturned := make(chan struct{})
	go func() { s.Run(ctx); close(runReturned) }()

	// The guard must clear too, or one bad sweep disables automatic pruning
	// until the next restart.
	waitFor(t, 5*time.Second, func() bool { return !s.PruningForTest() }, "the prune guard never cleared")

	select {
	case <-runReturned:
		t.Fatal("the supervisor exited because a prune panicked")
	case <-time.After(200 * time.Millisecond):
	}

	// Shut down inside the test rather than leaving Run to a deferred cancel:
	// a supervisor still ticking while t.TempDir() is removed writes into a
	// directory that is going away.
	cancel()
	select {
	case <-runReturned:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// An interrupted prune must not make the next supervisor immediately due, so
// the clock is written before the sweep as well as after (#423).
func TestTheClockIsRecordedBeforeTheSweep(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	s, dir := prunableSup(t)
	s.Prune = func(context.Context, supervise.PrunePolicy) (uint64, error) {
		close(started)
		<-release
		return 0, nil
	}
	old := time.Now().Add(-time.Hour)
	if err := supervise.WriteLastPrune(dir, old); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	<-started

	if got := supervise.ReadLastPrune(dir); !got.After(old) {
		t.Errorf("clock is still %s mid-sweep; an interrupted prune would re-fire at the next logon", got)
	}

	// Let the sweep finish and the supervisor stop before the state dir is
	// removed underneath them.
	close(release)
	cancel()
	waitFor(t, 10*time.Second, func() bool { return !s.PruningForTest() }, "the prune never finished")
}

// prunableSup is a supervisor with an engine up, nothing busy, and automatic
// pruning enabled on a hair trigger.
func prunableSup(t *testing.T) (*supervise.Supervisor, string) {
	t.Helper()
	s, dir := newSup(t, &fakeEngine{running: true}, time.Hour)
	s.Busy = func(context.Context) (bool, error) { return false, nil }
	s.PrunePolicy = func() supervise.PrunePolicy {
		return supervise.PrunePolicy{Every: time.Nanosecond, KeepSince: time.Hour}
	}
	return s, dir
}

// duePrune backdates the clock so the first tick prunes rather than scheduling.
func duePrune(t *testing.T, dir string) {
	t.Helper()
	if err := supervise.WriteLastPrune(dir, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
}
