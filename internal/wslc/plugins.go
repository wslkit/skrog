package wslc

import (
	"fmt"
	"sort"
	"strings"
)

// WSL plugin detection (#406).
//
// WSL loads host-side plugin DLLs and calls them on container and session
// events: WSLPluginAPI_ContainerStarted (whose return value can REFUSE the
// container), ContainerStopping, ImageCreated, ImageDeleted, OnSessionCreated.
// They are the integration point Defender-style tooling and Microsoft's own
// agent-sandbox work are expected to use.
//
// Those hooks fire from wslcsession, which a direct docker.sock relay skips —
// so on the wslc backend they do not fire at all. #322 closed the registry-
// allowlist half of that bypass by standing in for the policy: the rules are
// declarative, so Skrog can read the same keys and reach the same verdict.
//
// Hooks cannot be stood in for. They are arbitrary third-party code with a
// veto, and there is no way to substitute for code you do not have. Either it
// is invoked or it is not.
//
// So Skrog detects them and refuses, rather than quietly disabling somebody's
// security tooling. That is the same posture the project already takes for a
// policy it cannot read (#254, and ReadPolicies above): when the honest answer
// is "I cannot enforce what this machine was configured with", stop.

// PluginsKey is where WSL enumerates its plugins:
// HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Lxss\Plugins.
//
// Read out of wslservice.exe rather than assumed — the natural guess
// ("Windows Subsystem for Linux\Plugins") is wrong, and a detector looking at
// the wrong key would report "no plugins" on every machine, which is the
// silent fail-open this whole issue is about. The binary also carries
// PluginManager.cpp, WSLCPluginNotifier.cpp and the enumeration's own
// diagnostic ("Plugin value: '%ls' has incorrect type: %lu, skipping"), which
// is what confirms the values are name -> DLL path strings.
const PluginsKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Lxss\Plugins`

// Plugin is one registered WSL plugin.
type Plugin struct {
	// Name is the registry value name, which is the plugin's identifier.
	Name string `json:"name"`
	// Path is the DLL WSL loads.
	Path string `json:"path"`
}

func (p Plugin) String() string {
	if p.Path == "" {
		return p.Name
	}
	return p.Name + " (" + p.Path + ")"
}

// Plugins is the set registered on this machine.
type Plugins []Plugin

// Any reports whether anything is registered.
func (ps Plugins) Any() bool { return len(ps) > 0 }

// Names lists the plugin identifiers, sorted, for a message.
func (ps Plugins) Names() []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// ErrPluginsPresent is the refusal.
//
// It names the plugins rather than saying "plugins are registered", because
// the administrator reading this needs to know WHICH tool they were relying
// on, and the override is a decision they should make with that in front of
// them.
type ErrPluginsPresent struct {
	Plugins Plugins
}

func (e *ErrPluginsPresent) Error() string {
	return fmt.Sprintf(
		"this machine has WSL plugin(s) registered (%s), and they do NOT fire through Skrog's "+
			"docker.sock relay on the wslc backend (#406). A plugin can refuse a container from "+
			"WSLPluginAPI_ContainerStarted; through this pipe it is never asked. Refusing to serve "+
			"rather than silently disabling it.\n"+
			"  registered at HKLM\\%s\n"+
			"  To serve anyway, accepting that those hooks will not run:\n"+
			"    skrog config set wslc.ignore-plugins on\n"+
			"  The distro backend is unaffected: WSL plugins are a wslc mechanism.",
		strings.Join(e.Plugins.Names(), ", "), PluginsKey)
}
