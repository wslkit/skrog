package compact_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/compact"
)

// Ctrl-C during the wait must be reported as an interruption, not a timeout.
//
// The wait used to return the same "not ok" for a cancelled context and an
// expired deadline, so an interrupted run produced ErrStillHeld — telling the
// user "the WSL utility VM still had the disk after 11s; it releases it about a
// minute after the last distro stops" and advising a re-run or a longer
// --wait. A diagnosis of a problem that did not happen (#243).
func TestInterruptedWaitIsNotReportedAsATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// freeAfter far beyond what the test will reach: the disk never frees, so
	// only the cancel can end the wait.
	disk := &fakeDisk{size: 10, after: 10, freeAfter: 1 << 30}
	r := &compact.Runner{
		WSL:   &fakeWSL{},
		Disk:  disk,
		Stop:  func(context.Context) error { return nil },
		Start: func(context.Context) error { return nil },
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	o := opts()
	o.NoTrim = true
	o.Wait = 30 * time.Second // far beyond the cancel, so a timeout cannot be the cause
	_, err := r.Run(ctx, o)
	if err == nil {
		t.Fatal("expected the interrupted run to fail")
	}

	var still *compact.ErrStillHeld
	if errors.As(err, &still) {
		t.Errorf("an interrupted wait was reported as a timeout: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancellation is not in the error chain: %v", err)
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("the message does not say it was interrupted: %v", err)
	}
}
