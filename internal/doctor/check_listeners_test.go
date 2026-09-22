package doctor

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/procnet"
)

const (
	idWeb = "86f41237ad790c989eb3b2d0ffb768e308d17e140120ce87801da5747abedb01"
	idAPI = "aa5eb0ef4fcc1c0e4acacea400e16af7978c288306ec2dcb4527960dfd0f902d"
)

func listen(addr string, port int) procnet.Listener {
	return procnet.Listener{Addr: netip.MustParseAddr(addr), Port: port}
}

func pub(name, id string, host, container int) PublishedPort {
	return PublishedPort{Container: name, ContainerID: id, HostIP: "0.0.0.0", HostPort: host,
		ContainerPort: container, Proto: "tcp"}
}

func listenersResult(f Facts) Result {
	f.EngineReachable = true
	return checkContainerListeners().Run(f)
}

// The case the check exists for, as measured: a server bound to 127.0.0.1
// inside the container, published with -p, answering nothing.
func TestLoopbackOnlyListenerWarns(t *testing.T) {
	r := listenersResult(Facts{
		PublishedPorts:     []PublishedPort{pub("web", idWeb, 3000, 3000)},
		ContainerListeners: map[string][]procnet.Listener{idWeb: {listen("127.0.0.1", 3000)}},
	})
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn: %s", r.Status, r.Summary)
	}
	for _, want := range []string{"web", "3000", "127.0.0.1"} {
		if !strings.Contains(r.Summary, want) {
			t.Errorf("summary should name %q: %q", want, r.Summary)
		}
	}
	if !strings.Contains(r.Remedy, "0.0.0.0") {
		t.Errorf("remedy should say what to bind instead: %q", r.Remedy)
	}
}

// IPv6 loopback and an IPv4-mapped loopback are the same trap.
func TestLoopbackV6ListenersWarn(t *testing.T) {
	for _, addr := range []string{"::1", "::ffff:127.0.0.1"} {
		r := listenersResult(Facts{
			PublishedPorts:     []PublishedPort{pub("web", idWeb, 8080, 80)},
			ContainerListeners: map[string][]procnet.Listener{idWeb: {listen(addr, 80)}},
		})
		if r.Status != Warn {
			t.Errorf("%s: status = %v, want Warn", addr, r.Status)
		}
	}
}

// Listening on loopback AND on a reachable address is fine: -p reaches the
// second one.
func TestLoopbackPlusWildcardIsOK(t *testing.T) {
	r := listenersResult(Facts{
		PublishedPorts: []PublishedPort{pub("web", idWeb, 8080, 80)},
		ContainerListeners: map[string][]procnet.Listener{idWeb: {
			listen("127.0.0.1", 80), listen("::", 80),
		}},
	})
	if r.Status != OK {
		t.Errorf("status = %v, want OK: %s", r.Status, r.Summary)
	}
}

// The other trap: `-p 8080:80` for an app that listens on 3000.
func TestNothingOnTheContainerPortWarns(t *testing.T) {
	r := listenersResult(Facts{
		PublishedPorts:     []PublishedPort{pub("api", idAPI, 8080, 80)},
		ContainerListeners: map[string][]procnet.Listener{idAPI: {listen("0.0.0.0", 3000)}},
	})
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn", r.Status)
	}
	if !strings.Contains(r.Summary, "nothing listens on container port 80") {
		t.Errorf("summary should say which port has no listener: %q", r.Summary)
	}
	if !strings.Contains(r.Remedy, "right-hand side of -p") {
		t.Errorf("remedy should point at the mapping: %q", r.Remedy)
	}
}

// A container that was not measured is not judged: a false "nothing is
// listening" sends someone to debug an app that works.
func TestUnmeasuredContainerIsNotJudged(t *testing.T) {
	r := listenersResult(Facts{
		PublishedPorts:     []PublishedPort{pub("web", idWeb, 8080, 80)},
		ContainerListeners: map[string][]procnet.Listener{},
	})
	if r.Status != Skip {
		t.Errorf("status = %v with nothing measured, want Skip: %s", r.Status, r.Summary)
	}
}

func TestReachableListenerIsOK(t *testing.T) {
	r := listenersResult(Facts{
		PublishedPorts:     []PublishedPort{pub("web", idWeb, 8080, 80), pub("api", idAPI, 9090, 9090)},
		ContainerListeners: map[string][]procnet.Listener{idWeb: {listen("0.0.0.0", 80)}, idAPI: {listen("172.17.0.3", 9090)}},
	})
	if r.Status != OK {
		t.Errorf("status = %v, want OK: %s", r.Status, r.Summary)
	}
}

// Silent when there is nothing to say, like checkPublishedPorts.
func TestNoPublishedTCPPortSkips(t *testing.T) {
	udp := pub("dns", idWeb, 53, 53)
	udp.Proto = "udp"
	if r := listenersResult(Facts{PublishedPorts: []PublishedPort{udp}}); r.Status != Skip {
		t.Errorf("status = %v with only UDP published, want Skip", r.Status)
	}
	if r := checkContainerListeners().Run(Facts{}); r.Status != Skip {
		t.Errorf("status = %v with the engine down, want Skip", r.Status)
	}
}

func TestContainerListenersCheckIsRegistered(t *testing.T) {
	for _, c := range Registry() {
		if c.Name == "container-listeners" {
			return
		}
	}
	t.Error("container-listeners is not in the doctor registry")
}
