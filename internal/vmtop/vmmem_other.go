//go:build !windows

package vmtop

import "errors"

// ReadVmmem has nothing to read off Windows.
func ReadVmmem() (*Vmmem, error) {
	return nil, errors.New("vmmem is a Windows process")
}
