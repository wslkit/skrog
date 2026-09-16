// Command skrogw is the windowless launcher and watchdog for the supervisor —
// the javaw convention, plus a restart policy.
//
// It exists first because skrog.exe is a console binary, and anything that
// starts a console binary at logon (a Run key, an interactive scheduled task)
// flashes a console window at the user. skrogw is built for the GUI subsystem
// (-H=windowsgui), so no console is ever created.
//
// It stays resident because the supervisor is the always-on layer: while it is
// down the docker pipe is gone and every docker command fails until someone
// runs `skrog start`. A Go fatal runtime error (skrog#166) cannot be
// recovered inside the process, so recovery belongs here. The policy lives in
// internal/watchdog: clean exits are final, instant failures (a held
// single-instance lock, a bad flag) are not treated as crashes, restarts back
// off, and a crash budget stops a loop from churning all night.
//
// Set SKROG_NO_WATCHDOG=1 to go back to spawn-and-exit.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/wslkit/skrog/internal/logging"
	"github.com/wslkit/skrog/internal/selfexe"
	"github.com/wslkit/skrog/internal/watchdog"
)

func main() {
	self, err := selfexe.Path()
	if err != nil {
		os.Exit(1)
	}
	// The sibling console binary does the real work; keeping the launcher a
	// spawner rather than a second copy of the CLI keeps the zip honest about
	// where behavior lives.
	skrog := filepath.Join(filepath.Dir(self), "skrog.exe")
	if _, err := os.Stat(skrog); err != nil {
		os.Exit(1)
	}
	args := append([]string{"supervise"}, os.Args[1:]...)

	if os.Getenv("SKROG_NO_WATCHDOG") == "1" {
		cmd := supervisorCmd(skrog, args)
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
		_ = cmd.Process.Release()
		return
	}
	os.Exit(watch(skrog, args, stateDir(os.Args[1:])))
}

// supervisorCmd builds the hidden, console-free child process.
func supervisorCmd(skrog string, args []string) *exec.Cmd {
	cmd := exec.Command(skrog, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd
}

// watch runs the supervisor, restarting it per the policy, and returns the
// exit code to leave with.
func watch(skrog string, args []string, stateDir string) int {
	log := openLog(stateDir)
	if log != nil {
		defer log.Close()
	}
	logf := func(format string, a ...any) {
		if log == nil {
			return
		}
		fmt.Fprintf(log, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
	}

	// skrogw has no console, so anything the supervisor writes to stderr —
	// including a Go fatal error's goroutine dump, the only evidence a crash
	// leaves — would go nowhere. Capture it, bounded, so the next person to
	// look has the dump rather than a gap.
	stderr := openStderrLog(stateDir)
	if stderr != nil {
		defer stderr.Close()
	}

	policy := watchdog.Default
	var restarts []time.Time
	consecutive := 0

	for {
		cmd := supervisorCmd(skrog, args)
		if stderr != nil {
			cmd.Stderr = stderr
		}
		started := time.Now()
		if err := cmd.Start(); err != nil {
			logf("could not start the supervisor: %v", err)
			return 1
		}
		err := cmd.Wait()
		uptime := time.Since(started)
		code := exitCode(err)

		d := policy.Decide(code, uptime, restarts, consecutive, time.Now())
		if !d.Restart {
			if code != 0 {
				logf("%s", d.Reason)
			}
			return code
		}
		logf("%s in %s", d.Reason, d.Delay)
		time.Sleep(d.Delay)
		restarts = append(restarts, time.Now())
		if uptime < policy.MinUptime {
			consecutive++
		} else {
			consecutive = 0
		}
	}
}

// exitCode extracts a process exit code from Wait's error. A signal or an
// unknown failure counts as non-zero, which is what the policy cares about.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// openLog opens the watchdog's own log next to the supervisor's. A watchdog
// that cannot log still watches: logging is how the restart is explained
// afterwards, not how it works.
func openLog(stateDir string) *logging.RotatingWriter {
	if stateDir == "" {
		return nil
	}
	w, err := logging.NewRotatingWriter(filepath.Join(stateDir, "watchdog.log"), 0, 0)
	if err != nil {
		return nil
	}
	return w
}

// openStderrLog opens the capture file for the supervisor's stderr. It is
// small and keeps one predecessor: the supervisor already logs its structured
// lines to supervisor.log, so what matters here is the tail — the crash.
func openStderrLog(stateDir string) *logging.RotatingWriter {
	if stateDir == "" {
		return nil
	}
	w, err := logging.NewRotatingWriter(filepath.Join(stateDir, "supervisor-stderr.log"), 2*1024*1024, 1)
	if err != nil {
		return nil
	}
	return w
}

// stateDir finds --state-dir in the arguments passed through to supervise, or
// falls back to the default location, so the watchdog log lands beside the
// supervisor log it explains.
func stateDir(args []string) string {
	for i, a := range args {
		switch {
		case a == "--state-dir" || a == "-state-dir":
			if i+1 < len(args) {
				return args[i+1]
			}
		case len(a) > 12 && (a[:12] == "--state-dir=" || a[:11] == "-state-dir="):
			if idx := indexByte(a, '='); idx >= 0 {
				return a[idx+1:]
			}
		}
	}
	if base := os.Getenv("LOCALAPPDATA"); base != "" {
		return filepath.Join(base, "Skrog")
	}
	return ""
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
