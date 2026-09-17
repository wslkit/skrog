package doctor

import "os"

// sshAgentInfo reports how `docker build --ssh default` would find an agent.
//
// It stats the pipe rather than dialing it: a dial consumes one of the
// listener's instances and would show up in the agent's own logs, which is
// more than a diagnosis should cost.
func sshAgentInfo() SSHAgentInfo {
	info := SSHAgentInfo{AuthSock: os.Getenv("SSH_AUTH_SOCK")}
	if _, err := os.Stat(sshAgentPipe); err == nil {
		info.PipeExists = true
	}
	return info
}
