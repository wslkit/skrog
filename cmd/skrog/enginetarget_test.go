package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/provision"
)

// writeManifest drops a manifest where the provisioner will find it.
func writeManifest(t *testing.T, m provision.Manifest) provision.Options {
	t.Helper()
	dir := t.TempDir()
	opts := provision.Options{StateDir: dir}
	p := &provision.Provisioner{}
	if err := p.SaveManifest(opts, &m); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	return opts
}

// A manifest from before #335 has no backend field at all, and must keep
// resolving to the distro it names. This is every existing install.
func TestResolveEngineTargetDefaultsToDistro(t *testing.T) {
	opts := writeManifest(t, provision.Manifest{Distro: "skrog-engine"})
	got, ok := resolveEngineTarget(&provision.Provisioner{}, opts)
	if !ok {
		t.Fatal("an install with a distro was not found")
	}
	if got.Distro != "skrog-engine" {
		t.Errorf("distro = %q", got.Distro)
	}
}

// `supervise --distro X` with no manifest worked before any of this and must
// keep working, or driving Skrog by hand breaks.
func TestResolveEngineTargetFallsBackToTheFlag(t *testing.T) {
	opts := provision.Options{StateDir: t.TempDir(), Distro: "hand-rolled"}
	got, ok := resolveEngineTarget(&provision.Provisioner{}, opts)
	if !ok {
		t.Fatal("an explicit --distro was not honoured")
	}
	if got.Distro != "hand-rolled" {
		t.Errorf("got %+v", got)
	}
}

func TestResolveEngineTargetReportsNoInstall(t *testing.T) {
	opts := provision.Options{StateDir: t.TempDir()}
	if _, ok := resolveEngineTarget(&provision.Provisioner{}, opts); ok {
		t.Error("an empty state dir reported an install")
	}
}

// The manifest a distro install writes must not gain a field, or every
// existing install's file changes shape on the next write.
func TestDistroManifestStaysByteCompatible(t *testing.T) {
	dir := t.TempDir()
	opts := provision.Options{StateDir: dir}
	p := &provision.Provisioner{}
	if err := p.SaveManifest(opts, &provision.Manifest{Distro: "skrog-engine"}); err != nil {
		t.Fatal(err)
	}
	// Find it without depending on the private path helper.
	var found string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".json" {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatal("no manifest written")
	}
	b, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["backend"]; present {
		t.Errorf("a distro manifest now carries a backend field: %s", b)
	}
}

// legacyManifest is what Skrog 0.6.x wrote on the session backend: a backend
// field, and no distro at all. Written as raw JSON rather than through
// provision.Manifest so the test keeps describing the on-disk shape after the
// struct stops being able to express it.
func legacyManifest(t *testing.T) provision.Options {
	t.Helper()
	dir := t.TempDir()
	opts := provision.Options{StateDir: dir}
	p := &provision.Provisioner{}
	// SaveManifest to get the path right, then overwrite with the old shape.
	if err := p.SaveManifest(opts, &provision.Manifest{Distro: "placeholder"}); err != nil {
		t.Fatal(err)
	}
	var found string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".json" {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatal("no manifest written")
	}
	if err := os.WriteFile(found, []byte(`{"backend":"wslc","distro":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return opts
}

// An install created by 0.6.x on the removed backend must not resolve.
// Serving it is impossible: the code that spoke to that engine is gone, and
// resolving it would hand the distro path an empty name.
//
// It falls out of the distro check rather than needing one of its own, which
// is the point -- an explicit IsLegacySession() guard here passed this test
// with the guard deleted, because a legacy manifest records no distro either
// way. This asserts the behaviour; it does not pretend to cover a branch.
func TestResolveEngineTargetRefusesTheRemovedBackend(t *testing.T) {
	opts := legacyManifest(t)
	if got, ok := resolveEngineTarget(&provision.Provisioner{}, opts); ok {
		t.Errorf("a removed-backend install resolved as %+v", got)
	}
}

// ...and the user is told what it actually is. "no install found. Run `skrog
// install` first." is wrong twice over: there IS an install, and installing
// again would leave the old manifest in place. #451.
func TestNoInstallMessageExplainsTheRemovedBackend(t *testing.T) {
	msg := noInstallMessage(&provision.Provisioner{}, legacyManifest(t))
	if strings.Contains(msg, "no install found") {
		t.Errorf("claims nothing is installed:\n%s", msg)
	}
	for _, want := range []string{"removed", "skrog uninstall", "skrog install"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n%s", want, msg)
		}
	}
}

// And the ordinary case is untouched: an empty machine is still told to
// install, which is the message that was there before any backend existed.
func TestNoInstallMessageOnAnEmptyMachine(t *testing.T) {
	msg := noInstallMessage(&provision.Provisioner{}, provision.Options{StateDir: t.TempDir()})
	if !strings.Contains(msg, "no install found") {
		t.Errorf("an uninstalled machine should still be told to install: %q", msg)
	}
}

func TestResolveDistroForPassesThroughOnDistro(t *testing.T) {
	opts := writeManifest(t, provision.Manifest{Distro: "skrog-engine"})
	got, msg := resolveDistroFor(&provision.Provisioner{}, opts)
	if got != "skrog-engine" || msg != "" {
		t.Errorf("got (%q, %q), want (skrog-engine, \"\")", got, msg)
	}
}

func TestResolveDistroForStillReportsNoInstall(t *testing.T) {
	opts := provision.Options{StateDir: t.TempDir()}
	_, msg := resolveDistroFor(&provision.Provisioner{}, opts)
	if !strings.Contains(msg, "no install found") {
		t.Errorf("an uninstalled machine should still be told to install: %q", msg)
	}
}
