package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// create builds a container-create body the way the bridge decodes one.
func create(t *testing.T, jsonBody string) map[string]any {
	t.Helper()
	body, err := DecodeCreateBody([]byte(jsonBody))
	if err != nil {
		t.Fatalf("decoding test body: %v", err)
	}
	return body
}

func TestEmptyRulesAllowEverything(t *testing.T) {
	// The default state of an unconfigured machine. A guardrail nobody asked
	// for must not start refusing things.
	var r Rules
	if !r.Empty() {
		t.Fatal("a zero Rules must read as empty")
	}
	worst := create(t, `{"Image":"ubuntu","HostConfig":{"Privileged":true,"NetworkMode":"host",
		"CapAdd":["SYS_ADMIN"],"Binds":["C:\\secrets:/s"]}}`)
	if d := r.EvaluateCreate(worst); d.Denied {
		t.Errorf("empty rules denied a request: %s", d.Reason)
	}
}

func TestDenyPrivileged(t *testing.T) {
	r := Rules{DenyPrivileged: true}
	if d := r.EvaluateCreate(create(t, `{"HostConfig":{"Privileged":true}}`)); !d.Denied {
		t.Error("--privileged should be denied")
	} else if !strings.Contains(d.Reason, "privileged") {
		t.Errorf("the reason should name the flag: %q", d.Reason)
	}
	if d := r.EvaluateCreate(create(t, `{"HostConfig":{"Privileged":false}}`)); d.Denied {
		t.Errorf("Privileged:false should pass: %s", d.Reason)
	}
	// A body that never mentions it.
	if d := r.EvaluateCreate(create(t, `{"Image":"ubuntu"}`)); d.Denied {
		t.Errorf("a plain request should pass: %s", d.Reason)
	}
}

func TestCapabilities(t *testing.T) {
	t.Run("deny all added", func(t *testing.T) {
		r := Rules{DenyAddedCapabilities: true}
		if d := r.EvaluateCreate(create(t, `{"HostConfig":{"CapAdd":["NET_ADMIN"]}}`)); !d.Denied {
			t.Error("any --cap-add should be denied")
		}
	})

	t.Run("deny a named one, whatever the spelling", func(t *testing.T) {
		// docker accepts SYS_ADMIN and CAP_SYS_ADMIN; a rule set written with
		// one spelling must catch the other, or it is trivially bypassed.
		r := Rules{DenyCapabilities: []string{"SYS_ADMIN"}}
		for _, spelling := range []string{"SYS_ADMIN", "cap_sys_admin", "CAP_SYS_ADMIN", "sys_admin"} {
			body := create(t, `{"HostConfig":{"CapAdd":["`+spelling+`"]}}`)
			if d := r.EvaluateCreate(body); !d.Denied {
				t.Errorf("%q should be denied", spelling)
			}
		}
		if d := r.EvaluateCreate(create(t, `{"HostConfig":{"CapAdd":["NET_ADMIN"]}}`)); d.Denied {
			t.Errorf("an un-listed capability should pass: %s", d.Reason)
		}
	})
}

func TestDenyHostNamespaces(t *testing.T) {
	r := Rules{DenyHostNamespaces: true}
	for _, field := range []string{"NetworkMode", "PidMode", "IpcMode", "UTSMode"} {
		if d := r.EvaluateCreate(create(t, `{"HostConfig":{"`+field+`":"host"}}`)); !d.Denied {
			t.Errorf("%s=host should be denied", field)
		}
	}
	// The common, harmless values must not trip it.
	for _, ok := range []string{`{"HostConfig":{"NetworkMode":"bridge"}}`,
		`{"HostConfig":{"NetworkMode":"mynet"}}`, `{"HostConfig":{"PidMode":""}}`} {
		if d := r.EvaluateCreate(create(t, ok)); d.Denied {
			t.Errorf("%s should pass: %s", ok, d.Reason)
		}
	}
}

