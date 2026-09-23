package doctor

import (
	"context"
	"errors"

	"github.com/wslkit/skrog/internal/supervise"
)

// checkAutostart reports an install that will not come back after a reboot
// (#515).
//
// The supervisor starts at logon only through the per-user Run entry. Without
// it, everything works until the machine restarts, and then docker has no
// engine until someone runs `skrog start` -- which the user finds out from a
// failing `docker` command, not from Skrog. That is how #515 was found: a
// healthy supervisor killed at shutdown, and nothing registered to start it
// again.
//
// The verdict compares two things: what is registered, and what the user asked
// for, recorded by `autostart` (`skrog config set autostart`, `skrog autostart
// enable|disable`, install and --no-autostart all record it).
//
//   - asked for, and missing: the drift #515 was. A warning, and --fix puts
//     the entry back -- safe, because the user's own recorded choice says so.
//   - turned off on purpose: nothing to report.
//   - never recorded (an install from before the key): the old heuristic. The
//     desired state is "running", so a reboot is presumably meant to bring it
//     back too; warned about, but not fixed, since the entry may have been
//     removed on purpose and nothing says otherwise.
func checkAutostart() Check {
	c := Check{Name: "autostart", Title: "starts at logon"}
	c.Run = func(f Facts) Result {
		if !f.Report.Engine.Installed {
			return result(c, Skip, "no engine installed")
		}
		registered := f.Session0.AutostartConfigured
		switch {
		case registered:
			return result(c, OK, "the supervisor is registered to start at logon")
		case f.AutostartIntent == "off":
			return result(c, OK, "not started at logon, by choice (`skrog config set autostart on` to change it)")
		case f.AutostartIntent == "on":
			r := result(c, Warn, "autostart is on, but no logon entry is registered: Skrog will not start after a reboot")
			r.Detail = []string{
				"the Run entry that starts the supervisor at logon is missing, although",
				"autostart was turned on; after a reboot docker has no engine until `skrog start`",
			}
			r.Remedy = "`skrog doctor --fix` registers it again (or `skrog autostart enable`)."
			return r
		case f.Desired == string(supervise.DesiredStopped):
			// Stopped by request: not coming back after a reboot is what was
			// asked for, and nagging about it would be noise.
			return result(c, OK, "not registered to start at logon, and the engine is stopped by request")
		}
		r := result(c, Warn, "Skrog will not start at logon: no autostart entry is registered")
		r.Detail = []string{
			"the engine is set to run, but after a reboot nothing starts the supervisor,",
			"so docker has no engine until you run `skrog start`",
		}
		r.Remedy = "`skrog config set autostart on` registers it; " +
			"`skrog config set autostart off` records that you do not want it, and this goes quiet."
		return r
	}
	// Only the recorded-intent case is repaired. Re-registering an entry
	// nobody said they wanted would override a choice the user may have made.
	c.Fix = func(ctx context.Context, f Facts) (string, error) {
		if !f.Report.Engine.Installed || f.Session0.AutostartConfigured || f.AutostartIntent != "on" {
			return "", nil
		}
		if f.enableAutostart == nil {
			return "", errors.New("re-registering autostart is not available from this build")
		}
		if err := f.enableAutostart(); err != nil {
			return "", err
		}
		return "registered the supervisor to start at logon again", nil
	}
	return c
}
