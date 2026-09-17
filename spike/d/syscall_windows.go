//go:build windows

package main

import "syscall"

func syscallN(trap uintptr, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(trap, args...)
	return r
}
