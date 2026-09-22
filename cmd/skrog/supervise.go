package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wslkit/skrog/internal/audit"
	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/hostca"
	"github.com/wslkit/skrog/internal/logging"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/policy"
	"github.com/wslkit/skrog/internal/profile"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/selfexe"
	"github.com/wslkit/skrog/internal/supervise"
)

// engineAdapter satisfies supervise.Engine with the provisioner's primitives.
type engineAdapter struct {
	p    *provision.Provisioner
	opts provision.Options
	cfg  *config.Watcher
	log  *slog.Logger

	// Enumerating the Windows root store costs real time, and an idle wake is
	// an engine start, so the PEM is read once and reused. A read that failed
	// is not cached: the next start tries again.
	mu     sync.Mutex
	caPEM  []byte
	caRead bool
}

func (e *engineAdapter) Running(ctx context.Context) (bool, error) {
	// The error half matters here and only here (#437): the reconciler starts
	// the engine when this says false, so a probe that merely failed must not
	// be reported as a stopped engine.
	return e.p.EngineRunningErr(ctx, e.opts)
}

// Start brings the engine up with the settings as they are now, not as they
// were when the supervisor launched.
//
// Every per-start setting is re-read here. GPU already was (#83), because the
// supervisor outlives `skrog restart` and a value captured at launch goes
// stale. The corporate-network settings were not, so `skrog config set
// network.proxy ...` followed by the `skrog restart` the docs prescribe
// applied nothing — the same bug as the audit log, one layer down (#202).
func (e *engineAdapter) Start(ctx context.Context) error {
	// The list itself lives in withStartSettings now, shared with every
	// one-shot command that restarts the engine. It used to live only here,
	// which is why `engine upgrade`, `compact`, `relocate` and the snapshot
	// restore all brought the engine back with none of it (#490).
	//
	// The cached CA reader stays: this process starts the engine many times
	// and the certificate store does not change under it.
	opts := withStartSettings(e.opts, e.cfg.Config(), func() []byte { return e.hostCAs(ctx) })
	return e.p.StartEngine(ctx, opts)
}

func (e *engineAdapter) Stop(ctx context.Context) error {
	return e.p.StopEngine(ctx, e.opts)
}

// hostCAs returns the host root CA bundle to trust inside the engine, reading
// the Windows store at most once per successful read.
func (e *engineAdapter) hostCAs(ctx context.Context) []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.caRead {
		return e.caPEM
	}
	pem, err := hostca.HostRootCAs(ctx)
	if err != nil {
		e.log.Warn("host CA import is on but the store could not be read", "error", err)
		return nil
	}
	e.caPEM, e.caRead = pem, true
	e.log.Info("importing host CA certificates into the engine", "bytes", len(pem))
	return e.caPEM
}

// resolveDistro prefers the install manifest, like proxy does: it records
// which distro this machine actually has.
func resolveDistro(p *provision.Provisioner, opts provision.Options) (string, bool) {
	if m, err := p.ReadManifest(opts); err == nil && m.Distro != "" {
		return m.Distro, true
	}
	if opts.Distro != "" {
		return opts.Distro, true
	}
	return "", false
}

// engineTarget is what this install serves.
//
// It carried a Backend dimension while a WSL container session was a second
// backend (#335). That backend is gone (#451), so the only thing left to
// resolve is the distro -- but the type stays, because callers want "what do
// I serve" rather than "what did the flag say", and because the status JSON
// still reports a backend field that is part of a pinned contract (#179).
type engineTarget struct {
	Distro string
}

// resolveEngineTarget reads the manifest the way resolveDistro does.
//
// A machine with no manifest but an explicit --distro is still a target: that
// is how `supervise --distro X` worked before any of this, and breaking it
// would be a regression for anyone driving Skrog by hand.
//
// An install left behind by the removed backend falls out of this on its own:
// it recorded no distro, because there was none. It needs no separate check
// here -- and adding one would be untestable, which is how it was caught --
// but it does need a different MESSAGE, and that is noInstallMessage's job.
func resolveEngineTarget(p *provision.Provisioner, opts provision.Options) (engineTarget, bool) {
	if m, err := p.ReadManifest(opts); err == nil && m.Distro != "" {
		return engineTarget{Distro: m.Distro}, true
	}
	if opts.Distro != "" {
		return engineTarget{Distro: opts.Distro}, true
	}
	return engineTarget{}, false
}

