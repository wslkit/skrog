package supervise

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The served pipe is a DIFFERENT fact from the endpoint record, with a
// different lifetime, and conflating the two is what made #429 survive two
// fixes.
//
//	endpoint.json   where the engine is answering RIGHT NOW.
//	                Cleared when the supervisor exits, and it must be (#288):
//	                a record that outlived its process makes `skrog status`
//	                name a pipe nothing is listening on.
//
//	served-pipe     what the supervisor was ASKED to serve.
//	                Survives the process, because its whole job is to tell the
//	                NEXT supervisor what the last one was doing. A fact about
//	                configuration, not about a live binding.
//
// Both previous attempts read the pipe out of endpoint.json. The first read it
// after the old supervisor had cleared it on the way out; the second captured
// it earlier, which fixed that and still failed, because the acceptance suite
// deletes endpoint.json before restarting ON PURPOSE -- that is how it proves
// the replacement wrote its own record (#273) rather than inheriting one.
//
// So there is no moment at which endpoint.json can answer this question. A
// record that is correctly deleted cannot also be a handoff channel.

func servedPipePath(stateDir string) string {
	return filepath.Join(stateDir, "served-pipe")
}

// WriteServedPipe records the pipe this supervisor was asked for, so a
// replacement can ask for the same one.
//
// Written at supervisor start with whatever --pipe carried, INCLUDING empty:
// a supervisor started without one is saying "choose normally", and a stale
// value from a previous run must not resurrect a pipe nobody asked for.
// Empty removes the file rather than writing a blank, so "no file" and "no
// preference" are the same state on disk.
//
// Plain text, not JSON: it is one string, and a pipe name has no structure
// worth a schema.
func WriteServedPipe(stateDir, pipe string) error {
	if strings.TrimSpace(pipe) == "" {
		return ClearServedPipe(stateDir)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	return commit(servedPipePath(stateDir), []byte(pipe+"\n"))
}

// ReadServedPipe returns the recorded pipe, or "" when there is none.
//
// Deliberately NOT cleared when a supervisor exits. A crash leaves it, a clean
// exit leaves it, and both are correct: the next supervisor should serve what
// the last one served either way. It is replaced on the next start, which is
// the only event that changes the answer.
func ReadServedPipe(stateDir string) string {
	b, err := os.ReadFile(servedPipePath(stateDir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ClearServedPipe forgets the recorded pipe, so the next supervisor selects
// normally. Uninstall calls it; so does starting a supervisor with no --pipe.
func ClearServedPipe(stateDir string) error {
	if err := os.Remove(servedPipePath(stateDir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
