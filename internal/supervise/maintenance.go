package supervise

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A maintenance hold pauses the supervisor's reconciler while another command
// is operating on the distro (#486).
//
// The hazard is specific and was measured rather than theorised. `skrog engine
// upgrade` stops the engine, then copies binaries in with `wsl --exec`. The
// exec BOOTS the distro it runs in, dockerd comes up with it, and the next
// reconciler tick sees `desired == stopped && up` and terminates the distro --
// which kills every process in it, including the copy. The result is `exit
// status 1` with no output at all, on whichever file the copy had reached, and
// it succeeded or failed on tick timing alone.
//
// Writing the desired state is not enough on its own, and that is exactly what
// the upgrade already did: honoring "stopped" is what makes the supervisor
// terminate the distro. The two commands agree about the goal and fight over
// the route.
//
// So: a hold says "someone else is driving, do nothing at all", which is
// different from any desired state and cannot be expressed as one.
//
// It carries an EXPIRY rather than being a plain flag, because the holder can
// die. A crashed upgrade must not leave a supervisor that has quietly stopped
// reconciling forever -- that failure would look like the engine simply never
// recovering, with nothing in any log to say why. A stale hold expires and the
// supervisor resumes on its own.

func holdPath(stateDir string) string { return filepath.Join(stateDir, "maintenance-hold") }

// Hold pauses the reconciler until at least `until`, and returns a release
// function. Callers defer the release; the expiry is the backstop for the
// process that never reaches it.
func Hold(stateDir string, until time.Time) (release func(), err error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return func() {}, fmt.Errorf("creating state dir: %w", err)
	}
	stamp := until.UTC().Format(time.RFC3339Nano) + "\n"
	if err := commit(holdPath(stateDir), []byte(stamp)); err != nil {
		return func() {}, fmt.Errorf("committing maintenance hold: %w", err)
	}
	return func() { ClearHold(stateDir) }, nil
}

// HoldActive reports whether an unexpired hold is in place.
//
// An unreadable or unparsable file reads as NOT held. A hold is a pause on the
// one thing that keeps the engine alive, so the safe direction when its state
// cannot be established is to carry on supervising: the cost of ignoring a
// real hold is a race that already existed, and the cost of honoring a corrupt
// one is a supervisor that never works again.
func HoldActive(stateDir string) bool {
	b, err := os.ReadFile(holdPath(stateDir))
	if err != nil {
		return false
	}
	until, err := time.Parse(time.RFC3339Nano, trimSpace(string(b)))
	if err != nil {
		return false
	}
	return time.Now().Before(until)
}

// ClearHold removes a hold, whether or not one is present.
func ClearHold(stateDir string) error { return remove(holdPath(stateDir)) }

// trimSpace avoids importing strings for one call in a file that otherwise
// needs none.
func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
