//go:build windows

package policy

import "golang.org/x/sys/windows"

// ownedByAdminOrSystem answers the same question checkOwner does, for a test
// that has to know what the host actually produced rather than assume it.
//
// Deliberately a second, simpler path to the answer: it reads the owner and
// compares, without the reporting or the fallbacks, so a test can tell whether
// checkOwner's VERDICT is right on this machine.
func ownedByAdminOrSystem(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, err
	}
	for _, wk := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinBuiltinAdministratorsSid,
		windows.WinLocalSystemSid,
	} {
		sid, err := windows.CreateWellKnownSid(wk)
		if err != nil {
			continue
		}
		if owner.Equals(sid) {
			return true, nil
		}
	}
	return false, nil
}
