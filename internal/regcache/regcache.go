// Package regcache runs a pull-through registry cache on the engine (#385).
//
// `engine.registry-mirrors` already existed, but it points the engine at
// SOMEBODY ELSE's mirror. There was no cache Skrog runs, so every machine and
// every CI job pulled the same layers from the internet again. The costs that
// answers are concrete: Docker Hub rate-limits anonymous pulls per IP, so a
// corporate NAT or a runner fleet hits the limit as an organisation rather
// than as the developer who sees the error; and a slow, TLS-inspected
// corporate link pays for the same bytes every time.
//
// The cache is the upstream `registry:2` image in pull-through mode, running
// as a container on the engine with its store in a named volume. A volume
// rather than a bind or a distro path on purpose: `skrog prune`, `compact` and
// `relocate` already account for volumes, so the cache's disk is visible to
// every tool that already answers "where did my space go".
//
// # Why it does not pin the engine awake
//
// The cache is a long-lived container, and both the idle-stop and the
// automatic-prune paths veto on "are containers running". Left alone, enabling
// the cache would silently disable idle stops (#41) and scheduled prunes
// (#393) — the engine would hold its RAM forever because of a container the
// user did not think of as work. So the container carries a well-known name
// and the busy probe skips it. It is infrastructure, not work.
//
// # Why it does not weaken admission control
//
// A mirror changes where bytes come from, not which image was asked for.
// `allow-registries` and `require-digest` judge the reference in the request
// (#343, #374), and that reference is identical whether or not a mirror
// serves it. So the cache cannot become a route to a registry policy forbids.
package regcache

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

const (
	// ContainerName is the cache container. Well-known because the busy probe
	// has to recognise it; see the package comment.
	ContainerName = "skrog-cache"

	// VolumeName holds the cached layers.
	VolumeName = "skrog-cache-data"

	// DefaultPort is where the cache listens inside the engine distro.
	// dockerd reaches it on loopback, which Docker treats as insecure-by-
	// default, so no insecure-registries entry is needed.
	DefaultPort = 5000

	// DefaultUpstream is Docker Hub's registry endpoint — the rate limit this
	// feature exists to answer.
	DefaultUpstream = "https://registry-1.docker.io"

	// Image is the pinned upstream registry. Pinned by digest because nothing
	// in this project fetches "latest": an engine whose cache silently changed
	// version under it would be the same class of surprise the lockfile exists
	// to prevent. registry 2.8.3.
	Image = "registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"

	// MirrorKey is the daemon.json key the cache wires itself into.
	MirrorKey = "registry-mirrors"
)

// Runner executes the docker CLI. The seam keeps everything below testable
// without an engine, the same shape internal/prune uses.
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// Options configure the cache.
type Options struct {
	// Upstream is the registry being cached; empty means Docker Hub.
	Upstream string
	// Port is the loopback port inside the distro; zero means DefaultPort.
	Port int
}

func (o Options) upstream() string {
	if strings.TrimSpace(o.Upstream) == "" {
		return DefaultUpstream
	}
	return strings.TrimSpace(o.Upstream)
}

func (o Options) port() int {
	if o.Port <= 0 {
		return DefaultPort
	}
	return o.Port
}

// MirrorURL is what goes into daemon.json's registry-mirrors.
func (o Options) MirrorURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(o.port())
}

// RunArgs is the docker invocation that starts the cache.
//
// --restart unless-stopped so it returns with the engine: the engine stops for
// reboots, idle timeouts and crashes, and a cache that needed a manual start
// after each of those would be worse than no cache, because the failure is
// silent — pulls simply get slow again.
func RunArgs(o Options) []string {
	return []string{
		"run", "-d",
		"--name", ContainerName,
		"--restart", "unless-stopped",
		"-p", "127.0.0.1:" + strconv.Itoa(o.port()) + ":5000",
		"-v", VolumeName + ":/var/lib/registry",
		"-e", "REGISTRY_PROXY_REMOTEURL=" + o.upstream(),
		// Deletion enabled so the store can be garbage-collected in place
		// rather than only by removing the volume.
		"-e", "REGISTRY_STORAGE_DELETE_ENABLED=true",
		Image,
	}
}

