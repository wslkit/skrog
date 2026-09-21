package release_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/release"
)

func TestEmbeddedManifestIsValid(t *testing.T) {
	// The manifest is hand-edited alongside guest/rootfs/versions.env, so a
	// typo must fail the build's tests rather than a user's install.
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Engines) == 0 {
		t.Fatal("no engines in the embedded manifest")
	}

	defaults := 0
	for _, e := range m.Engines {
		if e.Default {
			defaults++
		}
		if e.Version == "" {
			t.Error("an engine entry has no version")
		}
		if e.Rootfs.URL == "" {
			t.Errorf("engine %s has no rootfs URL", e.Version)
		}
		// A digest, when present, must be a full SHA-256.
		if e.Rootfs.SHA256 != "" && len(e.Rootfs.SHA256) != 64 {
			t.Errorf("engine %s has a malformed sha256 (%d chars)", e.Version, len(e.Rootfs.SHA256))
		}
		for _, c := range []string{"dockerd", "containerd", "runc", "buildkit"} {
			if e.Components[c] == "" {
				t.Errorf("engine %s does not record the %s version", e.Version, c)
			}
		}
	}
	if defaults != 1 {
		t.Errorf("%d engines marked default, want exactly 1", defaults)
	}
}

func TestEmbeddedManifestMatchesRootfsPins(t *testing.T) {
	// The manifest and the rootfs build must agree, or `skrog version` would
	// report components the installed rootfs does not contain.
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, err := m.Engine("")
	if err != nil {
		t.Fatalf("default engine: %v", err)
	}

	pins := readVersionsEnv(t)
	if pins["ENGINE_VERSION"] != e.Version {
		t.Errorf("manifest engine %s != versions.env ENGINE_VERSION %s",
			e.Version, pins["ENGINE_VERSION"])
	}
	for envKey, component := range map[string]string{
		"CONTAINERD_VERSION":    "containerd",
		"RUNC_VERSION":          "runc",
		"BUILDKIT_VERSION":      "buildkit",
		"ALPINE_ROOTFS_VERSION": "alpine",
	} {
		want := strings.TrimPrefix(pins[envKey], "v")
		if want == "" {
			t.Fatalf("versions.env has no %s", envKey)
		}
		if got := e.Components[component]; got != want {
			t.Errorf("manifest %s = %q, versions.env %s = %q", component, got, envKey, want)
		}
	}

	// The rootfs URL must point at the *revisioned* artifact
	// (ENGINE_VERSION-ROOTFS_REVISION): a revision bump that forgets the
	// manifest would otherwise keep installing the previous rootfs while
	// `skrog version` claims the new one's contents.
	//
	// Two spellings are accepted, and the reason is a fact about what is
	// published rather than laxness. Since #388 the build names its output
	// skrog-rootfs-<version>-<rev>-<arch>.tar.gz, because an unsuffixed name
	// that silently means amd64 is the ambiguity that issue is about. Every
	// release up to rootfs-v29.8.1-1 was cut before that and carries the bare
	// name; those bytes exist under that name and will not be renamed.
	if rev := pins["ROOTFS_REVISION"]; rev != "" {
		stem := "skrog-rootfs-" + pins["ENGINE_VERSION"] + "-" + rev
		accepted := []string{
			stem + "-" + release.EngineArch + ".tar.gz", // cut after #388
			stem + ".tar.gz", // cut before it
		}
		ok := false
		for _, name := range accepted {
			if strings.HasSuffix(e.Rootfs.URL, "/"+name) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("manifest rootfs URL %q ends in none of %v (versions.env ROOTFS_REVISION=%s)",
				e.Rootfs.URL, accepted, rev)
		}
	}
}

func TestEngineSelectionIsExact(t *testing.T) {
	// A pin that quietly resolves to something else is not a pin.
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def, err := m.Engine("")
	if err != nil {
		t.Fatalf("default: %v", err)
	}

	if _, err := m.Engine(def.Version); err != nil {
		t.Errorf("exact version %q not found: %v", def.Version, err)
	}

	// A truncated prefix of a real version must not match it.
	prefix := def.Version[:len(def.Version)-2]
	if _, err := m.Engine(prefix); err == nil {
		t.Errorf("prefix %q resolved to an engine; selection must be exact", prefix)
	}
}

func TestEngineUnknownVersionListsChoices(t *testing.T) {
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = m.Engine("99.0.0")
	if err == nil {
		t.Fatal("unknown version succeeded")
	}
	// The error has to say what IS available, or the user is left guessing.
	for _, v := range m.Versions() {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("error %q does not list available version %q", err, v)
		}
	}
}

func TestPublishedRequiresBothURLAndChecksum(t *testing.T) {
	cases := []struct {
		name string
		r    release.Rootfs
		want bool
	}{
		{"both present", release.Rootfs{URL: "https://x/y.tar.gz", SHA256: strings.Repeat("a", 64)}, true},
		{"no checksum", release.Rootfs{URL: "https://x/y.tar.gz"}, false},
		{"no url", release.Rootfs{SHA256: strings.Repeat("a", 64)}, false},
		{"neither", release.Rootfs{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := release.Engine{Version: "1.0.0", Rootfs: tc.r}
			if got := e.Published(); got != tc.want {
				t.Errorf("Published() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestErrNotPublishedExplainsTheWayOut(t *testing.T) {
	// This is what a developer sees before the first release is cut, so it must
	// name the escape hatch rather than just refusing.
	err := error(&release.ErrNotPublished{Version: "29.7.2"})
	for _, want := range []string{"29.7.2", "--rootfs-url", "--rootfs-sha256", "build.sh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should mention %q:\n%s", want, err)
		}
	}
	var typed *release.ErrNotPublished
	if !errors.As(err, &typed) {
		t.Error("ErrNotPublished is not matchable with errors.As")
	}
}
