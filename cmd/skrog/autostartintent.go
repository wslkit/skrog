package main

import (
	"fmt"
	"os"

	"github.com/wslkit/skrog/internal/autostart"
	"github.com/wslkit/skrog/internal/config"
)

// applyAutostart turns logon autostart on or off and records that it was
// asked for (#515).
//
// The Run entry first, the record second, and the record only if the entry
// change worked: a record that said "on" over a machine with no entry would
// be exactly the drift #515 was, written down. The one caller that records
// despite a failed registration is install, which says so -- see
// recordAutostart.
func applyAutostart(stateDir string, on bool) error {
	if on {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := autostart.Enable(exe); err != nil {
			return err
		}
	} else if err := autostart.Disable(); err != nil {
		return err
	}
	return recordAutostart(stateDir, on)
}

// recordAutostart stores the intent without touching the Run entry.
//
// For install, where the user's choice is known before registration is
// attempted, and a registration that fails (a bare go-build binary with no
// skrogw.exe beside it) should leave doctor able to say "you asked for
// autostart and do not have it" rather than nothing at all.
func recordAutostart(stateDir string, on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	if err := config.Set(stateDir, config.KeyAutostart, v); err != nil {
		return fmt.Errorf("recording autostart: %w", err)
	}
	return nil
}

// autostartState is the recorded intent beside what the registry says.
type autostartState struct {
	// Recorded is "on", "off", or "" on an install that never recorded one.
	Recorded   string
	Registered bool
	Command    string
}

func readAutostart(stateDir string) autostartState {
	var s autostartState
	s.Recorded, _ = config.Get(stateDir, config.KeyAutostart)
	s.Registered, s.Command, _ = autostart.Status()
	return s
}

// Effective is the value `skrog config` reports: the recorded intent, or --
// on an install that never recorded one -- what is actually registered, so
// the listing never shows an empty value for a setting that has a real state.
func (s autostartState) Effective() string {
	if s.Recorded != "" {
		return s.Recorded
	}
	if s.Registered {
		return "on"
	}
	return "off"
}

// Describe is the human `skrog config` line's value: the setting, and the live
// state beside it when that is not simply what the setting says.
func (s autostartState) Describe() string {
	switch {
	case s.Recorded == "on" && !s.Registered:
		return "on (but no logon entry is registered; `skrog doctor --fix` restores it)"
	case s.Recorded == "off" && s.Registered:
		return "off (but a logon entry is registered; `skrog config set autostart off` removes it)"
	case s.Recorded == "":
		return s.Effective() + " (not recorded; this is what is registered)"
	}
	return s.Recorded
}