func runSupervise(args []string) int {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	var (
		distro    = fs.String("distro", "", "WSL distro (default: from the install manifest)")
		stateDir  = fs.String("state-dir", "", "override Skrog's state directory")
		pipeName  = fs.String("pipe", "", "pipe to serve (default: "+pipeproxy.DefaultPipeName+", or Skrog's own if taken)")
		noContext = fs.Bool("no-context", false, "do not create or update the skrog docker context")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog supervise [flags]

The always-on layer: serves the docker pipe AND keeps the engine alive —
crash restart with backoff, recovery from `+"`wsl --shutdown`"+` and sleep/resume,
honoring `+"`skrog stop`"+` until `+"`skrog start`"+`. One instance per install.

Runs in the foreground; `+"`skrog start`"+` spawns it in the background, and the
logon autostart (`+"`skrog autostart`"+`) runs it for you. Logs go to supervisor.log in the
state directory (rotated) as well as stderr.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := provision.Options{Distro: *distro, StateDir: *stateDir}
	opts = optsWithResolvedStateDir(opts)

	// Single instance before anything else: two supervisors would fight over
	// the pipe and the engine.
	lock, err := supervise.Acquire(opts.StateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	defer lock.Close()

	// Drop any predecessor's endpoint record the moment the lock is ours.
	//
	// A supervisor killed hard runs no cleanup, so its record survives. Between
	// Acquire here and the write after Listen below there is log setup, a
	// manifest read and SelectPipeName -- which dials with a timeout when
	// something else holds the default pipe. Throughout that window Held() is
	// true, so a reader would take the dead supervisor's record as live and
	// could be told the engine is on a pipe nothing is serving (#288).
	if err := supervise.ClearEndpoint(opts.StateDir); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: clearing a stale endpoint record: %v\n", err)
	}

	// Log to a rotating file and stderr both: the file for the months-long
	// logon session, stderr for a human running it in the foreground.
	logFile, err := logging.NewRotatingWriter(
		filepath.Join(opts.StateDir, "supervisor.log"), 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	defer logFile.Close()
	// Warn+ mirrors to the Event Log for admins; the file keeps everything.
	log := slog.New(logging.NewEventLogHandler(
		slog.NewTextHandler(io.MultiWriter(logFile, os.Stderr), nil),
		logging.EventSource))

	p := &provision.Provisioner{Logger: log}
	target, ok := resolveEngineTarget(p, opts)
	if !ok {
		fmt.Fprint(os.Stderr, noInstallMessage(p, opts))
		return exitNotFound
	}
	targetDistro := target.Distro
	opts.Distro = targetDistro

	// What this supervisor was ASKED for, recorded before anything can fail,
	// so a replacement can ask for the same thing (#429).
	//
	// The request, not the selection. Recording `selected` would pin the
	// FALLBACK pipe on a machine that happened to have Docker Desktop running
	// at this moment, and that machine would never take the default back once
	// Desktop was gone. Recording the request keeps normal selection normal
	// and pins only what someone named.
	//
	// Written unconditionally, including empty -- WriteServedPipe removes the
	// file for an empty value, so starting a supervisor with no --pipe erases
	// a previous run's preference rather than inheriting it.
	if err := supervise.WriteServedPipe(opts.StateDir, *pipeName); err != nil {
		log.Warn("could not record the requested pipe; a supervisor restart may not keep it",
			"error", err)
	}

	selected, reason := pipeproxy.SelectPipeName(*pipeName)
	listener, err := pipeproxy.Listen(selected, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	defer listener.Close()
	log.Info("serving pipe", "pipe", selected, "reason", reason)

	// Record what was actually bound, so `skrog status` can answer "where is
	// the engine listening" without asking the selector a second time (#273).
	// Recomputing would be wrong: Docker Desktop can start or stop after this
	// point, and the answer would then name a pipe nothing is serving.
	//
	// Best-effort in both directions. A status field must never be able to
	// stop the bridge from running, or to keep it from shutting down.
	if err := supervise.WriteEndpoint(opts.StateDir,
		supervise.Endpoint{Pipe: selected, Reason: reason}); err != nil {
		log.Warn("could not record the served endpoint; `skrog status` will not name it",
			"error", err)
	}
	defer func() {
		if err := supervise.ClearEndpoint(opts.StateDir); err != nil {
			log.Warn("could not clear the endpoint record", "error", err)
		}
	}()

	if !*noContext {
		if err := (&dockerctx.Manager{}).Ensure(context.Background(),
			pipeproxy.DockerHostFor(selected)); err != nil {
			log.Warn("docker context not wired", "reason", err)
		}
	}

	ctx, stop := interruptible()
	defer stop()

	// A restart request left behind by a supervisor that crashed before it
	// could act must not shut this one down on sight.
	if err := supervise.ClearRestart(opts.StateDir); err != nil {
		log.Warn("could not clear a stale restart request", "error", err)
	}
	go watchForRestart(ctx, opts.StateDir, stop, log)

	// Server and supervisor are deliberately entangled (#41): the server's
	// traffic feeds the supervisor's idle detection, and the supervisor's
	// Demand wakes an idle-stopped engine for the server's next connection.
	//
	cfg := config.NewWatcher(opts.StateDir)
	cfg.OnError = func(err error) {
		log.Error("settings file is not valid; the settings already in force stay",
			"error", err)
	}

	// Transport, engine adapter and idle probe. These were pluggable while
	// there were two backends (#335); with one backend there is one of each.
	d := engineDialer(targetDistro, "", opts.StateDir, log)
	var (
		dialer     pipeproxy.Dialer = d
		engineImpl supervise.Engine = &engineAdapter{p: p, opts: opts, cfg: cfg, log: log}
		busy                        = engineBusy(d, p, opts, log)
	)

	// One watcher, consulted wherever a setting is consumed (#202). `skrog
	// config` promises that "settings apply live: the supervisor re-reads
	// them every few seconds"; this is what makes that true rather than true
	// of one key. It stats before it reads, so consulting it per docker call
	// is cheap.
	// Bind-path rewriting is always on; the audit log (#121) and the policy
	// gate (#120) wrap it. Both follow their file live — neither is captured
	// here — because this process outlives `skrog restart`, so anything read
	// once at startup can only be changed by killing it.
	auditPath := filepath.Join(opts.StateDir, "audit.log")
	auditor := &audit.Switch{
		Enabled: func() bool { return cfg.Config().Audit },
		Open: func() (io.WriteCloser, error) {
			return logging.NewRotatingWriter(auditPath, 0, 0)
		},
		OnChange: func(enabled bool, err error) {
			switch {
			case err != nil:
				// Loud: an operator who turned auditing on believes it is on.
				log.Error("audit log could not be opened; auditing stays OFF",
					"path", auditPath, "error", err)
			case enabled:
				log.Info("audit log enabled", "path", auditPath)
			default:
				log.Info("audit log disabled")
			}
		},
	}
	defer auditor.Close()

	// The gate is always installed and re-reads policy.yaml when it changes,
	// so editing the rules -- or creating the file for the first time --
	// takes effect on the next container create, with nothing to restart.
	//
	// Reading once at start was the first cut and it was wrong: `skrog
	// restart` bounces the engine, not this process, so the documented advice
	// did not work; and a policy written after the supervisor started
	// installed no gate at all.
	watcher := policy.NewWatcher(opts.StateDir)
	watcher.OnError = func(err error) {
		// Which of the two it is matters to whoever reads this line, and the
		// old wording asserted the reassuring one unconditionally. A file that
		// has never parsed has no previous rules to stay in force (#254).
		if unavail := watcher.Unavailable(); unavail != nil {
			log.Error("policy file has never been read successfully; every container create is refused until it parses or is removed",
				"error", err, "path", policy.Path(opts.StateDir))
			return
		}
		log.Error("policy file is not valid; the previous rules stay in force",
			"error", err, "path", policy.Path(opts.StateDir))
	}
	switch {
	case watcher.Unavailable() != nil:
		// Said at startup as well as on change: this is the state that
		// survives a reboot, and the one where silence used to mean "no rules"
		// rather than "rules unknown".
		log.Error("admission control cannot start: the policy file does not parse",
			"path", policy.Path(opts.StateDir))
	case !watcher.Rules().Empty():
		log.Info("admission control enabled", "path", policy.Path(opts.StateDir))
	}

	// A Windows bind source maps to /mnt/<drive> inside the engine distro.
	handler := pipeproxy.RewriteBindsGuarded(auditor, watcher)

	metrics := &pipeproxy.Metrics{}
	srv := &pipeproxy.Server{
		Logger:  log,
		Handler: handler,
		Metrics: metrics,
	}
	sup := &supervise.Supervisor{
		Engine:   engineImpl,
		Config:   supervise.Config{StateDir: opts.StateDir},
		Log:      log,
		Activity: srv,
		// Read per tick, so `skrog config set idle-timeout` applies live. A
		// settings file that will not parse keeps the timeout already in
		// force rather than reverting to a default nobody chose; with no
		// readable file at all the zero value is off, so the supervisor still
		// never idle-stops on a guess.
		IdleTimeout: func() time.Duration { return cfg.Config().IdleTimeout },
		// Must be non-nil: supervise.maybeIdleStop vetoes every idle stop when
		// Busy is nil, which silently disables the reclaim it is meant to
		// enable.
		Busy: busy,
		// Lifecycle hooks (#70): fire off-thread and time-bounded so a user's
		// script never blocks the reconciler.
		Hook: hookRunner(opts.StateDir, log),
		// Automatic disk reclamation (#393), read per tick like IdleTimeout so
		// `skrog config set prune.every` applies live. Off unless configured.
		PrunePolicy: func() supervise.PrunePolicy {
			c := cfg.Config()
			return supervise.PrunePolicy{
				Every:      c.PruneEvery,
				KeepSince:  c.KeepSince(),
				BuildCache: c.PruneBuildCache,
			}
		},
		Prune: autoPrune(log),
	}
	srv.Dialer = &demandDialer{sup: sup, inner: dialer}
	go sup.Run(ctx)

	// Statistics the CLI cannot see from outside this process (#179), flushed
	// to a timestamped file so `skrog status --stats` can report both the
	// numbers and how old they are.
	go flushStats(ctx, opts.StateDir, sup, srv, metrics, dialer, log)

	// The pipe server carries traffic; both stop together.
	if err := srv.Serve(ctx, listener); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	log.Info("supervisor stopped")
	return exitOK
}

// optsWithResolvedStateDir freezes the default state dir into the options, so
// lock names, logs and desired-state files all agree on one path.
func optsWithResolvedStateDir(opts provision.Options) provision.Options {
	if opts.StateDir == "" {
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			opts.StateDir = filepath.Join(base, "Skrog")
		}
	}
	return opts
}

func runStart(args []string) int { return runStartPreserving(args, "") }

// runStartPreserving is runStart with the pipe a replacement supervisor must
// keep serving (#429).
//
// Separate from runStart, rather than a `--pipe` flag on `start`, because this
// is not something a user asks for: it is `restart --supervisor` carrying a
// value across a teardown that would otherwise erase it. A CLI flag would
// document a decision the caller never makes.
//
// Empty means "choose normally", which is every path except that one.
func runStartPreserving(args []string, preservePipe string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "override Skrog's state directory")
	timeout := fs.Duration("timeout", 2*time.Minute, "how long to wait for the engine")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog start

Records the desired state as running, launches the supervisor when none is
running, and waits for the engine to answer.
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	if err := supervise.WriteDesired(opts.StateDir, supervise.DesiredRunning); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	// Removing the idle marker is the wake-up poke: an idle-stopped engine is
	// down on purpose, and the supervisor will not restart it while the marker
	// stands — but `skrog start` is the user saying now.
	if err := supervise.WriteEngineState(opts.StateDir, supervise.EngineActive); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	p := &provision.Provisioner{Logger: cliLogger(false)}
	target, ok := resolveEngineTarget(p, opts)
	if !ok {
		fmt.Fprint(os.Stderr, noInstallMessage(p, opts))
		return exitNotFound
	}
	// The resolved name must actually be used: polling the default distro
	// while the install lives under a custom name reports a healthy engine as
	// missing — the poll timed out while `skrog status` said running.
	opts.Distro = target.Distro

	if !supervise.Held(opts.StateDir) {
		fmt.Fprintln(os.Stderr, "  starting the supervisor in the background")
		if err := spawnSupervisor(opts.StateDir, preservePipe); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: launching supervisor: %v\n", err)
			return exitError
		}
	}

	// 250 ms rather than a second (#398): the engine coming up is the thing
	// the user is waiting on, and a one-second poll added up to a second of
	// pure latency to a start that was already too slow. Matches the interval
	// StartEngine already uses to watch for the socket, and each probe is a
	// wsl exec bounded by the deadline above.
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		if p.EngineRunning(context.Background(), opts) {
			fmt.Println("engine is running")
			return exitOK
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "skrog: engine did not come up within %s; see supervisor.log in %s\n",
		*timeout, opts.StateDir)
	return exitError
}

func runStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "override Skrog's state directory")
	timeout := fs.Duration("timeout", time.Minute, "how long to wait for the engine to stop")
	supervisor := fs.Bool("supervisor", false, "also stop the supervisor process itself")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog stop [--supervisor]

Records the desired state as stopped and waits for the engine to stop. The
supervisor keeps honoring this until `+"`skrog start`"+` — a stopped engine stays
stopped. Only Skrog's own distro is touched, never other WSL distros.

  --supervisor   also stop the always-on process that serves the docker pipe

By default the supervisor keeps running, which is what makes `+"`skrog start`"+`
quick and keeps the pipe where it was. `+"`--supervisor`"+` is for replacing
skrog.exe on disk: Windows will not overwrite a running binary, and the
installer refuses rather than leave a half-replaced install. It stops the
watchdog too, or that would relaunch what you just stopped.
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	if err := supervise.WriteDesired(opts.StateDir, supervise.DesiredStopped); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	// An explicit stop supersedes an idle stop; the marker would make status
	// claim "idle (wakes on demand)" about an engine that must stay down.
	supervise.WriteEngineState(opts.StateDir, supervise.EngineActive)

	p := &provision.Provisioner{Logger: cliLogger(false)}
	target, ok := resolveEngineTarget(p, opts)
	if !ok {
		fmt.Fprint(os.Stderr, noInstallMessage(p, opts))
		return exitNotFound
	}
	opts.Distro = target.Distro

	// With no supervisor to do it, stop the engine directly.
	if !supervise.Held(opts.StateDir) {
		if err := p.StopEngine(context.Background(), opts); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
	}

	deadline := time.Now().Add(*timeout)
	stopped := false
	for time.Now().Before(deadline) {
		if !p.EngineRunning(context.Background(), opts) {
			stopped = true
			break
		}
		time.Sleep(time.Second)
	}
	if !stopped {
		fmt.Fprintf(os.Stderr, "skrog: engine still running after %s\n", *timeout)
		return exitError
	}
	fmt.Println("engine is stopped (and stays stopped until `skrog start`)")

	// The supervisor last, and only when asked (#482).
	//
	// Order matters: it is the supervisor that honors the desired state above,
	// so stopping it first would leave the engine to be stopped directly and
	// turn one sequence into two. And this is the rarer intent by far --
	// almost everything people want from `stop` is the engine, which is why
	// the supervisor survives it by default.
	if *supervisor {
		if !supervise.Held(opts.StateDir) {
			fmt.Println("no supervisor is running")
			return exitOK
		}
		if err := stopSupervisor(opts.StateDir); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		// The watchdog goes with it, without being asked: it treats a clean
		// exit as final (internal/watchdog), which is the same property that
		// stops it racing `restart --supervisor`.
		fmt.Println("supervisor stopped (skrog.exe can be replaced; `skrog start` brings it back)")
	}
	return exitOK
}

