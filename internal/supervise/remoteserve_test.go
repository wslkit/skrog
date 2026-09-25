package supervise_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/supervise"
)

// Remote clients of `skrog serve` never cross the supervisor's pipe, so the
// pipe says the engine is quiet while a remote build is running (#520). The
// serve record is what vetoes the stop.
func TestIdleStopWaitsForRemoteClients(t *testing.T) {
	s, e, _, dir := idleSup(t)
	if err := supervise.WriteRemoteServe(dir, supervise.RemoteServe{At: time.Now(), ActiveConns: 1}); err != nil {
		t.Fatal(err)
	}
	runTicks(s, 3, 60*time.Millisecond)
	if _, stops := e.counts(); stops != 0 {
		t.Fatalf("idle-stopped %d times with a remote client connected, want 0", stops)
	}
}

// When the last remote connection closes, the quiet window starts then --
// not at the last LOCAL activity, which may be an hour ago.
func TestRemoteActivityRestartsTheQuietWindow(t *testing.T) {
	s, e, _, dir := idleSup(t)
	s.IdleTimeout = func() time.Duration { return 300 * time.Millisecond }
	s.TickForTest(context.Background()) // up; the engine's own upSince starts its clock
	time.Sleep(350 * time.Millisecond)

	// A remote connection closed just now.
	if err := supervise.WriteRemoteServe(dir, supervise.RemoteServe{At: time.Now(), LastActivity: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s.TickForTest(context.Background())
	if _, stops := e.counts(); stops != 0 {
		t.Fatal("idle-stopped right after a remote connection closed")
	}

	time.Sleep(350 * time.Millisecond)
	if err := supervise.WriteRemoteServe(dir, supervise.RemoteServe{At: time.Now(),
		LastActivity: time.Now().Add(-350 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	s.TickForTest(context.Background())
	if _, stops := e.counts(); stops != 1 {
		t.Errorf("stopped %d times one full timeout after the last remote activity, want 1", stops)
	}
}

// A `skrog serve` killed hard cannot withdraw its record; the heartbeat
// stopping is what retires it, so a dead serve does not hold the engine awake
// forever.
func TestStaleRemoteServeRecordIsIgnored(t *testing.T) {
	s, e, _, dir := idleSup(t)
	if err := supervise.WriteRemoteServe(dir, supervise.RemoteServe{
		At: time.Now().Add(-time.Minute), ActiveConns: 3,
	}); err != nil {
		t.Fatal(err)
	}
	runTicks(s, 3, 60*time.Millisecond)
	if _, stops := e.counts(); stops != 1 {
		t.Errorf("stopped %d times with only a stale serve record, want 1", stops)
	}
}

func TestRemoteServeRecordRoundTripAndClear(t *testing.T) {
	dir := t.TempDir()
	if _, ok := supervise.ReadRemoteServe(dir); ok {
		t.Fatal("a record with nothing written")
	}
	now := time.Now()
	if err := supervise.WriteRemoteServe(dir, supervise.RemoteServe{At: now, ActiveConns: 2}); err != nil {
		t.Fatal(err)
	}
	r, ok := supervise.ReadRemoteServe(dir)
	if !ok || r.ActiveConns != 2 {
		t.Errorf("read back %+v, %v", r, ok)
	}
	if err := supervise.ClearRemoteServe(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "remote-serve.json")); !os.IsNotExist(err) {
		t.Errorf("record still present after clear: %v", err)
	}
	// A torn or foreign file is "no record", never a panic or a veto.
	os.WriteFile(filepath.Join(dir, "remote-serve.json"), []byte("{not json"), 0o644)
	if _, ok := supervise.ReadRemoteServe(dir); ok {
		t.Error("an unparsable record counted")
	}
}
