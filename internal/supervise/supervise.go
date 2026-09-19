package supervise

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Engine is what the supervisor drives. The seam matches what the provisioner
// and wsl packages already offer, and lets the loop be tested without WSL.
type Engine interface {
	// Running reports whether the engine socket answers.
	//
	// The error is the third answer, and it is load-bearing (#437). Before it
	// existed, both implementations collapsed a failed probe into false — and
	// the reconciler's next move after false is to START the engine. So a
	// wedged wslservice, a WSL update mid-flight, or any transient probe
	// failure read as "the engine is down" and provoked a start of an engine
	// that was probably running fine.
	//
	// Return (false, nil) only for "definitely not running". Anything you
	// could not determine is an error, and the reconciler will do nothing at
	// all that tick rather than guess.
	Running(ctx context.Context) (bool, error)
	// Start brings the engine up (idempotent; provisioner.StartEngine).
	Start(ctx context.Context) error
	// Stop terminates the engine's own distro — and only that distro. Stopping
	// anything wider (another distro, wsl --shutdown) is off the table by
	// design: Skrog shares the machine (PLAN §02, the Docker Desktop incident
	// on #35).
	Stop(ctx context.Context) error
}

// Config tunes the loop. Zero values get defaults.
type Config struct {
	// StateDir is where the desired state is read from.
	StateDir string
	// Interval between health checks. Also bounds how quickly a CLI-written
	// desired-state change is noticed. Default 3s.
	Interval time.Duration
	// BackoffMax caps the restart backoff. Default 60s.
	BackoffMax time.Duration
}

func (c Config) interval() time.Duration {
	if c.Interval <= 0 {
		return 3 * time.Second
	}
	return c.Interval
}

func (c Config) backoffMax() time.Duration {
	if c.BackoffMax <= 0 {
		return 60 * time.Second
	}
	return c.BackoffMax
}

// Activity reports bridge traffic, for idle detection. pipeproxy.Server
// implements it.
type Activity interface {
	// ActiveConns is how many client connections are open right now.
	ActiveConns() int
	// LastActivity is when a connection last opened or closed; the zero time
	// if there has been none.
	LastActivity() time.Time
}

// Supervisor reconciles the engine with the desired state.
type Supervisor struct {
	Engine Engine
	Config Config
	Log    *slog.Logger

	// Activity feeds idle detection (#41). Nil disables idle stops.
	Activity Activity

	// IdleTimeout is read every tick, so `skrog config set idle-timeout`
	// takes effect without a restart. Nil or a zero return disables idle
	// stops.
	IdleTimeout func() time.Duration

	// Busy reports whether the engine has running containers. Idle stops
	// require a definite "no": nil, an error, or true all veto the stop,
	// because stopping the engine kills whatever runs in it.
	Busy func(ctx context.Context) (bool, error)

	// Hook fires a lifecycle event (#70). It must return promptly — the caller
	// runs the user's script time-bounded and off the reconciler — so it never
	// blocks a tick or the mutex. Nil disables hooks.
	Hook func(event string)

	// PrunePolicy is the automatic-prune settings, read every tick so
	// `skrog config set prune.every` takes effect without a restart. Nil, or
	// a zero Every, disables automatic pruning — which is the default.
	PrunePolicy func() PrunePolicy

	// Prune reclaims disk and reports what it freed. Nil disables automatic
	// pruning regardless of policy, so a caller that has not wired an engine
	// client cannot accidentally schedule deletions it cannot perform.
	Prune func(ctx context.Context, p PrunePolicy) (reclaimed uint64, err error)

	// pruning guards against a second prune starting while one is still
	// running. A prune runs OFF the reconciler (it can take minutes, and the
	// tick holds mu across a cold start), so the tick needs a way to see that
	// one is already in flight without taking a lock the prune also wants.
	pruning atomic.Bool

	// lastUp is the engine health the reconciler saw on its most recent tick.
	// Atomic rather than guarded by mu on purpose: readers must never block on
	// the reconciler, which holds mu across a slow engine start (#192).
	lastUp atomic.Bool

	// mu serializes tick and Demand: a cold start must not race the
	// reconciler's own view of why the engine is down.
	mu sync.Mutex
	// startGen counts engine starts. tick probes OUTSIDE mu (#437), so a
	// Demand can cold-start the engine in the window between the probe and
	// the decision; the counter is how tick notices its reading went stale
	// and defers to the next one. Guarded by mu.
	startGen uint64
	// idleStopped mirrors the engine-state file; kept in memory so the tick
	// can tell "down because I idled it" from "down unexpectedly" without
	// re-reading, and re-adopted from the file after a supervisor restart.
	idleStopped bool
	// lifecycle and startedAt feed `skrog status --stats` (#179): counters the
	// CLI cannot derive, because only this process sees the transitions.
	lifecycle Lifecycle
	startedAt time.Time
	// upSince anchors the idle clock: a freshly started engine with no
	// traffic yet must age past the timeout before it can be idled.
	upSince time.Time

	// lastVeto dedupes the "idle stop deferred" log: one line per reason
	// streak, so a user asking "why is my engine not idling?" gets an answer
	// without the log drowning in per-tick repeats.
	lastVeto string

	// failures counts consecutive start failures, for backoff.
	failures int
	// nextTry is the earliest moment another start attempt is allowed.
	nextTry time.Time
}

