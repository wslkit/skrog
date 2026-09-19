package policy

import (
	"path/filepath"
	"strings"
	"testing"
)

func volBody(driver, device string) map[string]any {
	b := map[string]any{"Name": "v"}
	if driver != "" {
		b["Driver"] = driver
	}
	if device != "" {
		b["DriverOpts"] = map[string]any{"type": "none", "o": "bind", "device": device}
	}
	return b
}

// The exploit the rule exists to stop (#419): a local volume naming a host
// path outside the allowlist.
func TestVolumeCreateRefusesADeviceOutsideTheAllowlist(t *testing.T) {
	r := Rules{AllowBindSources: []string{`C:\work`}}
	reason, denied := r.DenyVolumeCreate(volBody("local", "/mnt/c/secrets"))
	if !denied {
		t.Fatal(`a local volume reached C:\secrets with allow-bind-sources: [C:\work]`)
	}
	for _, want := range []string{"/mnt/c/secrets", `C:\work`} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason does not mention %q: %q", want, reason)
		}
	}
}

// device=/ is the same trick against the whole guest filesystem, and it is not
// under any Windows root, so it must be refused rather than silently allowed
// for being untranslatable.
func TestVolumeCreateRefusesAGuestPathThatIsNotADrive(t *testing.T) {
	r := Rules{AllowBindSources: []string{`C:\work`}}
	for _, device := range []string{"/", "/etc", "/var/lib/docker", "/mnt/wsl"} {
		if _, denied := r.DenyVolumeCreate(volBody("local", device)); !denied {
			t.Errorf("device=%q was allowed; it is under no Windows root", device)
		}
	}
}

// A device inside an allowed root is fine — the rule restricts, it does not
// forbid the feature.
func TestVolumeCreateAllowsADeviceInsideTheAllowlist(t *testing.T) {
	r := Rules{AllowBindSources: []string{`C:\work`}}
	for _, device := range []string{"/mnt/c/work", "/mnt/c/work/proj", `C:\work\proj`} {
		if reason, denied := r.DenyVolumeCreate(volBody("local", device)); denied {
			t.Errorf(`device=%q inside C:\work was refused: %s`, device, reason)
		}
	}
}

// The ordinary named volume — no device at all — must stay allowed, or the
// rule would break every compose stack on a machine with an allowlist.
func TestVolumeCreateAllowsAnOrdinaryNamedVolume(t *testing.T) {
	r := Rules{AllowBindSources: []string{`C:\work`}}
	if _, denied := r.DenyVolumeCreate(volBody("local", "")); denied {
		t.Error("a plain named volume was refused")
	}
	if _, denied := r.DenyVolumeCreate(map[string]any{"Name": "v"}); denied {
		t.Error("a volume with no DriverOpts at all was refused")
	}
}

// No allowlist, no rule. Refusing volumes on a machine that has not restricted
// bind sources would close a hole that is not open.
func TestVolumeCreateIsInertWithoutAnAllowlist(t *testing.T) {
	if _, denied := (Rules{}).DenyVolumeCreate(volBody("local", "/mnt/c/secrets")); denied {
		t.Error("a volume was refused with no allow-bind-sources set")
	}
}

// A third-party driver's options are its own vocabulary, so "checked" would be
// a lie. Refuse and say why.
func TestVolumeCreateRefusesAnUncheckableDriver(t *testing.T) {
	r := Rules{AllowBindSources: []string{`C:\work`}}
	reason, denied := r.DenyVolumeCreate(volBody("some-vendor-driver", "/mnt/c/work"))
	if !denied {
		t.Fatal("an unknown driver's options were treated as checked")
	}
	if !strings.Contains(reason, "some-vendor-driver") {
		t.Errorf("reason does not name the driver: %q", reason)
	}
}

// Driver omitted means local, which is dockerd's own default — so the rule has
// to apply to it, or `-o device=` without `-d local` walks straight through.
func TestVolumeCreateTreatsAnOmittedDriverAsLocal(t *testing.T) {
	r := Rules{AllowBindSources: []string{`C:\work`}}
	if _, denied := r.DenyVolumeCreate(volBody("", "/mnt/c/secrets")); !denied {
		t.Error("a volume with no Driver field bypassed the rule")
	}
}

// The Watcher is what the bridge installs as its gate, and Rules is not.
// deny-unattributable-builds shipped as a no-op in this same release because
// every test called Rules and nothing called the method the product reaches.
func TestWatcherDenyVolumeCreateConsultsTheRules(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(MachineDirEnv, t.TempDir())
	write(t, filepath.Join(dir, FileName), "allow-bind-sources:\n  - C:\\work\n")

	w := NewWatcher(dir)
	if _, denied := w.DenyVolumeCreate(volBody("local", "/mnt/c/secrets")); !denied {
		t.Fatal("Watcher.DenyVolumeCreate allowed an out-of-tree device; the gate is a no-op")
	}
	if _, denied := w.DenyVolumeCreate(volBody("local", "/mnt/c/work/x")); denied {
		t.Error("Watcher.DenyVolumeCreate refused a device inside the allowed root")
	}
}

// An unreadable rule file refuses, like every other gate method (#254).
func TestWatcherDenyVolumeCreateFailsClosedOnABrokenFile(t *testing.T) {
	machineDir := t.TempDir()
	t.Setenv(MachineDirEnv, machineDir)
	write(t, filepath.Join(machineDir, FileName), "deny-priviledged: true\n")

	if _, denied := NewWatcher(t.TempDir()).DenyVolumeCreate(volBody("local", "/mnt/c/work")); !denied {
		t.Error("a broken machine policy allowed a volume unjudged")
	}
}
