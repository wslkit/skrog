package policy

import (
	"os"
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
