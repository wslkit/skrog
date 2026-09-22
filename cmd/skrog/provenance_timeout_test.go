package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// wedgedDialer hands back a connection that accepts the request and then never
// answers -- the classic wedged engine (containerd hung, disk full). It is NOT
// a refused dial: the dial succeeds, which is exactly the case a dial-only
// timeout does not cover.
type wedgedDialer struct{ conns chan net.Conn }

func (d *wedgedDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	client, server := net.Pipe()
	select {
	case d.conns <- server:
	default:
	}
	return client, nil
}

// The engine round trip must be bounded by resolveTimeout, not just the dial.
//
// It was not: the first version wrote the request by hand and blocked in
// http.ReadResponse with no deadline, and the vsock dialer CLEARS the deadline
// it used for its own handshake before handing the connection over. So a
// wedged engine hung `docker run` forever with no output -- while the feature's
// own documentation promises it fails open.
func TestAttributableIsBoundedWhenTheEngineAcceptsAndNeverAnswers(t *testing.T) {
	d := &wedgedDialer{conns: make(chan net.Conn, 4)}
	p := &imageProvenance{stateDir: t.TempDir(), dialer: d}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Attributable("ghcr.io/org/img:1")
	}()

	// Generous: the point is that it returns at all, near resolveTimeout.
	select {
	case <-done:
	case <-time.After(resolveTimeout + 20*time.Second):
		t.Fatal("Attributable never returned against an engine that accepted the " +
			"connection and went silent; a container create would hang forever")
	}
}

// Same for the recording half, which runs on the bridge's relay goroutine: a
// hang there never returns from rewriteBinds, so the bridge's client counter
// never decrements and idle-stop is vetoed for the life of the process.
func TestRecordPullIsBoundedAgainstAWedgedEngine(t *testing.T) {
	d := &wedgedDialer{conns: make(chan net.Conn, 4)}
	p := &imageProvenance{stateDir: t.TempDir(), dialer: d}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.RecordPull("ghcr.io/org/img:1")
	}()

	select {
	case <-done:
	case <-time.After(resolveTimeout + 20*time.Second):
		t.Fatal("RecordPull never returned; the relay goroutine is stuck and " +
			"idle-stop is now vetoed forever")
	}
}
