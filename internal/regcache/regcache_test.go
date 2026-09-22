package regcache

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeRunner struct {
	out map[string]string
	err map[string]bool
}

func (f fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	key := strings.Join(args, " ")
	for k, v := range f.out {
		if strings.HasPrefix(key, k) {
			if f.err[k] {
				return "", errors.New("boom")
			}
			return v, nil
		}
	}
	return "", errors.New("no such command")
}

// The image must stay pinned by digest. Nothing in this project fetches
// "latest", and a cache that silently changed version under an engine is the
// same class of surprise the lockfile exists to prevent.
func TestImageIsPinnedByDigest(t *testing.T) {
	if !strings.Contains(Image, "@sha256:") {
		t.Errorf("Image = %q; it must be pinned by digest", Image)
	}
}

// The container name is load-bearing: cmd/skrog's busy probe recognises it to
// keep the cache from pinning the engine awake. Renaming it here without
// updating the probe would silently disable idle stops and scheduled prunes.
func TestContainerNameIsStable(t *testing.T) {
	if ContainerName != "skrog-cache" {
		t.Errorf("ContainerName = %q; the busy probe matches on this exact name", ContainerName)
	}
}

func TestRunArgs(t *testing.T) {
	args := strings.Join(RunArgs(Options{}), " ")

	for _, want := range []string{
		"--name " + ContainerName,
		// Loopback only: the cache must not be reachable from the network.
		"-p 127.0.0.1:5000:5000",
		"-v " + VolumeName + ":/var/lib/registry",
		"REGISTRY_PROXY_REMOTEURL=" + DefaultUpstream,
		// It must come back with the engine, or the cache silently stops
		// working after the first reboot and pulls just get slow again.
		"--restart unless-stopped",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("RunArgs missing %q:\n%s", want, args)
		}
	}
}

func TestRunArgsHonorsOptions(t *testing.T) {
	args := strings.Join(RunArgs(Options{Upstream: "https://ghcr.io", Port: 5555}), " ")
	if !strings.Contains(args, "REGISTRY_PROXY_REMOTEURL=https://ghcr.io") {
		t.Errorf("upstream not applied:\n%s", args)
	}
	if !strings.Contains(args, "-p 127.0.0.1:5555:5000") {
		t.Errorf("port not applied:\n%s", args)
	}
	if got := (Options{Port: 5555}).MirrorURL(); got != "http://127.0.0.1:5555" {
		t.Errorf("MirrorURL = %q", got)
	}
}

func TestValidateUpstream(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{"https://registry-1.docker.io", false},
		{"https://ghcr.io", false},
		// This case asserted `false` until #421: plain HTTP was accepted
		// silently. It is refused now unless --insecure is passed, which the
		// tests below cover.
		{"http://registry.internal:5000", true},
		{"registry-1.docker.io", true},             // no scheme
		{"https://ghcr.io/myorg", true},            // a path, not a registry root
		{"https://registry-1.docker.io/v2/", true}, // ditto, the common mistake
	} {
		err := ValidateUpstream(tc.in, false)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateUpstream(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
	}
	// A trailing slash on a root is fine — people paste it.
	if err := ValidateUpstream("https://ghcr.io/", false); err != nil {
		t.Errorf("a trailing slash was rejected: %v", err)
	}
}

// No container is the ordinary disabled state, not an error to report.
func TestProbeDisabled(t *testing.T) {
	st, err := Probe(context.Background(), fakeRunner{}, nil)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if st.Enabled || st.Running {
		t.Errorf("no container reported as %+v; want disabled", st)
	}
}

func TestProbeRunningAndWired(t *testing.T) {
	r := fakeRunner{out: map[string]string{
		"container inspect skrog-cache --format {{.State.Running}}": "true|REGISTRY_PROXY_REMOTEURL=https://ghcr.io\n",
		"system df": "1.5GB",
		"container inspect skrog-cache --format {{range $k": "5000",
	}}

	st, err := Probe(context.Background(), r, []string{"http://127.0.0.1:5000"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !st.Enabled || !st.Running {
		t.Errorf("got %+v; want enabled and running", st)
	}
	if st.Upstream != "https://ghcr.io" {
		t.Errorf("Upstream = %q", st.Upstream)
	}
	if !st.Wired {
		t.Error("Wired = false although daemon.json lists the mirror")
	}
	if st.DataBytes == 0 {
		t.Error("DataBytes = 0; the volume size was not parsed")
	}
}

// Up but not wired is the state worth naming: the cache looks healthy and
// nothing is using it.
func TestProbeRunningButNotWired(t *testing.T) {
	r := fakeRunner{out: map[string]string{
		"container inspect skrog-cache --format {{.State.Running}}": "true|",
		"container inspect skrog-cache --format {{range $k":         "5000",
	}}
	st, _ := Probe(context.Background(), r, []string{"https://someone-elses-mirror.example"})
	if !st.Enabled || st.Wired {
		t.Errorf("got %+v; want enabled but not wired", st)
	}
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint64
	}{
		{"", 0},
		{"0B", 0},
		{"512MB", 512 << 20},
		{"1.5GB", uint64(1.5 * (1 << 30))},
		{"nonsense", 0},
	} {
		if got := parseSize(tc.in); got != tc.want {
			t.Errorf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestValidateUpstreamRefusesPlainHTTP(t *testing.T) {
	err := ValidateUpstream("http://mirror.internal", false)
	if err == nil {
		t.Fatal("accepted an http:// upstream")
	}
	// The reason has to say why this is different from an ordinary unencrypted
	// download, or it reads as pedantry and gets --insecure'd reflexively.
	for _, want := range []string{"every unpinned", "substitute", "--insecure"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestValidateUpstreamAllowsPlainHTTPWhenAsked(t *testing.T) {
	if err := ValidateUpstream("http://mirror.internal", true); err != nil {
		t.Errorf("--insecure did not allow http://: %v", err)
	}
}

func TestValidateUpstreamStillAcceptsHTTPS(t *testing.T) {
	if err := ValidateUpstream("https://ghcr.io", false); err != nil {
		t.Errorf("https rejected: %v", err)
	}
}

func TestUpstreamHost(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://ghcr.io", "ghcr.io", true},
		{"https://ghcr.io/", "ghcr.io", true},
		{"http://mirror.internal:5000", "mirror.internal:5000", true},
		{"https://ghcr.io/v2/path", "", false},
		{"", "", false},
	} {
		got, ok := UpstreamHost(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("UpstreamHost(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
