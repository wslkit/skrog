//go:build windows

package hooks

import (
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/wslkit/skrog/internal/selfexe"
)

// loadedModules enumerates this process's modules through the Tool Help
// snapshot API, which needs no privileges for one's own process.
func loadedModules() []Module {
	// TH32CS_SNAPMODULE32 alongside SNAPMODULE so a 32-bit module in a 64-bit
	// process is not silently skipped.
	snap, err := windows.CreateToolhelp32Snapshot(
		windows.TH32CS_SNAPMODULE|windows.TH32CS_SNAPMODULE32, uint32(os.Getpid()))
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)

	var me windows.ModuleEntry32
	me.Size = uint32(unsafe.Sizeof(me))
	if err := windows.Module32First(snap, &me); err != nil {
		return nil
	}
	var out []Module
	for {
		path := windows.UTF16ToString(me.ExePath[:])
		name := windows.UTF16ToString(me.Module[:])
		if name == "" && path != "" {
			name = filepath.Base(path)
		}
		if name != "" || path != "" {
			out = append(out, Module{Name: name, Path: path})
		}
		if err := windows.Module32Next(snap, &me); err != nil {
			return out
		}
	}
}

// executableDir is the directory holding this binary. Resolved through any
// symlink: a winget portable install is invoked through an alias link whose
// directory holds nothing else (#360).
func executableDir() string {
	return selfexe.Dir()
}
