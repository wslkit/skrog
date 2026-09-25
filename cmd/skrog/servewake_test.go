package main

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/supervise"
)

// okDialer succeeds and counts; the connection itself is never used.
type okDialer struct{ n atomic.Int32 }

func (d *okDialer) Dial(context.Context) (io.ReadWriteCloser, error) {
	d.n.Add(1)
	return nopRWC{}, nil
}

type nopRWC struct{}

func (nopRWC) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopRWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopRWC) Close() error                { return nil }

// An engine that is not idle is dialed at once, and the state files are left
// exactly as they were.
func TestWakingDialerDialsARunningEngineDirectly(t *testing.T) {
	dir := t.TempDir()
	inner := &okDialer{}
	d := &wakingDialer{inner: inner, stateDir: dir, running: func(context.Context) bool {
		t.Error("probed an engine that was not idle")
		return true
	}}
	if _, err := d.Dial(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inner.n.Load() != 1 {
		t.Errorf("dialed %d times, want 1", inner.n.Load())
	}
}

// The point of the dialer (#520): a remote client reaching an idle engine
// pokes the supervisor awake the way `skrog start` does, waits for it, and
// only then dials -- never dialing a down engine, whose socat fallback would
// boot the distro behind the supervisor's back.
func TestWakingDialerWakesAnIdleEngineBeforeDialing(t *testing.T) {
	dir := t.TempDir()
	if err := supervise.WriteEngineState(dir, supervise.EngineIdle); err != nil {
		t.Fatal(err)
	}
	inner := &okDialer{}
	var probes atomic.Int32
	d := &wakingDialer{inner: inner, stateDir: dir, poll: time.Millisecond,
		running: func(context.Context) bool {
			if inner.n.Load() != 0 {
				t.Error("dialed the engine before it was up")
			}
			return probes.Add(1) >= 3 // up on the third look
		}}
	if _, err := d.Dial(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := supervise.ReadEngineState(dir); got == supervise.EngineIdle {
		t.Error("the idle marker is still there: the supervisor was never poked")
	}
	if inner.n.Load() != 1 {
		t.Errorf("dialed %d times, want 1 once the engine was up", inner.n.Load())
	}
}

// Stopped beats idle, as in Demand: a remote client must not resurrect an
// engine someone stopped on purpose.
func TestWakingDialerRefusesAStoppedEngine(t *testing.T) {
	dir := t.TempDir()
	supervise.WriteEngineState(dir, supervise.EngineIdle)
	supervise.WriteDesired(dir, supervise.DesiredStopped)
	inner := &okDialer{}
	d := &wakingDialer{inner: inner, stateDir: dir, running: func(context.Context) bool { return true }}
	_, err := d.Dial(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("err = %v, want a refusal naming the stopped engine", err)
	}
	if inner.n.Load() != 0 {
		t.Error("dialed a stopped engine")
	}
	if supervise.ReadEngineState(dir) != supervise.EngineIdle {
		t.Error("the idle marker was removed for an engine that must stay stopped")
	}
}

// No supervisor to answer the poke: the connection fails with a reason, not a
// hang, and the engine is never dialed.
func TestWakingDialerGivesUpWhenNothingWakesTheEngine(t *testing.T) {
	dir := t.TempDir()
	supervise.WriteEngineState(dir, supervise.EngineIdle)
	inner := &okDialer{}
	d := &wakingDialer{inner: inner, stateDir: dir, poll: time.Millisecond,
		running: func(context.Context) bool { return false }}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := d.Dial(ctx); err == nil {
		t.Error("an engine that never came up was reported as dialed")
	}
	if inner.n.Load() != 0 {
		t.Error("dialed an engine that never came up")
	}
}

// serve publishes while it runs and withdraws the record when it exits, so a
// serve that stopped holds nothing awake.
func TestPublishRemoteServeWritesThenClears(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		publishRemoteServe(ctx, dir, &pipeproxy.Server{}, nil)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := supervise.ReadRemoteServe(dir); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never published its record")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if _, ok := supervise.ReadRemoteServe(dir); ok {
		t.Error("the record outlived the serve that wrote it")
	}
}
