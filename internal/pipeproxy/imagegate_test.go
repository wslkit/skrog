package pipeproxy_test

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/apibody"
	"github.com/wslkit/skrog/internal/pipeproxy"
)

// fakeImageGate records what it was asked about and denies on request. It
// implements both interfaces, because the handler reaches ImageGate through a
// type assertion on the Gate it was given.
type fakeImageGate struct {
	denyPull  string
	denyBuild string
	denyPush  string

	sawPull  string
	sawBuild bool
	sawPush  string
}

// If this ever stops holding, the handler's type assertion fails and every
// pull, build and push in these tests passes unjudged — which is what happened
// when DenyPush was added to the interface and this fake was not updated. The
// tests below caught it, but a compile error is the cheaper place to find out.
var (
	_ pipeproxy.Gate      = (*fakeImageGate)(nil)
	_ pipeproxy.ImageGate = (*fakeImageGate)(nil)
)

func (g *fakeImageGate) DenyCreate(map[string]any) (string, bool) { return "", false }

func (g *fakeImageGate) DenyPull(image string) (string, bool) {
	g.sawPull = image
	if g.denyPull == "" {
		return "", false
	}
	return g.denyPull, true
}

func (g *fakeImageGate) DenyBuild() (string, bool) {
	g.sawBuild = true
	if g.denyBuild == "" {
		return "", false
	}
	return g.denyBuild, true
}

func (g *fakeImageGate) DenyPush(image string) (string, bool) {
	g.sawPush = image
	if g.denyPush == "" {
		return "", false
	}
	return g.denyPush, true
}

// driveRequest sends one raw request through the bridge and returns what the
// client sees, plus whether the engine was ever contacted.
func driveRequest(t *testing.T, gate pipeproxy.Gate, rawReq string) (*http.Response, func() bool) {
	t.Helper()
	client, bridgeClient := net.Pipe()
	engineSide, bridgeEngine := net.Pipe()

	go pipeproxy.RewriteBindsGuarded(nil, gate)(bridgeClient, bridgeEngine)

	reached := make(chan struct{}, 1)
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
		select {
		case reached <- struct{}{}:
		default:
		}
		engineSide.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	}()

	go func() { client.Write([]byte(rawReq)) }()

	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	engineReached := func() bool {
		select {
		case <-reached:
			return true
		default:
			return false
		}
	}
	return resp, engineReached
}

// The whole point of gating a pull: a blocked image must not be fetched onto
// the machine at all, so the request cannot reach the engine.
func TestDeniedPullNeverReachesTheEngine(t *testing.T) {
	gate := &fakeImageGate{denyPull: "allowlist does not permit docker.io"}
	resp, engineReached := driveRequest(t, gate,
		"POST /v1.45/images/create?fromImage=busybox&tag=latest HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if engineReached() {
		t.Error("the engine was contacted for a denied pull; the image may have been fetched")
	}
	if gate.sawPull != "busybox:latest" {
		t.Errorf("gate saw %q; the tag query parameter must be folded into the reference", gate.sawPull)
	}
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if got := string(buf[:n]); !strings.Contains(got, "allowlist does not permit") {
		t.Errorf("the reason must reach the user verbatim, got %q", got)
	}
}

func TestAllowedPullReachesTheEngine(t *testing.T) {
	gate := &fakeImageGate{}
	resp, engineReached := driveRequest(t, gate,
		"POST /v1.45/images/create?fromImage=contoso.azurecr.io%2Fapp HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !engineReached() {
		t.Error("an allowed pull never reached the engine")
	}
	if gate.sawPull != "contoso.azurecr.io/app" {
		t.Errorf("gate saw %q", gate.sawPull)
	}
}

func TestDeniedBuildNeverReachesTheEngine(t *testing.T) {
	gate := &fakeImageGate{denyBuild: "allowlist is in force; a build cannot be attributed"}
	resp, engineReached := driveRequest(t, gate,
		"POST /v1.45/build?t=x HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if engineReached() {
		t.Error("the engine was contacted for a denied build")
	}
	if !gate.sawBuild {
		t.Error("the build gate was never consulted")
	}
}