// Lifecycle event names passed to Hook and used as the "hook.<event>" config
// suffix. Kept as plain strings so cmd and config agree without importing this
// package's constants across the boundary.
const (
	HookPostStart  = "post-start"
	HookPreStop    = "pre-stop"
	HookOnIdleStop = "on-idle-stop"
	HookOnWake     = "on-wake"
)

// fireHook dispatches a lifecycle event. Hook is expected to return promptly
// (it runs the actual script off-thread and time-bounded), so this is safe to
// call while holding s.mu.
func (s *Supervisor) fireHook(event string) {
	if s.Hook != nil {
		s.Hook(event)
	}
}

// veto records why an idle stop did not happen, logging only when the reason
// changes. Caller holds s.mu.
func (s *Supervisor) veto(reason string, kv ...any) {
	if s.lastVeto == reason {
		return
	}
	s.lastVeto = reason
	s.log().Info("idle stop deferred", append([]any{"reason", reason}, kv...)...)
}

func (s *Supervisor) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Run reconciles until the context ends. It never returns an error for the
// engine being down — that is a condition to repair, not a reason to exit —
// and it survives sleep/resume for free: a resumed machine simply fails the
// next health check and gets repaired like any other crash.
func (s *Supervisor) Run(ctx context.Context) {
	s.mu.Lock()
	s.startedAt = time.Now()
	s.mu.Unlock()

	t := time.NewTicker(s.Config.interval())
	defer t.Stop()

	// A second, much faster ticker that only looks at the two state FILES
	// (#398).
	//
	// tick() is expensive: Engine.Running is a wsl exec, ~165 ms on a warm
	// distro and seconds on a cold one, so the health interval cannot simply
	// be shortened. But the cost of noticing `skrog start` is an os.ReadFile
	// of a few bytes, and paying the health interval to learn about it meant
	// the user waited up to a full interval before anything began — measured
	// as ~3 s of a 6.6-9.4 s start, spent doing nothing at all.
	//
	// So: poll the files often, reconcile only when they have CHANGED. A
	// steady machine does the same amount of engine probing as before.
	poke := time.NewTicker(pokeInterval)
	defer poke.Stop()

	// Reconcile immediately rather than waiting out the first tick: the
	// supervisor usually starts at logon, and the user is waiting.
	s.tick(ctx)
	seen := s.readIntent()
	for {
		select {
		case <-ctx.Done():
			s.log().Info("supervisor stopping", "reason", ctx.Err())
			return
		case <-t.C:
			s.tick(ctx)
			seen = s.readIntent()
		case <-poke.C:
			// Only on a change, or this becomes the health loop it was
			// written to avoid becoming.
			if now := s.readIntent(); now != seen {
				seen = now
				s.tick(ctx)
			}
		}
	}
}