func TestAllowBindSources(t *testing.T) {
	r := Rules{AllowBindSources: []string{`C:\work`}}

	t.Run("both bind spellings are checked", func(t *testing.T) {
		// HostConfig.Binds is the legacy string form; Mounts is the structured
		// one. Checking only one would leave an open door.
		denied := []string{
			`{"HostConfig":{"Binds":["C:\\secrets:/s"]}}`,
			`{"HostConfig":{"Mounts":[{"Type":"bind","Source":"C:\\secrets","Target":"/s"}]}}`,
		}
		for _, b := range denied {
			if d := r.EvaluateCreate(create(t, b)); !d.Denied {
				t.Errorf("a bind outside the allowlist should be denied: %s", b)
			}
		}
	})

	t.Run("allowed roots pass, case and separator insensitively", func(t *testing.T) {
		for _, b := range []string{
			`{"HostConfig":{"Binds":["C:\\work\\proj:/app"]}}`,
			`{"HostConfig":{"Binds":["c:/work/proj:/app"]}}`,
			`{"HostConfig":{"Mounts":[{"Type":"bind","Source":"C:/WORK/x","Target":"/x"}]}}`,
		} {
			if d := r.EvaluateCreate(create(t, b)); d.Denied {
				t.Errorf("%s should pass: %s", b, d.Reason)
			}
		}
	})

	t.Run("a prefix that is not a path boundary does not count", func(t *testing.T) {
		// C:\workshop is not inside C:\work, however much it looks like it.
		body := create(t, `{"HostConfig":{"Binds":["C:\\workshop:/w"]}}`)
		if d := r.EvaluateCreate(body); !d.Denied {
			t.Error(`C:\workshop must not be treated as inside C:\work`)
		}
	})

	t.Run("volumes and tmpfs are not binds", func(t *testing.T) {
		// A named volume has no host path to restrict.
		for _, b := range []string{
			`{"HostConfig":{"Binds":["myvol:/data"]}}`,
			`{"HostConfig":{"Mounts":[{"Type":"volume","Source":"myvol","Target":"/d"}]}}`,
		} {
			if d := r.EvaluateCreate(create(t, b)); d.Denied {
				t.Errorf("%s is not a host bind and should pass: %s", b, d.Reason)
			}
		}
	})
}

func TestAllowRegistries(t *testing.T) {
	r := Rules{AllowRegistries: []string{"registry.example.com", "*.internal"}}

	denied := map[string]string{
		"docker hub, bare name":  "ubuntu",
		"docker hub, namespaced": "library/ubuntu",
		"explicit hub":           "docker.io/library/ubuntu",
		"some other registry":    "ghcr.io/foo/bar",
	}
	for name, image := range denied {
		t.Run(name, func(t *testing.T) {
			if d := r.EvaluateCreate(create(t, `{"Image":"`+image+`"}`)); !d.Denied {
				t.Errorf("%q should be denied", image)
			}
		})
	}

	allowed := []string{
		"registry.example.com/team/app",
		"registry.example.com:5000/team/app",
		"build.internal/app",
	}
	for _, image := range allowed {
		if d := r.EvaluateCreate(create(t, `{"Image":"`+image+`"}`)); d.Denied {
			t.Errorf("%q should pass: %s", image, d.Reason)
		}
	}
}

