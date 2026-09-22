package main

import (
	"strings"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/supervise"
)

// The stand-in supervisor is the real thing minus the engine: it holds the
// real single-instance claim and polls for the real request file, which is
// all watchForRestart does about being asked to stop. Faking either half
// would test the fake.
func fakeSupervisor(t *testing.T, stateDir string) (running func() bool, stop func()) {
	t.Helper()
	lock, err := supervise.Acquire(stateDir)
	if err != nil {
		t.Fatalf("acquiring the supervisor claim: %v", err)
	}
	// Belt and braces: a test that fails before the goroutine releases the
	// claim must not leave it held for the next one.
	t.Cleanup(func() { lock.Close() })

	if !supervise.Held(stateDir) {
		t.Fatal("the claim was acquired but does not read as held; the rest of this test proves nothing")
	}

	done := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-quit:
				return
			default:
			}
			if supervise.RestartRequested(stateDir) {
				// Same order as watchForRestart: clear the note first, so a
				// replacement does not find it and exit on sight.
				supervise.ClearRestart(stateDir)
				lock.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return func() bool {
			select {
			case <-done:
				return false
			default:
				return true
			}
		}, func() {
			close(quit)
			<-done
		}
}

func TestStopSupervisorWaitsForTheClaimToDrop(t *testing.T) {
	dir := t.TempDir()
	running, stop := fakeSupervisor(t, dir)
	defer stop()

	if err := stopSupervisor(dir); err != nil {
		t.Fatalf("stopSupervisor: %v", err)
	}

	// Returning is a promise that the supervisor is gone: `uninstall` goes
	// straight on to unregister the distro it was serving, and `restart
	// --supervisor` goes straight on to spawn a replacement that would fail
	// to get the claim.
	if supervise.Held(dir) {
		t.Error("stopSupervisor returned while the single-instance claim is still held")
	}
	if running() {
		t.Error("stopSupervisor returned while the supervisor is still running")
	}
	// A note that outlives the stop shuts down the NEXT supervisor on sight.
	if supervise.RestartRequested(dir) {
		t.Error("the restart request survived the stop")
	}
}

func TestStopSupervisorIsANoOpWhenNoneIsRunning(t *testing.T) {
	dir := t.TempDir()

	if err := stopSupervisor(dir); err != nil {
		t.Fatalf("stopSupervisor with nothing running: %v", err)
	}
	// The dangerous failure is not the error, it is the litter: a request
	// written for a supervisor that was already gone would make the next one
	// exit the moment it starts. `uninstall` runs this on installs that have
	// no supervisor at all, so this is the common case, not the edge.
	if supervise.RestartRequested(dir) {
		t.Error("left a restart request behind for an install with no supervisor")
	}
}

func TestStopSupervisorGivesUpAndWithdrawsTheRequest(t *testing.T) {
	dir := t.TempDir()

	// A supervisor that never answers: the claim is held and nothing is
	// watching for the note.
	lock, err := supervise.Acquire(dir)
	if err != nil {
		t.Fatalf("acquiring the supervisor claim: %v", err)
	}
	defer lock.Close()

	err = stopSupervisorWithin(dir, 300*time.Millisecond)
	if err == nil {
		t.Fatal("stopSupervisorWithin reported success while the claim is still held")
	}
	// The caller decides what to do about it — `uninstall` warns and carries
	// on, `restart --supervisor` refuses to spawn a second one — so the error
	// has to say where to look rather than just that something went wrong.
	if !strings.Contains(err.Error(), "did not exit") || !strings.Contains(err.Error(), "supervisor.log") {
		t.Errorf("error %q should say the supervisor did not exit and where its log is", err)
	}
	// Withdrawn, not left armed: a supervisor that was merely slow must not
	// shut itself down minutes later, long after this command gave up and
	// (for uninstall) the install it belonged to stopped existing.
	if supervise.RestartRequested(dir) {
		t.Error("the restart request was left armed after the wait expired")
	}
}
