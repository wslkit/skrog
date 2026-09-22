package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/doctor"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/portrelay"
	"github.com/wslkit/skrog/internal/provision"
)

// portRelayInterval is how often the supervisor reconciles the relayed set
// when nothing has told it to sooner.
//
// A backstop, not the mechanism (#510). The mechanism is the engine's event
// stream: a container starting or dying re-syncs the relay at once, so a LAN
// client does not sit on connection-refused for up to one interval after
// `docker run -p`. The poll stays for whatever the stream cannot promise -- a
// stream that dropped and has not re-subscribed yet, and a scope that was just
// turned on or off.
//
// An earlier version of this comment said the event stream would count as
// activity and keep the engine from idling. It does not: idle-stop counts
// connections through the PIPE (pipeproxy.Server), and the relay dials the
// engine directly, as the busy probe does. Idle-stop also needs zero running
// containers, and without running containers nothing is published.
const portRelayInterval = 5 * time.Second

// portEventsRetry is how long the watcher waits before subscribing again after
// the stream ends -- an engine restart, an idle stop, the socat fallback's
// inactivity timer. Same as the poll: sooner than that buys nothing the poll
// would not already have caught.
const portEventsRetry = 5 * time.Second

// relayDeps is what the relay loop reads, as functions so a test can script
// each one.
type relayDeps struct {
	// scopeLAN reports network.publish-scope, read on every pass (#202).
	scopeLAN func() bool
	// serving reports whether the engine may be dialed without booting it.
	serving func() bool
	// ports lists what running containers publish.
	ports func(context.Context) []doctor.PublishedPort
	// watch blocks while subscribed to container start/die events, calling
	// changed once when the subscription is established (anything that
	// happened before it is otherwise missed) and once per event. Nil runs the
	// loop on the poll alone.
	watch func(ctx context.Context, changed func()) error

	interval time.Duration
	retry    time.Duration
	log      *slog.Logger
}

// runPortRelay keeps the host-side listeners in step with what the engine
// publishes, while `network.publish-scope` is `lan` (#508).
//
// Reads the setting every pass rather than capturing it at launch, for the
// reason #202 records: the supervisor outlives `skrog config set`, and a value
// captured at start is a setting that silently does not apply. Turning the
// scope back to loopback therefore tears the listeners down without a restart,
// which matters more here than for most settings -- the thing being turned off
// is a network exposure.
func runPortRelay(
	ctx context.Context,
	cfg *config.Watcher,
	serving func() bool,
	dialer pipeproxy.Dialer,
	upstream portrelay.Upstream,
	log *slog.Logger,
) {
	// Resolving the address execs in the distro, which boots a stopped one, so
	// it takes the same gate as the dials below.
	relay := portrelay.New(func(ctx context.Context) (string, error) {
		if !serving() {
			return "", errEngineNotServing
		}
		return upstream(ctx)
	}, log)
	// Closed on the way out, always: these are host-wide binds, and a leaked
	// listener means the next supervisor cannot take the port and the user gets
	// a refusal from something that relays nothing.
	defer relay.Close()

	// Every background dial goes through the serving gate, including the ones
	// the transport makes on its own: the socat fallback runs wsl.exe, and
	// wsl.exe boots a stopped distro (#82).
	gated := &servingDialer{serving: serving, inner: dialer}
	relayLoop(ctx, relayDeps{
		scopeLAN: func() bool { return cfg.Config().PublishScope == config.PublishScopeLAN },
		serving:  serving,
		ports:    publishedPorts(gated, log),
		watch:    containerEvents(gated),
		interval: portRelayInterval,
		retry:    portEventsRetry,
		log:      log,
	}, relay.Sync)
}

// relayLoop is runPortRelay's body: sync on every tick and on every kick from
// the event watcher, and never dial an engine that is not serving.
func relayLoop(ctx context.Context, d relayDeps, sync func(context.Context, []portrelay.Port)) {
	// One slot: a burst of events (a compose stack coming up) collapses into
	// one re-sync, and the sync that runs lists the state after all of them.
	kick := make(chan struct{}, 1)
	changed := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	if d.watch != nil {
		go watchLoop(ctx, d, changed)
	}

	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}

		// Not an early `continue`: the scope may have just been turned OFF, or
		// the engine just stopped, and the listeners have to go with it. A
		// listener that outlives its container answers connection-refused
		// from somewhere surprising.
		if !d.scopeLAN() || !d.serving() {
			sync(ctx, nil)
			continue
		}
		sync(ctx, relayPorts(d.ports(ctx)))
	}
}

// watchLoop keeps one event subscription open while the relay has a reason to
// want one, and re-subscribes after it ends.
func watchLoop(ctx context.Context, d relayDeps, changed func()) {
	for ctx.Err() == nil {
		if d.scopeLAN() && d.serving() {
			if err := d.watch(ctx, changed); err != nil && ctx.Err() == nil && d.log != nil {
				// Debug: an idle stop ends the stream every time, and that is
				// the design working, not a fault worth a line in the log.
				d.log.Debug("container event stream ended", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.retry):
		}
	}
}

// containerEvents subscribes to container start and die events.
//
// Start and die only: a container's published ports are allocated before it
// starts and released when it dies, so those two are the only moments the
// relayed set can change. Stop, kill and restart all end in one of them.
func containerEvents(dialer pipeproxy.Dialer) func(ctx context.Context, changed func()) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				rwc, err := dialer.Dial(ctx)
				if err != nil {
					return nil, err
				}
				return rwcConn{rwc}, nil
			},
			DisableKeepAlives: true,
		},
	}
	filters := url.QueryEscape(`{"type":["container"],"event":["start","die"]}`)
	return func(ctx context.Context, changed func()) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://engine/v1.44/events?filters="+filters, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("engine answered %s to the event subscription", resp.Status)
		}
		// Subscribed: whatever started between the last sync and now would
		// otherwise wait for the next tick.
		changed()

		dec := json.NewDecoder(resp.Body)
		for {
			var ev struct {
				Action string `json:"Action"`
			}
			if err := dec.Decode(&ev); err != nil {
				if errors.Is(err, io.EOF) {
					return errors.New("the engine closed the event stream")
				}
				return err
			}
			changed()
		}
	}
}

// errEngineNotServing is a background dial refused by the serving gate.
var errEngineNotServing = errors.New("engine is not serving; not dialing it from the background")

// servingDialer refuses to dial while the engine is not serving.
//
// A dial is where a background reader boots a stopped engine by accident: the
// vsock agent is not there, the fallback runs `wsl.exe -d skrog-engine socat`,
// and wsl.exe starts the distro to run it (#82). The relay's own gate checks
// before a pass; this checks again at the moment of the dial, which is the
// only check that also covers a pass that began just before an idle stop.
type servingDialer struct {
	serving func() bool
	inner   pipeproxy.Dialer
}

func (d *servingDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	if !d.serving() {
		return nil, errEngineNotServing
	}
	return d.inner.Dial(ctx)
}

// distroUpstream resolves the engine distro's address for the relay.
//
// Re-resolved on every call rather than cached: WSL's NAT reassigns the address
// on each distro start, so a cached value is wrong precisely when the engine
// has just been restarted -- which is when someone is most likely to be
// retrying a connection.
func distroUpstream(p *provision.Provisioner, opts provision.Options) portrelay.Upstream {
	return func(ctx context.Context) (string, error) {
		out, err := p.DistroAddress(ctx, opts)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out), nil
	}
}
