package skrogfile

import (
	"strings"
	"testing"
)

func TestParseFull(t *testing.T) {
	yaml := `
distro: skrog-engine
data-dir: D:\skrog
engine-version: 29.7.2
idle-timeout: 30m
autostart: true
engine:
  registry-mirrors: https://mirror.example.com
  log-opts: max-size=10m,max-file=3
hooks:
  post-start: C:\me\login.cmd
integrations:
  - Ubuntu
  - Debian
`
	f, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if f.Distro != "skrog-engine" || f.EngineVersion != "29.7.2" || f.IdleTimeout != "30m" {
		t.Fatalf("scalars wrong: %+v", f)
	}
	if f.Autostart == nil || !*f.Autostart {
		t.Fatal("autostart should be true")
	}
	if f.Engine["registry-mirrors"] != "https://mirror.example.com" {
		t.Fatalf("engine map wrong: %+v", f.Engine)
	}
	if f.Hooks["post-start"] != `C:\me\login.cmd` {
		t.Fatalf("hooks wrong: %+v", f.Hooks)
	}
	if len(f.Integrations) != 2 || f.Integrations[0] != "Ubuntu" {
		t.Fatalf("integrations wrong: %+v", f.Integrations)
	}
}

func TestParseRejectsUnknownTopLevelField(t *testing.T) {
	_, err := Parse([]byte("distru: typo\n"))
	if err == nil {
		t.Fatal("unknown top-level field should fail")
	}
}

func TestParseRejectsUnknownEngineKey(t *testing.T) {
	_, err := Parse([]byte("engine:\n  not-a-real-key: x\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown engine key") {
		t.Fatalf("want unknown-engine-key error, got %v", err)
	}
}

func TestParseRejectsUnknownHookEvent(t *testing.T) {
	_, err := Parse([]byte("hooks:\n  on-explode: x\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown hook event") {
		t.Fatalf("want unknown-hook-event error, got %v", err)
	}
}

func TestParseEmptyIsValid(t *testing.T) {
	f, err := Parse([]byte(""))
	if err != nil {
		t.Fatalf("empty file should be valid: %v", err)
	}
	if f.Distro != "" || f.Engine != nil {
		t.Fatalf("empty file should be zero: %+v", f)
	}
}

func TestMarshalRoundTrips(t *testing.T) {
	on := true
	orig := File{
		Distro:        "skrog-engine",
		EngineVersion: "29.7.2",
		IdleTimeout:   "20m",
		Autostart:     &on,
		Engine:        map[string]string{"dns": "1.1.1.1"},
		Hooks:         map[string]string{"on-wake": "warm.ps1"},
		Integrations:  []string{"Ubuntu"},
	}
	b, err := orig.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, b)
	}
	if got.Distro != orig.Distro || got.IdleTimeout != orig.IdleTimeout ||
		got.Autostart == nil || *got.Autostart != true ||
		got.Engine["dns"] != "1.1.1.1" || got.Hooks["on-wake"] != "warm.ps1" ||
		len(got.Integrations) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestMarshalOmitsEmpty(t *testing.T) {
	b, err := File{Distro: "d"}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "engine:") || strings.Contains(s, "hooks:") || strings.Contains(s, "autostart:") {
		t.Fatalf("empty fields should be omitted:\n%s", s)
	}
}

func TestTargetKey(t *testing.T) {
	f, err := Parse([]byte("target: proj-a\nengine-version: 29.7.2\nengine:\n  registry-mirrors: https://m.example\n"))
	if err != nil {
		t.Fatalf("a target beside the engine: settings map: %v", err)
	}
	if f.Target != "proj-a" || f.Engine["registry-mirrors"] == "" {
		t.Errorf("parsed %+v", f)
	}

	for name, doc := range map[string]string{
		"reserved":         "target: local\n",
		"bad characters":   "target: Proj A\n",
		"distro conflicts": "target: proj-a\ndistro: skrog-other\n",
		"data-dir clashes": "target: proj-a\ndata-dir: D:/x\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: %q was accepted", name, doc)
		}
	}

	// No target: nothing changes for a file that never had one.
	b, err := File{Distro: "d"}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "target") {
		t.Errorf("an unset target leaked into the YAML: %s", b)
	}
}