// restartPollInterval is how often the supervisor looks for a restart
// request. A second is imperceptible to someone who just typed the command,
// and the check is one stat.
const restartPollInterval = time.Second

// supervisorExitTimeout bounds the wait for the old supervisor to let go of
// the single-instance claim. It only has to finish serving in-flight
// connections; anything longer than this means it is wedged, and saying so
// beats spawning a second one that cannot get the lock.
const supervisorExitTimeout = 20 * time.Second

// watchForRestart exits the supervisor when `skrog restart --supervisor`
// asks it to (#202).
//
// Exiting is the whole mechanism: the CLI waits for the single-instance claim
// to drop and then launches a replacement, and exiting zero is what tells the
// watchdog this was asked for rather than a crash, so it does not race the
// CLI to respawn.
func watchForRestart(ctx context.Context, stateDir string, stop func(), log *slog.Logger) {
	t := time.NewTicker(restartPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !supervise.RestartRequested(stateDir) {
				continue
			}
			// Cleared before exiting, so the replacement does not find the
			// note and immediately exit too.
			if err := supervise.ClearRestart(stateDir); err != nil {
				log.Warn("could not clear the restart request", "error", err)
			}
			log.Info("restart requested; exiting so a fresh supervisor can take over")
			stop()
			return
		}
	}
}

