//go:build !windows

package policy

import "errors"

func ownedByAdminOrSystem(string) (bool, error) {
	return false, errors.New("ownership is a Windows question")
}
