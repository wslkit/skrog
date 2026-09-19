package pipeproxy_test

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/pipeproxy"
)

// A client that stalls mid-upload must not wedge the bridge (#435).
//
// TestEarlyErrorOnAbandonedUploadIsSalvaged already covers the other half of
// this: the engine answers early and stops reading, so the body writer is
// blocked WRITING, and engine.Close() frees it. That case always worked.
//
// This is the case that did not. The engine keeps draining, so the writer is
// blocked READING a client that has gone quiet — and engine.Close() cannot
// touch a read. The teardown then sat on `<-bodySent` forever, which meant
// rewriteBinds never returned, Server.handle never ran s.clients.Add(-1),
// ActiveConns never dropped, and idle-stop was dead for the life of the
// process.
//
// The assertion is deliberately just "it returns". That is the whole bug.
func TestStalledUploadDoesNotWedgeTheConnection(t *testing.T) {
	client, bridgeClient := net.Pipe()
	engineSide, bridgeEngine := net.Pipe()
	defer client.Close()
	defer engineSide.Close()

	done := make(chan error, 1)
	go func() { done <- pipeproxy.RewriteBinds(bridgeClient, bridgeEngine) }()

	// Engine: read the head, answer at once, then KEEP DRAINING. The draining
	// is the point — it guarantees the body writer can always write, so the
	// only thing it can be stuck on is the read.
	go func() {
		br := bufio.NewReader(engineSide)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		engineSide.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 3\r\n\r\nbad"))
		io.Copy(io.Discard, engineSide)
	}()

	// Client: promise a megabyte, send a handful of bytes, then go silent —
	// a sleeping laptop, a dropped VPN, a killed CLI.
	go func() {
		client.Write([]byte("POST /build HTTP/1.1\r\nHost: d\r\nContent-Length: 1048576\r\n\r\n"))
		client.Write(make([]byte, 16))
	}()
	// Drain the response, or relaying it would block on the unbuffered pipe
	// and the test would stall for a reason that is not the one under test.
	go io.Copy(io.Discard, client)

	select {
	case <-done:
		// Returned. Whether it returned an error does not matter: the
		// connection is written off either way, and the caller's deferred
		// bookkeeping is what had to run.
	case <-time.After(30 * time.Second):
		t.Fatal("RewriteBinds never returned on a stalled upload: the connection is wedged, " +
			"so ActiveConns never drops and idle-stop is disabled for the life of the process (#435)")
	}
}

// The bounded wait must not fire in the ordinary case. If abandonBody always
// waited out its grace, every abandoned upload would add seconds to a
// connection's teardown, which would turn a correctness fix into a
// latency bug nobody attributed to it.
func TestAbandonedUploadTearsDownPromptly(t *testing.T) {
	client, bridgeClient := net.Pipe()
	engineSide, bridgeEngine := net.Pipe()
	defer client.Close()
	defer engineSide.Close()

	done := make(chan error, 1)
	go func() { done <- pipeproxy.RewriteBinds(bridgeClient, bridgeEngine) }()

	go func() {
		br := bufio.NewReader(engineSide)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		engineSide.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 3\r\n\r\nbad"))
		io.Copy(io.Discard, engineSide)
	}()
	go func() {
		client.Write([]byte("POST /build HTTP/1.1\r\nHost: d\r\nContent-Length: 1048576\r\n\r\n"))
		client.Write(make([]byte, 16))
	}()
	go io.Copy(io.Discard, client)

	start := time.Now()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("RewriteBinds never returned (#435)")
	}
	// 250ms grace + teardown. The 5s bound in abandonBody is the failsafe for
	// a writer neither close reached, not the expected path.
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("teardown took %v; the bounded wait is being waited out rather than "+
			"the writer being unblocked", took)
	}
}
