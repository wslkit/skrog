//go:build !windows

package wsl

// isServiceError is always false off Windows: there is no COM session, so
// every error came from somewhere else.
func isServiceError(error) bool { return false }
