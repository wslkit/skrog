package config

import (
	"strings"
	"testing"
)

func TestAppliesAnswersForEveryKey(t *testing.T) {
	// A key added without an answer here would silently fall through to "in
	// effect now", which is exactly the wrong direction to be wrong in: it is
	// the claim that was false about the audit log for two releases.
	valid := map[string]bool{
		AppliesNow:        true,
		AppliesOnStart:    true,
		AppliesOnUse:      true,
		AppliesOnWSLApply: true,
		AppliesAtLogon:    true,
	}
	for _, k := range Keys() {
		got := Applies(k)
		if !valid[got] {
			t.Errorf("Applies(%q) = %q, which is none of the known answers", k, got)
		}
	}
}

func TestAppliesMatchesWhatTheSupervisorDoes(t *testing.T) {
	// The contract, written down. Each expectation names the mechanism, so a
	// change to one without the other fails here rather than in a user's
	// terminal.
	cases := []struct {
		key  string
		want string
		why  string
	}{
		{KeyAudit, AppliesNow, "audit.Switch follows the setting per request"},
		{KeyIdleTimeout, AppliesNow, "the supervisor reads it per health tick"},
		{KeyHookPostStart, AppliesNow, "hookRunner reads the path when the event fires"},
		{KeyProxy, AppliesOnStart, "engineAdapter.Start re-reads it and dockerd must restart"},
		{KeyNoProxy, AppliesOnStart, "same path as the proxy"},
		{KeyImportHostCAs, AppliesOnStart, "the CA bundle is injected at engine start"},
		{KeyGPU, AppliesOnStart, "the CDI spec is installed at engine start"},
		{KeyVerifySignature, AppliesOnUse, "only `skrog install` reads it"},
		{KeyDiskWarnBelow, AppliesOnUse, "only `skrog doctor` reads it"},
		{KeyAutostart, AppliesAtLogon, "`config set autostart` writes the Run entry now; Windows reads it at logon"},
	}
	for _, c := range cases {
		if got := Applies(c.key); got != c.want {
			t.Errorf("Applies(%q) = %q, want %q (%s)", c.key, got, c.want, c.why)
		}
	}
}

func TestAppliesCoversEveryHookKey(t *testing.T) {
	for _, k := range HookKeys() {
		if got := Applies(k); got != AppliesNow {
			t.Errorf("Applies(%q) = %q, want %q — hooks are read when the event fires",
				k, got, AppliesNow)
		}
	}
}

func TestAppliesSentencesReadAsSentences(t *testing.T) {
	// They are printed straight after "key = value", so a trailing period or
	// a capital would look wrong there.
	for _, s := range []string{AppliesNow, AppliesOnStart, AppliesOnUse, AppliesAtLogon} {
		if strings.HasSuffix(s, ".") {
			t.Errorf("%q ends with a period", s)
		}
		if s == "" || strings.ToLower(s[:1]) != s[:1] {
			t.Errorf("%q should start lower-case", s)
		}
	}
}

// Every wsl.* key is an intention until `skrog wsl-config apply` writes it and
// the WSL VM restarts. They used to fall through to the default and report "in
// effect now", which is the same false claim this function was written to stop
// — and the "answers for every key" test could not catch it, because the wrong
// answer was still one of the valid three.
func TestAppliesWSLSizingKeysAreNotInEffectYet(t *testing.T) {
	if len(WSLKeys) == 0 {
		t.Fatal("no wsl.* keys; this test would pass vacuously")
	}
	for k := range WSLKeys {
		if got := Applies(k); got != AppliesOnWSLApply {
			t.Errorf("Applies(%q) = %q, want %q — nothing writes ~/.wslconfig until `wsl-config apply`",
				k, got, AppliesOnWSLApply)
		}
	}
}

// The sentence has to name the command that actually does the work, or it is
// the same class of dead end as telling people to run `skrog restart`.
func TestAppliesOnWSLApplyNamesTheCommand(t *testing.T) {
	if !strings.Contains(AppliesOnWSLApply, "wsl-config apply") {
		t.Errorf("AppliesOnWSLApply does not name the command: %q", AppliesOnWSLApply)
	}
}
