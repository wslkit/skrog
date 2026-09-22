// Package procnet reads listening TCP sockets out of /proc/net/tcp and
// /proc/net/tcp6 (#510).
//
// Read from /proc/<pid>/net/*, those files describe the network namespace of
// that process -- so the init process of a container answers "what is listening
// inside this container", with no tool installed in the container and nothing
// run inside it. That is the whole reason for parsing a kernel table by hand:
// `ss` and `netstat` would have to exist in the image, and in most images they
// do not.
package procnet

import (
	"encoding/hex"
	"net/netip"
	"strconv"
	"strings"
)

// Listener is one socket in the LISTEN state.
type Listener struct {
	Addr netip.Addr
	Port int
}

// Loopback reports whether only this machine's own namespace can reach it:
// 127.0.0.0/8, ::1, or an IPv4-mapped loopback address.
func (l Listener) Loopback() bool { return l.Addr.Unmap().IsLoopback() }

// tcpListen is TCP_LISTEN in the kernel's state numbering, as the `st` column
// prints it.
const tcpListen = "0A"

// Parse returns the listening sockets in the text of one or more
// /proc/net/tcp or /proc/net/tcp6 tables, concatenated. Header lines and
// anything that does not parse are skipped: a partial answer is more useful
// to a diagnosis than none, and the caller decides what "none" means.
func Parse(text string) []Listener {
	var out []Listener
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		// sl local_address rem_address st ...
		if len(f) < 4 || f[3] != tcpListen {
			continue
		}
		host, port, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		addr, ok := parseAddr(host)
		if !ok {
			continue
		}
		p, err := strconv.ParseUint(port, 16, 16)
		if err != nil {
			continue
		}
		out = append(out, Listener{Addr: addr, Port: int(p)})
	}
	return out
}

// parseAddr decodes the kernel's address column: the address as 32-bit words
// in host byte order, printed in hex. On the little-endian machines WSL runs
// on (amd64, arm64) each word's bytes are therefore reversed, so 127.0.0.1
// prints as 0100007F.
func parseAddr(s string) (netip.Addr, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.Addr{}, false
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	if len(b) == 4 {
		return netip.AddrFrom4([4]byte(b)), true
	}
	return netip.AddrFrom16([16]byte(b)), true
}
