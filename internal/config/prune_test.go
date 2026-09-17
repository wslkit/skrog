package config

import (
	"testing"
	"time"
)

// The age guard must not be turnable off. An automatic prune with no window
// would take the image someone pulled an hour ago for tomorrow's demo, and
// the only trace would be a slow pull later.
func TestKeepSinceGuardCannotBeDisabled(t *testing.T) {
	for _, v := range []string{"off", "0"} {
		if _, err := validators[KeyPruneKeepSince](v); err == nil {
			t.Errorf("config set %s %q was accepted; the guard must not be removable",
				KeyPruneKeepSince, v)
		}
	}
	// Clearing the key is allowed: it restores the default, it does not
	// disable the guard.
	if got, err := validators[KeyPruneKeepSince](""); err != nil || got != "" {
		t.Errorf("clearing the key = (%q, %v); want it accepted", got, err)
	}
	// Normalized through Duration.String(), the same shape idle-timeout stores.
	if got, err := validators[KeyPruneKeepSince]("72h"); err != nil || got != "72h0m0s" {
		t.Errorf("72h = (%q, %v); want (\"72h0m0s\", nil)", got, err)
	}
}

// KeepSince defaults rather than returning zero, so a caller cannot launch an
// unguarded prune by forgetting to check.
func TestKeepSinceAlwaysResolves(t *testing.T) {
	if got := (Config{}).KeepSince(); got != DefaultPruneKeepSince {
		t.Errorf("unset KeepSince = %v, want %v", got, DefaultPruneKeepSince)
	}
	if got := (Config{PruneKeepSince: 24 * time.Hour}).KeepSince(); got != 24*time.Hour {
		t.Errorf("KeepSince = %v, want 24h", got)
	}
}

// prune.every is off unless set, because automatic deletion is opt-in.
func TestPruneIsOffByDefault(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PruneEvery != 0 {
		t.Errorf("PruneEvery = %v on a fresh install; automatic pruning must be opt-in", c.PruneEvery)
	}
	if c.PruneBuildCache {
		t.Error("PruneBuildCache is on by default; the cache is expensive to rebuild")
	}
}

// There must be no way to ask a timer to delete volumes.
func TestNoVolumeSettingExists(t *testing.T) {
	for k := range validators {
		if k == "prune.volumes" {
			t.Fatal("a prune.volumes setting exists; a timer must never be able to delete a database")
		}
	}
}
