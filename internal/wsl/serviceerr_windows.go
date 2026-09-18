//go:build windows

package wsl

import "errors"

// isServiceError reports whether the service answered with an error, as
// opposed to the COM plumbing failing.
//
// Only the second kind should demote the fast path. See ServiceError.
func isServiceError(err error) bool {
	var se *ServiceError
	return errors.As(err, &se)
}
