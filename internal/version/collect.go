package version

import (
	"context"

	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/wsl"
)

// Collector assembles a Report from the machine. Every source is injectable so
// the whole thing is testable without WSL, PATH, or an engine.
type Collector struct {
	// App is Skrog's own version string.
	App string
	// Env supplies PATH and docker config discovery.
	Env Env
	// WSL reports the host's WSL release. Optional.
	WSL wsl.WSL
	// Provisioner reads the install manifest. Optional.
	Provisioner *provision.Provisioner
	// Options locates the manifest.
	Options provision.Options
	// ProbeAPI returns the negotiated engine API version. Optional: when nil or
	// failing, APIVersion is left empty rather than guessed.
	ProbeAPI func(ctx context.Context) (string, error)
}

// Collect gathers everything, degrading gracefully. A component that cannot be
// determined is reported as unknown rather than failing the command: `skrog
// version` is the first thing someone runs when things are broken, so it must
// work on a half-installed machine.
func (c *Collector) Collect(ctx context.Context) *Report {
	r := &Report{App: c.App}

	if c.WSL != nil {
		if st, err := c.WSL.Status(ctx); err == nil {
			r.WSL = st.Version
		}
	}

	if c.Provisioner != nil {
		if m, err := c.Provisioner.ReadManifest(c.Options); err == nil {
			r.Engine = EngineInfo{
				Installed:    true,
				Backend:      m.BackendName(),
				Version:      m.EngineVersion,
				Distro:       m.Distro,
				Rootfs:       m.RootfsSHA256,
				Ref:          m.EngineRefOrDerived(),
				WSLAtInstall: m.WSLVersion,
			}
		}
	}

	r.Docker = FindDockerBinaries(c.Env)
	r.Context, r.ContextSource = DockerContext(c.Env)

	if c.ProbeAPI != nil {
		if v, err := c.ProbeAPI(ctx); err == nil {
			r.APIVersion = v
		}
	}

	r.analyze()
	return r
}
