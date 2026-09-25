package config

import (
	"os"
	"sync"
	"time"
)

// Watcher hands out the current settings, re-reading config.json only when it
// has actually changed on disk (#202).
//
// `skrog config` tells users "settings apply live: the supervisor re-reads
// them every few seconds". That promise was true for exactly one key. Every
// other consumer captured its value once when the supervisor started, so
// `skrog config set audit on` — followed by the `skrog restart` the help
// text recommended — enabled nothing: restart bounces the engine, not the
// supervisor that holds the setting. This type is how the promise is kept:
// wherever a setting is consumed, it is consumed through a Watcher.
//
// Reading is stat-gated because consumers are on the hot path — the audit
// sink is consulted once per proxied docker call — and a stat is orders of
// magnitude cheaper than opening and parsing the file.
//
// The zero value is not useful; use NewWatcher.
type Watcher struct {
	stateDir string

	mu      sync.Mutex
	cfg     Config
	modTime time.Time
	size    int64
	loaded  bool
	// lastErr is remembered so a file that breaks mid-edit is reported once
	// rather than on every request.
	lastErr string

	// OnError reports a settings file that stopped parsing. Optional.
	OnError func(err error)
}

// NewWatcher returns a Watcher over the install's settings file. It reads
// nothing yet: the first Config call loads it.
func NewWatcher(stateDir string) *Watcher { return &Watcher{stateDir: stateDir} }

// Config returns the current settings, re-reading if the file changed.
//
// On a parse error the last settings that worked stay in force. Reverting a
// user's whole configuration because they saved a half-typed edit would be a
// far bigger surprise than ignoring the edit until it parses — and for the
// audit log, silently reverting to "off" is the failure direction that made
// this issue worth filing.
func (w *Watcher) Config() Config {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refreshLocked()
	return w.cfg
}

func (w *Watcher) refreshLocked() {
	fi, err := os.Stat(path(w.stateDir))
	switch {
	case os.IsNotExist(err):
		// No file is the default configuration, exactly as Load treats it.
		w.cfg, w.loaded, w.modTime, w.size = Defaults(), true, time.Time{}, 0
		w.lastErr = ""
		return
	case err != nil:
		return // unreadable right now; keep what we have
	}
	if w.loaded && fi.ModTime().Equal(w.modTime) && fi.Size() == w.size {
		return
	}

	cfg, err := Load(w.stateDir)
	if err != nil {
		if w.OnError != nil && err.Error() != w.lastErr {
			w.OnError(err)
		}
		w.lastErr = err.Error()
		// Record the stamp anyway so a broken file is not re-read and
		// re-reported on every single request.
		w.modTime, w.size = fi.ModTime(), fi.Size()
		return
	}
	w.cfg, w.loaded = cfg, true
	w.modTime, w.size = fi.ModTime(), fi.Size()
	w.lastErr = ""
}
