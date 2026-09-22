package doctor

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// checkPublishedPorts reports that a published port is reachable from this
// machine only (#507).
//
// `docker run -p 8080:80` looks like it worked from every angle a user can see:
// the container is healthy, `docker ps` shows the mapping, and
// `curl http://localhost:8080` answers. A phone on the same wifi gets
// connection refused, and nothing anywhere says why.
//
// Under WSL2's default NAT networking dockerd publishes 0.0.0.0:8080 INSIDE the
// distro, and WSL's localhostForwarding relay binds 127.0.0.1 on the Windows
// side and nothing else. Measured on the reporter's machine; docs/ports.md has
// the numbers and the listening socket.
//
// Not #163, which was published ports unreachable even from loopback under
// MIRRORED networking, and is fixed. Here loopback works. That is also why this
// stayed invisible: the acceptance stage #163 added fetches 127.0.0.1, so it
// passes and always will.
//
// Silent unless a port is actually published, for the reason checkMultiArch is
// silent: a standing warning about something nobody is using is what teaches
// people to skim doctor output.
func checkPublishedPorts() Check {
	c := Check{Name: "published-ports", Title: "published ports beyond this machine"}
	c.Run = func(f Facts) Result {
		if !f.EngineReachable {
			return result(c, Skip, "engine is not running, so nothing is published")
		}
		if len(f.PublishedPorts) == 0 {
			return result(c, Skip, "no container is publishing a port")
		}

		list := describePorts(f.PublishedPorts)

		// Mirrored networking gives the distro the host's own interfaces, so a
		// published port lands on them without any of this.
		if strings.EqualFold(f.WSLNetworkingMode, "mirrored") {
			return result(c, OK, fmt.Sprintf(
				"%s reachable beyond this machine (WSL networking is mirrored)", list))
		}
		if f.PublishScopeLAN {
			return result(c, OK, fmt.Sprintf(
				"%s relayed to every interface (network.publish-scope is lan)", list))
		}

		r := result(c, Warn, fmt.Sprintf(
			"%s reachable from this machine only", list))
		r.Detail = []string{
			"dockerd publishes inside the distro and WSL's own forwarder binds 127.0.0.1",
			"on the Windows side, so nothing else on your network can reach the container.",
			"Testing a web app from your phone is the usual way people meet this.",
		}
		r.Remedy = "`skrog config set network.publish-scope lan` relays published ports to " +
			"every interface (opt-in: it puts your containers on the network). " +
			"Or set networkingMode=mirrored in ~/.wslconfig, which changes every WSL distro " +
			"on the machine -- see `skrog wsl-config`. Neither is needed if localhost is all you use."
		return r
	}
	return c
}

// describePorts renders the ports for a one-line summary, bounded so a machine
// running a compose stack does not produce a paragraph.
func describePorts(ports []PublishedPort) string {
	nums := make([]int, 0, len(ports))
	for _, p := range ports {
		nums = append(nums, p.HostPort)
	}
	sort.Ints(nums)

	const max = 4
	shown := nums
	extra := 0
	if len(shown) > max {
		extra = len(shown) - max
		shown = shown[:max]
	}
	parts := make([]string, 0, len(shown))
	for _, n := range shown {
		parts = append(parts, strconv.Itoa(n))
	}
	s := "port " + strings.Join(parts, ", ")
	if len(nums) > 1 {
		s = "ports " + strings.Join(parts, ", ")
	}
	if extra > 0 {
		s += fmt.Sprintf(" and %d more", extra)
	}
	return s
}
