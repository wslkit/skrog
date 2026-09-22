// Package portrelay publishes a WSL2 engine's container ports beyond the
// Windows loopback address (#508).
//
// Under WSL2's default NAT networking, `docker run -p 8080:80` ends up bound to
// 127.0.0.1 on the Windows side: dockerd publishes 0.0.0.0:8080 inside the
// distro, and WSL's own localhostForwarding relay binds loopback only. So the
// container answers on the machine that started it and nowhere else -- not to a
// phone on the same wifi, not to a colleague, not to an agent on another host.
// docs/ports.md carries the measurement.
//
// This package closes that gap by doing on the host what WSL declines to: bind
// 0.0.0.0 and forward into the distro. It is OFF by default and turned on with
// `skrog config set network.publish-scope lan`, because binding every published
// port to every interface changes the machine's exposure, and that is a
// decision rather than a convenience to discover after the fact.
//
// Deliberately NOT a general port forwarder. It relays exactly the ports the
// engine reports as published, appearing and disappearing with the containers
// that published them: a listener that outlives its container is a port that
// answers connection-refused from somewhere surprising.
package portrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// Port is one published TCP port the relay should carry.
type Port struct {
	// Number is the host-side port dockerd published.
	Number int
	// Container names the container that published it, for the log line. A
	// relay that says "port 8080" and not which container is a relay whose log
	// is useless on a machine running six things.
	Container string
}

// Upstream resolves where to forward to: the engine distro's address, which
// NAT reassigns on every boot.
//
// A function rather than a string because the answer expires. The relay
// re-resolves after a failed dial rather than on a timer, so a distro that
// restarted is repaired by the first connection that notices, and a machine
// nobody connects to pays nothing.
type Upstream func(ctx context.Context) (string, error)

// dialTimeout bounds one upstream connection.
const dialTimeout = 5 * time.Second

// Relay keeps one listener per published port.
//
// Safe for concurrent use. Sync is the only entry point that changes the set,
// and it is idempotent: call it with the same ports and nothing happens.
type Relay struct {
	upstream Upstream
	log      *slog.Logger

	mu     sync.Mutex
	active map[int]*listener
	closed bool
	// binds counts every successful bind, so a test can tell "the same port is
	// still bound" from "the port was torn down and bound again". The two look
	// identical through Active(), and only the second severs connections.
	binds int
}

type listener struct {
	ln        net.Listener
	container string
	cancel    context.CancelFunc
}

// New returns a relay that forwards to upstream. Nothing binds until Sync.
func New(upstream Upstream, log *slog.Logger) *Relay {
	return &Relay{
		upstream: upstream,
		log:      log,
		active:   map[int]*listener{},
	}
}

// Sync makes the bound set match want: it binds ports that appeared and closes
// listeners whose ports are gone.
//
// Errors are logged, never returned. A port that cannot be bound -- something
// else on Windows already holds it, the firewall refuses -- must not stop the
// others and must not fail anything the caller is doing: the container is
// dockerd's and runs fine either way. The relay is an addition to a working
// system and behaves like one.
func (r *Relay) Sync(ctx context.Context, want []Port) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}

	keep := make(map[int]bool, len(want))
	for _, p := range want {
		keep[p.Number] = true
	}
	for port, l := range r.active {
		if keep[port] {
			continue
		}
		r.logger().Info("no longer publishing a container port beyond loopback",
			"port", port, "container", l.container)
		r.stopLocked(port)
	}

	for _, p := range want {
		if _, ok := r.active[p.Number]; ok {
			continue
		}
		r.startLocked(ctx, p)
	}
}

// startLocked binds one port. Caller holds mu.
func (r *Relay) startLocked(ctx context.Context, p Port) {
	// Explicitly 0.0.0.0 rather than an empty host: which address gets bound is
	// the entire point of this package, and a reader should not have to know
	// Go's default to know the answer.
	//
	// Measured on Windows, and worth stating because it is more than the string
	// says: netstat then reports BOTH `0.0.0.0:<port>` and `[::]:<port>`, and
	// the port answers over IPv6 as well as IPv4. So "lan" means every
	// interface in both families, not just the v4 ones.
	ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(p.Number))
	if err != nil {
		// Usually something else already holds the port -- including a previous
		// supervisor that has not finished exiting.
		r.logger().Warn("could not publish a container port beyond loopback",
			"port", p.Number, "container", p.Container, "error", err)
		return
	}

	lctx, cancel := context.WithCancel(ctx)
	l := &listener{ln: ln, container: p.Container, cancel: cancel}
	r.active[p.Number] = l
	r.binds++
	r.logger().Info("publishing a container port beyond loopback",
		"port", p.Number, "container", p.Container, "bind", "0.0.0.0")

	go r.serve(lctx, l)
}

// serve accepts until the listener is closed.
func (r *Relay) serve(ctx context.Context, l *listener) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			// Closed on purpose, or the listener died. Either way this
			// goroutine is finished; Sync owns the map entry.
			return
		}
		go r.forward(ctx, conn, l.ln.Addr())
	}
}

// forward joins one accepted connection to the engine distro.
func (r *Relay) forward(ctx context.Context, client net.Conn, addr net.Addr) {
	defer client.Close()

	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return
	}

	host, err := r.upstream(ctx)
	if err != nil {
		r.logger().Debug("could not resolve the engine address for a relayed port",
			"port", portStr, "error", err)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var d net.Dialer
	up, err := d.DialContext(dctx, "tcp", net.JoinHostPort(host, portStr))
	if err != nil {
		r.logger().Debug("could not reach a relayed port inside the engine",
			"port", portStr, "upstream", host, "error", err)
		return
	}
	defer up.Close()

	// Both directions, and the connection is done when EITHER finishes. A
	// half-closed relay leaks a goroutine per connection, which on a busy dev
	// machine is how a supervisor up for a week ends up holding thousands.
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, client); done <- struct{}{} }()
	go func() { io.Copy(client, up); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// stopLocked closes one listener. Caller holds mu.
func (r *Relay) stopLocked(port int) {
	l, ok := r.active[port]
	if !ok {
		return
	}
	l.cancel()
	l.ln.Close()
	delete(r.active, port)
}

// Close releases every listener.
//
// Called on supervisor shutdown, and not out of politeness: these are host-wide
// TCP binds. Leaving one behind means the port stays held by a process that is
// no longer relaying anything, so the next supervisor cannot bind it and the
// user gets a refusal from a listener that answers nothing.
func (r *Relay) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	var errs []error
	for port, l := range r.active {
		if err := l.ln.Close(); err != nil {
			errs = append(errs, fmt.Errorf("port %d: %w", port, err))
		}
		l.cancel()
		delete(r.active, port)
	}
	return errors.Join(errs...)
}

// Active reports the ports currently bound, for tests and `skrog status`.
func (r *Relay) Active() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, len(r.active))
	for port := range r.active {
		out = append(out, port)
	}
	return out
}

func (r *Relay) logger() *slog.Logger {
	if r.log != nil {
		return r.log
	}
	return slog.Default()
}

// binds reports how many listeners have been opened over this relay's life.
//
// For tests: "port 8080 is bound" and "port 8080 was torn down and bound again"
// are indistinguishable through Active(), and only the second drops every
// connection in flight.
func (r *Relay) bindCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.binds
}