// The regression that matters most here: gating /build alone gates nothing a
// user would ever hit. buildx has been the default `docker build` since Docker
// 23 and never touches /build — it opens a session and hijacks /grpc. Against a
// real machine with an allowlist deployed, `DOCKER_BUILDKIT=0 docker build` was
// refused while the ordinary `docker build` beside it pulled `FROM busybox` off
// a forbidden docker.io and succeeded.
func TestBuildKitEndpointsAreGatedToo(t *testing.T) {
	for _, path := range []string{"/session", "/grpc", "/v1.45/session", "/v1.45/grpc"} {
		t.Run(path, func(t *testing.T) {
			gate := &fakeImageGate{denyBuild: "allowlist is in force"}
			resp, engineReached := driveRequest(t, gate,
				"POST "+path+" HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403 — BuildKit reaches the daemon here, so the allowlist is void unless it is judged", resp.StatusCode)
			}
			if engineReached() {
				t.Error("a denied BuildKit build reached the engine")
			}
			if !gate.sawBuild {
				t.Error("the build gate was never consulted")
			}
		})
	}
}

// The other half: a gate that allows builds must leave BuildKit alone entirely.
// /session and /grpc are hijacking endpoints, so judging them wrongly would
// break every build on a machine with no policy deployed at all.
func TestBuildKitEndpointsPassWhenBuildsAreAllowed(t *testing.T) {
	for _, path := range []string{"/session", "/grpc"} {
		gate := &fakeImageGate{}
		resp, engineReached := driveRequest(t, gate,
			"POST "+path+" HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, resp.StatusCode)
		}
		if !engineReached() {
			t.Errorf("%s: an allowed build was blocked", path)
		}
	}
}

// An import from a tarball names no registry, so there is nothing to judge and
// it must not be mistaken for a pull.
func TestImportIsNotTreatedAsAPull(t *testing.T) {
	gate := &fakeImageGate{denyPull: "should not be consulted"}
	resp, engineReached := driveRequest(t, gate,
		"POST /v1.45/images/create?fromSrc=-&repo=x HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — an import has no registry to gate", resp.StatusCode)
	}
	if !engineReached() {
		t.Error("an import was blocked")
	}
	if gate.sawPull != "" {
		t.Errorf("the pull gate was consulted for an import, with %q", gate.sawPull)
	}
}

// A plain Gate must keep working untouched: pulls and builds pass as they
// always have, with no type assertion surprises.
func TestGateWithoutImageGateIsUnaffected(t *testing.T) {
	resp, engineReached := driveRequest(t, &fakeGate{},
		"POST /v1.45/images/create?fromImage=busybox HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !engineReached() {
		t.Error("a pull was blocked by a gate that does not judge pulls")
	}
}

// Reads and listings must not be judged: gating them would break `docker
// images` and every status call for no security benefit.
func TestUnrelatedRequestsAreNotJudged(t *testing.T) {
	gate := &fakeImageGate{denyPull: "x", denyBuild: "y"}
	resp, engineReached := driveRequest(t, gate,
		"GET /v1.45/images/json HTTP/1.1\r\nHost: d\r\n\r\n")

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !engineReached() {
		t.Error("an image listing was blocked")
	}
	if gate.sawPull != "" || gate.sawBuild {
		t.Error("the gate was consulted for a listing")
	}
}

// The bypass: dockerd calls ParseForm and reads r.Form, where a urlencoded
// BODY shadows the query string. Reading only the query meant a pull with no
// query string at all was not recognised as a pull, so the gate never ran and
// the daemon fetched from a forbidden registry.
func TestFormBodyPullIsJudged(t *testing.T) {
	const body = "fromImage=evil.example.com/bad&tag=latest"
	gate := &fakeImageGate{denyPull: "allowlist does not permit evil.example.com"}
	resp, engineReached := driveRequest(t, gate,
		"POST /v1.44/images/create HTTP/1.1\r\nHost: d\r\n"+
			"Content-Type: application/x-www-form-urlencoded\r\n"+
			"Content-Length: "+itoa(len(body))+"\r\n\r\n"+body)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 — a form-body pull bypassed the gate", resp.StatusCode)
	}
	if engineReached() {
		t.Error("the engine was contacted for a denied pull")
	}
	if gate.sawPull != "evil.example.com/bad:latest" {
		t.Errorf("gate judged %q, want the reference from the BODY", gate.sawPull)
	}
}

// And where both are present, the body wins — exactly as dockerd resolves it.
func TestFormBodyShadowsTheQueryString(t *testing.T) {
	const body = "fromImage=evil.example.com/bad"
	gate := &fakeImageGate{}
	driveRequest(t, gate,
		"POST /v1.44/images/create?fromImage=contoso.azurecr.io/ok HTTP/1.1\r\nHost: d\r\n"+
			"Content-Type: application/x-www-form-urlencoded\r\n"+
			"Content-Length: "+itoa(len(body))+"\r\n\r\n"+body)

	if gate.sawPull != "evil.example.com/bad" {
		t.Errorf("gate judged %q; the body shadows the query the way ParseForm does", gate.sawPull)
	}
}

