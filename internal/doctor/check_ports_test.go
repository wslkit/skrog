package doctor

import (
	"strings"
	"testing"
)

func portsResult(t *testing.T, f Facts) Result {
	t.Helper()
	return checkPublishedPorts().Run(f)
}

func published(ports ...int) []PublishedPort {
	out := make([]PublishedPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, PublishedPort{Container: "web", HostIP: "0.0.0.0", HostPort: p, Proto: "tcp"})
	}
	return out
}

// The case the check exists for: a port published under NAT, reachable from
// this machine and nowhere else, with nothing anywhere saying so.
func TestPublishedPortUnderNATWarns(t *testing.T) {
	r := portsResult(t, Facts{EngineReachable: true, PublishedPorts: published(8080)})
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn", r.Status)
	}
	if !strings.Contains(r.Summary, "8080") {
		t.Errorf("summary should name the port: %q", r.Summary)
	}
	if !strings.Contains(r.Summary, "this machine only") {
		t.Errorf("summary should say what is wrong, not just that something is: %q", r.Summary)
	}
	if r.Remedy == "" {
		t.Error("a warning with no remedy is the kind of doctor output people learn to skim")
	}
	// Both routes out, and the honest third option.
	for _, want := range []string{"publish-scope", "mirrored"} {
		if !strings.Contains(r.Remedy, want) {
			t.Errorf("remedy should mention %q: %q", want, r.Remedy)
		}
	}
}

// Mirrored networking gives the distro the host's interfaces, so the port is
// already reachable and there is nothing to report.
func TestMirroredNetworkingIsOK(t *testing.T) {
	r := portsResult(t, Facts{
		EngineReachable:   true,
		PublishedPorts:    published(8080),
		WSLNetworkingMode: "mirrored",
	})
	if r.Status != OK {
		t.Errorf("status = %v under mirrored networking, want OK", r.Status)
	}
}

// ...and so does the relay, which is the other way to make it true.
func TestPublishScopeLANIsOK(t *testing.T) {
	r := portsResult(t, Facts{
		EngineReachable: true,
		PublishedPorts:  published(8080),
		PublishScopeLAN: true,
	})
	if r.Status != OK {
		t.Errorf("status = %v with publish-scope lan, want OK", r.Status)
	}
}

// Silent when nothing is published, and when the engine is down.
//
// A check that warned on every machine would be a check people stop reading --
// the same reason checkMultiArch never warns about a capability nobody uses.
func TestNoPublishedPortsIsSilent(t *testing.T) {
	if r := portsResult(t, Facts{EngineReachable: true}); r.Status != Skip {
		t.Errorf("no ports: status = %v, want Skip", r.Status)
	}
	if r := portsResult(t, Facts{PublishedPorts: published(8080)}); r.Status != Skip {
		t.Errorf("engine down: status = %v, want Skip", r.Status)
	}
}

// A compose stack publishes a lot of ports, and the summary is one line.
func TestManyPortsAreSummarisedNotListed(t *testing.T) {
	r := portsResult(t, Facts{
		EngineReachable: true,
		PublishedPorts:  published(5432, 6379, 8080, 8081, 9000, 9090),
	})
	if !strings.Contains(r.Summary, "more") {
		t.Errorf("six ports should be elided, not listed: %q", r.Summary)
	}
	if len(r.Summary) > 120 {
		t.Errorf("summary is %d characters; it has to fit a terminal line: %q",
			len(r.Summary), r.Summary)
	}
	// Sorted, so the same machine reads the same way twice. The API returns
	// them in container order, which changes as containers restart.
	if !strings.Contains(r.Summary, "5432, 6379, 8080, 8081") {
		t.Errorf("ports should be sorted and elided from the end: %q", r.Summary)
	}
}

// Registered, not merely written.
func TestPublishedPortsCheckIsRegistered(t *testing.T) {
	for _, c := range Registry() {
		if c.Name == "published-ports" {
			return
		}
	}
	t.Fatal("checkPublishedPorts is not in Registry(), so `skrog doctor` never runs it")
}
