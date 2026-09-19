package supervise

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Automatic disk reclamation (#393).
//
// The engine's disk grows until somebody remembers to run `skrog prune`, and
// the failure mode — a full data volume — shows up as the engine misbehaving
// in ways that do not obviously say "disk". The supervisor already wakes on a
// timer and already knows whether the engine is up, so a periodic prune
// belongs here rather than in a scheduled task. `internal/autostart` uses the
// Run key rather than Task Scheduler, so there is no OS scheduler to reuse
// even if one were wanted.
//
// Three rules shape everything below, in the order they would hurt if broken:
//
//  1. OFF BY DEFAULT. A tool that deletes a user's images because a timer
//     fired, without being asked, deserves the distrust that follows.
//  2. NEVER VOLUMES. `skrog prune --volumes` exists for a human who typed it.
//     A timer must not be able to drop a database because nothing referenced
//     it this week. There is no setting for this, which is the point.
//  3. ALWAYS AN AGE GUARD. Config refuses to express "no guard", so the
//     window a user relies on cannot be removed by a config typo.

// PrunePolicy is the automatic-prune settings as the supervisor sees them.
type PrunePolicy struct {
	// Every is the interval between automatic prunes; zero means off.
	Every time.Duration
	// KeepSince is the age guard: nothing younger is touched. The config
	// layer defaults it, so a zero here means the caller built the policy by
	// hand and gets the conservative answer rather than an unguarded sweep.
	KeepSince time.Duration
	// BuildCache widens the prune to the BuildKit cache.
	BuildCache bool
}

// enabled reports whether automatic pruning should happen at all.
func (p PrunePolicy) enabled() bool { return p.Every > 0 }

// guard is the age window actually applied, never zero: a policy assembled
// without one prunes nothing younger than a week rather than everything.
func (p PrunePolicy) guard() time.Duration {
	if p.KeepSince <= 0 {
		return 168 * time.Hour
	}
	return p.KeepSince
}

func lastPrunePath(stateDir string) string {
	return filepath.Join(stateDir, "last-prune")
}

// ReadLastPrune returns when the supervisor last completed an automatic prune,
// or the zero time if it never has.
//
// This is on disk rather than in memory because the alternative is a prune on
// every supervisor start: the supervisor restarts at every logon, and an
// in-memory "last run" would be zero each time, which reads as "overdue".
func ReadLastPrune(stateDir string) time.Time {
	b, err := os.ReadFile(lastPrunePath(stateDir))
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}
	}
	return t
}

// WriteLastPrune records a completed prune atomically.
func WriteLastPrune(stateDir string, t time.Time) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	if err := commit(lastPrunePath(stateDir), []byte(t.UTC().Format(time.RFC3339)+"\n")); err != nil {
		return fmt.Errorf("committing last prune: %w", err)
	}
	return nil
}

// duePrune reports whether an automatic prune is owed, given the policy and
// when one last completed.
//
// A machine that has never pruned is NOT immediately due: the first interval
// is measured from now, recorded on the first tick that sees the policy
// enabled. Otherwise turning the setting on would delete things in the same
// breath, before the user had a chance to see what it was going to do.
func duePrune(p PrunePolicy, last, now time.Time) bool {
	if !p.enabled() || last.IsZero() {
		return false
	}
	return now.Sub(last) >= p.Every
}

// maybePrune reclaims disk on a schedule. Caller holds s.mu.
//
// The decision is made here and the work happens on its own goroutine: a prune
// on a large engine takes minutes, and the reconciler's mutex is the one a
// cold start needs, so doing it inline would stall every `docker` command that
// arrives while it ran.
func (s *Supervisor) maybePrune(ctx context.Context) {
	if s.PrunePolicy == nil || s.Prune == nil {
		return
	}
	policy := s.PrunePolicy()
	if !policy.enabled() {
		return
	}
	if s.pruning.Load() {
		return // one is already running; say nothing, it logs for itself
	}

	// First sight of an enabled policy starts the clock rather than firing.
	// "I turned it on and it deleted things immediately" is a bad first
	// experience for a feature whose whole job is to be trusted unattended.
	last := ReadLastPrune(s.Config.StateDir)
	if last.IsZero() {
		if err := WriteLastPrune(s.Config.StateDir, time.Now()); err != nil {
			s.log().Warn("could not record the automatic-prune clock", "error", err)
			return
		}
		s.log().Info("automatic prune scheduled", "every", policy.Every,
			"keepSince", policy.guard(), "buildCache", policy.BuildCache,
			"firstRun", time.Now().Add(policy.Every).Format(time.RFC3339))
		return
	}
	if !duePrune(policy, last, time.Now()) {
		return
	}

	// Running containers veto, for the same reason they veto an idle stop:
	// the supervisor must not remove things out from under work in progress,
	// and a build on a CI runner is exactly when losing the cache hurts most.
	// A machine that is always busy therefore never auto-prunes, which is the
	// conservative failure and is documented as such.
	if s.Busy == nil {
		return
	}
	busy, err := s.Busy(ctx)
	if err != nil || busy {
		return
	}

	s.pruning.Store(true)
	go s.runPrune(policy)
}

// runPrune performs the prune off the reconciler and records its completion.
func (s *Supervisor) runPrune(policy PrunePolicy) {
	defer s.pruning.Store(false)

	// Rule 3 (ALWAYS AN AGE GUARD) is applied HERE, to the value that leaves
	// this package, and not only to the log lines.
	//
	// It used to be applied only to the log lines. guard() appeared in three
	// places, all of them slog calls, while `s.Prune` received the raw policy
	// -- so cmd/skrog's autoPrune passed `Until: p.KeepSince` straight to
	// `docker image prune -a`. A PrunePolicy with KeepSince unset logged
	// "keepSince: 168h" and then ran with NO `--filter until=`: every unused
	// image, regardless of age. Exactly the "image someone pulled an hour ago
	// for tomorrow's demo" that rule 3 exists to protect.
	//
	// The shipped wiring never hit it, because cmd/skrog/supervise.go passes
	// c.KeepSince(), which defaults. But guard() is unexported, so autoPrune
	// *could not* have called it, and KeepSince's own doc comment promises
	// that a zero "gets the conservative answer rather than an unguarded
	// sweep" -- a promise nothing kept. Normalising at the boundary makes the
	// promise structural: no caller of s.Prune can be handed a zero.
	policy.KeepSince = policy.guard()

	// Bounded so a wedged engine cannot leave the guard set forever, which
	// would silently disable automatic pruning until the next restart.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	started := time.Now()
	s.log().Info("automatic prune starting",
		"keepSince", policy.guard(), "buildCache", policy.BuildCache)

	reclaimed, err := s.Prune(ctx, policy)
	if err != nil {
		// Not fatal and not retried early: the next interval comes around.
		// The clock is still recorded so a permanently failing prune cannot
		// turn into a prune attempt on every single tick.
		s.log().Error("automatic prune failed", "error", err,
			"took", time.Since(started).Round(time.Second))
	} else {
		// Logged, always, because a missing image needs an explanation
		// somewhere. `skrog logs` is where someone will look.
		s.log().Info("automatic prune done",
			"reclaimedBytes", reclaimed,
			"keepSince", policy.guard(),
			"took", time.Since(started).Round(time.Second))
	}
	if err := WriteLastPrune(s.Config.StateDir, time.Now()); err != nil {
		s.log().Warn("could not record the automatic-prune clock", "error", err)
	}
}