// TestAllowRegistriesResolvesReferencesLikeDocker checks the wiring rather than
// the parsing: the rule itself now lives in internal/imageref and is tested
// there (#371). What this asserts is that allow-registries actually applies it
// — which is the half that would break if the call site regressed.
//
// The cases are the ones that matter to an allowlist: a Hub namespace must not
// be mistaken for a registry, or an allowlist naming only an internal registry
// would silently permit half of Docker Hub.
func TestAllowRegistriesResolvesReferencesLikeDocker(t *testing.T) {
	r := Rules{AllowRegistries: []string{"ghcr.io"}}

	denied := []string{
		"ubuntu",                   // Hub short name
		"library/ubuntu",           // Hub namespace, NOT a registry called "library"
		"myuser/myimage",           // ditto
		"docker.io/library/ubuntu", // Hub, named explicitly
		"localhost/x",              // localhost is a host, and is not allowed here
		"localhost:5000/x",
		"registry.example.com:5/a/b",
	}
	for _, image := range denied {
		if !deniedBy(r, image) {
			t.Errorf("allow-registries=[ghcr.io] should deny %q", image)
		}
	}

	for _, image := range []string{"ghcr.io/o/r", "ghcr.io/o/r:v1"} {
		if deniedBy(r, image) {
			t.Errorf("allow-registries=[ghcr.io] should permit %q", image)
		}
	}

	// The discriminating case. Everything above stays green even if the parser
	// wrongly returned the first path component — "library" is not in the
	// allowlist either, so a broken parser still denies and the test still
	// passes. Allowing Docker Hub is what separates the two: a parser that
	// reads "library/ubuntu" as a registry called "library" denies this, and a
	// correct one permits it.
	hub := Rules{AllowRegistries: []string{"docker.io"}}
	for _, image := range []string{"ubuntu", "library/ubuntu", "myuser/myimage"} {
		if deniedBy(hub, image) {
			t.Errorf("allow-registries=[docker.io] should permit %q "+
				"(a Hub namespace is not a registry)", image)
		}
	}
	if !deniedBy(hub, "ghcr.io/o/r") {
		t.Error("allow-registries=[docker.io] should deny ghcr.io/o/r")
	}

	// #355 on this side of the house. An uppercase first component is a domain
	// to Docker, so allow-registries=[docker.io] must NOT permit MYREG/img —
	// the daemon would contact a single-label host called MYREG, not Hub.
	//
	// This case was added after a control run: reverting the parser left this
	// whole test green, because every other expectation here is one the old
	// hand-parser also got right.
	for _, image := range []string{"MYREG/img", "MyReg/img"} {
		if !deniedBy(hub, image) {
			t.Errorf("allow-registries=[docker.io] must not permit %q — Docker resolves it to that host", image)
		}
	}

	// An unparseable reference cannot be attributed, so it is refused rather
	// than guessed into Docker Hub and thereby allowed.
	for _, image := range []string{"user@host/img", "/leading", "UPPER/UPPER"} {
		if !deniedBy(hub, image) {
			t.Errorf("unattributable reference %q should be refused, not resolved to Hub", image)
		}
	}
}

// deniedBy reports whether allow-registries refuses this image.
func deniedBy(r Rules, image string) bool {
	_, denied := r.DenyCreate(map[string]any{"Image": image})
	return denied
}

func TestRequireDigest(t *testing.T) {
	r := Rules{RequireDigest: true}
	for _, bad := range []string{"ubuntu", "ubuntu:24.04", "registry.example.com/a:v1"} {
		if d := r.EvaluateCreate(create(t, `{"Image":"`+bad+`"}`)); !d.Denied {
			t.Errorf("%q is not pinned and should be denied", bad)
		}
	}
	pinned := `{"Image":"ubuntu@sha256:aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999"}`
	if d := r.EvaluateCreate(create(t, pinned)); d.Denied {
		t.Errorf("a digest-pinned image should pass: %s", d.Reason)
	}
}

func TestParseRefusesUnknownFields(t *testing.T) {
	// A misspelled rule that silently does nothing is the worst failure a
	// guardrail can have: it looks exactly like one that works.
	if _, err := Parse([]byte("deny-priviliged: true\n")); err == nil {
		t.Fatal("a misspelled key must be refused, not ignored")
	}
	r, err := Parse([]byte("deny-privileged: true\n"))
	if err != nil {
		t.Fatalf("a valid file must parse: %v", err)
	}
	if !r.DenyPrivileged {
		t.Error("deny-privileged did not take effect")
	}
}

