package wsl

import (
	"context"
	"sync"
)

// Fast is Local with a COM fast path for the calls that have a proven one.
//
// Today that is exactly one call: List. It is the one the supervisor makes on
// every health tick, and it is the one spike/d established end to end —
// 62-67 ms as a wsl.exe spawn against 0.66-0.81 ms over COM. Everything else is
// Local, unchanged, including Exec and Start: CreateLxProcess carries handles
// and a process lifecycle across the RPC boundary and nothing has established
// it yet (#356).
//
// This is an optimisation with a fallback, not a replacement. Local stays the
// reference implementation and keeps its tests, and any failure on the COM side
// demotes this instance to Local permanently rather than failing the call. A
// machine where the COM surface has moved is slower, not broken.
type Fast struct {
	*Local

	once sync.Once
	mu   sync.Mutex
	com  *comSession
	// why records what went wrong, for `doctor` to report. Empty while the
	// fast path is working.
	why string
}

// NewFast returns a WSL that uses the COM fast path where it can.
//
// It costs nothing to construct: the COM session is created on first use, so a
// process that never lists — most of the CLI — never pays for an apartment it
// does not need.
func NewFast() *Fast { return &Fast{Local: NewLocal()} }

var _ WSL = (*Fast)(nil)

// newSession is indirected so the fallback logic can be tested on a host with
// no wslservice — which includes CI, and every non-Windows machine.
var newSession = newCOMSession

// session returns the COM session, creating it once. A nil session means the
// fast path is unavailable and callers should use Local.
func (f *Fast) session() *comSession {
	f.once.Do(func() {
		s, err := newSession()
		if err != nil {
			f.mu.Lock()
			f.why = err.Error()
			f.mu.Unlock()
			return
		}
		f.mu.Lock()
		f.com = s
		f.mu.Unlock()
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.com
}

// demote turns the fast path off for the rest of this process.
//
// Retrying a surface that has moved would pay the failure on every tick
// forever. One failure is enough to decide: the CLI works, and being slower is
// the correct outcome of not understanding the service any more.
func (f *Fast) demote(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.com != nil {
		f.com.Close()
		f.com = nil
	}
	if f.why == "" && err != nil {
		f.why = err.Error()
	}
}

// List reports the registered distros, over COM when that is available.
func (f *Fast) List(ctx context.Context) ([]Distro, error) {
	s := f.session()
	if s == nil {
		return f.Local.List(ctx)
	}
	out, err := s.list(ctx)
	if err != nil {
		// A cancelled context is the caller's doing, not the backend's, and
		// must not be read as the surface having moved.
		if ctx.Err() != nil {
			return nil, err
		}
		f.demote(err)
		return f.Local.List(ctx)
	}
	return out, nil
}

// Accelerated reports whether the COM fast path is in use, and why not when it
// is not. For `skrog doctor`, which is where a machine that has silently fallen
// back should become visible rather than just being mysteriously slower.
//
// It forces the session to be created, because "would this work?" is the
// question doctor is asking.
func (f *Fast) Accelerated() (bool, string) {
	s := f.session()
	f.mu.Lock()
	defer f.mu.Unlock()
	if s != nil && f.com != nil {
		return true, ""
	}
	why := f.why
	if why == "" {
		why = "unavailable"
	}
	return false, why
}

// Close releases the COM session if one was created.
func (f *Fast) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.com != nil {
		f.com.Close()
		f.com = nil
	}
}
