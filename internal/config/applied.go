package config

import "strings"

// Applies says when a change to key actually takes effect, so `skrog config
// set` can tell the user instead of leaving them to find out (#202).
//
// This exists because the CLI and the supervisor drifted apart: the help text
// told people to run `skrog restart` to enable the audit log, and `restart`
// bounces the engine, not the supervisor that held the setting. Turning the
// audit log on therefore did nothing and said nothing. Keeping the answer in
// one exported function, next to the keys themselves and covered by a test
// that every key has one, is what stops that recurring.
//
// The three answers are the only three there are:
//
//	AppliesNow        the supervisor follows the file; it is in force already
//	AppliesOnStart    it configures the engine, so the engine has to come back
//	AppliesOnUse      nothing is running that holds it; the next command reads it
//	AppliesOnWSLApply it is only an intention until `skrog wsl-config apply`
func Applies(key string) string {
	switch key {
	case KeyProxy, KeyNoProxy, KeyImportHostCAs, KeyGPU, KeyGPUVendor:
		return AppliesOnStart
	case KeyVerifySignature, KeyDiskWarnBelow:
		return AppliesOnUse
	case KeyAutostart:
		return AppliesAtLogon
	}
	// The wsl.* keys are the furthest thing from "in effect now": they are
	// recorded in Skrog's own config, written to the GLOBAL ~/.wslconfig only
	// by `skrog wsl-config apply`, and read by WSL only when the VM next
	// starts. Saying "in effect now" is the same false claim the audit log
	// made for two releases, and it was doing it for every sizing key.
	if _, ok := WSLKeys[key]; ok {
		return AppliesOnWSLApply
	}
	if strings.HasPrefix(key, "hook.") {
		return AppliesNow
	}
	// idle-timeout and audit: both read through the supervisor's config
	// watcher, so a change is picked up without anything being restarted.
	return AppliesNow
}

// The sentences `skrog config set` prints. Phrased as what the user gets,
// not as what the implementation does.
const (
	AppliesNow        = "in effect now"
	AppliesOnStart    = "applies to the engine on its next start (`skrog restart`)"
	AppliesOnUse      = "applies the next time it is read"
	AppliesAtLogon    = "the logon entry is written or removed now; it takes effect at your next logon"
	AppliesOnWSLApply = "recorded; `skrog wsl-config apply` writes it to ~/.wslconfig, " +
		"and WSL reads it when the VM next starts"
)
