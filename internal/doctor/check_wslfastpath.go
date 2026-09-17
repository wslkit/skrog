package doctor

// checkWSLFastPath reports whether Skrog is talking to wslservice over COM or
// falling back to spawning wsl.exe (#356).
//
// This is never a failure, so both outcomes are OK. The fallback is correct behaviour and
// the machine works either way — `wsl.exe` is the reference implementation and
// always has been. What it is not is fast: the supervisor asks for the distro
// list every 3 seconds for the life of the session, and that call is a process
// spawn on the fallback path against a function call on the fast one, measured
// at 56 ms against 0.9 ms on the machine this was developed on.
//
// It exists because a machine that has quietly lost the fast path — a WSL
// update that reshaped the COM surface, a policy that blocks the class — would
// otherwise present as nothing at all, just a supervisor that costs more than
// it used to. That is precisely the shape of regression nobody reports, and the
// issue asked for it to be visible rather than silent.
func checkWSLFastPath() Check {
	c := Check{Name: "wsl-fast-path", Title: "WSL service access"}
	c.Run = func(f Facts) Result {
		if f.WSLFastPath {
			return result(c, OK, "talking to wslservice over COM (no process spawn per query)")
		}
		r := result(c, OK, "falling back to spawning wsl.exe for distro queries")
		if f.WSLFastPathWhy != "" {
			r.Detail = []string{"reason: " + f.WSLFastPathWhy}
		}
		r.Detail = append(r.Detail,
			"This is not a fault: wsl.exe is the reference implementation and everything works.",
			"It costs a process spawn per query, which the supervisor makes every 3 seconds.")
		r.Remedy = "nothing to do. If this appeared after a `wsl --update`, the service's " +
			"COM interface may have changed; please report it with the reason above."
		return r
	}
	return c
}