// pokeInterval is how often the supervisor checks whether the user has asked
// for something. Not configurable: it is two small file reads, and a knob here
// would only ever be turned the wrong way.
const pokeInterval = 250 * time.Millisecond

// intent is what the CLI has asked for, as the two files record it.
//
// Both matter, and watching only the first was the bug worth avoiding: `skrog
// start` on an IDLE-stopped engine leaves desired at "running" (idle never
// changed it) and instead deletes the engine-state marker. Watching desired
// alone would therefore see no change and make exactly the case that most
// needs waking wait out the full interval.
type intent struct {
	desired Desired
	engine  EngineState
}

func (s *Supervisor) readIntent() intent {
	return intent{
		desired: ReadDesired(s.Config.StateDir),
		engine:  ReadEngineState(s.Config.StateDir),
	}
}

// probeTimeout bounds one health probe. Generous: a cold `wsl.exe --list` on a
// loaded machine is not fast, and this is a ceiling for a probe that has
// stopped answering, not a latency target.
const probeTimeout = 60 * time.Second

func (s *Supervisor) tick(ctx context.Context) {
	// The probe runs OUTSIDE mu, and bounded (#437).
	//
	// It used to run under the lock with the supervisor's process-lifetime
	// context, so a wslservice that stopped answering parked the reconciler
	// forever WHILE HOLDING mu: every Demand() blocked, so every docker
	// command hung instead of failing, LifecycleSnapshot froze so the tray
	// could not even show the supervisor was stuck, and the poke loop never
	// ran again. The COM funnel made it worse, since every COM caller now
	// queues behind one thread.
	s.mu.Lock()
	gen := s.startGen
	s.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	up, probeErr := s.Engine.Running(probeCtx)
	cancel()

	s.mu.Lock()
	defer s.mu.Unlock()

	if probeErr != nil {
		// "Cannot tell" is not "down". Doing nothing leaves lastUp at the
		// last reading we trust, and the next tick asks again — which is
		// right, because the alternative is starting an engine that is
		// probably running.
		s.log().Warn("engine health probe failed; skipping this tick", "error", probeErr)
		return
	}
	if s.startGen != gen {
		// A Demand cold-started the engine while we were probing, so `up` is
		// describing a machine that no longer exists. Acting on it would log
		// "engine is down" about an engine somebody just started, and call
		// Start a second time. Start is idempotent, so this is a tidiness fix
		// rather than a correctness one -- but a spurious start in the log is
		// how an operator loses trust in the log.
		return
	}

	desired := ReadDesired(s.Config.StateDir)
	s.lastUp.Store(up)

	switch {
	case desired == DesiredRunning && !up:
		// Down on purpose? The file is the shared truth: this supervisor may
		// have idled the engine (idleStopped), or a previous incarnation did
		// (file says idle after a restart) — either way the engine stays down
		// until demand. Deleting the file is the wake-up poke (`skrog start`
		// does it), so a set flag with no file means someone asked.
		fileIdle := ReadEngineState(s.Config.StateDir) == EngineIdle
		if fileIdle {
			s.idleStopped = true
			return // idle by design; Demand or a poke wakes it
		}
		if s.idleStopped {
			s.log().Info("idle marker removed; waking the engine")
			s.idleStopped = false
		}

		if time.Now().Before(s.nextTry) {
			return // still backing off
		}
		s.log().Warn("engine is down; starting it", "consecutiveFailures", s.failures)
		if err := s.Engine.Start(ctx); err != nil {
			s.failures++
			delay := s.backoff()
			s.nextTry = time.Now().Add(delay)
			s.log().Error("engine start failed",
				"error", err, "retryIn", delay, "consecutiveFailures", s.failures)
			return
		}
		s.failures = 0
		s.nextTry = time.Time{}
		s.startGen++ // #437: a concurrent probe's reading is now stale
		s.upSince = time.Now()
		s.lifecycle.EngineStarts++
		if s.lifecycle.IdleStops > 0 && s.lifecycle.LastWakeAt.Before(s.lifecycle.LastIdleStopAt) {
			// Started after an idle stop: this is the wake half of the cycle,
			// and the gap between the two is what says whether idle-timeout is
			// set somewhere useful.
			s.lifecycle.LastWakeAt = time.Now()
		}
		s.log().Info("engine recovered")
		s.fireHook(HookPostStart)

	case desired == DesiredStopped && up:
		s.log().Info("desired state is stopped; stopping the engine")
		s.fireHook(HookPreStop)
		if err := s.Engine.Stop(ctx); err != nil {
			s.log().Error("engine stop failed", "error", err)
		}
		s.idleStopped = false
		WriteEngineState(s.Config.StateDir, EngineActive)

	case desired == DesiredStopped && !up:
		// Stopped and down is the state the user asked for — but the path
		// into it may have gone through an idle stop, leaving the in-memory
		// flag set (#80: `skrog stop` clears the FILE, not this process's
		// memory). Clear it here, or a later Demand would treat the engine as
		// merely idle and resurrect what the user explicitly stopped.
		if s.idleStopped {
			s.idleStopped = false
			WriteEngineState(s.Config.StateDir, EngineActive)
		}

	case desired == DesiredRunning && up:
		// Healthy: a success observed by the loop also resets backoff, so one
		// bad patch (a WSL update mid-flight, say) does not tax the next.
		s.failures = 0
		s.nextTry = time.Time{}
		if s.upSince.IsZero() {
			s.upSince = time.Now()
		}
		// A running engine can't be idle-stopped state; clear a stale marker
		// (someone started the engine by hand while the file said idle).
		if s.idleStopped || ReadEngineState(s.Config.StateDir) == EngineIdle {
			s.idleStopped = false
			WriteEngineState(s.Config.StateDir, EngineActive)
		}
		// Prune first: it only decides here and runs on its own goroutine, and
		// maybeIdleStop below refuses to stop an engine while that goroutine
		// is still working.
		s.maybePrune(ctx)
		s.maybeIdleStop(ctx)
	}
}