func runRestart(args []string) int {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	var (
		supervisor = fs.Bool("supervisor", false,
			"recycle the supervisor process too, not just the engine")
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		timeout  = fs.Duration("timeout", 0,
			"how long to wait for each phase (default: 1m to stop, 2m to start)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog restart [--supervisor]

Stops the engine and starts it again.

By default this is the ENGINE only. The supervisor — the always-on process
that serves the docker pipe — keeps running across it, which is what lets a
restart be quick and the pipe stay put.

  --supervisor   also replace the supervisor process

Almost nothing needs `+"`--supervisor`"+`: settings are re-read live, and the engine
picks up config changes on this plain restart. Reach for it when the
supervisor itself is misbehaving, or after replacing skrog.exe on disk.
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return exitUsage
	}

	// Rebuilt rather than forwarded verbatim, so stop and start each keep
	// their own default timeout when the user did not ask for one.
	var pass []string
	if *stateDir != "" {
		pass = append(pass, "--state-dir", *stateDir)
	}
	if *timeout != 0 {
		pass = append(pass, "--timeout", timeout.String())
	}

	if code := runStop(pass); code != exitOK && code != exitNotFound {
		return code
	}
	if *supervisor {
		dir := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir}).StateDir

		// Captured BEFORE recycling, because the supervisor deletes its own
		// endpoint record on the way out -- runSupervise clears it in a defer,
		// so a clean exit is precisely the case where the record is gone by
		// the time anything downstream could read it (#429).
		//
		// Reading it afterwards is what the first attempt at this did. It
		// worked for a hard kill, where the record survives, and not for the
		// restart it was written for.
		keepPipe := customPipeToPreserve(dir)

		if code := recycleSupervisor(dir); code != exitOK {
			return code
		}
		return runStartPreserving(pass, keepPipe)
	}
	return runStart(pass)
}

// recycleSupervisor asks the running supervisor to exit and waits for it to
// let go of the single-instance claim. The caller's `skrog start` then
// launches the replacement, which is the one code path that knows about
// skrogw.exe and the detached-window handling.
func recycleSupervisor(stateDir string) int {
	if !supervise.Held(stateDir) {
		fmt.Println("  no supervisor is running; one will be started")
		return exitOK
	}
	fmt.Println("  stopping the supervisor")
	if err := stopSupervisor(stateDir); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	return exitOK
}

