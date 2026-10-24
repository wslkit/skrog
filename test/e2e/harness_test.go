//go:build e2e

package e2e

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A timeout has to end the wait even when the command started something that
// inherited its output pipe. Killing only the direct child leaves that
// process holding the pipe, and the old harness then waited out its whole
// run: a 2s timeout that took 11s (#554).
func TestTimeoutEndsTheWait(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the harness's process-tree kill is Windows-specific")
	}
	const timeout = 2 * time.Second
	cases := []struct {
		name   string
		args   []string
		lostOK bool // whether the helper may give up on the output
	}{
		// cmd.exe waits on ping, its child, which inherited the pipe: the
		// tree is intact when the timeout fires, so the tree kill closes the
		// pipe and the output is kept.
		{"grandchild", []string{"/c", "ping -n 30 127.0.0.1"}, false},
		// cmd.exe exits at once and leaves ping running with the pipe: there
		// is no live parent left to walk a tree from, so only the grace
		// period ends the wait.
		{"orphan", []string{"/c", "start /b ping -n 30 127.0.0.1"}, true},
	}
	helpers := []struct {
		name string
		run  func(args []string) error
	}{
		{"run", func(a []string) error { _, err := run(t, timeout, "cmd", a...); return err }},
		{"runEnv", func(a []string) error { _, err := runEnv(t, os.Environ(), timeout, "cmd", a...); return err }},
		{"dockerE", func(a []string) error { _, err := dockerE(t, &state{docker: "cmd"}, timeout, a...); return err }},
	}
	for _, c := range cases {
		for _, h := range helpers {
			t.Run(c.name+"/"+h.name, func(t *testing.T) {
				start := time.Now()
				err := h.run(c.args)
				took := time.Since(start)
				if err == nil || !strings.Contains(err.Error(), "timed out") {
					t.Errorf("err = %v, want a timeout", err)
				}
				if !c.lostOK && err != nil && strings.Contains(err.Error(), "output lost") {
					t.Errorf("the tree kill did not close the pipe: %v", err)
				}
				// ping -n 30 runs ~29s; the bound leaves room for the kill
				// and the grace period, and none for waiting ping out.
				if took > 12*time.Second {
					t.Errorf("a %s timeout returned after %s", timeout, took.Round(100*time.Millisecond))
				}
			})
		}
	}
}
