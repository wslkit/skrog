//go:build windows

// Package autostart registers the supervisor to start at logon.
//
// The mechanism is the per-user Run key rather than a scheduled task, for
// three reasons that all trace back to constraints this project has already
// measured: it needs no elevation (install must work non-admin), it produces
// an *interactive* process in the user's session (a "run whether logged on or
// not" task performs a batch logon into session 0, where WSL cannot start —
// Spike B, #3), and it is trivially inspectable and removable by the user in
// Task Manager's Startup tab. The registered command is skrogw.exe, the
// GUI-subsystem launcher, so no console flashes at logon.
package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// ValueName is the Run-key entry name; visible to users in Task Manager.
const ValueName = "Skrog"

// runKeyPath is a var so tests can point at a scratch key instead of the
// user's real Run key.
var runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// Enable registers skrogw.exe (next to the given skrog.exe) to run at logon.
//
// The path is resolved through any symlink first. A winget `portable` install
// puts an alias SYMLINK on PATH and keeps the real files elsewhere, so the
// launcher sits next to the TARGET, never next to the link the user invoked
// (#360).
func Enable(skrogExe string) error {
	if resolved, err := filepath.EvalSymlinks(skrogExe); err == nil {
		skrogExe = resolved
	}
	launcher := filepath.Join(filepath.Dir(skrogExe), "skrogw.exe")
	if _, err := os.Stat(launcher); err != nil {
		return fmt.Errorf("autostart needs the skrogw.exe launcher next to skrog.exe "+
			"(a console binary at logon would flash a console window): %w", err)
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("opening the Run key: %w", err)
	}
	defer k.Close()

	// Quoted, because the install dir may contain spaces.
	if err := k.SetStringValue(ValueName, `"`+launcher+`"`); err != nil {
		return fmt.Errorf("writing the Run entry: %w", err)
	}
	return nil
}

// Disable removes the logon entry. Removing an entry that does not exist is
// success: the user asked for a state, not an action.
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("opening the Run key: %w", err)
	}
	defer k.Close()

	if err := k.DeleteValue(ValueName); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("removing the Run entry: %w", err)
	}
	return nil
}

// DisableIfOwned removes the logon entry only when it points into the given
// install directory. The Run entry is one-per-user while installs are
// per-state-dir, so an uninstall of a secondary install (the e2e suite runs
// one beside a real install, with its binary in a temp dir) must not delete
// the entry belonging to the primary. Returns whether an entry was removed.
func DisableIfOwned(installDir string) (bool, error) {
	enabled, cmd, err := Status()
	if err != nil || !enabled {
		return false, err
	}
	registered := strings.ToLower(filepath.Clean(strings.Trim(cmd, `"`)))
	dir := strings.ToLower(filepath.Clean(installDir))
	if !strings.HasPrefix(registered, dir+string(filepath.Separator)) {
		return false, nil // someone else's entry; leave it alone
	}
	return true, Disable()
}

// Status returns whether autostart is registered, and the command if so.
func Status() (enabled bool, command string, err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false, "", fmt.Errorf("opening the Run key: %w", err)
	}
	defer k.Close()

	v, _, err := k.GetStringValue(ValueName)
	if err == registry.ErrNotExist {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("reading the Run entry: %w", err)
	}
	return true, v, nil
}