func TestParseEmptyIsNoRules(t *testing.T) {
	for _, in := range []string{"", "\n", "# just a comment\n"} {
		r, err := Parse([]byte(in))
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
		}
		if !r.Empty() {
			t.Errorf("Parse(%q) should be an empty rule set", in)
		}
	}
}

func TestLoadMissingFileIsNoRules(t *testing.T) {
	// No policy is the default, and must not be an error.
	r, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("a missing policy file must not be an error: %v", err)
	}
	if !r.Empty() {
		t.Error("a missing file should mean no rules")
	}
}

func TestFirstObjectionWins(t *testing.T) {
	// Several rules would refuse this; the reason has to be one actionable
	// thing rather than a list.
	r := Rules{DenyPrivileged: true, DenyHostNamespaces: true, RequireDigest: true}
	d := r.EvaluateCreate(create(t, `{"Image":"ubuntu","HostConfig":{"Privileged":true,"NetworkMode":"host"}}`))
	if !d.Denied {
		t.Fatal("should be denied")
	}
	if d.Rule != "deny-privileged" {
		t.Errorf("rule = %q, want the first objection (deny-privileged)", d.Rule)
	}
}

func TestDenyCreateAdaptsToTheBridgeInterface(t *testing.T) {
	// The bridge takes a structural Gate so pipeproxy need not import this
	// package; that adapter has to actually line up.
	r := Rules{DenyPrivileged: true}
	reason, denied := r.DenyCreate(create(t, `{"HostConfig":{"Privileged":true}}`))
	if !denied || reason == "" {
		t.Errorf("DenyCreate = (%q, %v), want a denial with a reason", reason, denied)
	}
	if _, denied := r.DenyCreate(create(t, `{"Image":"ubuntu"}`)); denied {
		t.Error("a plain request should not be denied")
	}
}