// maybeIdleStop stops a healthy engine that nothing is using, returning its
// RAM to the machine (#41). Caller holds s.mu.
func (s *Supervisor) maybeIdleStop(ctx context.Context) {
	if s.Activity == nil || s.IdleTimeout == nil {
		return
	}
	timeout := s.IdleTimeout()
	if timeout <= 0 {
		s.lastVeto = ""
		return // idle stops are off (the default)
	}
	if n := s.Activity.ActiveConns(); n > 0 {
		s.veto("open client connections", "conns", n)
		return
	}
	// An automatic prune runs off this goroutine and talks to the engine the
	// whole time (#393). Stopping it mid-sweep would fail the prune and leave
	// the reclaim half-done, and the bridge sees none of that traffic, so
	// nothing else here would notice.
	if s.pruning.Load() {
		s.veto("an automatic prune is running")
		return
	}
	// The idle clock starts at whichever is later: the last connection, or
	// the engine coming up — a fresh engine with no traffic yet still gets
	// its full timeout.
	quietSince := s.Activity.LastActivity()
	if s.upSince.After(quietSince) {
		quietSince = s.upSince
	}
	if time.Since(quietSince) < timeout {
		s.veto("waiting out the quiet window",
			"quiet", time.Since(quietSince).Round(time.Second), "idleTimeout", timeout)
		return
	}
	// Stopping the engine kills whatever runs in it, so idling requires a
	// definite "nothing is running": no probe, a probe error, or running
	// containers all veto.
	if s.Busy == nil {
		s.veto("no container probe wired")
		return
	}
	busy, err := s.Busy(ctx)
	if err != nil {
		s.veto("container probe failed", "error", err)
		return
	}
	if busy {
		s.veto("containers are running")
		return
	}
	s.lastVeto = ""

	s.log().Info("bridge quiet and no containers running; stopping the engine until demand",
		"quiet", time.Since(quietSince).Round(time.Second), "idleTimeout", timeout)
	if err := WriteEngineState(s.Config.StateDir, EngineIdle); err != nil {
		s.log().Error("could not record the idle state; leaving the engine running", "error", err)
		return
	}
	if err := s.Engine.Stop(ctx); err != nil {
		s.log().Error("idle stop failed", "error", err)
		WriteEngineState(s.Config.StateDir, EngineActive)
		return
	}
	s.idleStopped = true
	s.lifecycle.IdleStops++
	s.lifecycle.LastIdleStopAt = time.Now()
	s.upSince = time.Time{}
	s.fireHook(HookOnIdleStop)
}

