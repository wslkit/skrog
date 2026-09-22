package main

import (
	"strings"
	"testing"
)

// An image reference is whatever the client sent, and it reaches a log an
// operator reads and `skrog doctor --report` pastes into an issue. A newline
// in it forges a log line.
func TestLogSafeStripsControlCharacters(t *testing.T) {
	got := logSafe("ghcr.io/x:1\nlevel=ERROR msg=\"engine gone\"")
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("logSafe left a control character in %q", got)
	}
	if !strings.Contains(got, "ghcr.io/x:1") {
		t.Errorf("logSafe destroyed the readable part: %q", got)
	}
	// Replaced, not dropped: a tampered reference should still look tampered.
	if !strings.Contains(got, "�") {
		t.Errorf("logSafe removed the evidence rather than escaping it: %q", got)
	}
}

// An unbounded reference is a different denial of the same log.
func TestLogSafeBoundsLength(t *testing.T) {
	got := logSafe(strings.Repeat("a", 10_000))
	if len(got) > 300 {
		t.Errorf("logSafe returned %d bytes; a client should not choose how big a log line is", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncation is not marked: %q", got[max(0, len(got)-10):])
	}
}

// ...and the bound is on BYTES, which plain ASCII does not test.
//
// logSafe truncates at 256 bytes and then replaces each control character
// with U+FFFD, which is three bytes. A reference made entirely of control
// characters therefore expands threefold AFTER the cut, so the ASCII test
// above passes a bound three times looser than it claims to pin.
func TestLogSafeBoundsBytesNotRunes(t *testing.T) {
	got := logSafe(strings.Repeat("", 10_000))
	if len(got) > 300 {
		t.Errorf("logSafe returned %d bytes for a control-character reference; "+
			"the cut happens before the expansion", len(got))
	}
}

// An ordinary reference passes through untouched, or the log becomes useless.
func TestLogSafeLeavesAnOrdinaryReferenceAlone(t *testing.T) {
	const ref = "contoso.azurecr.io/team/app:1.2.3"
	if got := logSafe(ref); got != ref {
		t.Errorf("logSafe(%q) = %q", ref, got)
	}
}
