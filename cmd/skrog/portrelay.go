package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/doctor"
	"github.com/wslkit/skrog/internal/portrelay"
	"github.com/wslkit/skrog/internal/provision"
)

// portRelayInterval is how often the supervisor reconciles the relayed set.
//
// Polling rather than the engine's event stream, and that is a deliberate
// trade. The event stream is a long-lived connection to the engine, and a
// long-lived connection is exactly what idle-stop counts as ACTIVITY -- so
// subscribing to events to relay ports would quietly prevent the engine ever
// idling, which is the bug #496 is open about. A short poll costs one listing
// and stamps the same clock, so the interval is kept above the idle timeout's
// resolution rather than being made snappy.
const portRelayInterval = 5 * time.Second

// runPortRelay keeps the host-side listeners in step with what the engine
// publishes, while `network.publish-scope` is `lan` (#508).
//
// Reads the setting every tick rather than capturing it at launch, for the
// reason #202 records: the supervisor outlives `skrog config set`, and a value
// captured at start is a setting that silently does not apply. Turning the
// scope back to loopback therefore tears the listeners down without a restart,
// which matters more here than for most settings -- the thing being turned off
// is a network exposure.
func runPortRelay(
	ctx context.Context,
	cfg *config.Watcher,
	ports func(context.Context) []doctor.PublishedPort,
	upstream portrelay.Upstream,
	log *slog.Logger,
) {
	relay := portrelay.New(upstream, log)
	// Closed on the way out, always: these are host-wide binds, and a leaked
	// listener means the next supervisor cannot take the port and the user gets
	// a refusal from something that relays nothing.
	defer relay.Close()

	t := time.NewTicker(portRelayInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		if cfg.Config().PublishScope != config.PublishScopeLAN {
			// Not an early `continue`: the scope may have just been turned OFF,
			// and the listeners have to go with it.
			relay.Sync(ctx, nil)
			continue
		}
		relay.Sync(ctx, relayPorts(ports(ctx)))
	}
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
