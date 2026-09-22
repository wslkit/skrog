package portrelay

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// echoUpstream stands in for a published port inside the distro.
func echoUpstream(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

// freePort returns a port nothing holds, so the relay binds the SAME number the
// upstream uses -- which is the relay's actual contract: host port N goes to
// distro port N.
func upstreamOn(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Skipf("cannot bind 127.0.0.1:%d for the fake upstream: %v", port, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
}

// A relayed port carries traffic end to end.
func TestSyncBindsAndForwards(t *testing.T) {
	// Pick a free port, let it go, then have both the upstream and the relay
	// use that number.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	host, _ := echoUpstream(t)
	upstreamOn(t, port)

	r := New(func(context.Context) (string, error) { return host, nil }, quiet())
	defer r.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Sync(ctx, []Port{{Number: port, Container: "web"}})

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("the relayed port did not accept: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("nothing came back through the relay: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("got %q through the relay, want %q", buf, "ping")
	}
}

// A port that stops being published stops being bound.
//
// The failure this pins is a listener outliving its container: the port still
// answers, the connection goes nowhere, and the user gets a hang or a refusal
// from something that looks like it is running.
func TestSyncClosesListenersForPortsThatWentAway(t *testing.T) {
	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	r := New(func(context.Context) (string, error) { return "127.0.0.1", nil }, quiet())
	defer r.Close()
	ctx := context.Background()

	r.Sync(ctx, []Port{{Number: port, Container: "web"}})
	if got := r.Active(); len(got) != 1 {
		t.Fatalf("active = %v, want one port bound", got)
	}

	r.Sync(ctx, nil)
	if got := r.Active(); len(got) != 0 {
		t.Fatalf("active = %v after the port went away, want none", got)
	}
	// And the port is genuinely free again, which Active() alone does not show.
	ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("the port is still held after Sync dropped it: %v", err)
	}
	ln.Close()
}

// Sync is idempotent: the same set twice must not rebind.
//
// Rebinding drops every connection in flight on a set that did not change, and
// the relay is polled every few seconds -- so a rebinding Sync would sever a
// long-lived connection (an SSE stream, a websocket, a database session) on a
// timer, which is the kind of fault nobody traces back to a port relay.
//
// Asserted by round-tripping through an ALREADY-OPEN connection after the
// second Sync, because that is what a rebind actually breaks. An earlier
// version wrote one byte and checked for an error; a write to a socket whose
// peer has just gone away succeeds into the send buffer, so it passed with the
// rebind deliberately put back.
func TestSyncIsIdempotent(t *testing.T) {
	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	host, _ := echoUpstream(t)
	upstreamOn(t, port)

	r := New(func(context.Context) (string, error) { return host, nil }, quiet())
	defer r.Close()
	ctx := context.Background()

	r.Sync(ctx, []Port{{Number: port}})
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("first bind did not accept: %v", err)
	}
	defer conn.Close()
	if err := roundTrip(conn, "one"); err != nil {
		t.Fatalf("before the second Sync: %v", err)
	}

	r.Sync(ctx, []Port{{Number: port}})
	if got := r.Active(); len(got) != 1 {
		t.Fatalf("active = %v after an identical Sync, want one", got)
	}
	if got := r.bindCount(); got != 1 {
		t.Fatalf("bind count = %d after two identical Syncs, want 1: the port was "+
			"torn down and bound again, which drops every connection in flight", got)
	}
	if err := roundTrip(conn, "two"); err != nil {
		t.Errorf("an identical Sync severed a connection in flight: %v", err)
	}
}

// roundTrip writes to the relayed connection and reads the echo back.
func roundTrip(conn net.Conn, msg string) error {
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(msg)); err != nil {
		return err
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != msg {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// Close releases everything.
//
// Not politeness: these are host-wide binds, so a leaked listener means the
// NEXT supervisor cannot take the port and the user gets a refusal from a
// process that relays nothing.
func TestCloseReleasesEveryListener(t *testing.T) {
	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	r := New(func(context.Context) (string, error) { return "127.0.0.1", nil }, quiet())
	r.Sync(context.Background(), []Port{{Number: port}})
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("the port is still held after Close: %v", err)
	}
	ln.Close()

	// And a closed relay stays closed: a late Sync from a goroutine that has
	// not noticed shutdown must not bind anything new.
	r.Sync(context.Background(), []Port{{Number: port}})
	if got := r.Active(); len(got) != 0 {
		t.Errorf("a closed relay bound %v", got)
	}
}

// A port something else already holds is logged and skipped, and the others
// still bind. The container is dockerd's and runs fine either way.
func TestABusyPortDoesNotStopTheOthers(t *testing.T) {
	busy, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	freePort := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	r := New(func(context.Context) (string, error) { return "127.0.0.1", nil }, quiet())
	defer r.Close()

	r.Sync(context.Background(), []Port{
		{Number: busyPort, Container: "taken"},
		{Number: freePort, Container: "web"},
	})

	active := r.Active()
	if len(active) != 1 || active[0] != freePort {
		t.Errorf("active = %v, want only the free port %d", active, freePort)
	}
}

// An upstream that cannot be resolved closes the client connection rather than
// hanging it. A relay that accepts and then holds is worse than one that
// refuses: the caller waits on a socket nothing will ever answer.
func TestAnUnresolvableUpstreamDoesNotHangTheClient(t *testing.T) {
	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	r := New(func(context.Context) (string, error) {
		return "", io.ErrUnexpectedEOF
	}, quiet())
	defer r.Close()
	r.Sync(context.Background(), []Port{{Number: port}})

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	// The relay closes its side, so the read ends rather than blocking.
	if _, err := io.ReadAll(conn); err != nil {
		// A reset is also an ending; a timeout is not.
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("the relay accepted the connection and then hung it")
		}
	}
}
