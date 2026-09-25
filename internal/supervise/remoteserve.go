package supervise

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// RemoteServe is what a running `skrog serve` publishes about its own
// traffic, so the supervisor's idle stop can see it.
//
// `skrog serve` is a separate process with its own listener: remote clients
// never cross the supervisor's pipe, so the pipe's connection count -- the
// only activity idle stop used to read -- says nothing about them. With the
// idle timeout on (5m by default since #520), that let the supervisor
// stop the engine under a remote client, even mid-build: a BuildKit build
// runs no containers, so "no containers running" did not save it either.
type RemoteServe struct {
	// At is when this was written. A heartbeat, not a lock: a `skrog serve`
	// killed hard cannot clean up, so staleness is what retires the record.
	At time.Time `json:"at"`
	// ActiveConns is how many remote connections are open right now.
	ActiveConns int `json:"activeConns"`
	// LastActivity is when a remote connection last opened or closed.
	LastActivity time.Time `json:"lastActivity,omitempty"`
}

// RemoteServeInterval is how often `skrog serve` rewrites the record.
const RemoteServeInterval = 5 * time.Second

// remoteServeStale is when a record stops counting: several missed heartbeats,
// so one slow write never lets an idle stop through, and a dead `skrog serve`
// stops holding the engine awake within half a minute.
const remoteServeStale = 30 * time.Second

func remoteServePath(stateDir string) string {
	return filepath.Join(stateDir, "remote-serve.json")
}

// WriteRemoteServe records serve's traffic, atomically.
func WriteRemoteServe(stateDir string, r RemoteServe) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return commit(remoteServePath(stateDir), append(b, '\n'))
}

// ReadRemoteServe returns a fresh record, or false when there is none, it will
// not parse, or its heartbeat has stopped.
func ReadRemoteServe(stateDir string) (RemoteServe, bool) {
	b, err := os.ReadFile(remoteServePath(stateDir))
	if err != nil {
		return RemoteServe{}, false
	}
	var r RemoteServe
	if err := json.Unmarshal(b, &r); err != nil {
		return RemoteServe{}, false
	}
	if time.Since(r.At) > remoteServeStale {
		return RemoteServe{}, false
	}
	return r, true
}

// ClearRemoteServe removes the record, whether or not one is present.
func ClearRemoteServe(stateDir string) error { return remove(remoteServePath(stateDir)) }
