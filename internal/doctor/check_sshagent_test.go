package doctor

import (
	"strings"
	"testing"
)

func TestCheckSSHAgent(t *testing.T) {
	run := checkSSHAgent().Run

	for _, tc := range []struct {
		name string
		in   SSHAgentInfo
		want Status
	}{
		{"agent pipe present", SSHAgentInfo{PipeExists: true}, OK},
		{"SSH_AUTH_SOCK set", SSHAgentInfo{AuthSock: `\\.\pipe\other-agent`}, OK},
		// A set SSH_AUTH_SOCK wins even with no pipe: it is what buildx reads.
		{"sock set, no pipe", SSHAgentInfo{AuthSock: "/tmp/agent.sock"}, OK},
		{"nothing at all", SSHAgentInfo{}, Warn},
		{"not askable", SSHAgentInfo{Err: "ssh agent check is Windows-only"}, Skip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := run(Facts{SSHAgent: tc.in})
			if got.Status != tc.want {
				t.Errorf("status = %v, want %v (summary %q)", got.Status, tc.want, got.Summary)
			}
		})
	}
}

// The warning is the whole point of the check, so assert it carries something
// the reader can act on rather than only a status.
func TestCheckSSHAgentWarningIsActionable(t *testing.T) {
	r := checkSSHAgent().Run(Facts{SSHAgent: SSHAgentInfo{}})
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn", r.Status)
	}
	if !strings.Contains(r.Remedy, "Start-Service ssh-agent") {
		t.Errorf("remedy does not say how to start the agent: %q", r.Remedy)
	}
	// Someone who cannot elevate still needs a way through.
	if !strings.Contains(r.Remedy, "keyfile") {
		t.Errorf("remedy does not mention the no-agent alternative: %q", r.Remedy)
	}
	if !strings.Contains(strings.Join(r.Detail, "\n"), sshAgentPipe) {
		t.Errorf("detail does not name the pipe it looked for: %q", r.Detail)
	}
}

// Warn, not Fail: a machine that never builds with --ssh is not broken. This is
// a deliberate severity choice, so pin it.
func TestCheckSSHAgentNeverFails(t *testing.T) {
	for _, in := range []SSHAgentInfo{{}, {PipeExists: true}, {Err: "x"}} {
		if got := checkSSHAgent().Run(Facts{SSHAgent: in}).Status; got == Fail {
			t.Errorf("SSHAgentInfo%+v produced Fail; the check must never fail a run", in)
		}
	}
}
