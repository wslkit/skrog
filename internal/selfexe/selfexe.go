// Package selfexe reports where this binary actually lives.
//
// os.Executable is not that answer when the program was launched through a
// symlink: it returns the SYMLINK's path, not the target. Anything deriving a
// sibling from it — skrogw.exe next to skrog.exe, the bundled docker CLI in the
// same bin directory, the guest agent shipped beside the release binary — then
// looks in the wrong directory and finds nothing.
//
// That is not hypothetical. winget's `portable` installer type extracts a
// package to %LOCALAPPDATA%\Microsoft\WinGet\Packages\<id>\ and puts an alias
// SYMLINK in ...\WinGet\Links\, which is what lands on PATH. So every
// invocation from a winget install runs through a symlink (#360). Measured:
//
//	direct:      os.Executable() -> ...\pkg\app.exe    sibling found: true
//	via symlink: os.Executable() -> ...\links\app.exe  sibling found: FALSE
//	             EvalSymlinks    -> ...\pkg\app.exe
//
// Use Path wherever a sibling file is derived. Plain os.Executable remains
// correct for "what command did the user invoke", which is a different
// question — `skrog upgrade` re-exec and the supervisor respawn want the path
// the user typed, not the file behind it.
package selfexe

import (
	"os"
	"path/filepath"
)

// executable is indirected so tests can stand in a temporary layout.
var executable = os.Executable

// Path is the resolved path of the running binary, with symlinks followed.
//
// A resolution failure is not an error: the unresolved path is still the best
// answer available, and a broken lookup downstream reports itself far more
// clearly than a startup failure here would.
func Path() (string, error) {
	exe, err := executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// Dir is the directory holding the running binary — the one its siblings are
// in. Empty when the executable cannot be located at all.
func Dir() string {
	exe, err := Path()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}
