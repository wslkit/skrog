package dockercli

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
)

// licenseFS carries each upstream project's LICENSE text, embedded so the
// attribution ships with the binary and is written out even for an air-gap
// install (no network to fetch it). Files are named <component>.LICENSE.txt.
//
//go:embed licenses
var licenseFS embed.FS

// Options configures Stage: where each kind of tool is placed and how downloads
// are fetched and cached.
type Options struct {
	// BinDir holds docker.exe, the credential helper, and the licenses/ dir; it
	// is the directory put on PATH.
	BinDir string
	// PluginDir is docker's cli-plugins directory, where compose and buildx go
	// (typically %USERPROFILE%\.docker\cli-plugins).
	PluginDir string
	// CacheDir holds verified downloads, reused across runs.
	CacheDir string
	// Arch overrides the target architecture; empty means the host's.
	Arch string
	// HTTPClient overrides the download client (tests, proxy handling).
	HTTPClient *http.Client
	// Logger receives progress; nil is fine.
	Logger *slog.Logger
}

func (o Options) logf(format string, args ...any) {
	if o.Logger != nil {
		o.Logger.Info(fmt.Sprintf(format, args...))
	}
}

func (o Options) arch() string {
	if o.Arch != "" {
		return o.Arch
	}
	return HostArch()
}

// InstalledTool records one placed tool.
type InstalledTool struct {
	Name    string
	Version string
	Role    Role
	Path    string
}

// Result summarizes a Stage run.
type Result struct {
	// Installed lists every tool placed, in manifest order.
	Installed []InstalledTool
	// Skipped names components with no published asset for the target arch, so
	// the caller can report an honest partial install (e.g. arm64 before an
	// upstream ships an arm64 binary) rather than silently omitting them.
	Skipped []string
}

// Stage fetches, verifies, and places every CLI tool published for the target
// architecture. It is idempotent: a cached, checksum-matching download is
// reused, and placement overwrites in place, so re-running upgrades cleanly.
func Stage(ctx context.Context, m *Manifest, opts Options) (Result, error) {
	arch := opts.arch()
	var res Result

	for _, c := range m.Components {
		if !c.Published(arch) {
			res.Skipped = append(res.Skipped, c.Name)
			continue
		}
		asset := c.Arch[arch]

		ext := ".exe"
		if asset.ZipEntry != "" {
			ext = ".zip"
		}
		cached := filepath.Join(opts.CacheDir, fmt.Sprintf("%s-%s-%s%s", c.Name, c.Version, arch, ext))
		opts.logf("fetching %s %s (%s)", c.Name, c.Version, arch)
		if err := opts.fetchVerified(ctx, asset.URL, asset.SHA256, cached); err != nil {
			return res, fmt.Errorf("%s: %w", c.Name, err)
		}

		dest, err := opts.placement(c)
		if err != nil {
			return res, err
		}
		if asset.ZipEntry != "" {
			if err := extractZipEntry(cached, asset.ZipEntry, dest); err != nil {
				return res, fmt.Errorf("%s: %w", c.Name, err)
			}
		} else if err := copyFile(cached, dest, 0o755); err != nil {
			return res, fmt.Errorf("%s: placing binary: %w", c.Name, err)
		}

		if err := opts.writeLicense(c); err != nil {
			return res, fmt.Errorf("%s: writing license: %w", c.Name, err)
		}
		res.Installed = append(res.Installed, InstalledTool{
			Name: c.Name, Version: c.Version, Role: c.Role, Path: dest,
		})
		opts.logf("installed %s -> %s", c.Name, dest)
	}
	return res, nil
}

// placement is where a component's file lands, by role.
func (o Options) placement(c Component) (string, error) {
	switch c.Role {
	case RoleCLI, RoleHelper:
		return filepath.Join(o.BinDir, c.Target), nil
	case RolePlugin:
		if o.PluginDir == "" {
			return "", fmt.Errorf("%s is a plugin but no plugin directory was configured", c.Name)
		}
		return filepath.Join(o.PluginDir, c.Target), nil
	default:
		return "", fmt.Errorf("%s has unknown role %q", c.Name, c.Role)
	}
}

// writeLicense lays the component's embedded LICENSE beside the binaries, so the
// Apache-2.0/MIT attribution ships with every install.
func (o Options) writeLicense(c Component) error {
	data, err := licenseFS.ReadFile("licenses/" + c.Name + ".LICENSE.txt")
	if err != nil {
		// A missing embedded license is a build-time omission, not a reason to
		// fail a user's install; note it and move on.
		o.logf("no embedded license for %s (%s); skipping", c.Name, c.License)
		return nil
	}
	dir := filepath.Join(o.BinDir, "licenses")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, c.Name+".LICENSE.txt"), data, 0o644)
}
