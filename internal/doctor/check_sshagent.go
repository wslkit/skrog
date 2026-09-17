package doctor

// sshAgentPipe is the well-known Windows OpenSSH agent endpoint. buildx dials
// this name by itself for `--ssh default`. It is a var so tests can point at a
// pipe they created rather than depending on the machine's real agent, and it
// lives here rather than beside the Windows gatherer because the check prints
// it on every platform.
var sshAgentPipe = `\\.\pipe\openssh-ssh-agent`

// checkSSHAgent reports whether `docker build --ssh default` can find an agent.
//
// Skrog does no SSH forwarding of its own and does not need to: buildx dials
// the Windows OpenSSH agent pipe by name, and the forwarding channel rides the
// ordinary API connection, so it crosses the bridge like any other request
// (#391). The only thing that breaks it is a host-side one — Windows ships the
// ssh-agent service disabled, so the first build that needs a private
// dependency fails at the end of a build rather than before it, which is an
// expensive place to learn.
//
// Warn, never Fail: a machine that never builds with `--ssh` is not broken, and
// most do not.
func checkSSHAgent() Check {
	c := Check{Name: "ssh-agent", Title: "SSH agent for docker build --ssh"}
	c.Run = func(f Facts) Result {
		a := f.SSHAgent
		if a.Err != "" {
			return result(c, Skip, a.Err)
		}

		// buildx prefers SSH_AUTH_SOCK when it is set, so an agent reached that
		// way is a working setup even with the Windows service off.
		if a.AuthSock != "" {
			r := result(c, OK, "SSH_AUTH_SOCK is set; --ssh default will use it")
			r.Detail = []string{"  SSH_AUTH_SOCK=" + a.AuthSock}
			return r
		}

		if a.PipeExists {
			return result(c, OK, "the Windows OpenSSH agent is running")
		}

		r := result(c, Warn, "no SSH agent; `docker build --ssh default` will fail")
		r.Detail = []string{
			"  " + sshAgentPipe + " is absent and SSH_AUTH_SOCK is unset",
			"  Windows ships the ssh-agent service disabled by default.",
			"  Only builds that use --ssh (private Go modules, npm or pip",
			"  dependencies over git) are affected; everything else is fine.",
		}
		r.Remedy = "start the agent and load a key, in an elevated shell: " +
			"Set-Service ssh-agent -StartupType Automatic; Start-Service ssh-agent; ssh-add. " +
			"Or point SSH_AUTH_SOCK at another agent. `docker build --ssh default=<keyfile>` " +
			"works without an agent at all."
		return r
	}
	return c
}
