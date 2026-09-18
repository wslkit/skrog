//go:build !windows

package wsl

import (
	"context"
	"errors"
)

// The COM fast path is Windows-only. On other platforms every call falls
// through to wsl.exe, which is what the tests run against anyway.

type comSession struct{}

func newCOMSession() (*comSession, error) {
	return nil, errors.New("wsl: the wslservice COM interface exists only on Windows")
}

func (s *comSession) list(context.Context) ([]Distro, error) {
	return nil, errors.New("wsl: unsupported")
}

// terminate exists here only so the package compiles off Windows. It is
// unreachable in practice: newCOMSession always fails, so Fast.session()
// returns nil and every caller takes the Local path.
//
// Added late, which is the point of the note below.
func (s *comSession) terminate(context.Context, string) error {
	return errors.New("wsl: unsupported")
}

func (s *comSession) Close() {}

// ANY method added to comSession in com_windows.go must be added here too, or
// this package stops building for !windows — and CI will not tell you, because
// it builds only windows-latest and windows-11-arm. `GOOS=linux go build
// ./internal/wsl/` is the check; it caught `terminate` missing after #408.
