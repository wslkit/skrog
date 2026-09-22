package supervise

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHoldPausesUntilItExpires(t *testing.T) {
	dir := t.TempDir()
	if HoldActive(dir) {
		t.Fatal("held with no hold written")
	}

	release, err := Hold(dir, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if !HoldActive(dir) {
		t.Error("not held immediately after Hold")
	}

	release()
	if HoldActive(dir) {
		t.Error("still held after release")
	}
}

// The holder can die. A hold that outlived its process would leave a
// supervisor that has silently stopped reconciling -- which presents as an
// engine that never recovers, with nothing in any log to say why.
func TestExpiredHoldIsNotHonored(t *testing.T) {
	dir := t.TempDir()
	if _, err := Hold(dir, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if HoldActive(dir) {
		t.Error("an expired hold is still pausing the supervisor")
	}
}

// Unreadable state must fail OPEN. A hold pauses the one thing keeping the
// engine alive, so when its state cannot be established the safe direction is
// to carry on supervising: ignoring a real hold costs a race that already
// existed, honoring a corrupt one costs a supervisor that never works again.
func TestUnparsableHoldIsNotHonored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "maintenance-hold"), []byte("not a time"), 0o644); err != nil {
		t.Fatal(err)
	}
	if HoldActive(dir) {
		t.Error("a corrupt hold file is pausing the supervisor")
	}
}

func TestClearHoldToleratesAbsence(t *testing.T) {
	if err := ClearHold(t.TempDir()); err != nil {
		t.Errorf("ClearHold with nothing to clear: %v", err)
	}
}

// Trailing newline and whitespace are what commit() writes, so parsing has to
// survive them -- a hold that never reads as active is a fix that does nothing.
func TestHoldSurvivesTheNewlineItIsWrittenWith(t *testing.T) {
	dir := t.TempDir()
	if _, err := Hold(dir, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "maintenance-hold"))
	if err != nil {
		t.Fatal(err)
	}
	if b[len(b)-1] != '\n' {
		t.Fatalf("expected the committed file to end in a newline, got %q", b)
	}
	if !HoldActive(dir) {
		t.Error("the newline it is written with defeats the parse")
	}
}
