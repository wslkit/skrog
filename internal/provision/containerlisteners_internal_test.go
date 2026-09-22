package provision

import (
	"strings"
	"testing"
)

const (
	idLoopback = "86f41237ad790c989eb3b2d0ffb768e308d17e140120ce87801da5747abedb01"
	idWildcard = "aa5eb0ef4fcc1c0e4acacea400e16af7978c288306ec2dcb4527960dfd0f902d"
)

// The shape listenersScript printed on skrog-engine on 2026-09-22, for one
// container bound to 127.0.0.1:8000 and one bound to [::]:8000.
const listenerOutput = `== ` + idLoopback + `
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 244403 1 00000000c70a597a 100 0 0 10 0
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
== ` + idWildcard + `
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:1F40 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 245060 1 0000000040f4fa72 100 0 0 10 0
`

// Each section's sockets land under its own container, and not the one
// before it.
func TestParseListenerSectionsKeepsContainersApart(t *testing.T) {
	got := parseListenerSections(listenerOutput)
	if len(got) != 2 {
		t.Fatalf("parsed %d containers, want 2: %+v", len(got), got)
	}
	lo, any := got[idLoopback], got[idWildcard]
	if len(lo) != 1 || !lo[0].Loopback() || lo[0].Port != 8000 {
		t.Errorf("loopback container: %+v, want one loopback listener on 8000", lo)
	}
	if len(any) != 1 || any[0].Loopback() || any[0].Port != 8000 {
		t.Errorf("wildcard container: %+v, want one non-loopback listener on 8000", any)
	}
}

// A container that was read and has no listeners is present with none; one
// the script skipped is absent. The check tells those apart.
func TestParseListenerSectionsMeasuredEmptyIsPresent(t *testing.T) {
	got := parseListenerSections("== " + idLoopback + "\n  sl  local_address rem_address st\n")
	l, ok := got[idLoopback]
	if !ok {
		t.Fatal("a measured container with no listeners is missing from the map")
	}
	if len(l) != 0 {
		t.Errorf("listeners = %+v, want none", l)
	}
	if _, ok := got[idWildcard]; ok {
		t.Error("an unmeasured container appeared in the map")
	}
}

// Only full container IDs reach the shell, and only as positional arguments.
func TestListenerArgsPassesOnlyContainerIDs(t *testing.T) {
	args := listenerArgs([]string{idLoopback, "$(reboot)", "abc", idWildcard})
	if len(args) != 6 {
		t.Fatalf("args = %q, want sh -c script $0 and the two real IDs", args)
	}
	if args[0] != "sh" || args[1] != "-c" || args[3] != "sh" {
		t.Errorf("args = %q, want the IDs after `sh -c <script> sh`", args[:4])
	}
	if args[4] != idLoopback || args[5] != idWildcard {
		t.Errorf("IDs = %q", args[4:])
	}
	if strings.Contains(args[2], idLoopback) || strings.Contains(args[2], "reboot") {
		t.Error("an ID was spliced into the script text")
	}
	if listenerArgs([]string{"nope"}) != nil {
		t.Error("no valid IDs should mean no exec at all")
	}
}
