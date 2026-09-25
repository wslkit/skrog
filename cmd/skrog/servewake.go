package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/supervise"
)

// wakeWait bounds how long a remote client waits for an idle engine: the
// supervisor's cold start was 4-8 s measured, and a supervisor that is not
// there at all should fail the connection, not hang it.
const wakeWait = 2 * time.Minute

// wakingDialer wakes an idle-stopped engine for `skrog serve`.
//
// The supervisor's own pipe does this through Demand, in-process. `skrog serve`
// is another process, so it asks the way `skrog start` does: removing the idle
// marker is the wake-up poke the supervisor already watches for. It then waits
// for the engine through running, which lists before it probes -- the one
// thing it must not do is dial the engine while it is down, because the socat
// fallback's wsl.exe would boot the distro behind the supervisor's back (#82).
type wakingDialer struct {
	inner    pipeproxy.Dialer
	stateDir string
	running  func(context.Context) bool
	log      *slog.Logger
	// poll is how often the wait re-checks; zero is 500 ms.
	poll time.Duration
}

func (d *wakingDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	if supervise.ReadEngineState(d.stateDir) == supervise.EngineIdle {
		if err := d.wake(ctx); err != nil {
			return nil, err
		}
	}
	return d.inner.Dial(ctx)
}

func (d *wakingDialer) wake(ctx context.Context) error {
	// Stopped beats idle, as it does in Demand: a remote client must not
	// resurrect an engine someone stopped on purpose.
	if supervise.ReadDesired(d.stateDir) == supervise.DesiredStopped {
		return errors.New("engine is stopped; run `skrog start` on the serving machine")
	}
	if d.log != nil {
		d.log.Info("engine is idle; waking it for a remote client")
	}
	if err := supervise.WriteEngineState(d.stateDir, supervise.EngineActive); err != nil {
		return fmt.Errorf("waking the engine: %w", err)
	}
	poll := d.poll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, wakeWait)
	defer cancel()
	for {
		if d.running(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the engine did not wake within %s; is the supervisor running (`skrog status`)?", wakeWait)
		case <-time.After(poll):
		}
	}
}

// publishRemoteServe keeps the supervisor told about serve's traffic until the
// context ends, then withdraws the record so an exited serve holds nothing
// awake.
func publishRemoteServe(ctx context.Context, stateDir string, srv *pipeproxy.Server, log *slog.Logger) {
	write := func() {
		if err := supervise.WriteRemoteServe(stateDir, supervise.RemoteServe{
			At:           time.Now(),
			ActiveConns:  srv.ActiveConns(),
			LastActivity: srv.LastActivity(),
		}); err != nil && log != nil {
			log.Debug("could not publish remote-serve activity", "error", err)
		}
	}
	defer func() {
		if err := supervise.ClearRemoteServe(stateDir); err != nil && log != nil {
			log.Debug("could not clear remote-serve activity", "error", err)
		}
	}()
	t := time.NewTicker(supervise.RemoteServeInterval)
	defer t.Stop()
	write()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			write()
		}
	}
}
