package dockercli_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/dockercli"
)

// third_party/docker-cli builds the Windows arm64 docker.exe that upstream
// does not publish (#450). Its pinned version and the manifest's `docker`
// component are two hand-edited files describing the same binary, so they
// drift the moment someone bumps one.
//
// The failure that drift produces is nasty and quiet: `skrog cli install`
// would lay down an arm64 docker.exe built from one tag while `skrog version`
// and the manifest both claim another, and the only way to notice is to run
// `docker version` on an ARM machine and compare it against a JSON file.
//
// Mirrors internal/release/versions_env_test.go, which pins the engine the
// same way and for the same reason.
func readCLIVersionsEnv(t *testing.T) map[string]string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	var path string
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "third_party", "docker-cli", "versions.env")
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
			break
		}
		dir = filepath.Dir(dir)
	}
	if path == "" {
		t.Skip("third_party/docker-cli/versions.env not found; skipping cross-check")
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return out
}

func TestDockerCLIVersionMatchesTheBuildPin(t *testing.T) {
	pins := readCLIVersionsEnv(t)

	m, err := dockercli.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var docker *dockercli.Component
	for i := range m.Components {
		if m.Components[i].Name == "docker" {
			docker = &m.Components[i]
			break
		}
	}
	if docker == nil {
		t.Fatal("the manifest has no `docker` component")
	}

	if want := pins["DOCKER_CLI_VERSION"]; want == "" {
		t.Error("versions.env has no DOCKER_CLI_VERSION")
	} else if docker.Version != want {
		t.Errorf("manifest docker version %q != versions.env DOCKER_CLI_VERSION %q\n"+
			"  the same binary is installed on both architectures; two versions is a support trap",
			docker.Version, want)
	}

	// The tag is the version with a v, and the commit must be pinned at all.
	// build.sh only WARNS on an unpinned SHA, because a warning is right for
	// someone bisecting locally -- but shipping without one means a moved tag
	// silently changes what every docker command on the machine runs (#88).
	if tag, ver := pins["DOCKER_CLI_TAG"], pins["DOCKER_CLI_VERSION"]; tag != "v"+ver {
		t.Errorf("DOCKER_CLI_TAG %q is not v%s", tag, ver)
	}
	if sha := pins["DOCKER_CLI_SHA"]; len(sha) != 40 {
		t.Errorf("DOCKER_CLI_SHA %q is not a 40-hex commit; build.sh would only warn", sha)
	}
}

// The arm64 asset, once published, must come from this repository's own
// release. It is the one binary Skrog builds rather than fetches, and a URL
// pointing anywhere else would mean the attestation and signature attached to
// the tag describe different bytes than the ones installed.
//
// Skipped while arm64 is still a placeholder, which is the state before the
// first dockercli- release is cut.
func TestArm64DockerCLIComesFromOurOwnRelease(t *testing.T) {
	m, err := dockercli.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range m.Components {
		if c.Name != "docker" {
			continue
		}
		a, ok := c.Arch["arm64"]
		if !ok || a.URL == "" {
			t.Skip("no arm64 docker CLI published yet (#450)")
		}
		const prefix = "https://github.com/wslkit/skrog/releases/download/dockercli-v"
		if !strings.HasPrefix(a.URL, prefix) {
			t.Errorf("arm64 docker CLI URL %q does not come from a dockercli- release of this repo.\n"+
				"  Skrog builds this binary; its provenance is the tag it was attached to.", a.URL)
		}
		// And amd64 must still be Docker's, which is the whole point of the
		// asymmetry. Building both would put every user behind our build.
		if up, ok := c.Arch["amd64"]; ok && !strings.Contains(up.URL, "download.docker.com") {
			t.Errorf("amd64 docker CLI URL %q is no longer Docker's own published binary.\n"+
				"  If that is deliberate, third_party/docker-cli/build.sh and docs/docker-cli.md\n"+
				"  both argue the opposite and need rewriting first.", up.URL)
		}
	}
}
