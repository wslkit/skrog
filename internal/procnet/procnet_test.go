package procnet

import (
	"net/netip"
	"testing"
)

// Captured from skrog-engine (kernel 6.18.40.1-microsoft-standard-WSL2) on
// 2026-09-22: /proc/<pid>/net/tcp then tcp6 for two python:3-alpine
// containers running `python -m http.server`, one with `--bind 127.0.0.1` and
// one with `--bind ::`, both published with -p. Through -p the first answered
// nothing and the second answered 200 -- the difference this package exists
// to see.
const loopbackOnly = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 244403 1 00000000c70a597a 100 0 0 10 0
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
`

const wildcardV6 = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:1F40 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 245060 1 0000000040f4fa72 100 0 0 10 0
`

func TestParseLoopbackListener(t *testing.T) {
	got := Parse(loopbackOnly)
	if len(got) != 1 {
		t.Fatalf("Parse = %+v, want one listener", got)
	}
	if got[0].Port != 8000 || got[0].Addr != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("Parse = %+v, want 127.0.0.1:8000", got[0])
	}
	if !got[0].Loopback() {
		t.Error("127.0.0.1 not reported as loopback")
	}
}

func TestParseWildcardV6Listener(t *testing.T) {
	got := Parse(wildcardV6)
	if len(got) != 1 {
		t.Fatalf("Parse = %+v, want one listener", got)
	}
	if got[0].Port != 8000 || got[0].Addr != netip.IPv6Unspecified() {
		t.Errorf("Parse = %+v, want [::]:8000", got[0])
	}
	if got[0].Loopback() {
		t.Error("[::] reported as loopback")
	}
}

// The word-swapped encodings the captured fixtures do not happen to cover.
func TestParseAddressForms(t *testing.T) {
	for _, tc := range []struct {
		line     string
		addr     string
		loopback bool
	}{
		// ::1 -- the last word is 00000001, printed byte-reversed.
		{"0: 00000000000000000000000001000000:0BB8 00000000000000000000000000000000:0000 0A", "::1", true},
		// ::ffff:127.0.0.1, an IPv4-mapped loopback on a dual-stack socket.
		{"0: 0000000000000000FFFF00000100007F:0BB8 00000000000000000000000000000000:0000 0A", "::ffff:127.0.0.1", true},
		// 172.17.0.2, a container's own eth0 address: reachable through -p.
		{"0: 020011AC:0BB8 00000000:0000 0A", "172.17.0.2", false},
		// 0.0.0.0
		{"0: 00000000:0BB8 00000000:0000 0A", "0.0.0.0", false},
	} {
		got := Parse(tc.line)
		if len(got) != 1 {
			t.Errorf("%s: Parse = %+v, want one listener", tc.addr, got)
			continue
		}
		if got[0].Addr != netip.MustParseAddr(tc.addr) || got[0].Port != 3000 {
			t.Errorf("Parse(%q) = %+v, want %s:3000", tc.line, got[0], tc.addr)
		}
		if got[0].Loopback() != tc.loopback {
			t.Errorf("%s: Loopback() = %v, want %v", tc.addr, got[0].Loopback(), tc.loopback)
		}
	}
}

// A connected or closing socket is not a listener, however it is addressed.
func TestParseSkipsNonListeningSockets(t *testing.T) {
	established := "0: 0100007F:1F40 0100007F:D431 01 00000000:00000000 00:00000000 00000000 0 0 1 1"
	if got := Parse(established); len(got) != 0 {
		t.Errorf("Parse(established) = %+v, want none", got)
	}
	if got := Parse("garbage\n\n0: zz:1F40 00:0000 0A"); len(got) != 0 {
		t.Errorf("Parse(garbage) = %+v, want none", got)
	}
}
