package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// touchForward makes a file's mtime unambiguously newer. Some filesystems
// have coarse timestamps, and a test that writes twice in the same
// millisecond would otherwise look unchanged to the stat gate — which is a
// property of the test machine, not of the code.
func touchForward(t *testing.T, p string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestWatcherSeesASettingChangeWithNoRestart(t *testing.T) {
	// The bug this type exists for: `skrog config set audit on` had to be
	// picked up by a process that was already running.
	dir := t.TempDir()
	w := NewWatcher(dir)

	if w.Config().Audit {
		t.Fatal("audit is on with no config file")
	}
	if err := Set(dir, KeyAudit, "on"); err != nil {
		t.Fatal(err)
	}
	touchForward(t, path(dir))

	if !w.Config().Audit {
		t.Error("audit still reads off after the setting was turned on")
	}
}

func TestWatcherSeesASettingTurnedBackOff(t *testing.T) {
	dir := t.TempDir()
	if err := Set(dir, KeyAudit, "on"); err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(dir)
	if !w.Config().Audit {
		t.Fatal("audit reads off immediately after being set on")
	}

	if err := Set(dir, KeyAudit, "off"); err != nil {
		t.Fatal(err)
	}
	touchForward(t, path(dir))

	if w.Config().Audit {
		t.Error("audit still reads on after being turned off")
	}
}

func TestWatcherKeepsTheLastGoodConfigOnAParseError(t *testing.T) {
	// A half-typed edit must not revert every setting to its default. For the
	// audit log in particular, silently reverting to off is the failure
	// direction worth guarding.
	dir := t.TempDir()
	if err := Set(dir, KeyIdleTimeout, "20m"); err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(dir)
	var reported []string
	w.OnError = func(err error) { reported = append(reported, err.Error()) }
	if got := w.Config().IdleTimeout; got != 20*time.Minute {
		t.Fatalf("IdleTimeout = %v, want 20m", got)
	}

	if err := os.WriteFile(path(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	touchForward(t, path(dir))

	if got := w.Config().IdleTimeout; got != 20*time.Minute {
		t.Errorf("IdleTimeout = %v after a broken edit, want the last good 20m", got)
	}
	if len(reported) != 1 {
		t.Fatalf("OnError called %d times, want exactly 1", len(reported))
	}
	// Re-reading must not re-report: the supervisor log would fill with one
	// copy of the same error per docker call.
	for i := 0; i < 5; i++ {
		w.Config()
	}
	if len(reported) != 1 {
		t.Errorf("OnError called %d times over repeated reads, want 1", len(reported))
	}
}

func TestWatcherRecoversWhenTheFileParsesAgain(t *testing.T) {
	dir := t.TempDir()
	p := path(dir)
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(dir)
	w.Config() // absorbs the error

	// Written directly, not through Set: Set refuses to touch a file it
	// cannot parse, which is correct of it and beside the point here.
	if err := os.WriteFile(p, []byte(`{"audit":"on"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	touchForward(t, p)

	if !w.Config().Audit {
		t.Error("a repaired settings file was not picked up")
	}
}

func TestWatcherTreatsADeletedFileAsDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := Set(dir, KeyAudit, "on"); err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(dir)
	if !w.Config().Audit {
		t.Fatal("audit reads off immediately after being set on")
	}

	if err := os.Remove(path(dir)); err != nil {
		t.Fatal(err)
	}
	if w.Config().Audit {
		t.Error("audit still on after the settings file was deleted")
	}
}

func TestWatcherDoesNotRereadAnUnchangedFile(t *testing.T) {
	// The gate is what makes this cheap enough to call once per docker API
	// call, so it is worth pinning that it actually gates.
	dir := t.TempDir()
	if err := Set(dir, KeyIdleTimeout, "5m"); err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(dir)
	w.Config()

	// Rewrite the contents underneath, keeping mtime and size identical: a
	// reader that ignored the stamp would see the new value.
	fi, err := os.Stat(path(dir))
	if err != nil {
		t.Fatal(err)
	}
	same := []byte(`{"idle-timeout":"9m0s"}`)
	if len(same) != int(fi.Size()) {
		t.Skipf("cannot forge a same-size file (%d vs %d)", len(same), fi.Size())
	}
	if err := os.WriteFile(path(dir), same, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path(dir), fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	if got := w.Config().IdleTimeout; got != 5*time.Minute {
		t.Errorf("IdleTimeout = %v; an unchanged stat should not have re-read", got)
	}
}

func TestWatcherWithNoStateDirIsDefaults(t *testing.T) {
	w := NewWatcher(filepath.Join(t.TempDir(), "does-not-exist"))
	// The defaults, not a zero Config: a watcher over a missing file must
	// agree with Load about what unset means -- idle-timeout included.
	if c := w.Config(); c != Defaults() {
		t.Errorf("Config() = %+v, want Defaults() %+v", c, Defaults())
	}
}