// ValidateUpstream refuses an upstream that would not work, early and with a
// reason, rather than leaving a container that crash-loops.
func ValidateUpstream(u string) error {
	u = strings.TrimSpace(u)
	if u == "" {
		return nil // means the default
	}
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("upstream %q needs a scheme, e.g. https://registry-1.docker.io", u)
	}
	// A pull-through cache proxies exactly one upstream. Pointing it at a
	// path, rather than a registry root, produces a cache that 404s every
	// manifest and looks like a network problem.
	rest := u
	for _, p := range []string{"https://", "http://"} {
		rest = strings.TrimPrefix(rest, p)
	}
	if strings.Contains(strings.TrimSuffix(rest, "/"), "/") {
		return fmt.Errorf("upstream %q must be a registry root, not a path", u)
	}
	return nil
}

// Status is what `skrog cache status` reports.
type Status struct {
	// Enabled is true when the container exists at all.
	Enabled bool `json:"enabled"`
	// Running is true when it is up right now. It can be false on an engine
	// that is merely stopped, which is not an error.
	Running bool `json:"running"`
	// Upstream is the registry being cached.
	Upstream string `json:"upstream,omitempty"`
	// MirrorURL is the address dockerd was pointed at.
	MirrorURL string `json:"mirrorUrl,omitempty"`
	// Wired is true when daemon.json actually lists MirrorURL. Enabled but not
	// wired means the container is up and nothing is using it, which is the
	// failure worth naming rather than leaving someone to wonder.
	Wired bool `json:"wired"`
	// DataBytes is the size of the cache volume; 0 when it could not be read.
	DataBytes uint64 `json:"dataBytes"`
}

// Probe reads the cache's current state.
func Probe(ctx context.Context, r Runner, mirrors []string) (Status, error) {
	var st Status

	out, err := r.Run(ctx, "container", "inspect", ContainerName,
		"--format", "{{.State.Running}}|{{range .Config.Env}}{{println .}}{{end}}")
	if err != nil {
		// No such container is the normal disabled case, not a failure.
		return st, nil
	}
	st.Enabled = true

	parts := strings.SplitN(strings.TrimSpace(out), "|", 2)
	st.Running = strings.TrimSpace(parts[0]) == "true"
	if len(parts) == 2 {
		for _, line := range strings.Split(parts[1], "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "REGISTRY_PROXY_REMOTEURL="); ok {
				st.Upstream = v
			}
		}
	}

	if size, err := r.Run(ctx, "system", "df", "-v", "--format",
		"{{range .Volumes}}{{if eq .Name \""+VolumeName+"\"}}{{.Size}}{{end}}{{end}}"); err == nil {
		st.DataBytes = parseSize(strings.TrimSpace(size))
	}

	// Which port it took is read back from the container rather than assumed,
	// so a cache enabled on a non-default port still reports truthfully.
	if p, err := r.Run(ctx, "container", "inspect", ContainerName,
		"--format", "{{range $k, $v := .HostConfig.PortBindings}}{{range $v}}{{.HostPort}}{{end}}{{end}}"); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(p)); convErr == nil && n > 0 {
			st.MirrorURL = Options{Port: n}.MirrorURL()
		}
	}
	if st.MirrorURL == "" {
		st.MirrorURL = Options{}.MirrorURL()
	}

	for _, m := range mirrors {
		if strings.TrimRight(m, "/") == st.MirrorURL {
			st.Wired = true
		}
	}
	return st, nil
}

// parseSize reads docker's human sizes ("1.234GB", "512MB", "0B").
func parseSize(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	units := []struct {
		suffix string
		mult   float64
	}{
		{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"kB", 1 << 10}, {"KB", 1 << 10}, {"B", 1},
	}
	for _, u := range units {
		if rest, ok := strings.CutSuffix(s, u.suffix); ok {
			n, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err != nil {
				return 0
			}
			return uint64(n * u.mult)
		}
	}
	return 0
}
