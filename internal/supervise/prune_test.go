package supervise

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDuePrune(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	week := PrunePolicy{Every: 168 * time.Hour}

	for _, tc := range []struct {
		name   string
		policy PrunePolicy
		last   time.Time
		want   bool
	}{
		{"off", PrunePolicy{}, now.Add(-999 * time.Hour), false},
		{"never run is not immediately due", week, time.Time{}, false},
		{"not yet", week, now.Add(-100 * time.Hour), false},
		{"exactly due", week, now.Add(-168 * time.Hour), true},
		{"overdue", week, now.Add(-400 * time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := duePrune(tc.policy, tc.last, now); got != tc.want {
				t.Errorf("duePrune = %v, want %v", got, tc.want)
			}
		})
	}
}

// Turning the setting on must not delete anything in the same breath. The
// first tick records the clock; the prune comes an interval later.
func TestFirstTickSchedulesRatherThanPrunes(t *testing.T) {
	dir := t.TempDir()
	var calls int
	s := &Supervisor{
		Config:      Config{StateDir: dir},
		PrunePolicy: func() PrunePolicy { return PrunePolicy{Every: time.Hour, KeepSince: time.Hour} },
		Prune: func(context.Context, PrunePolicy) (uint64, error) {
			calls++
			return 0, nil
		},
		Busy: func(context.Context) (bool, error) { return false, nil },
	}

	s.maybePrune(context.Background())

	if calls != 0 {
		t.Errorf("pruned %d times on the first sight of the policy; want 0", calls)
	}
	if ReadLastPrune(dir).IsZero() {
		t.Error("the clock was not started, so it would never become due")
	}
}

// The supervisor restarts at every logon. If the clock lived in memory, every
// logon would look like "never pruned" and then like "overdue".
func TestLastPruneSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	want := time.Date(2026, 9, 10, 8, 30, 0, 0, time.UTC)

	if err := WriteLastPrune(dir, want); err != nil {
		t.Fatalf("WriteLastPrune: %v", err)
	}
	if got := ReadLastPrune(dir); !got.Equal(want) {
		t.Errorf("ReadLastPrune = %v, want %v", got, want)
	}
	if got := ReadLastPrune(t.TempDir()); !got.IsZero() {
		t.Errorf("a fresh state dir reported %v; want the zero time", got)
	}
}

// Running containers veto, for the same reason they veto an idle stop.
func TestBusyVetoesPrune(t *testing.T) {
	for _, tc := range []struct {
		name string
		busy func(context.Context) (bool, error)
	}{
		{"containers running", func(context.Context) (bool, error) { return true, nil }},
		{"probe failed", func(context.Context) (bool, error) { return false, errors.New("boom") }},
		{"no probe wired", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Due: the clock is a fortnight old and the interval is an hour.
			if err := WriteLastPrune(dir, time.Now().Add(-336*time.Hour)); err != nil {
				t.Fatal(err)
			}
			var calls int
			s := &Supervisor{
				Config:      Config{StateDir: dir},
				PrunePolicy: func() PrunePolicy { return PrunePolicy{Every: time.Hour} },
				Prune: func(context.Context, PrunePolicy) (uint64, error) {
					calls++
					return 0, nil
				},
				Busy: tc.busy,
			}
			s.maybePrune(context.Background())
			if calls != 0 {
				t.Errorf("pruned despite %s", tc.name)
			}
		})
	}
}

// A prune runs off the reconciler, so the tick must not launch a second one
// while the first is still working.
func TestPruneDoesNotOverlap(t *testing.T) {
	dir := t.TempDir()
	if err := WriteLastPrune(dir, time.Now().Add(-336*time.Hour)); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var mu sync.Mutex
	var calls int
	s := &Supervisor{
		Config:      Config{StateDir: dir},
		PrunePolicy: func() PrunePolicy { return PrunePolicy{Every: time.Hour} },
		Busy:        func(context.Context) (bool, error) { return false, nil },
		Prune: func(context.Context, PrunePolicy) (uint64, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			<-release
			return 0, nil
		},
	}

	s.maybePrune(context.Background())
	// Wait for the goroutine to be in flight before asking again.
	for i := 0; i < 100 && !s.pruning.Load(); i++ {
		time.Sleep(time.Millisecond)
	}
	s.maybePrune(context.Background())
	s.maybePrune(context.Background())

	close(release)
	for i := 0; i < 200 && s.pruning.Load(); i++ {
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("Prune called %d times; want 1 — a second was launched over the first", calls)
	}
}

// A failing prune must still move the clock, or a permanently broken engine
// turns into a prune attempt on every tick forever.
func TestFailedPruneStillRecordsTheClock(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().Add(-336 * time.Hour)
	if err := WriteLastPrune(dir, before); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{
		Config:      Config{StateDir: dir},
		PrunePolicy: func() PrunePolicy { return PrunePolicy{Every: time.Hour} },
		Busy:        func(context.Context) (bool, error) { return false, nil },
		Prune: func(context.Context, PrunePolicy) (uint64, error) {
			return 0, errors.New("engine is wedged")
		},
	}

	s.runPrune(PrunePolicy{Every: time.Hour})

	if got := ReadLastPrune(dir); !got.After(before) {
		t.Errorf("clock still at %v after a failed prune; it must advance", got)
	}
}

// The guard can never be zero, whatever the caller passes. This is the rule
// that stops an automatic sweep taking an image pulled an hour ago.
func TestPolicyGuardIsNeverZero(t *testing.T) {
	if got := (PrunePolicy{}).guard(); got <= 0 {
		t.Fatalf("an unset guard resolved to %v; automatic prunes must always keep a window", got)
	}
	if got := (PrunePolicy{KeepSince: -5 * time.Hour}).guard(); got <= 0 {
		t.Errorf("a negative guard resolved to %v", got)
	}
	if got := (PrunePolicy{KeepSince: 3 * time.Hour}).guard(); got != 3*time.Hour {
		t.Errorf("guard = %v, want the configured 3h", got)
	}
}
