package pipeproxy_test

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/pipeproxy"
)

// fakeProv records what the bridge asked it, so these tests assert the WIRING
// -- that the hooks are reached at all -- rather than the store's behaviour,
// which is tested in internal/provenance.
type fakeProv struct {
	mu sync.Mutex
	// recorded is every ref handed to RecordPull.
	recorded []string
	// asked is every ref handed to Attributable.
	asked []string
	// id and known are what Attributable answers.
	id    string
	known bool
}

func (p *fakeProv) RecordPull(ref string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recorded = append(p.recorded, ref)
}

func (p *fakeProv) Attributable(ref string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, ref)
	return p.id, p.known
}

func (p *fakeProv) recordedRefs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.recorded...)
}

func (p *fakeProv) askedRefs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

// provGate is a gate that also judges provenance, so the bridge's type
// assertion for ProvenanceGate has something to find.
type provGate struct {
	fakeGate
	reasonForUnattributable string
	sawID                   string
	sawKnown                bool
	sawRef                  string
	consultedProvenance     bool
}

func (g *provGate) DenyUnattributableImage(ref, id string, known bool) (string, bool) {
	g.consultedProvenance = true
	g.sawRef, g.sawID, g.sawKnown = ref, id, known
	if g.reasonForUnattributable == "" || known {
		return "", false
	}
	return g.reasonForUnattributable, true
}

// drive runs one request through a provenance-wired bridge.
func drive(t *testing.T, gate pipeproxy.Gate, prov pipeproxy.Provenance, rawReq, engineReply string) *http.Response {
	t.Helper()
	client, bridgeClient := net.Pipe()
	engineSide, bridgeEngine := net.Pipe()

	go pipeproxy.RewriteBindsProvenanced(nil, gate, prov)(bridgeClient, bridgeEngine)

	go func() {
		br := bufio.NewReader(engineSide)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		engineSide.Write([]byte(engineReply))
	}()

	go func() { client.Write([]byte(rawReq)) }()

	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return resp
}

// A pull the gate allowed, relayed successfully, is recorded (#343).
//
// This is the wiring, and it is the half most at risk of being written and
// never called: the hook sits after the response body relay, which is a code
// path no other test reaches.
func TestSuccessfulPullIsRecorded(t *testing.T) {
	prov := &fakeProv{}
	gate := &provGate{}
	req := "POST /v1.45/images/create?fromImage=ghcr.io%2Forg%2Fimg&tag=1 HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n"
	resp := drive(t, gate, prov, req,
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}")
	resp.Body.Close()

	// The recording happens after the body is relayed, so give the handler a
	// moment past the response the client already has.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(prov.recordedRefs()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	got := prov.recordedRefs()
	if len(got) != 1 {
		t.Fatalf("RecordPull calls = %v, want exactly one", got)
	}
	if !strings.Contains(got[0], "ghcr.io/org/img") {
		t.Errorf("recorded %q, want the reference the client asked for", got[0])
	}
}

// A pull the engine refused is NOT recorded: there is nothing to attribute.
func TestFailedPullIsNotRecorded(t *testing.T) {
	prov := &fakeProv{}
	gate := &provGate{}
	req := "POST /v1.45/images/create?fromImage=ghcr.io%2Forg%2Fimg&tag=1 HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n"
	resp := drive(t, gate, prov, req,
		"HTTP/1.1 404 Not Found\r\nContent-Length: 2\r\n\r\n{}")
	resp.Body.Close()

	time.Sleep(200 * time.Millisecond)
	if got := prov.recordedRefs(); len(got) != 0 {
		t.Errorf("recorded %v for a pull the engine refused", got)
	}
}

// A container create asks about the image's provenance, and the answer reaches
// the gate.
func TestContainerCreateConsultsProvenance(t *testing.T) {
	prov := &fakeProv{id: "sha256:abc", known: true}
	gate := &provGate{}
	body := `{"Image":"ghcr.io/org/img:1"}`
	req := "POST /v1.45/containers/create HTTP/1.1\r\nHost: d\r\nContent-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	resp := drive(t, gate, prov, req, "HTTP/1.1 201 Created\r\nContent-Length: 2\r\n\r\n{}")
	resp.Body.Close()

	if got := prov.askedRefs(); len(got) != 1 || got[0] != "ghcr.io/org/img:1" {
		t.Fatalf("Attributable calls = %v, want the image from the body", got)
	}
	if !gate.consultedProvenance {
		t.Fatal("the gate was never asked to judge provenance")
	}
	if gate.sawID != "sha256:abc" || !gate.sawKnown {
		t.Errorf("gate saw id=%q known=%v; the resolver's answer did not reach it", gate.sawID, gate.sawKnown)
	}
}

// And a refusal stops the request: it must never reach the engine.
func TestUnattributableImageIsRefusedBeforeTheEngine(t *testing.T) {
	prov := &fakeProv{id: "sha256:abc", known: false}
	gate := &provGate{reasonForUnattributable: "no record of where this came from"}
	body := `{"Image":"ghcr.io/org/img:1"}`
	req := "POST /v1.45/containers/create HTTP/1.1\r\nHost: d\r\nContent-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body

	// The engine reply is deliberately a success: if the request reached it,
	// the client would see 201 rather than the refusal.
	resp := drive(t, gate, prov, req, "HTTP/1.1 201 Created\r\nContent-Length: 2\r\n\r\n{}")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// The image is read through apibody, so the spellings dockerd honours are all
// seen. An exact-key lookup would miss two of these three and judge a request
// the daemon acts on differently.
func TestProvenanceReadsTheImageFieldCaseInsensitively(t *testing.T) {
	for _, spelling := range []string{"Image", "image", "IMAGE"} {
		prov := &fakeProv{id: "sha256:abc", known: true}
		gate := &provGate{}
		body := `{"` + spelling + `":"ghcr.io/org/img:1"}`
		req := "POST /v1.45/containers/create HTTP/1.1\r\nHost: d\r\nContent-Type: application/json\r\n" +
			"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
		resp := drive(t, gate, prov, req, "HTTP/1.1 201 Created\r\nContent-Length: 2\r\n\r\n{}")
		resp.Body.Close()

		got := prov.askedRefs()
		if len(got) != 1 || got[0] != "ghcr.io/org/img:1" {
			t.Errorf("spelling %q: asked about %v, want the image reference", spelling, got)
		}
	}
}

// A bridge with no provenance hook behaves exactly as before: this is an
// addition, and the default path must not change.
func TestNilProvenanceChangesNothing(t *testing.T) {
	gate := &provGate{reasonForUnattributable: "would refuse"}
	body := `{"Image":"ghcr.io/org/img:1"}`
	req := "POST /v1.45/containers/create HTTP/1.1\r\nHost: d\r\nContent-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	resp := drive(t, gate, nil, req, "HTTP/1.1 201 Created\r\nContent-Length: 2\r\n\r\n{}")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201 with no provenance wired", resp.StatusCode)
	}
	if gate.consultedProvenance {
		t.Error("the provenance gate was consulted with no provenance hook")
	}
}
