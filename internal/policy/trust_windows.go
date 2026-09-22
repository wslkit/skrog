//go:build windows

package policy

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// checkOwner reports whether a path is owned by an account a standard user
// cannot act as (#418).
//
// This is the check that makes the machine layer mean something. The default
// ACL on C:\ProgramData is:
//
//	BUILTIN\Users:(CI)(WD,AD)          -- create subdirectories
//	CREATOR OWNER:(OI)(CI)(IO)(F)      -- and own what you create
//
// so on any machine where a fleet policy has not landed yet, a standard user
// can create ProgramData\skrog first and hold Full Control of it and
// everything an administrator later drops inside. LoadMachine was a bare
// os.ReadFile: no owner check, no refusal to read a file the reader could have
// written. The layer described as fleet configuration was, on those machines,
// the user's own file.
//
// Owner, deliberately, and not a full DACL walk. The documented attack is
// exactly an ownership one -- CREATOR OWNER grants Full Control BECAUSE the
// user owns the object -- so the owner answers it. Walking the DACL would also
// catch "an administrator created it and then granted Users write", which is a
// misconfiguration rather than a bypass, and a hand-written ACE walker that
// gets an edge case wrong fails a correctly deployed fleet silently. That
// trade is recorded rather than assumed: if the misconfiguration case matters
// later, the DACL walk belongs here, with tests over real ACLs.
func checkOwner(path string) (trusted bool, why string) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		// Unreadable security information is not a pass. A machine layer whose
		// provenance cannot be established is exactly the case this exists for.
		return false, fmt.Sprintf("its owner could not be read (%v)", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, fmt.Sprintf("its owner could not be read (%v)", err)
	}

	// Administrators and SYSTEM, and nothing else. Those are what an MSI, a
	// GPO drop or an Intune deployment leaves as the owner of a ProgramData
	// subtree. TrustedInstaller owns Windows component files rather than
	// application data, so it is deliberately not here: adding SIDs that have
	// never been observed owning this file widens the trusted set for no case
	// anyone has hit.
	for _, wk := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinBuiltinAdministratorsSid,
		windows.WinLocalSystemSid,
	} {
		sid, err := windows.CreateWellKnownSid(wk)
		if err != nil {
			continue
		}
		if owner.Equals(sid) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("it is owned by %s, not by Administrators or SYSTEM", ownerName(owner))
}

// ownerName renders an owner for a human. A raw SID is correct and useless in
// a message whose whole job is to tell someone what went wrong -- "owned by
// S-1-5-21-1826575991-..." does not say "owned by you".
//
// Falls back to the SID when the account cannot be resolved, which happens for
// a deleted account or across a trust that is not reachable. That is still
// better than nothing, and it is the case where the raw value is genuinely the
// only answer.
func ownerName(sid *windows.SID) string {
	if account, domain, _, err := sid.LookupAccount(""); err == nil {
		if domain != "" {
			return domain + `\` + account
		}
		return account
	}
	return sid.String()
}
