//go:build !windows

package vhdx

// Compact is Windows-only: virtdisk.dll is where CompactVirtualDisk lives, and
// a WSL2 disk exists nowhere else. The stub keeps the package importable so
// callers need no build tags of their own.
func Compact(path string) (Result, error) { return Result{}, ErrUnsupported }

// SizeOnDisk is likewise Windows-only.
func SizeOnDisk(path string) (uint64, error) { return 0, ErrUnsupported }