func writePolicy(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(Path(dir), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// The watcher notices a change by mtime and size. Filesystem timestamp
	// resolution is coarse enough that two writes in the same tick can look
	// identical, so move the stamp deliberately rather than sleeping.
	future := time.Now().Add(time.Duration(writeSeq) * time.Second)
	writeSeq++
	if err := os.Chtimes(Path(dir), future, future); err != nil {
		t.Fatal(err)
	}
}

var writeSeq = 1

func TestWatcherPicksUpEditsWithNoRestart(t *testing.T) {
	// The bug this exists for: the first cut read the file once at supervisor
	// start and told users to run `skrog restart`, which bounces the engine
	// and not the supervisor — so the advice did not work even when followed.
	dir := t.TempDir()
	w := NewWatcher(dir)

	priv := map[string]any{"HostConfig": map[string]any{"Privileged": true}}

	if _, denied := w.DenyCreate(priv); denied {
		t.Fatal("no policy file yet: nothing should be denied")
	}

	writePolicy(t, dir, "deny-privileged: true\n")
	if _, denied := w.DenyCreate(priv); !denied {
		t.Error("a policy file created after the watcher should take effect")
	}

	writePolicy(t, dir, "deny-host-namespaces: true\n")
	if _, denied := w.DenyCreate(priv); denied {
		t.Error("removing a rule should take effect")
	}

	if err := os.Remove(Path(dir)); err != nil {
		t.Fatal(err)
	}
	host := map[string]any{"HostConfig": map[string]any{"NetworkMode": "host"}}
	if _, denied := w.DenyCreate(host); denied {
		t.Error("deleting the file should remove every rule")
	}
}

func TestWatcherKeepsLastGoodRulesWhenTheFileBreaks(t *testing.T) {
	// A typo saved mid-edit must not silently drop the guardrail: that is the
	// failure direction this package exists to avoid.
	dir := t.TempDir()
	var reported int
	w := NewWatcher(dir)
	w.OnError = func(error) { reported++ }

	writePolicy(t, dir, "deny-privileged: true\n")
	priv := map[string]any{"HostConfig": map[string]any{"Privileged": true}}
	if _, denied := w.DenyCreate(priv); !denied {
		t.Fatal("the good rules should be in force")
	}

	writePolicy(t, dir, "deny-priviliged: true\n") // typo
	if _, denied := w.DenyCreate(priv); !denied {
		t.Error("a broken file must keep the previous rules, not fail open")
	}
	if reported == 0 {
		t.Error("a broken file should be reported")
	}

	// ...and reported once, not on every request.
	before := reported
	for i := 0; i < 5; i++ {
		w.DenyCreate(priv)
	}
	if reported != before {
		t.Errorf("the same error was reported %d extra times", reported-before)
	}

	// Fixing it takes effect immediately.
	writePolicy(t, dir, "deny-host-namespaces: true\n")
	if _, denied := w.DenyCreate(priv); denied {
		t.Error("a fixed file should take effect")
	}
}

// TestDenyPullAppliesTheImageRules is #334.
//
// allow-registries was evaluated only on container create, so a blocked image
// could not RUN but could still be FETCHED onto the machine. require-digest had
// the same gap.
func TestDenyPullAppliesTheImageRules(t *testing.T) {
	r := Rules{AllowRegistries: []string{"registry.example.com"}}

	for _, image := range []string{"ubuntu", "library/ubuntu", "ghcr.io/o/r", "MYREG/img"} {
		if _, denied := r.DenyPull(image); !denied {
			t.Errorf("allow-registries should refuse the pull of %q", image)
		}
	}
	if _, denied := r.DenyPull("registry.example.com/app:v1"); denied {
		t.Error("an allowed registry should pull")
	}

	// An empty reference is not a pull to judge.
	if _, denied := r.DenyPull(""); denied {
		t.Error("an empty reference should not be refused")
	}
}

// TestRequireDigestAtPullAcceptsADigest is the case that would have been broken
// by the obvious implementation.
//
// A real `docker pull ubuntu@sha256:...` does NOT put the digest in fromImage.
// Captured from the CLI:
//
//	fromImage=docker.io%2Flibrary%2Fubuntu&tag=sha256%3A1e622c5f...
//
// so the bridge has to rejoin it with "@". If it joins with ":" instead, the
// reference reads "...ubuntu:sha256:1e622c5f..." and require-digest refuses
// precisely the pull it exists to encourage.
func TestRequireDigestAtPullAcceptsADigest(t *testing.T) {
	r := Rules{RequireDigest: true}

	const pinned = "docker.io/library/ubuntu@sha256:1e622c5f073b4f6bfad6632f2616c7f59ef256e96fe78bf6a595d1dc4376ac02"
	if reason, denied := r.DenyPull(pinned); denied {
		t.Errorf("a digest-pinned pull must be allowed, got %q", reason)
	}
	for _, image := range []string{"ubuntu", "ubuntu:24.04", "docker.io/library/ubuntu:latest"} {
		if _, denied := r.DenyPull(image); !denied {
			t.Errorf("require-digest should refuse the unpinned pull of %q", image)
		}
	}
}

// TestDenyPushAppliesAllowRegistriesOnly: require-digest is about what is
// consumed, and a push publishes something built locally. Demanding a digest
// there would refuse every ordinary `docker push app:v1`.
func TestDenyPushAppliesAllowRegistriesOnly(t *testing.T) {
	if _, denied := (Rules{RequireDigest: true}).DenyPush("registry.example.com/app:v1"); denied {
		t.Error("require-digest must not apply to a push")
	}

	r := Rules{AllowRegistries: []string{"registry.example.com"}}
	if _, denied := r.DenyPush("evil.example.com/x"); !denied {
		t.Error("allow-registries should refuse a push to a registry outside it")
	}
	if _, denied := r.DenyPush("registry.example.com/app:v1"); denied {
		t.Error("an allowed registry should accept a push")
	}
}

// Builds are deliberately not refused; see Rules.DenyBuild and #376. Pinned so
// that changing it is a conscious act rather than a side effect.
func TestBuildsAreNotRefusedByPolicyYAML(t *testing.T) {
	r := Rules{AllowRegistries: []string{"registry.example.com"}}
	if reason, denied := r.DenyBuild(); denied {
		t.Errorf("policy.yaml does not refuse builds (#376); got %q", reason)
	}
}

// A mirror is a registry this machine fetches image content from, and the
// allowlist had no view on it: the reference is untouched by a mirror, so the
// reference check passed whatever the mirror pointed at (#421).
func TestDenyMirrorJudgesTheMirrorHost(t *testing.T) {
	r := Rules{AllowRegistries: []string{"docker.io", "contoso.azurecr.io"}}

	if _, denied := r.DenyMirror("contoso.azurecr.io"); denied {
		t.Error("an allowed registry was refused as a mirror")
	}
	reason, denied := r.DenyMirror("evil.example.com")
	if !denied {
		t.Fatal("a registry outside the allowlist was accepted as a mirror")
	}
	for _, want := range []string{"evil.example.com", "mirror", "docker.io"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q should mention %q", reason, want)
		}
	}
}

