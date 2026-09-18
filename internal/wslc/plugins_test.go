package wslc

import (
	"strings"
	"testing"
)

// The key is read out of wslservice.exe, not guessed. The natural guess
// ("Windows Subsystem for Linux\Plugins") is wrong, and a detector pointed at
// the wrong key reports "no plugins" on every machine — a silent fail-open,
// which is the exact failure #406 exists to prevent. Pinned so a tidy-up
// cannot quietly change it.
func TestPluginsKeyIsTheOneWSLEnumerates(t *testing.T) {
	const want = `SOFTWARE\Microsoft\Windows\CurrentVersion\Lxss\Plugins`
	if PluginsKey != want {
		t.Errorf("PluginsKey = %q, want %q (from strings in wslservice.exe)", PluginsKey, want)
	}
}

func TestPluginsAnyAndNames(t *testing.T) {
	var none Plugins
	if none.Any() {
		t.Error("an empty set reported Any() = true")
	}
	ps := Plugins{
		{Name: "zeta", Path: `C:\z.dll`},
		{Name: "alpha", Path: `C:\a.dll`},
	}
	if !ps.Any() {
		t.Error("a populated set reported Any() = false")
	}
	// Sorted, so the refusal message is stable between runs.
	got := ps.Names()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Errorf("Names() = %v, want [alpha zeta]", got)
	}
}

// The refusal has to be actionable: which plugin, where it is registered, and
// how to proceed anyway. An administrator reading it needs to know which tool
// they were relying on before deciding to override.
func TestErrPluginsPresentIsActionable(t *testing.T) {
	err := &ErrPluginsPresent{Plugins: Plugins{{Name: "defender-wsl", Path: `C:\p.dll`}}}
	msg := err.Error()

	for _, want := range []string{
		"defender-wsl",                  // which plugin
		PluginsKey,                      // where it is registered
		"wslc.ignore-plugins",           // how to proceed anyway
		"WSLPluginAPI_ContainerStarted", // what is actually lost
		"#406",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, msg)
		}
	}
	// It must not imply the distro backend is affected; it is not.
	if !strings.Contains(msg, "distro backend is unaffected") {
		t.Errorf("refusal does not scope itself to the wslc backend:\n%s", msg)
	}
}

func TestPluginString(t *testing.T) {
	if got := (Plugin{Name: "p", Path: `C:\p.dll`}).String(); got != `p (C:\p.dll)` {
		t.Errorf("String() = %q", got)
	}
	if got := (Plugin{Name: "p"}).String(); got != "p" {
		t.Errorf("String() with no path = %q, want just the name", got)
	}
}
