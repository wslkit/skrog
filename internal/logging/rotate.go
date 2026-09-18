// Package logging provides the rotating file writer the supervisor logs to.
//
// Rotation is size-based and self-contained rather than a dependency: the
// engine's own logs rotate inside the distro (daemon.json), so this only
// covers Skrog's supervisor — a low-volume log that must simply never eat the
// disk on a machine that stays logged on for months (PLAN §05 v0.2).
package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingWriter is an io.Writer that caps the file at MaxBytes and keeps a
// bounded number of predecessors (log, log.1, log.2, ...).
type RotatingWriter struct {
	path     string
	maxBytes int64
	keep     int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// NewRotatingWriter opens (or creates) path for appending. maxBytes <= 0
// defaults to 5 MB; keep <= 0 defaults to 3 predecessors.
func NewRotatingWriter(path string, maxBytes int64, keep int) (*RotatingWriter, error) {
	if maxBytes <= 0 {
		maxBytes = 5 * 1024 * 1024
	}
	if keep <= 0 {
		keep = 3
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating log dir: %w", err)
	}
	w := &RotatingWriter{path: path, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		w.f = nil // never leave a stale (closed) handle behind (#93)
		return fmt.Errorf("opening log: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		// Joined rather than dropped. Nothing has been written to this handle
		// yet, so Close should never have anything to report -- which is the
		// argument for surfacing it if it ever does, not for discarding it on
		// a writable file (CodeQL go/unhandled-writable-file-close).
		return errors.Join(fmt.Errorf("stat log: %w", err), f.Close())
	}
	w.f = f
	w.size = st.Size()
	return nil
}

// Write appends, rotating first when the write would cross the cap. A single
// write larger than the cap still lands (in a fresh file) rather than being
// dropped: losing the one huge record — a panic trace, say — would discard
// exactly the evidence rotation exists to preserve.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			// Rotation failing must not lose log lines; keep appending to the
			// oversized file and let the next write retry the rotation.
			fmt.Fprintf(os.Stderr, "skrog: log rotation failed: %v\n", err)
		}
	}

	// A prior rotation may have closed the file and failed to reopen it (#93),
	// leaving w.f nil; a logless supervisor is a serious blind spot, so try to
	// recover the handle on every write rather than silently discarding.
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}

	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate shifts path -> path.1 -> path.2 ... dropping the oldest.
func (w *RotatingWriter) rotate() error {
	// Close best-effort: a Close error must not abort rotation and strand a
	// full log forever (#93). The handle is being replaced regardless.
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	// Shift from the oldest end so each rename lands on a free name.
	os.Remove(fmt.Sprintf("%s.%d", w.path, w.keep))
	for i := w.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		// Reopen regardless: appending must survive a failed rename.
		w.open()
		return err
	}
	return w.open()
}

// Close flushes and closes the current file.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		return w.f.Close()
	}
	return nil
}