// A port on the mirror host follows the same rule as everywhere else: an entry
// without a port means that host on any port.
func TestDenyMirrorFollowsThePortRule(t *testing.T) {
	r := Rules{AllowRegistries: []string{"registry.example.com"}}
	if _, denied := r.DenyMirror("registry.example.com:5000"); denied {
		t.Error("a port made an allowed host fail; the allowlist treats a portless entry as any port")
	}
}

// No allowlist, no opinion — the same rule the rest of the package follows.
// Refusing mirrors on a machine that does not restrict registries would break
// working setups to close a hole that is not open.
func TestDenyMirrorIsSilentWithoutAnAllowlist(t *testing.T) {
	if _, denied := (Rules{}).DenyMirror("anything.example.com"); denied {
		t.Error("refused a mirror with no allow-registries set")
	}
}

// The judgement half of #343: an image this machine has no record of, while
// the rule and an allowlist are both in force.
func TestDenyUnattributableImage(t *testing.T) {
	strict := Rules{
		AllowRegistries:          []string{"contoso.azurecr.io"},
		DenyUnattributableImages: true,
	}

	reason, denied := strict.DenyUnattributableImage("contoso.azurecr.io/x:1", "sha256:abc", false)
	if !denied {
		t.Fatal("an image with no provenance was allowed under the strict rule")
	}
	// The refusal has to say what to do, or it reads as Skrog being broken:
	// the reference passes allow-registries, so the user's mental model says
	// this should work.
	for _, want := range []string{"contoso.azurecr.io/x:1", "no record", "built locally", "contoso.azurecr.io"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason should mention %q: %q", want, reason)
		}
	}

	// A recorded image passes.
	if _, denied := strict.DenyUnattributableImage("contoso.azurecr.io/x:1", "sha256:abc", true); denied {
		t.Error("an image with provenance was refused")
	}
}

// Unresolvable is not unattributable. `docker run` of an image that is not
// here yet resolves to nothing, and the pull that follows is judged by
// DenyPull -- refusing here as well would break the ordinary case.
func TestDenyUnattributableImageAllowsAnUnresolvedReference(t *testing.T) {
	strict := Rules{
		AllowRegistries:          []string{"contoso.azurecr.io"},
		DenyUnattributableImages: true,
	}
	if _, denied := strict.DenyUnattributableImage("contoso.azurecr.io/x:1", "", false); denied {
		t.Error("refused an image that is simply not present yet")
	}
}