// A non-form body must not be parsed as one, and the ordinary query-only pull
// must keep working untouched.
func TestQueryOnlyPullStillWorks(t *testing.T) {
	gate := &fakeImageGate{}
	driveRequest(t, gate,
		"POST /v1.44/images/create?fromImage=busybox&tag=latest HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")
	if gate.sawPull != "busybox:latest" {
		t.Errorf("gate judged %q, want busybox:latest", gate.sawPull)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// caseGate records exactly what the create gate was shown, so a test can prove
// the gate and the daemon would agree.
type caseGate struct {
	sawImage  string
	sawPriv   bool
	consulted bool
}

func (g *caseGate) DenyCreate(body map[string]any) (string, bool) {
	g.consulted = true
	// Uses apibody, as every real gate must: the handler passes the map as
	// decoded, without rewriting keys, because several Docker objects are keyed
	// by data rather than by field name.
	g.sawImage = apibody.String(body, "Image")
	if hc, ok := apibody.Map(body, "HostConfig"); ok {
		p, _ := apibody.Field(hc, "Privileged")
		g.sawPriv, _ = p.(bool)
	}
	return "", false
}

// The second bypass: dockerd decodes into structs, so it matches keys
// case-insensitively and the LAST spelling wins. A gate reading a decoded map
// saw "Image" and the daemon ran "image". Refusing is the only honest answer,
// because a decoded map has lost the document order needed to say which wins.
func TestCaseVariantCreateBodyIsRefused(t *testing.T) {
	for _, body := range []string{
		`{"Image":"contoso.azurecr.io/ok","image":"evil.example.com/bad"}`,
		`{"Image":"ok","HostConfig":{"Privileged":false},"hostconfig":{"Privileged":true}}`,
		`{"Image":"ok","HostConfig":{"Privileged":false,"privileged":true}}`,
	} {
		gate := &caseGate{}
		resp, engineReached := driveRequest(t, gate,
			"POST /v1.44/containers/create HTTP/1.1\r\nHost: d\r\nContent-Type: application/json\r\n"+
				"Content-Length: "+itoa(len(body))+"\r\n\r\n"+body)

		if resp.StatusCode == http.StatusOK {
			t.Errorf("an ambiguous body was accepted: %s", body)
		}
		if engineReached() {
			t.Errorf("an ambiguous body reached the engine: %s", body)
		}
	}
}

// A single lower-case spelling is not ambiguous, and must still be JUDGED --
// dockerd honours it, so the gate has to see it too.
func TestLowercaseCreateFieldsAreStillJudged(t *testing.T) {
	const body = `{"image":"evil.example.com/bad","hostconfig":{"privileged":true}}`
	gate := &caseGate{}
	driveRequest(t, gate,
		"POST /v1.44/containers/create HTTP/1.1\r\nHost: d\r\nContent-Type: application/json\r\n"+
			"Content-Length: "+itoa(len(body))+"\r\n\r\n"+body)

	if !gate.consulted {
		t.Fatal("the gate was never consulted for a lowercase body")
	}
	if gate.sawImage != "evil.example.com/bad" {
		t.Errorf("gate saw Image=%q; dockerd would read the lowercase spelling", gate.sawImage)
	}
	if !gate.sawPriv {
		t.Error("gate saw Privileged=false; dockerd would read true from `privileged`")
	}
}

// Endpoints that fetch or run an image without naming it where this gate can
// judge it. Refused on the same ground as a build: with an allowlist in force,
// traffic that cannot be attributed must not proceed.
func TestUnattributableEndpointsAreRefused(t *testing.T) {
	for _, path := range []string{
		// Swarm genuinely cannot be attributed without parsing a TaskSpec,
		// and refusing beats parsing it and getting it subtly wrong.
		"/v1.44/services/create",
		"/v1.44/services/abc123/update",
		"/v1.44/swarm/init",
		// A plugin pull with NO remote: attributable in principle, not in
		// this request. It keeps the conservative treatment rather than
		// passing unjudged (#420).
		"/plugins/pull",
	} {
		gate := &fakeImageGate{denyBuild: "allowlist is in force"}
		resp, engineReached := driveRequest(t, gate,
			"POST "+path+" HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, resp.StatusCode)
		}
		if engineReached() {
			t.Errorf("%s: reached the engine", path)
		}
	}
}

// A plugin pull that NAMES its registry is judged as the pull it is (#420),
// not as an unattributable build.
//
// This is the hole the split closes. These endpoints used to inherit the build
// default — allowed unless deny-unattributable-builds was explicitly set — so
// a plain allow-registries let `docker plugin install evil.example.com/p`
// through. A plugin gets host device and mount access where an image gets a
// container, which made it a worse hole than the build one the permissive
// default was chosen to tolerate.
func TestPluginPullIsJudgedAsAPull(t *testing.T) {
	for _, path := range []string{
		"/v1.44/plugins/pull?remote=evil.example.com/rogue",
		"/v1.44/plugins/evil%2Frogue/upgrade?remote=evil.example.com/rogue",
		"/plugins/pull?remote=evil.example.com/rogue&name=rogue",
	} {
		// Note: denyBuild is EMPTY. The whole point is that the registry
		// allowlist alone refuses this, with no opt-in.
		gate := &fakeImageGate{denyPull: "policy does not allow images from evil.example.com"}
		resp, engineReached := driveRequest(t, gate,
			"POST "+path+" HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403 — allow-registries alone must refuse this", path, resp.StatusCode)
		}
		if engineReached() {
			t.Errorf("%s: reached the engine", path)
		}
		if gate.sawPull != "evil.example.com/rogue" {
			t.Errorf("%s: gate saw pull %q, want the remote", path, gate.sawPull)
		}
	}
}

// ...and a plugin from an ALLOWED registry still installs. The rule restricts
// where plugins come from; it does not ban the feature.
func TestPluginPullFromAnAllowedRegistryProceeds(t *testing.T) {
	gate := &fakeImageGate{} // allows everything
	resp, engineReached := driveRequest(t, gate,
		"POST /v1.44/plugins/pull?remote=registry.example.com/ok HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	if resp.StatusCode != http.StatusOK || !engineReached() {
		t.Errorf("a permitted plugin was blocked (status %d, reached=%v)", resp.StatusCode, engineReached())
	}
	if gate.sawPull != "registry.example.com/ok" {
		t.Errorf("gate saw pull %q, want the remote", gate.sawPull)
	}
}

// And with no allowlist they pass untouched: this must not break swarm or
// plugins on a machine with no policy deployed.
func TestUnattributableEndpointsPassWithoutAPolicy(t *testing.T) {
	for _, path := range []string{"/v1.44/plugins/pull", "/v1.44/services/create"} {
		gate := &fakeImageGate{}
		resp, engineReached := driveRequest(t, gate,
			"POST "+path+" HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")
		if resp.StatusCode != http.StatusOK || !engineReached() {
			t.Errorf("%s: blocked with no policy in force (status %d)", path, resp.StatusCode)
		}
	}
}

// Ordinary endpoints with similar-looking paths must not be caught.
func TestUnattributableMatcherIsNotOverBroad(t *testing.T) {
	for _, path := range []string{
		"/v1.44/services/abc123",   // GET-shaped inspect, POSTed
		"/v1.44/plugins",           // list
		"/v1.44/swarm",             // inspect
		"/v1.44/containers/create", // handled by its own gate
	} {
		gate := &fakeImageGate{denyBuild: "allowlist is in force"}
		resp, _ := driveRequest(t, gate,
			"POST "+path+" HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")
		if resp.StatusCode == http.StatusForbidden && path != "/v1.44/containers/create" {
			t.Errorf("%s: refused as unattributable, but it fetches nothing", path)
		}
	}
}

// A blocked registry must not be a place this machine can SEND to. An allowlist
// gating only inbound controls what enters the machine and says nothing about
// what leaves it, and leaving is the direction that moves data off it (#353).
func TestDeniedPushNeverReachesTheEngine(t *testing.T) {
	gate := &fakeImageGate{denyPush: "allowlist does not permit pushing to evil.example.com"}
	resp, engineReached := driveRequest(t, gate,
		"POST /v1.45/images/evil.example.com/x/push?tag=latest HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if engineReached() {
		t.Error("the engine was contacted for a denied push; the layers may already have left the machine")
	}
	if gate.sawPush != "evil.example.com/x" {
		t.Errorf("gate saw %q, want %q — the reference is the path up to /push", gate.sawPush, "evil.example.com/x")
	}
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if got := string(buf[:n]); !strings.Contains(got, "does not permit pushing") {
		t.Errorf("the reason must reach the user verbatim, got %q", got)
	}
}

func TestAllowedPushReachesTheEngine(t *testing.T) {
	gate := &fakeImageGate{}
	_, engineReached := driveRequest(t, gate,
		"POST /v1.45/images/contoso.azurecr.io/team/app/push?tag=v1 HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	if !engineReached() {
		t.Error("an allowed push must reach the engine")
	}
	if gate.sawPush != "contoso.azurecr.io/team/app" {
		t.Errorf("gate saw %q, want %q — a multi-segment repository must survive intact",
			gate.sawPush, "contoso.azurecr.io/team/app")
	}
}

// `docker plugin push` fetches nothing but sends a plugin to a registry, so it
// is the same outbound question and the same route.
func TestPluginPushIsJudged(t *testing.T) {
	gate := &fakeImageGate{denyPush: "nope"}
	resp, engineReached := driveRequest(t, gate,
		"POST /v1.45/plugins/evil.example.com/p/push HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if engineReached() {
		t.Error("the engine was contacted for a denied plugin push")
	}
	if gate.sawPush != "evil.example.com/p" {
		t.Errorf("gate saw %q", gate.sawPush)
	}
}

// Endpoints that merely LOOK like a push must not be swept up. /images/json is
// a listing, and a container named "push" is a container, not a registry
// operation.
func TestNonPushPathsAreNotJudgedAsPushes(t *testing.T) {
	for _, raw := range []string{
		"GET /v1.45/images/json HTTP/1.1\r\nHost: d\r\n\r\n",
		"POST /v1.45/containers/push/start HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n",
		"GET /v1.45/images/evil.example.com/x/push HTTP/1.1\r\nHost: d\r\n\r\n", // GET, not POST
	} {
		gate := &fakeImageGate{denyPush: "should not be consulted"}
		_, engineReached := driveRequest(t, gate, raw)
		if gate.sawPush != "" {
			t.Errorf("%q was judged as a push (gate saw %q)", strings.SplitN(raw, "\r\n", 2)[0], gate.sawPush)
		}
		if !engineReached() {
			t.Errorf("%q should have reached the engine", strings.SplitN(raw, "\r\n", 2)[0])
		}
	}
}

// A digest pull must reach the gate AS a digest.
//
// The docker CLI does not put the digest in fromImage. Captured from a real
// `docker pull ubuntu@sha256:...` against a listener standing in for the engine:
//
//	POST /v1.56/images/create?fromImage=docker.io%2Flibrary%2Fubuntu&tag=sha256%3A1e622c5f...
//
// Rejoining that with ":" yields "docker.io/library/ubuntu:sha256:1e622c5f...",
// which no rule can recognise as pinned — so a require-digest rule would refuse
// exactly the pull it exists to encourage.
func TestDigestPullReachesTheGateAsADigest(t *testing.T) {
	const digest = "sha256:1e622c5f073b4f6bfad6632f2616c7f59ef256e96fe78bf6a595d1dc4376ac02"
	gate := &fakeImageGate{}
	driveRequest(t, gate,
		"POST /v1.45/images/create?fromImage=docker.io%2Flibrary%2Fubuntu&tag="+
			strings.ReplaceAll(digest, ":", "%3A")+" HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	want := "docker.io/library/ubuntu@" + digest
	if gate.sawPull != want {
		t.Errorf("gate saw %q, want %q — a digest tag must rejoin with @, not :", gate.sawPull, want)
	}
}

// An ordinary tag still joins with ":", which is the case the digest handling
// must not disturb.
func TestTagPullStillJoinsWithColon(t *testing.T) {
	gate := &fakeImageGate{}
	driveRequest(t, gate,
		"POST /v1.45/images/create?fromImage=docker.io%2Flibrary%2Fubuntu&tag=24.04 HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")

	if want := "docker.io/library/ubuntu:24.04"; gate.sawPull != want {
		t.Errorf("gate saw %q, want %q", gate.sawPull, want)
	}
}

// A tag that merely contains a colon is not a digest. "sha256" alone, or an
// unknown algorithm, must not be treated as one.
func TestOnlyRealDigestAlgorithmsRejoinWithAt(t *testing.T) {
	for _, tag := range []string{"sha256", "weird:abc", "v1:2"} {
		gate := &fakeImageGate{}
		driveRequest(t, gate,
			"POST /v1.45/images/create?fromImage=x.example.com%2Fa&tag="+
				strings.ReplaceAll(tag, ":", "%3A")+" HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\n\r\n")
		if want := "x.example.com/a:" + tag; gate.sawPull != want {
			t.Errorf("tag %q: gate saw %q, want %q", tag, gate.sawPull, want)
		}
	}
}