// Demand wakes an idle-stopped engine for an incoming connection, blocking
// until it is up; a no-op when the engine is not idle. The pipe server's
// dialer calls this before every engine dial, so the first `docker` command
// after an idle stop cold-starts the engine transparently.
//
// Two refusals, both from the v0.2.0 review: a stopped engine is the user's
// explicit intent and is never resurrected by traffic (#80 — background
// pollers like an IDE's Docker extension would otherwise flap it against the
// reconciler); and a failing engine is retried on the same backoff schedule
// the tick uses (#89 — without it, every queued connection serially paid a
// full failed StartTimeout while holding the lock).
func (s *Supervisor) Demand(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.idleStopped && ReadEngineState(s.Config.StateDir) != EngineIdle {
		return nil
	}
	if ReadDesired(s.Config.StateDir) == DesiredStopped {
		// Stopped beats idle: clear the leftover idle state so later ticks
		// and Demands agree, and refuse the wake.
		s.idleStopped = false
		WriteEngineState(s.Config.StateDir, EngineActive)
		return errors.New("engine is stopped; run `skrog start` to use it")
	}
	if time.Now().Before(s.nextTry) {
		return fmt.Errorf("engine start is backing off after %d failure(s); retrying by %s",
			s.failures, s.nextTry.Format("15:04:05"))
	}
	s.log().Info("connection while idle; cold-starting the engine")
	began := time.Now()
	if err := s.Engine.Start(ctx); err != nil {
		s.failures++
		delay := s.backoff()
		s.nextTry = time.Now().Add(delay)
		s.log().Error("cold start failed",
			"error", err, "retryIn", delay, "consecutiveFailures", s.failures)
		return err
	}
	s.failures = 0
	s.nextTry = time.Time{}
	s.startGen++ // #437: a tick probing right now is holding a stale reading
	// Cleared only after a successful start, so a second connection arriving
	// mid-start blocks on the mutex and then sees a running engine, rather
	// than racing ahead to dial an engine that is not up yet.
	s.idleStopped = false
	WriteEngineState(s.Config.StateDir, EngineActive)
	s.upSince = time.Now()
	// Measured, not assumed: the issue asked for the real cold-start number.
	s.log().Info("engine cold-started on demand", "took", time.Since(began).Round(10*time.Millisecond))
	s.fireHook(HookOnWake)
	return nil
}

// backoff is exponential from 2s, capped: crash loops must not hammer WSL,
// but a transient failure (sleep/resume races, `wsl --shutdown`) should be
// repaired in seconds, not minutes.
func (s *Supervisor) backoff() time.Duration {
	d := 2 * time.Second
	for i := 1; i < s.failures; i++ {
		d *= 2
		if d >= s.Config.backoffMax() {
			return s.Config.backoffMax()
		}
	}
	return d
}

// EngineStatus reports the engine state the reconciler last observed, in the
// same vocabulary `skrog status --json` uses: running, idle or stopped.
//
// The point is that it costs nothing. The reconciler probes the engine every
// tick regardless, so a reader -- the tray, through the published stats -- can
// have that answer instead of spawning a process to ask the same question
// again (#192). It reads an atomic and at most one small file, and never takes
// the reconciler's mutex.
func (s *Supervisor) EngineStatus() string {
	if s.lastUp.Load() {
		return "running"
	}
	if ReadEngineState(s.Config.StateDir) == EngineIdle {
		return "idle"
	}
	return "stopped"
}
