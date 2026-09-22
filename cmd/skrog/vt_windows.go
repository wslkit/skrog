//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT turns on escape-sequence handling for stdout, so `skrog top` can
// redraw in place. Windows Terminal has it on already; the classic console
// needs asking. A redirected stdout is not a console, and nothing is changed.
func enableVT() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
}
