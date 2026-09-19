package pipeproxy_test

import (
	"bufio"
	"io"
	"net"
	"strings"
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

// A 101 must be refused while the body writer is still live (#436).
//
// relayBuffered does Peek/Discard on clientR while the abandoned writer is
// still inside req.Body reading the same bufio.Reader. Two goroutines, one
// bufio.Reader, no synchronisation: the heap-corruption class of #166.
//
// What this test can and cannot show, stated plainly. It shows the REFUSAL:
// no 101 reaches the client and the relay returns an error. It does not show
// that corruption is prevented, because a data race on a bufio.Reader is not
// something a test can deterministically provoke — and the race detector will
// not see it either, which is exactly why this path survived review twice.
//
// So the guard is what is pinned here, and the argument for the guard lives in
// the comment at the call site.
//
// TestRewriteFallsBackToRawOn101 is the other half: a 101 with a body that DID
// finish still upgrades normally. That is the case `docker exec` and
// `docker attach` actually take, and it must keep working.
func TestHijackIsRefusedWhileTheBodyWriterIsLive(t *testing.T) {
	client, bridgeClient := net.Pipe()
	engineSide, bridgeEngine := net.Pipe()
	defer client.Close()
	defer engineSide.Close()

	done := make(chan error, 1)
	go func() { done <- pipeproxy.RewriteBinds(bridgeClient, bridgeEngine) }()

	// Engine: answer 101 immediately, then keep draining so the body writer is
	// stuck on the READ and stays live for the whole would-be session.
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
		engineSide.Write([]byte("HTTP/1.1 101 UPGRADED\r\n" +
			"Content-Type: application/vnd.docker.raw-stream\r\n" +
			"Connection: Upgrade\r\nUpgrade: tcp\r\n\r\n"))
		io.Copy(io.Discard, engineSide)
	}()

	// Client: an attach that promises a megabyte and then goes quiet.
	go func() {
		client.Write([]byte("POST /v1.55/containers/abc/attach?stream=1 HTTP/1.1\r\n" +
			"Host: d\r\nUpgrade: tcp\r\nConnection: Upgrade\r\n" +
			"Content-Length: 1048576\r\n\r\n"))
		client.Write(make([]byte, 16))
	}()

	// Nothing should arrive. Read with a deadline: a timeout is the pass.
	got := make(chan string, 1)
	go func() {
		client.SetReadDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 64)
		n, _ := client.Read(b)
		got <- string(b[:n])
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("RewriteBinds never returned")
	}
	if err == nil {
		t.Fatal("the upgrade was allowed with a live body writer; " +
			"relayBuffered and req.Write now share clientR (#436)")
	}
	if !strings.Contains(err.Error(), "436") {
		t.Errorf("error does not point at the reason: %v", err)
	}
	if s := <-got; strings.Contains(s, "101") {
		t.Errorf("a 101 reached the client, so the raw relay was entered: %q", s)
	}
}
