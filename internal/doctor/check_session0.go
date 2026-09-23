package doctor

// checkSession0 is advisory guidance for unattended (no interactive logon)
// operation. Verifying that NT VIRTUAL MACHINE\Virtual Machines (S-1-5-83-0)
// holds SeServiceLogonRight reliably needs elevation, so rather than assert a
// value it may not be able to read as a standard user, doctor explains the
// requirement and points at the playbook — turning the finding from #3 into
// guidance instead of a fragile probe.
//
// When a logon autostart is configured, this is a non-issue for the common
// case (the engine starts when the user logs in), so the check reports OK and
// keeps the note short.
func checkSession0() Check {
	c := Check{Name: "session0", Title: "unattended startup readiness"}
	c.Run = func(f Facts) Result {
		if f.Session0.AutostartConfigured {
			return result(c, OK, "logon autostart configured; the engine starts when you sign in")
		}
		// Not "only matters for headless hosts": a desktop install without
		// autostart does not come back after a reboot either, and saying it
		// only mattered elsewhere is how that went unnoticed (#515). The
		// autostart check carries that verdict; this one stays about
		// unattended hosts.
		r := result(c, Skip, "no logon autostart configured, so there is no unattended startup to check")
		r.Detail = []string{
			"for a build agent or server that must run before any user logs in, the",
			"engine needs the WSL VM's service account to hold the service logon right",
		}
		r.Remedy = "for interactive machines, run `skrog autostart enable`. For " +
			"unattended hosts, follow docs/auto-logon-runner.md (grants NT VIRTUAL " +
			"MACHINE\\Virtual Machines the 'Log on as a service' right)."
		return r
	}
	return c
}
