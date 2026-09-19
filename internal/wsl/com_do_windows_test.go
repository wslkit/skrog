//go:build windows

package wsl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeApartment runs comSession's loop without COM, so do()'s behaviour can be
// tested on any Windows machine rather than only one with a live wslservice.
// The loop is byte-for-byte the shape of the real one in newCOMSession.
func fakeApartment(t *testing.T, timeout time.Duration) *comSession {
	t.Helper()
	s := &comSession{
		calls:       make(chan func()),
		stop:        make(chan struct{}),
		callTimeout: timeout,
	}
	go func() {
		for {
			select {
			case fn := <-s.calls:
				fn()
			case <-s.stop:
				return
			}
		}
	}()
	t.Cleanup(s.Close)
	return s
}

// A panicking call must not take the process down, must not read as success,
// and must not kill the apartment loop (#437).
//
// All three matter separately. Without the recover, a panic on the apartment
// goroutine unwinds past the loop and kills the whole supervisor — bridge and
// reconciler included. With a recover but no error, `list` would return an
// empty slice and a nil error, so a machine with distros would report having
// none. And if the loop died, every later call would block until its context
// fired, which on the supervisor's path is never.
//
// There is a real trigger: unsafe.Slice(arr, count) panics if the service
// returns count > 0 with a nil array, and hr == 0 is the only guard.
func TestCOMCallPanicIsReportedAndTheLoopSurvives(t *testing.T) {
	s := fakeApartment(t, time.Second)

	err := s.do(context.Background(), func() { panic("boom") })
	if err == nil {
		t.Fatal("a panicking call reported success; `list` would return no distros and no error")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("error does not say it panicked: %v", err)
	}

	// The loop has to still be there. This is the assertion that would catch
	// a recover placed at the loop level instead of per call.
	ran := false
	if err := s.do(context.Background(), func() { ran = true }); err != nil {
		t.Fatalf("the apartment loop did not survive the panic: %v", err)
	}
	if !ran {
		t.Error("the call after the panic never ran")
	}
}

// A call that never returns must not park the caller forever (#437).
//
// This is the whole bug: the supervisor passes its process-lifetime context
// down, so before this bound there was nothing to stop `do` waiting for the
// life of the process — while tick held the reconciler's mutex.
func TestCOMCallIsBoundedEvenWithAContextThatNeverFires(t *testing.T) {
	s := fakeApartment(t, 150*time.Millisecond)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	// context.Background() never fires — exactly what the supervisor passes.
	err := s.do(context.Background(), func() { <-release })
	took := time.Since(start)

	if err == nil {
		t.Fatal("a call that never returned reported success")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if took > 5*time.Second {
		t.Errorf("took %v: the bound is not being applied", took)
	}
}

// The caller's own deadline must still win when it is sooner, and must be
// distinguishable — Fast.List demotes the fast path on our timeout but passes
// the caller's cancellation straight through without demoting.
func TestCallersOwnDeadlineStillWins(t *testing.T) {
	s := fakeApartment(t, 30*time.Second)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := s.do(ctx, func() { <-release }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v; the caller's shorter deadline did not win", took)
	}
	if ctx.Err() == nil {
		t.Error("the caller's context should be the one that fired, which is how " +
			"Fast.List tells the two apart")
	}
}

// An ordinary call still works, and is not slowed by any of the above.
func TestCOMCallStillRunsNormally(t *testing.T) {
	s := fakeApartment(t, time.Second)
	got := 0
	if err := s.do(context.Background(), func() { got = 42 }); err != nil {
		t.Fatalf("do: %v", err)
	}
	if got != 42 {
		t.Errorf("fn did not run on the apartment thread (got %d)", got)
	}
}
