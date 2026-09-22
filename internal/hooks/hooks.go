// Package hooks reports third-party DLLs injected into this process (#166).
//
// Endpoint-security products (EDR/DLP agents) hook Win32 and syscall entry
// points by loading a DLL into every process and rewriting function prologues
// to jump through their own trampolines. Those trampolines assume a C thread
// stack. Go does not have one: goroutine stacks are small, movable, and the
// calling convention is its own, so a hook that saves and restores the wrong
// frame corrupts a Go stack rather than a C one.
//
// The result is a fatal runtime error with no bug behind it in the program that
// died -- observed on this project as "unexpected return pc", "found pointer to
// free object", and an access violation at an image-base-shaped address, all
// inside the goroutine blocked in GetQueuedCompletionStatus. That is impossible
// to fix in the process, and impossible to diagnose from the dump, which is why
// doctor names the injected module instead.
//
// This is detection, not attribution: a module being loaded does not prove it
// caused anything. The point is to hand someone the one fact the crash dump
// cannot give them.
package hooks

import (
	"path/filepath"
	"strings"
)

// Module is one loaded DLL: its file name and full path.
type Module struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Injected returns the modules loaded into this process that did not come from
// Windows itself or from the executable's own directory -- i.e. the ones
// something else put there. Empty on non-Windows, and empty (not an error) when
// the module list cannot be read.
func Injected() []Module {
	return filterInjected(loadedModules(), executableDir())
}

// filterInjected is the pure half, so the classification is testable without a
// process to inspect.
func filterInjected(mods []Module, exeDir string) []Module {
	var out []Module
	for _, m := range mods {
		if m.Path == "" || isSystemModule(m.Path) {
			continue
		}
		// A DLL beside the executable is ours (or the user's own build layout),
		// not an injection.
		if exeDir != "" && sameDir(m.Path, exeDir) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// isSystemModule reports whether a path is part of Windows. Everything under
// the Windows directory is treated as the platform's own: WinSxS, System32,
// SysWOW64 and the servicing trees all live there, and enumerating them adds
// noise without adding information.
//
// WSL's own directory counts too, and not counting it was a bug (#488). Modern
// WSL is serviced separately from Windows and installs to `%ProgramFiles%\WSL`,
// so its COM proxy/stub -- `wslserviceproxystub.dll` -- read as a third-party
// injection. Skrog loads that DLL ITSELF: it talks to wslservice over COM
// (#380), and Windows maps the marshalling stub into any process that does.
//
// So doctor warned about a consequence of Skrog's own design, named a
// Microsoft-signed component, and told the user to ask their endpoint-security
// vendor for an exclusion that nobody could grant. A check whose whole value is
// that its output is short and means something cannot afford that.
func isSystemModule(path string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	for _, root := range []string{"c:/windows/", "/windows/system32/", "/windows/syswow64/"} {
		if strings.Contains(p, root) {
			return true
		}
	}
	return isWSLModule(p)
}

// isWSLModule reports whether a lowercase slash-form path is inside a WSL
// installation.
//
// Matched by directory name rather than by a full path so it survives WSL being
// installed outside Program Files and a non-English Program Files, both of
// which a hardcoded `c:/program files/wsl/` would miss. The segment must be a
// whole directory called `wsl`, so `C:\Vendor\wslhook\evil.dll` is not exempted
// by accident -- the point is to quieten one component, not to hand anything
// named after WSL a free pass.
func isWSLModule(lowerSlashPath string) bool {
	for _, seg := range strings.Split(lowerSlashPath, "/") {
		if seg == "wsl" {
			return true
		}
	}
	return false
}

func sameDir(path, dir string) bool {
	a := strings.ToLower(filepath.ToSlash(filepath.Dir(path)))
	b := strings.ToLower(filepath.ToSlash(dir))
	return a == strings.TrimSuffix(b, "/")
}
