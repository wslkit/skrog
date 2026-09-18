//go:build !windows

package wslc

// ReadPlugins has nothing to read off Windows. The wslc backend only runs
// there, so reporting none is correct rather than merely convenient.
func ReadPlugins() (Plugins, error) { return nil, nil }
