//go:build windows

package wslc

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// ReadPlugins enumerates the WSL plugins registered on this machine.
//
// A missing key is the ordinary state of a machine with no plugins, and is not
// an error.
//
// A key that exists but cannot be enumerated IS an error, and the caller fails
// closed on it — the same reasoning readRegistryAllowlist records: a list that
// cannot be read is not an absent list, and guessing in the permissive
// direction is how a bypass ships.
func ReadPlugins() (Plugins, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, PluginsKey, registry.READ)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil, nil
		}
		return nil, fmt.Errorf("opening %s: %w", PluginsKey, err)
	}
	defer key.Close()

	names, err := key.ReadValueNames(-1)
	if err != nil {
		return nil, fmt.Errorf("enumerating %s: %w", PluginsKey, err)
	}

	var out Plugins
	for _, n := range names {
		// WSL itself skips values of the wrong type ("Plugin value: '%ls' has
		// incorrect type: %lu, skipping"), so a non-string value is genuinely
		// not a loaded plugin and must not be reported as one -- this detector
		// has to match what WSL will actually load, in both directions.
		path, _, err := key.GetStringValue(n)
		if err != nil {
			continue
		}
		out = append(out, Plugin{Name: n, Path: path})
	}
	return out, nil
}
