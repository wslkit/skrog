package doctor

import "github.com/wslkit/skrog/internal/supervise"

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
// Only session0 looked at autostart before, and it reported a missing entry
// as a skip, "only matters for headless/unattended hosts". For a desktop
// install that is exactly backwards.
//
// A heuristic, and says so: nothing records whether autostart was turned off
// on purpose, so the verdict rests on the desired state -- someone who asked
// for the engine running presumably wants it running after a reboot too. That
// is also why this check has no fix: re-registering an entry the user may have
// removed deliberately is not a safe remedy.
func checkAutostart() Check {
	c := Check{Name: "autostart", Title: "starts at logon"}
	c.Run = func(f Facts) Result {
		if !f.Report.Engine.Installed {
			return result(c, Skip, "no engine installed")
		}
		if f.Session0.AutostartConfigured {
			return result(c, OK, "the supervisor is registered to start at logon")
		}
		if f.Desired == string(supervise.DesiredStopped) {
			// Stopped by request: not coming back after a reboot is what was
			// asked for, and nagging about it would be noise.
			return result(c, OK, "not registered to start at logon, and the engine is stopped by request")
		}
		r := result(c, Warn, "Skrog will not start at logon: no autostart entry is registered")
		r.Detail = []string{
			"the engine is set to run, but after a reboot nothing starts the supervisor,",
			"so docker has no engine until you run `skrog start`",
		}
		r.Remedy = "`skrog autostart enable` registers it (a per-user Run entry; no elevation). " +
			"If you turned it off on purpose, this is expected."
		return r
	}
	return c
}
