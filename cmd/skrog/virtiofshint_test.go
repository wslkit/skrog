package main

import (
	"strings"
	"testing"
)

// install offers virtiofs only where taking the offer would do something
// (#523): the engine is on 9p AND this WSL honours the key. Offering it on a
// WSL that ignores the key silently would send someone through a
// `wsl --shutdown` for nothing.
func TestVirtiofsHintOnlyWhereItWouldWork(t *testing.T) {
	cases := []struct {
		transport, wsl string
		want           bool
	}{
		{"9p", "3.0.1.0", true},
		{"9p", "2.9.11.0", true},
		{"9p", "2.7.14.0", false}, // ignores virtiofs=true silently
		{"9p", "", false},         // inbox WSL, no `wsl --version`
		{"9p", "garbage", false},  // cannot tell: stay quiet
		{"virtiofs", "3.0.1.0", false},
		{"", "3.0.1.0", false}, // live mount unread
	}
	for _, c := range cases {
		got := virtiofsHint(c.transport, c.wsl) != ""
		if got != c.want {
			t.Errorf("virtiofsHint(%q, %q) offered=%v, want %v", c.transport, c.wsl, got, c.want)
		}
	}
}

// The hint gives the same three commands doctor's virtiofs check does, so the
// two cannot drift into telling people different things.
func TestVirtiofsHintGivesDoctorsCommands(t *testing.T) {
	h := virtiofsHint("9p", "3.0.1.0")
	for _, want := range []string{
		"skrog config set wsl.virtiofs true",
		"skrog wsl-config apply",
		"wsl --shutdown",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("hint does not contain %q:\n%s", want, h)
		}
	}
}
