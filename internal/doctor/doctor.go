// Package doctor turns the WSL2 quirk zoo into a diagnosable surface (#61).
//
// The design rule is one check = one file + one test: a check is a pure
// function from gathered Facts to a Result, so its logic is testable without a
// real WSL, PATH, engine, or registry. The impure part — reading the machine —
// lives once in Gather (host.go); everything downstream is deterministic.
//
// Checks are seeded from failures this project has actually hit, not
// hypotheticals: a missing docker credential helper (which broke `skrog
// migrate` live), PATH shadowing, WSL version skew, the supervisor/status
// disagreement, and session-0 readiness for unattended runs.
package doctor

import "context"

// Status is a check's verdict, ordered by severity so the worst result in a run
// decides the process exit code.
type Status int

const (
	// OK: the check passed; nothing to do.
	OK Status = iota
	// Skip: the check could not run (a precondition was absent), which is not a
	// failure — e.g. the engine is not installed, so engine checks are moot.
	Skip
	// Warn: something is off but docker still works; it will bite later.
	Warn
	// Fail: something is broken now.
	Fail
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Skip:
		return "skip"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return "unknown"
	}
}

// Result is one check's finding. Summary is the one-line verdict; Detail carries
// supporting lines; Remedy says what to do and is empty when OK or Skip.
type Result struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Status Status `json:"-"`
	// StatusText mirrors Status for JSON consumers, since Status marshals as an
	// opaque integer otherwise.
	StatusText string   `json:"status"`
	Summary    string   `json:"summary"`
	Detail     []string `json:"detail,omitempty"`
	Remedy     string   `json:"remedy,omitempty"`
	// Fixed records what `--fix` did on this run: "" when no fix ran.
	Fixed string `json:"fixed,omitempty"`
}

// Check is a single diagnosis. Run is pure over Facts. Fix, when present,
// remediates the problem the check reports and is invoked only by `--fix` and
// only when Run did not return OK or Skip; it must be safe to run when there is
// nothing to fix. A check without a Fix prints its Remedy for the user to apply.
type Check struct {
	Name  string
	Title string
	Run   func(Facts) Result
	Fix   func(ctx context.Context, f Facts) (done string, err error)
}

// Registry is the ordered list of checks doctor runs. Order is roughly
// foundation-first (WSL, then the CLI, then the engine, then the supervisor) so
// that reading top-to-bottom tells a story: the first Fail is usually the cause
// and the ones below it the symptoms.
func Registry() []Check {
	return []Check{
		checkWSL(),
		checkWSLFastPath(),
		checkDockerCLI(),
		checkCredentialHelper(),
		checkSSHAgent(),
		checkEngine(),
		checkContext(),
		checkSupervisor(),
		checkCLI(),
		checkNetwork(),
		checkVPN(),
		checkGPU(),
		checkMultiArch(),
		checkEmulation(),
		checkPruneIdle(),
		checkPublishedPorts(),
		checkDisk(),
		checkSession0(),
		checkRunner(),
		checkWSLSizing(),
		checkVirtiofs(),
		checkHooks(),
	}
}

// result is a small helper so checks read as data, not boilerplate.
func result(c Check, s Status, summary string) Result {
	return Result{
		Name:       c.Name,
		Title:      c.Title,
		Status:     s,
		StatusText: s.String(),
		Summary:    summary,
	}
}

// Run executes every check against already-gathered Facts and returns the
// results in registry order. It never runs fixes; see Fix.
func Run(reg []Check, f Facts) []Result {
	out := make([]Result, 0, len(reg))
	for _, c := range reg {
		out = append(out, c.Run(f))
	}
	return out
}

// Worst returns the most severe status across results, which is what a caller
// maps to an exit code. An empty run is OK.
func Worst(results []Result) Status {
	worst := OK
	for _, r := range results {
		if r.Status > worst {
			worst = r.Status
		}
	}
	return worst
}
