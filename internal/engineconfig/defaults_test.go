package engineconfig

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestApplyDefaultsAddsOnlyAbsentKeys(t *testing.T) {
	f := newFakeDistro()
	f.files[DefaultDaemonPath] = `{"log-driver":"json-file"}`
	m := &Manager{WSL: f, Distro: "d"}

	added, err := m.ApplyDefaults(context.Background())
	if err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if len(added) != 1 || added[0] != "userland-proxy" {
		t.Fatalf("added = %v, want [userland-proxy]", added)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(f.files[DefaultDaemonPath]), &cfg); err != nil {
		t.Fatalf("daemon.json not JSON after defaults: %v", err)
	}
	if cfg["userland-proxy"] != false {
		t.Errorf("userland-proxy = %v, want false", cfg["userland-proxy"])
	}
	if cfg["log-driver"] != "json-file" {
		t.Errorf("existing key lost: log-driver = %v", cfg["log-driver"])
	}
	// The result was validated before the live file was replaced.
	if strings.Join(f.ops, ",") != "read,write-candidate,validate,backup,write-live" {
		t.Errorf("ops = %v", f.ops)
	}
}

func TestApplyDefaultsRespectsUserChoice(t *testing.T) {
	f := newFakeDistro()
	// The user turned the proxy on deliberately; a default must not undo that.
	f.files[DefaultDaemonPath] = `{"userland-proxy": true}`
	m := &Manager{WSL: f, Distro: "d"}

	added, err := m.ApplyDefaults(context.Background())
	if err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("added = %v, want nothing", added)
	}
	if len(f.ops) != 1 || f.ops[0] != "read" {
		t.Errorf("a no-op default still touched the distro: ops = %v", f.ops)
	}
	if f.files[DefaultDaemonPath] != `{"userland-proxy": true}` {
		t.Errorf("daemon.json rewritten: %s", f.files[DefaultDaemonPath])
	}
}

func TestApplyDefaultsOnEmptyFile(t *testing.T) {
	f := newFakeDistro()
	m := &Manager{WSL: f, Distro: "d"}
	if _, err := m.ApplyDefaults(context.Background()); err != nil {
		t.Fatalf("ApplyDefaults on missing daemon.json: %v", err)
	}
	if !strings.Contains(f.files[DefaultDaemonPath], `"userland-proxy": false`) {
		t.Errorf("daemon.json = %s", f.files[DefaultDaemonPath])
	}
}

func TestApplyDefaultsRefusedByValidate(t *testing.T) {
	f := newFakeDistro()
	f.validateOK = false
	f.files[DefaultDaemonPath] = `{}`
	m := &Manager{WSL: f, Distro: "d"}
	if _, err := m.ApplyDefaults(context.Background()); err == nil {
		t.Fatal("expected the dockerd --validate refusal to surface")
	}
	if f.files[DefaultDaemonPath] != `{}` {
		t.Errorf("live daemon.json replaced despite failed validation: %s", f.files[DefaultDaemonPath])
	}
}

func TestBoolKeyRoundTrip(t *testing.T) {
	f := newFakeDistro()
	m := &Manager{WSL: f, Distro: "d"}
	if _, err := m.Set(context.Background(), "userland-proxy", "true"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := m.Get(context.Background(), "userland-proxy")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "true" {
		t.Errorf("Get = %q, want true", got)
	}
	if _, err := m.Set(context.Background(), "userland-proxy", "maybe"); err == nil {
		t.Error("Set of a non-boolean succeeded")
	}
}

// Engine 29.10.0 stopped treating loopback as insecure once insecure-registries
// is set (moby/moby#53868), which would have cut `skrog cache` off for anyone
// with a corporate registry configured. Setting the key keeps loopback in it.
func TestSetInsecureRegistriesKeepsLoopback(t *testing.T) {
	f := newFakeDistro()
	m := &Manager{WSL: f, Distro: "d"}

	res, err := m.Set(context.Background(), "insecure-registries", "registry.internal:5000")
	if err != nil {
		t.Fatal(err)
	}
	if want := "registry.internal:5000,127.0.0.0/8,::1/128"; res.Applied != want {
		t.Fatalf("Applied = %q, want %q", res.Applied, want)
	}

	// Already listed: not duplicated.
	res, err = m.Set(context.Background(), "insecure-registries", "::1/128,127.0.0.0/8,r:5000")
	if err != nil {
		t.Fatal(err)
	}
	if want := "::1/128,127.0.0.0/8,r:5000"; res.Applied != want {
		t.Fatalf("Applied = %q, want %q", res.Applied, want)
	}

	// Cleared: the key goes away, and dockerd's own loopback default applies.
	if _, err := m.Set(context.Background(), "insecure-registries", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.files[DefaultDaemonPath], "insecure-registries") {
		t.Errorf("cleared key written back: %s", f.files[DefaultDaemonPath])
	}
}

func TestSetManyInsecureRegistriesKeepsLoopback(t *testing.T) {
	f := newFakeDistro()
	m := &Manager{WSL: f, Distro: "d"}
	if _, err := m.SetMany(context.Background(), map[string]string{"insecure-registries": "r:5000"}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(context.Background(), "insecure-registries")
	if got != "r:5000,127.0.0.0/8,::1/128" {
		t.Fatalf("insecure-registries = %q", got)
	}
}

// An install upgraded to 29.10.0 with insecure-registries already in
// daemon.json gets loopback added before dockerd starts.
func TestApplyDefaultsKeepsLoopbackInExistingInsecureRegistries(t *testing.T) {
	f := newFakeDistro()
	f.files[DefaultDaemonPath] = `{"userland-proxy": false, "insecure-registries": ["r:5000"]}`
	m := &Manager{WSL: f, Distro: "d"}

	added, err := m.ApplyDefaults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "insecure-registries" {
		t.Fatalf("added = %v, want [insecure-registries]", added)
	}
	got, _ := m.Get(context.Background(), "insecure-registries")
	if got != "r:5000,127.0.0.0/8,::1/128" {
		t.Fatalf("insecure-registries = %q", got)
	}

	// Second start: nothing left to do, nothing written.
	f.ops = nil
	if added, err := m.ApplyDefaults(context.Background()); err != nil || len(added) != 0 {
		t.Fatalf("second ApplyDefaults = %v, %v", added, err)
	}
}

// Absent or empty: dockerd's default covers loopback, and writing the list
// would turn that default into a choice the user did not make.
func TestApplyDefaultsLeavesUnsetInsecureRegistriesAlone(t *testing.T) {
	for _, daemon := range []string{
		`{"userland-proxy": false}`,
		`{"userland-proxy": false, "insecure-registries": []}`,
	} {
		f := newFakeDistro()
		f.files[DefaultDaemonPath] = daemon
		m := &Manager{WSL: f, Distro: "d"}
		added, err := m.ApplyDefaults(context.Background())
		if err != nil || len(added) != 0 {
			t.Fatalf("%s: added = %v, %v", daemon, added, err)
		}
		if f.files[DefaultDaemonPath] != daemon {
			t.Errorf("%s rewritten: %s", daemon, f.files[DefaultDaemonPath])
		}
	}
}
