package main

import (
	"testing"

	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/provision"
)

// Every per-start setting has to survive the fold, because the bug was a
// caller that applied none of them (#490). Asserted field by field rather than
// with a struct compare, so a NEW per-start option added to provision.Options
// and forgotten here shows up as a missing assertion rather than passing
// silently.
func TestWithStartSettingsCarriesEveryPerStartSetting(t *testing.T) {
	c := config.Config{
		GPU:                true,
		GPUVendor:          "amd",
		EmulationPlatforms: "arm64",
		Proxy:              "http://proxy.corp:3128",
		NoProxy:            "localhost,.corp",
		ImportHostCAs:      true,
	}
	got := withStartSettings(provision.Options{StateDir: `C:\state`}, c,
		func() []byte { return []byte("PEM") })

	if !got.GPUEnabled {
		t.Error("GPUEnabled dropped")
	}
	if got.GPUVendor != "amd" {
		t.Errorf("GPUVendor = %q", got.GPUVendor)
	}
	if got.EmulationPlatforms != "arm64" {
		t.Errorf("EmulationPlatforms = %q; this is the one that fails loudly, "+
			"with `exec format error` on a machine where it used to work", got.EmulationPlatforms)
	}
	if got.Network.Proxy != "http://proxy.corp:3128" || got.Network.NoProxy != "localhost,.corp" {
		t.Errorf("proxy settings dropped: %+v", got.Network)
	}
	if string(got.Network.HostCAPEM) != "PEM" {
		t.Errorf("HostCAPEM = %q", got.Network.HostCAPEM)
	}
	// What the caller already set must survive.
	if got.StateDir != `C:\state` {
		t.Errorf("StateDir = %q", got.StateDir)
	}
}

// The CA store is read only when the setting asks for it. Reading it anyway
// would cost a Windows certificate enumeration on every engine start, on every
// machine, for a feature almost nobody turns on.
func TestWithStartSettingsSkipsTheCAStoreWhenOff(t *testing.T) {
	called := false
	got := withStartSettings(provision.Options{}, config.Config{ImportHostCAs: false},
		func() []byte { called = true; return []byte("PEM") })
	if called {
		t.Error("read the host CA store with ImportHostCAs off")
	}
	if got.Network.HostCAPEM != nil {
		t.Errorf("HostCAPEM = %q with the setting off", got.Network.HostCAPEM)
	}
}

// A nil provider is not a crash: the supervisor supplies a cached reader, a
// one-shot command supplies a live one, and a caller that has neither should
// start an engine rather than panic.
func TestWithStartSettingsToleratesNoCAProvider(t *testing.T) {
	got := withStartSettings(provision.Options{}, config.Config{ImportHostCAs: true}, nil)
	if got.Network.HostCAPEM != nil {
		t.Errorf("HostCAPEM = %q with no provider", got.Network.HostCAPEM)
	}
}