// stopSupervisor asks a running supervisor to exit and waits for it to let go
// of the single-instance claim. It reports nil once nothing holds the claim,
// including when nothing held it to begin with.
//
// The note is the only way to ask. There is no SIGTERM on Windows and no pid
// to aim one at — the claim is a handle, not a pid file, deliberately (see
// internal/supervise) — so the supervisor polls for the request, clears it,
// and exits 0. Exiting 0 is load-bearing beyond this function: it is what
// tells skrogw.exe the exit was asked for rather than a crash, so the
// watchdog does not respawn what was just stopped (internal/watchdog).
//
// Two callers, and what is NOT here is the difference between them:
// `restart --supervisor` starts a replacement afterwards, `uninstall` (#474)
// does not.
func stopSupervisor(stateDir string) error {
	return stopSupervisorWithin(stateDir, supervisorExitTimeout)
}

// stopSupervisorWithin is stopSupervisor with the wait as a parameter, so the
// give-up path can be tested without a test spending the real 20 seconds
// waiting for a supervisor that is never going to exit.
func stopSupervisorWithin(stateDir string, wait time.Duration) error {
	if !supervise.Held(stateDir) {
		return nil
	}
	if err := supervise.RequestRestart(stateDir); err != nil {
		return err
	}

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !supervise.Held(stateDir) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Withdraw the request rather than leave it armed: a supervisor that is
	// merely slow should not shut down minutes later, long after the command
	// that asked for it reported failure.
	if err := supervise.ClearRestart(stateDir); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: withdrawing the restart request: %v\n", err)
	}
	return fmt.Errorf("the supervisor did not exit within %s and is still running; "+
		"see supervisor.log in %s", wait, stateDir)
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "override Skrog's state directory")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	withStats := fs.Bool("stats", false, "add engine, disk, VM, uptime and bridge statistics (needs a running engine for the first three)")
	asProm := fs.Bool("prometheus", false, "emit Prometheus text (node_exporter textfile format); implies --stats")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog status [--json] [--stats] [--prometheus]

Reports the distro, whether the supervisor and engine are running, the desired
state the user last asked for, and — while a supervisor is running — the pipe
it actually bound, with the DOCKER_HOST spelling of it.

The endpoint is what the running supervisor recorded when it bound, not a
fresh guess: Docker Desktop can start or stop after Skrog chose, so recomputing
the answer could name a pipe nothing is serving. No supervisor, no endpoint.

Reads host-side files only — it never starts the engine to answer, and never
wakes an idle-stopped one. Safe to poll.

The engine is one of:

  running   answering the docker API
  idle      stopped by the idle timeout, ON PURPOSE; the next docker command
            wakes it. Not an error, and the exit code says so
  stopped   down, and staying down until `+"`skrog start`"+`

--stats adds engine, disk, VM, uptime and bridge counters. It is opt-in
because collecting them costs WSL calls a readiness probe should not pay; the
default shape is the pinned probe contract (%s).

--prometheus emits the same numbers as Prometheus text, for node_exporter's
textfile collector (a scheduled task writes it into the collector directory).
It implies --stats. Nothing leaves this machine: there is no listener and no
telemetry — it is the operator measuring their own host.

