// Package dockercli vendors the upstream Docker command-line tools — the docker
// CLI, the compose and buildx plugins, and the wincred credential helper — so a
// user can uninstall Docker Desktop entirely and still have a working `docker`
// on Windows (#66).
//
// The model mirrors the engine (internal/release): exact versions and per-arch
// URL+SHA256 are pinned in an embedded manifest, fetched and verified at install
// time, and cached — deterministic, "nothing fetched as latest", and packable
// into the air-gap bundle. The bytes come from each upstream project's own
// release assets, so no new hosting (or Docker Desktop) is involved.
//
// Everything here is Apache-2.0 (docker/cli, compose, buildx) or MIT (the
// credential helper); Stage lays each project's LICENSE down beside the binary.
package dockercli

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

// Role is how an installed tool is wired: the docker CLI goes on PATH, plugins
// go in the docker cli-plugins directory, and a helper goes on PATH under the
// exact name docker will exec.
type Role string

const (
	// RoleCLI is docker.exe itself — placed on PATH as `docker`.
	RoleCLI Role = "cli"
	// RolePlugin is a docker CLI plugin (compose, buildx) — placed in the
	// cli-plugins directory so `docker compose` / `docker buildx` resolve it.
	RolePlugin Role = "plugin"
	// RoleHelper is a credential helper — placed on PATH under the name docker
	// execs (docker-credential-<store>).
	RoleHelper Role = "helper"
)

// Manifest is the set of CLI tools a build can install.
type Manifest struct {
	SchemaVersion int         `json:"schemaVersion"`
	Components    []Component `json:"components"`
}

// Component is one installable tool and its per-arch, checksum-pinned assets.
type Component struct {
	// Name is the logical id, e.g. "docker", "compose", "buildx", "wincred".
	Name string `json:"name"`
	// Version is the exact upstream version installed.
	Version string `json:"version"`
	// Role decides where the installed file lands (see Role).
	Role Role `json:"role"`
	// Target is the installed filename, e.g. "docker.exe",
	// "docker-compose.exe", "docker-credential-wincred.exe".
	Target string `json:"target"`
	// License is the SPDX id; LicenseURL is the raw URL of the project's LICENSE.
	License    string `json:"license"`
	LicenseURL string `json:"licenseURL,omitempty"`
	// Arch maps a GOARCH ("amd64", "arm64") to its pinned asset.
	Arch map[string]Asset `json:"arch"`
}

// Asset locates one downloadable file and the digest it must match.
type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	// ZipEntry, when set, means this asset is a .zip and this
	// slash-separated path inside it is the file to extract; empty means the
	// asset is the binary itself.
	//
	// Per ASSET, not per component, since #450. Docker publishes its amd64
	// CLI as a zip containing docker/docker.exe, and the arm64 binary Skrog
	// builds is a bare .exe -- one component, two shapes. While this lived on
	// the Component, staging arm64 would have tried to unzip an executable.
	ZipEntry string `json:"zipEntry,omitempty"`
}

// Published reports whether the component can be installed for the given arch:
// a non-placeholder asset carries both a URL and a checksum, since nothing is
// imported unverified. An arch with no entry (or an empty one) is treated as
// "not published for this platform", exactly like the engine manifest.
func (c Component) Published(arch string) bool {
	a, ok := c.Arch[arch]
	return ok && a.URL != "" && a.SHA256 != ""
}

// Load parses the embedded manifest.
func Load() (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("parsing embedded docker-cli manifest: %w", err)
	}
	if m.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported docker-cli manifest schemaVersion %d", m.SchemaVersion)
	}
	if len(m.Components) == 0 {
		return nil, fmt.Errorf("docker-cli manifest lists no components")
	}
	return &m, nil
}

// HostArch is the running architecture as a manifest arch key.
func HostArch() string { return runtime.GOARCH }

// ForArch returns the components that are published for arch, and the names of
// any that are not (so the caller can report an honest partial install rather
// than silently skipping tools). Order follows the manifest.
func (m *Manifest) ForArch(arch string) (available []Component, unavailable []string) {
	for _, c := range m.Components {
		if c.Published(arch) {
			available = append(available, c)
		} else {
			unavailable = append(unavailable, c.Name)
		}
	}
	return available, unavailable
}

// Component returns the entry with the given name.
func (m *Manifest) Component(name string) (*Component, error) {
	for i := range m.Components {
		if m.Components[i].Name == name {
			return &m.Components[i], nil
		}
	}
	return nil, fmt.Errorf("no docker-cli component %q (have: %s)", name, strings.Join(m.names(), ", "))
}

func (m *Manifest) names() []string {
	out := make([]string, 0, len(m.Components))
	for _, c := range m.Components {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}
