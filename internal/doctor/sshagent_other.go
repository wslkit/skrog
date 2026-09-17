//go:build !windows

package doctor

// sshAgentInfo is Windows-only in practice: the thing it diagnoses is the
// Windows OpenSSH agent service. On other platforms (tests on CI Linux, the
// guest build) it reports unavailable rather than failing to compile.
func sshAgentInfo() SSHAgentInfo {
	return SSHAgentInfo{Err: "ssh agent check is Windows-only"}
}
