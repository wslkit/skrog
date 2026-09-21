// Package release exposes the locked version manifest compiled into the binary.
//
// PLAN §04: nothing is fetched as "latest at install time". The manifest ships
// inside the executable, so a given Skrog build installs exactly the
// components it was tested with, and `skrog install` on a CI runner in six
// months produces the same engine it produces today. That determinism is a
// direct anti-feature of Docker Desktop's auto-updating, and the reason the
// manifest is embedded rather than downloaded.
package release

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
)

//go:embed manifest.json
var manifestJSON []byte

// Manifest is the set of engines a build can install.
type Manifest struct {
	SchemaVersion int      `json:"schemaVersion"`
	Engines       []Engine `json:"engines"`
}

// Engine is one installable engine version and the rootfs that carries it,
// per architecture.
//
// Rootfs is keyed by GOARCH ("amd64", "arm64"), not by Alpine's or Docker's
// spelling, because every consumer compares it against runtime.GOARCH. An
// architecture that is absent from the map is one this engine was never built
// for -- which is a different thing from one that was built and not yet
// published, and the two produce different errors.
type Engine struct {
	Version    string            `json:"version"`
	Default    bool              `json:"default"`
	Rootfs     map[string]Rootfs `json:"rootfs"`
	Components map[string]string `json:"components"`
}

// Rootfs locates a rootfs tarball and the digest it must match.
type Rootfs struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Architectures lists the GOARCH values this engine has an entry for, sorted.
// Used to say what IS available when the host's architecture is not.
func (e Engine) Architectures() []string {
	out := make([]string, 0, len(e.Rootfs))
	for a := range e.Rootfs {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// RootfsFor returns the rootfs for one architecture.
//
// The two failure modes are deliberately distinct, because the user's next
// move differs. No entry at all means this engine was never built for that
// host: nothing they wait for will change it, and the answer is another
// engine or another product. An entry with no checksum means the release has
// not been cut yet: it is coming, and a development build is the usual cause.
func (e Engine) RootfsFor(arch string) (Rootfs, error) {
	r, ok := e.Rootfs[arch]
	if !ok {
		return Rootfs{}, &ErrUnsupportedHostArch{Host: arch, Available: e.Architectures()}
	}
	if r.URL == "" || r.SHA256 == "" {
		return Rootfs{}, &ErrNotPublished{Version: e.Version, Arch: arch}
	}
	return r, nil
}

// HostRootfs is RootfsFor(runtime.GOARCH), which is what every caller that is
// about to install something actually wants.
func (e Engine) HostRootfs() (Rootfs, error) { return e.RootfsFor(runtime.GOARCH) }

// Published reports whether this entry can be installed ON THIS HOST.
//
// Host-relative on purpose. `skrog engine list` and `engine rollback` offer
// what this machine can actually run; an amd64-only engine is not a rollback
// target on an arm64 box, and listing it as one would fail at the download.
func (e Engine) Published() bool {
	_, err := e.HostRootfs()
	return err == nil
}

// ErrNotPublished reports an engine entry whose rootfs release has not been
// cut for this architecture yet.
type ErrNotPublished struct {
	Version string
	Arch    string
}

func (e *ErrNotPublished) Error() string {
	arch := e.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	return fmt.Sprintf("engine %s has no published %s rootfs checksum in this build's manifest.\n"+
		"This happens in a development build before the rootfs release is cut. Either:\n"+
		"  - install a release build of skrog, or\n"+
		"  - build the rootfs yourself (guest/rootfs/build.sh) and pass\n"+
		"    --rootfs-url file:///... together with --rootfs-sha256 <digest>",
		e.Version, arch)
}

// Load parses the embedded manifest.
func Load() (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("parsing embedded release manifest: %w", err)
	}
	// 2 since #388: rootfs became a map keyed by GOARCH. Schema 1 had a single
	// rootfs object, which json.Unmarshal would quietly decode into an empty
	// map here -- an engine with no architectures at all, refusing every
	// install with a message about the host rather than about the manifest.
	// Rejecting the version outright is how that stays legible.
	if m.SchemaVersion != 2 {
		// A future binary reading an older embedded file cannot happen, but a
		// hand-edited manifest can, and silently misreading it would be worse.
		return nil, fmt.Errorf("unsupported manifest schemaVersion %d", m.SchemaVersion)
	}
	if len(m.Engines) == 0 {
		return nil, fmt.Errorf("release manifest lists no engines")
	}
	for _, e := range m.Engines {
		if len(e.Rootfs) == 0 {
			return nil, fmt.Errorf("engine %s lists no rootfs for any architecture", e.Version)
		}
	}
	return &m, nil
}

// Engine returns the entry for a version, or the default when version is empty.
//
// Selection is exact: `--engine-version 29.7` does not resolve to 29.7.2,
// because a pin that quietly matches something else is not a pin.
func (m *Manifest) Engine(version string) (*Engine, error) {
	if version == "" {
		for i := range m.Engines {
			if m.Engines[i].Default {
				return &m.Engines[i], nil
			}
		}
		return nil, fmt.Errorf("release manifest marks no default engine (available: %s)",
			strings.Join(m.Versions(), ", "))
	}
	for i := range m.Engines {
		if m.Engines[i].Version == version {
			return &m.Engines[i], nil
		}
	}
	return nil, fmt.Errorf("engine version %q is not in this build's manifest (available: %s)",
		version, strings.Join(m.Versions(), ", "))
}

// Versions lists every engine version the build can install, sorted.
func (m *Manifest) Versions() []string {
	out := make([]string, 0, len(m.Engines))
	for _, e := range m.Engines {
		out = append(out, e.Version)
	}
	sort.Strings(out)
	return out
}
