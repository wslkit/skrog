//go:build !windows

package main

// enableVT is a no-op off Windows: terminals there handle escapes already.
func enableVT() {}
