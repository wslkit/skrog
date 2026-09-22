//go:build !windows

package policy

// checkOwner is Windows-specific. Everywhere else the machine layer is a test
// fixture, and there is no ProgramData ACL to be wrong about.
func checkOwner(string) (bool, string) { return true, "" }
