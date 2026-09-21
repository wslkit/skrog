package dockercli

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// zipWith builds an in-memory zip containing one entry, as the docker CLI zip
// does.
func zipWith(t *testing.T, entry string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStagePlacesEachRole(t *testing.T) {
	dockerExe := []byte("I am docker.exe")
	dockerZip := zipWith(t, "docker/docker.exe", dockerExe)
	composeExe := []byte("I am docker-compose.exe")
	credExe := []byte("I am the credential helper")

	mux := http.NewServeMux()
	mux.HandleFunc("/docker.zip", func(w http.ResponseWriter, r *http.Request) { w.Write(dockerZip) })
	mux.HandleFunc("/compose.exe", func(w http.ResponseWriter, r *http.Request) { w.Write(composeExe) })
	mux.HandleFunc("/cred.exe", func(w http.ResponseWriter, r *http.Request) { w.Write(credExe) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := &Manifest{SchemaVersion: 1, Components: []Component{
		{
			Name: "docker", Version: "1.0", Role: RoleCLI, Target: "docker.exe",
			License: "Apache-2.0",
			Arch: map[string]Asset{"amd64": {URL: srv.URL + "/docker.zip",
				SHA256: sha(dockerZip), ZipEntry: "docker/docker.exe"}},
		},
		{
			Name: "compose", Version: "2.0", Role: RolePlugin, Target: "docker-compose.exe",
			License: "Apache-2.0",
			Arch:    map[string]Asset{"amd64": {URL: srv.URL + "/compose.exe", SHA256: sha(composeExe)}},
		},
		{
			Name: "wincred", Version: "3.0", Role: RoleHelper, Target: "docker-credential-wincred.exe",
			License: "MIT",
			Arch:    map[string]Asset{"amd64": {URL: srv.URL + "/cred.exe", SHA256: sha(credExe)}},
		},
		{
			// Published only for arm64: on an amd64 Stage this must be Skipped.
			Name: "armonly", Version: "9.0", Role: RolePlugin, Target: "armonly.exe",
			Arch: map[string]Asset{"arm64": {URL: srv.URL + "/x", SHA256: "deadbeef"}},
		},
	}}

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	pluginDir := filepath.Join(root, "plugins")
	res, err := Stage(context.Background(), m, Options{
		BinDir:    binDir,
		PluginDir: pluginDir,
		CacheDir:  filepath.Join(root, "cache"),
		Arch:      "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}

	// docker.exe extracted from the zip into binDir.
	if got, _ := os.ReadFile(filepath.Join(binDir, "docker.exe")); !bytes.Equal(got, dockerExe) {
		t.Errorf("docker.exe content = %q, want %q", got, dockerExe)
	}
	// compose plugin into pluginDir.
	if got, _ := os.ReadFile(filepath.Join(pluginDir, "docker-compose.exe")); !bytes.Equal(got, composeExe) {
		t.Errorf("compose content = %q", got)
	}
	// credential helper into binDir.
	if _, err := os.Stat(filepath.Join(binDir, "docker-credential-wincred.exe")); err != nil {
		t.Errorf("cred helper not placed: %v", err)
	}
	// docker's real embedded license shipped alongside.
	if _, err := os.Stat(filepath.Join(binDir, "licenses", "docker.LICENSE.txt")); err != nil {
		t.Errorf("license not staged: %v", err)
	}
	if len(res.Installed) != 3 {
		t.Errorf("installed %d, want 3", len(res.Installed))
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "armonly" {
		t.Errorf("skipped = %v, want [armonly]", res.Skipped)
	}
}

// The docker CLI is the one component whose asset SHAPE differs by
// architecture (#450): amd64 is Docker's zip containing docker/docker.exe,
// arm64 is a bare .exe Skrog builds because upstream publishes none.
//
// This is why ZipEntry lives on the Asset rather than the Component. While it
// was per-component, staging arm64 would have taken the zip branch and tried
// to extract an entry from a Windows executable — a failure nobody here could
// have hit, on the one architecture nobody here can test.
func TestStagePlacesABareExeWhereTheOtherArchUsesAZip(t *testing.T) {
	dockerExe := []byte("I am docker.exe, amd64, from a zip")
	dockerZip := zipWith(t, "docker/docker.exe", dockerExe)
	armExe := []byte("I am docker.exe, arm64, built by skrog")

	mux := http.NewServeMux()
	mux.HandleFunc("/docker.zip", func(w http.ResponseWriter, r *http.Request) { w.Write(dockerZip) })
	mux.HandleFunc("/docker-arm64.exe", func(w http.ResponseWriter, r *http.Request) { w.Write(armExe) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := &Manifest{SchemaVersion: 1, Components: []Component{{
		Name: "docker", Version: "1.0", Role: RoleCLI, Target: "docker.exe",
		License: "Apache-2.0",
		Arch: map[string]Asset{
			"amd64": {URL: srv.URL + "/docker.zip", SHA256: sha(dockerZip),
				ZipEntry: "docker/docker.exe"},
			"arm64": {URL: srv.URL + "/docker-arm64.exe", SHA256: sha(armExe)},
		},
	}}}

	for _, tc := range []struct {
		arch string
		want []byte
	}{
		{"amd64", dockerExe},
		{"arm64", armExe},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if _, err := Stage(context.Background(), m, Options{
				BinDir:    binDir,
				PluginDir: filepath.Join(root, "plugins"),
				CacheDir:  filepath.Join(root, "cache"),
				Arch:      tc.arch,
			}); err != nil {
				t.Fatalf("Stage(%s): %v", tc.arch, err)
			}
			got, err := os.ReadFile(filepath.Join(binDir, "docker.exe"))
			if err != nil {
				t.Fatalf("docker.exe not placed: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("docker.exe on %s = %q, want %q", tc.arch, got, tc.want)
			}
		})
	}
}

func TestStageRejectsChecksumMismatch(t *testing.T) {
	body := []byte("real content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()

	m := &Manifest{SchemaVersion: 1, Components: []Component{{
		Name: "compose", Version: "2.0", Role: RolePlugin, Target: "docker-compose.exe",
		Arch: map[string]Asset{"amd64": {URL: srv.URL, SHA256: sha([]byte("different content"))}},
	}}}

	root := t.TempDir()
	_, err := Stage(context.Background(), m, Options{
		BinDir: filepath.Join(root, "bin"), PluginDir: filepath.Join(root, "plugins"),
		CacheDir: filepath.Join(root, "cache"), Arch: "amd64",
	})
	if err == nil {
		t.Fatal("Stage must fail on a checksum mismatch")
	}
	// And nothing must be placed.
	if _, statErr := os.Stat(filepath.Join(root, "plugins", "docker-compose.exe")); statErr == nil {
		t.Error("a mismatched download must not be placed")
	}
}
