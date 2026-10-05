package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsWhenNoFile(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with no file: %v", err)
	}
	// 5m since #520, matching Docker Desktop's Resource Saver. It was off.
	if c.IdleTimeout != DefaultIdleTimeout || DefaultIdleTimeout != 5*time.Minute {
		t.Errorf("default IdleTimeout = %v, want 5m", c.IdleTimeout)
	}
	if v, err := Get(dir, KeyIdleTimeout); err != nil || v != "5m0s" {
		t.Errorf("Get default = %q, %v; want 5m0s", v, err)
	}
	// "off" still means off: the default changed, the opt-out did not.
	if err := Set(dir, KeyIdleTimeout, "off"); err != nil {
		t.Fatal(err)
	}
	if c, _ := Load(dir); c.IdleTimeout != 0 {
		t.Errorf("idle-timeout off loads as %v, want 0", c.IdleTimeout)
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := Set(dir, KeyIdleTimeout, "20m"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, _ := Get(dir, KeyIdleTimeout); v != "20m0s" {
		t.Errorf("Get = %q, want normalized 20m0s", v)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleTimeout != 20*time.Minute {
		t.Errorf("IdleTimeout = %v, want 20m", c.IdleTimeout)
	}
}

func TestSetOff(t *testing.T) {
	dir := t.TempDir()
	Set(dir, KeyIdleTimeout, "1h")
	for _, off := range []string{"off", "0", "OFF"} {
		if err := Set(dir, KeyIdleTimeout, off); err != nil {
			t.Fatalf("Set(%q): %v", off, err)
		}
		c, _ := Load(dir)
		if c.IdleTimeout != 0 {
			t.Errorf("after Set(%q), IdleTimeout = %v, want 0", off, c.IdleTimeout)
		}
	}
}

func TestSetRejectsBadValues(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"soon", "-5m", "2s", "5"} {
		if err := Set(dir, KeyIdleTimeout, bad); err == nil {
			t.Errorf("Set(%q) accepted", bad)
		}
	}
	// And nothing was persisted by the failures.
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Error("a rejected Set still wrote the config file")
	}
}

func TestUnknownKeyRefused(t *testing.T) {
	dir := t.TempDir()
	if err := Set(dir, "idle-timout", "20m"); err == nil {
		t.Error("typo'd key accepted; it would configure nothing")
	}
	if _, err := Get(dir, "nope"); err == nil {
		t.Error("unknown key Get succeeded")
	}
}

func TestCorruptFileIsAnErrorNotDefaults(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o644)
	if _, err := Load(dir); err == nil {
		t.Error("corrupt config silently became defaults")
	}
}

func TestAllListsDefaultsAndSet(t *testing.T) {
	dir := t.TempDir()
	Set(dir, KeyIdleTimeout, "30m")
	all, err := All(dir)
	if err != nil {
		t.Fatal(err)
	}
	if all[KeyIdleTimeout] != "30m0s" {
		t.Errorf("All()[%s] = %q", KeyIdleTimeout, all[KeyIdleTimeout])
	}
}

func TestSetDataDirPointsAtTheRelocateCommand(t *testing.T) {
	// PLAN.md and #64 both spell it `config set data-dir`, so it gets typed.
	// The answer must name the command that actually does it rather than
	// dead-ending on "unknown config key".
	err := Set(t.TempDir(), "data-dir", `D:\skrog`)
	if err == nil {
		t.Fatal("data-dir must not be storable as a setting")
	}
	if !strings.Contains(err.Error(), "skrog relocate") {
		t.Errorf("the error should point at `skrog relocate`, got: %v", err)
	}
}
