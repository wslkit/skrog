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

func (s *comSession) Close() {}
