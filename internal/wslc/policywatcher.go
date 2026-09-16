package wslc

import (
	"log/slog"
	"sync"
)

// PolicyWatcher keeps the deployed WSL container policy current.
//
// The policy lives in the registry, and the supervisor that enforces it runs
// for months — it starts at logon and survives sleep, resume and
// `wsl --shutdown`. Reading it once at startup therefore means a machine whose
// bridge started before an allowlist was deployed, or before an existing one
// was tightened, keeps enforcing the STALE, LOOSER policy until someone
// restarts it (#354). That is the wrong failure direction for an admission
// control whose whole purpose is to honour what an administrator deployed.
//
// This is the same bug internal/policy.Watcher already fixed for policy.yaml,
// and the fix is deliberately the same shape: re-read on each judged request.
// A container create is a human-scale event and reading four registry values
// is microseconds, so there is no staleness window to reason about and no
// refresh interval to tune.
//
// Two rules govern errors, and both exist so a transient failure can never
// widen what is enforced:
//
//   - a failed re-read keeps the last policy that was read successfully, rather
//     than falling back to the permissive zero value;
//   - the first read still fails closed at startup, where the caller can refuse
//     to serve at all. Refusing every request forever because the registry
//     blipped once would be a denial of service, so after that point the last
//     known good policy stands.
type PolicyWatcher struct {
	// read is indirected so tests can drive it without a registry.
	read func() (Policies, error)

	// Logger records policy transitions and re-read failures. Nil is silent.
	Logger *slog.Logger

	mu      sync.Mutex
	cur     Policies
	lastErr string
}

// NewPolicyWatcher returns a watcher seeded with a policy the caller has
// already read successfully — which is what makes the startup fail-closed
// check the caller's job rather than this type's.
func NewPolicyWatcher(initial Policies) *PolicyWatcher {
	return &PolicyWatcher{read: ReadPolicies, cur: initial}
}

// Policies returns the policy to enforce right now, re-reading it first.
func (w *PolicyWatcher) Policies() Policies {
	w.mu.Lock()
	defer w.mu.Unlock()

	next, err := w.read()
	if err != nil {
		// Keep enforcing what we last knew. Reported once, not on every
		// request, so a persistently broken key does not drown the log.
		if msg := err.Error(); msg != w.lastErr {
			w.lastErr = msg
			if w.Logger != nil {
				w.Logger.Warn("cannot re-read the WSL container policy; "+
					"continuing to enforce the last one read", "error", err)
			}
		}
		return w.cur
	}
	if w.lastErr != "" {
		w.lastErr = ""
		if w.Logger != nil {
			w.Logger.Info("the WSL container policy is readable again")
		}
	}

	if !samePolicies(w.cur, next) {
		if w.Logger != nil {
			w.Logger.Info("the deployed WSL container policy changed",
				"registry-allowlist", next.RegistryAllowlist,
				"privileged-allowed", next.PrivilegedAllowed,
				"containers-allowed", next.ContainersAllowed)
		}
		w.cur = next
	}
	return w.cur
}

// samePolicies compares two policies by value. The allowlist is order- and
// case-sensitive here on purpose: this decides whether to LOG a change, and
// reporting a reordered list once is better than missing a real edit.
func samePolicies(a, b Policies) bool {
	if a.ContainersAllowed != b.ContainersAllowed ||
		a.PrivilegedAllowed != b.PrivilegedAllowed ||
		len(a.RegistryAllowlist) != len(b.RegistryAllowlist) {
		return false
	}
	for i := range a.RegistryAllowlist {
		if a.RegistryAllowlist[i] != b.RegistryAllowlist[i] {
			return false
		}
	}
	return true
}
