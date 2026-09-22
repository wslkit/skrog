package hooks

import "testing"

func TestFilterInjectedIgnoresWindowsAndOurOwnDirectory(t *testing.T) {
	mods := []Module{
		{Name: "skrog.exe", Path: `C:\Skrog\bin\skrog.exe`},
		{Name: "KERNEL32.DLL", Path: `C:\Windows\System32\KERNEL32.DLL`},
		{Name: "ucrtbase.dll", Path: `C:\Windows\System32\ucrtbase.dll`},
		{Name: "wow64.dll", Path: `C:\Windows\SysWOW64\wow64.dll`},
		{Name: "sxs.dll", Path: `C:\Windows\WinSxS\x86_something\sxs.dll`},
		{Name: "skrogw.exe", Path: `C:\Skrog\bin\skrogw.exe`},
		{Name: "InProcessClient64.dll", Path: `C:\Program Files\SomeEDR\Agent 1.2\InProcessClient64.dll`},
		{Name: "noPath.dll", Path: ""},
	}
	got := filterInjected(mods, `C:\Skrog\bin`)
	if len(got) != 1 {
		t.Fatalf("got %d modules, want 1: %+v", len(got), got)
	}
	if got[0].Name != "InProcessClient64.dll" {
		t.Errorf("flagged the wrong module: %+v", got[0])
	}
}

func TestFilterInjectedIsCaseAndSeparatorInsensitive(t *testing.T) {
	mods := []Module{
		{Name: "kernel32.dll", Path: `c:/WINDOWS/system32/kernel32.dll`},
		{Name: "self.dll", Path: `C:/Skrog/Bin/self.dll`},
		{Name: "hook.dll", Path: `C:\Vendor\hook.dll`},
	}
	got := filterInjected(mods, `c:\skrog\bin`)
	if len(got) != 1 || got[0].Name != "hook.dll" {
		t.Errorf("got %+v, want only hook.dll", got)
	}
}

func TestFilterInjectedWithoutAnExecutableDir(t *testing.T) {
	// os.Executable can fail; the classification must still work, just without
	// the "beside the exe" exemption.
	mods := []Module{{Name: "hook.dll", Path: `C:\Vendor\hook.dll`}}
	if got := filterInjected(mods, ""); len(got) != 1 {
		t.Errorf("got %+v, want the module reported", got)
	}
}

func TestInjectedDoesNotPanic(t *testing.T) {
	// Whatever this machine has loaded, reading our own module list must not
	// fail: doctor calls it on every run.
	for _, m := range Injected() {
		if m.Name == "" && m.Path == "" {
			t.Error("an empty module made it through")
		}
	}
}

// WSL's own COM proxy/stub is not an injection: Skrog loads it by talking to
// wslservice over COM (#380). It read as third-party because modern WSL is
// serviced separately from Windows and lives in Program Files (#488).
func TestFilterInjectedIgnoresWSLsOwnModules(t *testing.T) {
	mods := []Module{
		{Name: "wslserviceproxystub.dll", Path: `C:\Program Files\WSL\wslserviceproxystub.dll`},
		{Name: "wslclient.dll", Path: `C:\Program Files\WSL\wslclient.dll`},
		// WSL installed somewhere else, and a non-English Program Files: a
		// hardcoded path would miss both.
		{Name: "wslserviceproxystub.dll", Path: `D:\Apps\WSL\wslserviceproxystub.dll`},
		{Name: "wslserviceproxystub.dll", Path: `C:\Programme\WSL\wslserviceproxystub.dll`},
		{Name: "InProcessClient64.dll", Path: `C:\Program Files\SomeEDR\Agent\InProcessClient64.dll`},
	}
	got := filterInjected(mods, `C:\Skrog\bin`)
	if len(got) != 1 {
		t.Fatalf("got %d modules, want only the EDR one: %+v", len(got), got)
	}
	if got[0].Name != "InProcessClient64.dll" {
		t.Errorf("flagged %+v; WSL's own modules must not be reported", got[0])
	}
}

// The exemption is for a directory called `wsl`, not for anything with those
// three letters in it. An agent that ships in a folder whose name merely starts
// with "wsl" is exactly what this check exists to surface.
func TestFilterInjectedDoesNotExemptLookalikeDirectories(t *testing.T) {
	mods := []Module{
		{Name: "hook.dll", Path: `C:\Program Files\wslhook\hook.dll`},
		{Name: "agent.dll", Path: `C:\Vendor\wsl-security\agent.dll`},
	}
	got := filterInjected(mods, `C:\Skrog\bin`)
	if len(got) != 2 {
		t.Errorf("got %d modules, want both: %+v", len(got), got)
	}
}