Exit codes: 0 engine running or idle, %d engine down, %d usage, %d not installed.
--prometheus always exits 0 when it could write metrics, because the engine's
state is IN the metrics: a scrape that failed because the engine was down would
discard exactly the reading worth having.
`, "docs/cli-json.md", exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *asProm && *asJSON {
		fmt.Fprintln(os.Stderr, "skrog: choose --json or --prometheus, not both")
		return exitUsage
	}
	// The metrics are drawn from the statistics, so asking for them asks for
	// those too rather than quietly exporting an almost-empty file.
	if *asProm {
		*withStats = true
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	p := &provision.Provisioner{Logger: cliLogger(true)}

	st := statusJSON{
		StateDir:   opts.StateDir,
		Supervisor: "stopped",
		Engine:     "stopped",
		Desired:    string(supervise.ReadDesired(opts.StateDir)),
		Profile:    (&profile.Manager{StateDir: opts.StateDir}).Active(),
	}
	if c, err := config.Load(opts.StateDir); err == nil {
		st.GPU.Enabled = c.GPU
		st.GPU.Vendor = c.GPUVendor
		// The probes below read the vendor spec path, so it has to travel too.
		opts.GPUVendor = c.GPUVendor
	}

	if target, ok := resolveEngineTarget(p, opts); ok {
		st.Installed = true
		// Always the distro now (#451). The field stays because the status
		// JSON is a pinned contract (#179): a reader that switches on it must
		// keep parsing rather than find the key gone.
		st.Backend = provision.BackendDistro
		st.Distro = target.Distro
		opts.Distro = target.Distro
		if supervise.Held(opts.StateDir) {
			st.Supervisor = "running"
			// Only under the lock: the record outlives a supervisor that was
			// killed hard, and naming a pipe nothing is listening on is worse
			// than saying nothing (#273).
			if e, ok := supervise.ReadEndpoint(opts.StateDir); ok {
				st.Endpoint = &endpointJSON{
					Pipe:       e.Pipe,
					DockerHost: pipeproxy.DockerHostFor(e.Pipe),
					Reason:     e.Reason,
				}
			}
		}
		switch {
		case p.EngineRunning(context.Background(), opts):
			st.Engine = "running"
			// GPU probes need the distro up (never boot it for status, #82) and
			// are only worth two wsl calls when GPU is enabled at all.
			if st.GPU.Enabled {
				st.GPU.Probed = true
				st.GPU.Visible = p.GPUAvailable(context.Background(), opts)
				st.GPU.SpecInstalled = p.GPUSpecInstalled(context.Background(), opts)
			}
		case supervise.ReadEngineState(opts.StateDir) == supervise.EngineIdle:
			// Down by design (#41): the idle timeout elapsed, and the next
			// docker command wakes it. Scripts get to tell this from broken.
			st.Engine = "idle"
		}
	}

	// Statistics are opt-in and additive: the default shape is a pinned
	// readiness-probe contract (#179), and collecting them costs WSL calls that
	// a probe should not pay.
	if *withStats && st.Installed {
		s := gatherStats(context.Background(), opts, st.Distro, st.Engine == "running")
		st.Stats = &s
	}

	// Before the not-installed early return: "no engine here" is a fact a fleet
	// dashboard wants, and skrog_installed 0 is how it says so.
	if *asProm {
		if err := writePrometheus(os.Stdout, st, buildVersion); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: writing metrics: %v\n", err)
			return exitError
		}
		return exitOK
	}

	if *asJSON {
		return emitJSON(st)
	}
	if !st.Installed {
		fmt.Println("not installed (run `skrog install`)")
		return exitNotFound
	}
	// The first line names what this install serves.
	fmt.Printf("distro      %s\n", st.Distro)
	fmt.Printf("supervisor  %s\nengine      %s\ndesired     %s\n",
		st.Supervisor, st.Engine, st.Desired)
	if st.Profile != "" {
		fmt.Printf("profile     %s\n", st.Profile)
	}
	// Both forms, because both are asked for: the pipe is what the install
	// output named, and the npipe spelling is what goes in DOCKER_HOST for a
	// tool that does not read docker contexts. Same layout as `skrog install`.
	if st.Endpoint != nil {
		fmt.Printf("endpoint    %s", st.Endpoint.Pipe)
		if st.Endpoint.Reason != "" {
			fmt.Printf("  (%s)", st.Endpoint.Reason)
		}
		fmt.Printf("\ndocker host %s\n", st.Endpoint.DockerHost)
	}
	if st.Stats != nil {
		printStats(*st.Stats)
	}
	// Exit code mirrors engine health, so scripts can gate on it directly.
	// Idle counts as healthy: the engine is a docker command away, on purpose.
	if st.Engine != "running" && st.Engine != "idle" {
		return exitError
	}
	return exitOK
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// spawnSupervisor launches `skrog supervise` detached and windowless.
//
// Through skrogw.exe when it is there (a release zip, not a bare go build):
// the launcher stays resident as the supervisor's watchdog, so a crash costs
// seconds of pipe downtime rather than every docker command until the next
// `skrog start` (#166). Falling back to spawning supervise directly keeps a
// single-binary checkout working, just without the watchdog.
func spawnSupervisor(stateDir, preservePipe string) error {
	self, err := selfexe.Path()
	if err != nil {
		return err
	}
	target, args := supervisorCommand(self, stateDir, preservePipe)
	cmd := exec.Command(target, args...)
	configureDetached(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Released, not waited on: it must outlive this CLI invocation.
	return cmd.Process.Release()
}

// supervisorCommand builds what spawnSupervisor launches.
//
// Split out from spawnSupervisor so the ARGUMENTS can be tested without
// starting a process. That distinction is not pedantry: the first version of
// this fix put the --pipe wiring inline, and the test for
// customPipeToPreserve passed with the wiring deleted — a correct helper
// nothing called, which is the exact shape of three other defects in this
// release.
func supervisorCommand(self, stateDir, preservePipe string) (target string, args []string) {
	target, args = self, []string{"supervise", "--state-dir", stateDir}
	if launcher := filepath.Join(filepath.Dir(self), "skrogw.exe"); fileExists(launcher) {
		target, args = launcher, []string{"--state-dir", stateDir}
	}

	// Two sources, and the order matters.
	//
	// An explicit value comes from `restart --supervisor`, which read the
	// endpoint BEFORE tearing the old supervisor down — the record is gone by
	// now, because runSupervise clears it on a clean exit.
	//
	// The recorded fallback covers the other case: a supervisor that died
	// hard ran no cleanup, so its record survives and is the only thing that
	// remembers which pipe was being served.
	pipe := preservePipe
	if pipe == "" {
		pipe = customPipeToPreserve(stateDir)
	}
	if pipe != "" {
		args = append(args, "--pipe", pipe)
	}
	return target, args
}

// customPipeToPreserve returns the pipe a replacement supervisor must keep
// serving, or "" to let it choose normally (#429).
//
// `skrog restart --supervisor` rebuilds the argument list rather than
// forwarding it, and the pipe was not among what it carried. So a supervisor
// started with `--pipe <custom>` came back on the DEFAULT pipe, DOCKER_HOST
// stopped working, and the error named a missing file rather than a moved
// pipe. Same for the watchdog path: skrogw relaunches through here too, so a
// crash lost the pipe the same way.
//
// Only a genuinely custom pipe is preserved, and the two exclusions are what
// keep this from breaking the ordinary case:
//
//   - The DEFAULT pipe is not preserved. Normal selection takes it again when
//     it is free, and correctly falls back when something else (Docker
//     Desktop) has taken it in the meantime. Pinning it would turn that
//     graceful fallback into a hard failure to bind.
//   - The FALLBACK pipe is not preserved either, because normal selection
//     re-derives it: it is only ever chosen when the default is taken. Pinning
//     it would make the fallback sticky, so a machine that stopped running
//     Desktop would never take the default pipe back — and "plain docker just
//     works" is the thing the default pipe buys.
//
// What is left is a pipe someone asked for by name, which is exactly the case
// that was being lost.
func customPipeToPreserve(stateDir string) string {
	pipe := supervise.ReadServedPipe(stateDir)
	if pipe == "" {
		// Fall back to the endpoint record. It is right for the crash path --
		// a supervisor killed hard runs no cleanup, so its record survives --
		// and it is what an install that predates served-pipe has. It is NOT
		// enough on its own, which is the whole of #429: a clean exit deletes
		// it, and the acceptance suite deletes it too, on purpose.
		e, ok := supervise.ReadEndpoint(stateDir)
		if !ok {
			return ""
		}
		pipe = e.Pipe
	}
	if pipe == "" {
		return ""
	}
	// Both sides are normalised. DefaultPipeName and FallbackPipeName are full
	// `\\.\pipe\...` paths, and a recorded endpoint should be too -- but
	// comparing one against the other's bare name silently matches nothing,
	// which makes every exclusion below a no-op and the fallback sticky.
	// Trimming both is what makes the comparison mean what it reads as.
	if pipeEq(pipe, pipeproxy.DefaultPipeName) || pipeEq(pipe, pipeproxy.FallbackPipeName) {
		return ""
	}
	return pipe
}

// pipeEq compares two pipe names, tolerating the `\\.\pipe\` prefix on either
// side. Windows pipe names are case-insensitive.
func pipeEq(a, b string) bool {
	const prefix = `\\.\pipe\`
	return strings.EqualFold(strings.TrimPrefix(a, prefix), strings.TrimPrefix(b, prefix))
}

// statsFlushInterval is how often the supervisor publishes its counters. Five
// seconds keeps a reading current enough to act on while costing one small
// atomic write; supervise.Stats.Fresh() allows six times that before calling a
// reading stale, so a busy machine never flaps between the two.
const statsFlushInterval = 5 * time.Second

// flushStats publishes the supervisor's counters until the context ends, then
// writes one last reading so a clean shutdown leaves the final numbers rather
// than a reading from five seconds before the end.
func flushStats(ctx context.Context, stateDir string, sup *supervise.Supervisor,
	srv *pipeproxy.Server, m *pipeproxy.Metrics, dialer pipeproxy.Dialer, log *slog.Logger) {
	write := func() {
		snap := m.Snapshot()
		st := supervise.Stats{
			Engine:    sup.EngineStatus(),
			Lifecycle: sup.LifecycleSnapshot(),
			Bridge: supervise.Bridge{
				Connections:   snap.Connections,
				BytesToEngine: snap.BytesToEngine,
				BytesToClient: snap.BytesToClient,
				ActiveConns:   srv.ActiveConns(),
				Transport:     transportName(dialer),
			},
		}
		// A statistic must never be able to take the supervisor down, so a
		// failed write is logged at debug and forgotten.
		if err := supervise.WriteStats(stateDir, st); err != nil {
			log.Debug("could not write supervisor stats", "error", err)
		}
	}

	t := time.NewTicker(statsFlushInterval)
	defer t.Stop()
	write()
	for {
		select {
		case <-ctx.Done():
			write()
			return
		case <-t.C:
			write()
		}
	}
}

// transportName reports which engine transport is carrying traffic, because
// that is the difference between ~0.6 ms and ~165 ms per connection -- and the
// answer to "docker feels slow" when the engine itself is healthy (#179).
//
//	vsock     the fast path
//	fallback  the vsock agent is unreachable and socat is carrying it
//	socat     SKROG_NO_VSOCK pinned the slow path deliberately
//
// Anything else is reported as unknown rather than guessed at: `skrog proxy`
// and the tests wire dialers directly.
func transportName(d pipeproxy.Dialer) string {
	switch t := d.(type) {
	case *pipeproxy.FallbackDialer:
		if t.Degraded() {
			return "fallback"
		}
		return "vsock"
	case *pipeproxy.WSLDialer:
		return "socat"
	}
	return "unknown"
}
