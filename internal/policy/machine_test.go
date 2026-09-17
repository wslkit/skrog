package policy

import (
	"os"
	"path/filepath"
	"testing"
)

// The guarantee the whole layer rests on: whatever the user writes, anything
// the machine layer denies stays denied.
//
// Asserted by brute force over a matrix of requests rather than by reading the
// merge code, because "it only tightens" is exactly the kind of claim that
// survives a refactor in the comments while quietly dying in the code.
func TestMergeNeverRelaxes(t *testing.T) {
	machine := Rules{
		DenyPrivileged:        true,
		DenyAddedCapabilities: true,
		DenyCapabilities:      []string{"SYS_ADMIN"},
		DenyHostNamespaces:    true,
		RequireDigest:         true,
		AllowBindSources:      []string{`C:\work`},
		AllowRegistries:       []string{"registry.example.com"},
	}

	// Every one of these is a user trying to undo the machine layer.
	attempts := []struct {
		name string
		user Rules
	}{
		{"empty user layer", Rules{}},
		{"user allows everything it can", Rules{
			AllowBindSources: []string{`C:\`, `D:\`},
			AllowRegistries:  []string{"*", "docker.io", "evil.example.com"},
		}},
		{"user widens bind sources", Rules{AllowBindSources: []string{`C:\Users`}}},
		{"user adds a registry", Rules{AllowRegistries: []string{"docker.io"}}},
		{"user tightens further", Rules{
			AllowBindSources: []string{`C:\work\proj`},
			AllowRegistries:  []string{"registry.example.com"},
		}},
	}

	// Requests the machine layer must refuse no matter what.
	denied := []struct {
		name string
		body map[string]any
	}{
		{"privileged", map[string]any{"HostConfig": map[string]any{"Privileged": true}}},
		{"cap-add", map[string]any{"HostConfig": map[string]any{"CapAdd": []any{"NET_ADMIN"}}}},
		{"host network", map[string]any{"HostConfig": map[string]any{"NetworkMode": "host"}}},
		{"bind outside the allowed root", map[string]any{
			"HostConfig": map[string]any{"Binds": []any{`D:\secrets:/s`}},
		}},
	}

	for _, a := range attempts {
		merged := Merge(machine, a.user)
		for _, d := range denied {
			t.Run(a.name+"/"+d.name, func(t *testing.T) {
				if _, ok := merged.DenyCreate(d.body); !ok {
					t.Errorf("the user layer relaxed a machine rule: %q was allowed", d.name)
				}
			})
		}
		// An image from a registry the machine forbids must stay forbidden.
		t.Run(a.name+"/forbidden registry", func(t *testing.T) {
			if _, ok := merged.DenyPull("evil.example.com/thing@sha256:" + fakeDigest); !ok {
				t.Error("the user layer relaxed allow-registries")
			}
		})
	}
}

const fakeDigest = "0000000000000000000000000000000000000000000000000000000000000000"

// A user may forbid MORE than the machine does.
func TestMergeLetsTheUserTighten(t *testing.T) {
	merged := Merge(Rules{}, Rules{DenyPrivileged: true})
	body := map[string]any{"HostConfig": map[string]any{"Privileged": true}}
	if _, ok := merged.DenyCreate(body); !ok {
		t.Error("a user-only rule was dropped; the user layer must still apply on its own")
	}
}

func TestMergeAllowListSemantics(t *testing.T) {
	for _, tc := range []struct {
		name          string
		machine, user []string
		want          []string
	}{
		{"both empty", nil, nil, nil},
		{"machine only", []string{`C:\work`}, nil, []string{`C:\work`}},
		{"user only", nil, []string{`C:\mine`}, []string{`C:\mine`}},
		// The subtle one: a user path UNDER a machine root survives, because
		// the machine already permitted it. A set intersection would drop it.
		{"user narrows within the machine root",
			[]string{`C:\work`}, []string{`C:\work\proj`}, []string{`C:\work\proj`}},
		// Nothing the user asked for is permitted, so fall back to the
		// machine's list: the user's would grant what the machine forbade, and
		// an empty list would mean "no restriction" and grant everything.
		{"user asks for nothing permitted",
			[]string{`C:\work`}, []string{`D:\other`}, []string{`C:\work`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Merge(Rules{AllowBindSources: tc.machine},
				Rules{AllowBindSources: tc.user}).AllowBindSources
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// An empty allow-list must never be the result of tightening, because empty
// means "no restriction" — the one value that grants more than it looks like.
func TestMergeNeverProducesAnEmptyAllowListFromANonEmptyOne(t *testing.T) {
	got := Merge(Rules{AllowRegistries: []string{"registry.example.com"}},
		Rules{AllowRegistries: []string{"docker.io", "quay.io"}}).AllowRegistries
	if len(got) == 0 {
		t.Fatal("tightening produced an empty allow-list, which means NO restriction")
	}
}

// Capability deny-lists union, and the two spellings of one capability must
// not both survive.
func TestMergeUnionsCapabilities(t *testing.T) {
	got := Merge(Rules{DenyCapabilities: []string{"SYS_ADMIN"}},
		Rules{DenyCapabilities: []string{"cap_sys_admin", "NET_ADMIN"}}).DenyCapabilities
	if len(got) != 2 {
		t.Errorf("DenyCapabilities = %v; want SYS_ADMIN and NET_ADMIN with no duplicate spelling", got)
	}
}

func TestLoadLayered(t *testing.T) {
	machineDir := t.TempDir()
	stateDir := t.TempDir()
	t.Setenv(MachineDirEnv, machineDir)

	write(t, filepath.Join(machineDir, FileName), "deny-privileged: true\n")
	write(t, filepath.Join(stateDir, FileName), "require-digest: true\n")

	rules, src, err := LoadLayered(stateDir)
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	if !rules.DenyPrivileged || !rules.RequireDigest {
		t.Errorf("effective rules = %+v; want both layers applied", rules)
	}
	if src.MachinePath == "" || src.UserPath == "" {
		t.Errorf("Source = %+v; both paths should be reported", src)
	}
	if !src.Machine.DenyPrivileged || !src.User.RequireDigest {
		t.Error("the layers were not reported separately")
	}
}

// A machine file that will not parse is an error, not an empty layer: ignoring
// it would turn a typo in a deployment into an unenforced machine.
func TestLoadLayeredRefusesABrokenMachineFile(t *testing.T) {
	machineDir := t.TempDir()
	t.Setenv(MachineDirEnv, machineDir)
	write(t, filepath.Join(machineDir, FileName), "deny-privileged: yes: no\n")

	if _, _, err := LoadLayered(t.TempDir()); err == nil {
		t.Fatal("a broken machine policy parsed as empty; it must be an error")
	}
}

func TestLoadLayeredWithNeitherFile(t *testing.T) {
	t.Setenv(MachineDirEnv, t.TempDir())
	rules, src, err := LoadLayered(t.TempDir())
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	if !rules.Empty() {
		t.Errorf("rules = %+v; want empty", rules)
	}
	if src.MachinePath != "" || src.UserPath != "" {
		t.Errorf("Source = %+v; want no paths", src)
	}
}

// The watcher is what the bridge actually consults, so the machine layer has
// to reach it — and take effect without a restart, like the user layer.
func TestWatcherAppliesTheMachineLayer(t *testing.T) {
	machineDir := t.TempDir()
	stateDir := t.TempDir()
	t.Setenv(MachineDirEnv, machineDir)

	w := NewWatcher(stateDir)
	body := map[string]any{"HostConfig": map[string]any{"Privileged": true}}
	if _, ok := w.DenyCreate(body); ok {
		t.Fatal("denied with no policy at all")
	}

	write(t, filepath.Join(machineDir, FileName), "deny-privileged: true\n")
	if _, ok := w.DenyCreate(body); !ok {
		t.Error("a machine policy written after the watcher started was not picked up")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