// Opt-in, and inert without an allowlist -- the same two guards
// deny-unattributable-builds has, for the same reasons.
func TestDenyUnattributableImageIsOptInAndNeedsAnAllowlist(t *testing.T) {
	off := Rules{AllowRegistries: []string{"contoso.azurecr.io"}}
	if _, denied := off.DenyUnattributableImage("x", "sha256:a", false); denied {
		t.Error("refused with the rule off")
	}

	noList := Rules{DenyUnattributableImages: true}
	if _, denied := noList.DenyUnattributableImage("x", "sha256:a", false); denied {
		t.Error("refused with no allow-registries; the rule has nothing to protect")
	}
}

// Either layer may turn it on, and the user layer may only tighten.
func TestMergeCarriesDenyUnattributableImages(t *testing.T) {
	if got := Merge(Rules{DenyUnattributableImages: true}, Rules{}); !got.DenyUnattributableImages {
		t.Error("the machine layer could not turn it on")
	}
	if got := Merge(Rules{}, Rules{DenyUnattributableImages: true}); !got.DenyUnattributableImages {
		t.Error("the user layer could not turn it on")
	}
	// A user layer cannot turn OFF what the machine set: that is the whole
	// point of the merge algebra.
	if got := Merge(Rules{DenyUnattributableImages: true}, Rules{DenyUnattributableImages: false}); !got.DenyUnattributableImages {
		t.Error("the user layer loosened a machine rule")
	}
}

// A file that sets only this key is a configured file, and reporting "no
// policy" for it would be the kind of lie this package exists to avoid.
func TestRulesWithOnlyTheImageRuleAreNotEmpty(t *testing.T) {
	if (Rules{DenyUnattributableImages: true}).Empty() {
		t.Error("a rule set with deny-unattributable-images reads as empty")
	}
}

// The key has to parse, or the documentation describes a file skrog ignores.
func TestDenyUnattributableImagesParses(t *testing.T) {
	r, err := Parse([]byte("deny-unattributable-images: true\nallow-registries:\n  - r.example.com\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !r.DenyUnattributableImages {
		t.Fatal("the key did not parse")
	}
	if _, denied := r.DenyUnattributableImage("r.example.com/x:1", "sha256:abc", false); !denied {
		t.Error("parsed rules did not refuse an unattributable image")
	}
}

// The Watcher is what the supervisor installs as the bridge's gate, and the
// bridge finds the provenance half by TYPE ASSERTION -- so a Watcher without
// this method leaves the rule parsing, reporting as active, and doing nothing.
// That is not hypothetical here: deny-unattributable-builds shipped exactly
// that way, and this is the test that was missing then.
func TestWatcherDenyUnattributableImageConsultsTheRules(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(MachineDirEnv, t.TempDir())
	write(t, filepath.Join(dir, FileName),
		"allow-registries:\n  - registry.example.com\ndeny-unattributable-images: true\n")

	w := NewWatcher(dir)
	reason, denied := w.DenyUnattributableImage("registry.example.com/x:1", "sha256:abc", false)
	if !denied {
		t.Fatal("Watcher.DenyUnattributableImage allowed an unattributable image with the rule set; the gate is a no-op")
	}
	if !strings.Contains(reason, "registry.example.com") {
		t.Errorf("reason does not name the allowlist in force: %q", reason)
	}
	if _, denied := w.DenyUnattributableImage("registry.example.com/x:1", "sha256:abc", true); denied {
		t.Error("Watcher refused an image this machine has a record of")
	}
}

// ...and allows by default, so turning the feature on stays the operator's
// decision rather than something an upgrade does to them.
func TestWatcherDenyUnattributableImageAllowsByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(MachineDirEnv, t.TempDir())
	write(t, filepath.Join(dir, FileName), "allow-registries:\n  - registry.example.com\n")

	if _, denied := NewWatcher(dir).DenyUnattributableImage("registry.example.com/x:1", "sha256:abc", false); denied {
		t.Error("Watcher refused without deny-unattributable-images set")
	}
}
