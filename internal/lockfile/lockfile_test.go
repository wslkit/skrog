package lockfile

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/release"
)

const goodSHA = "aee4312306d7d613ca3d0c23049c19837707cd4837b266ea057b544ac9605af4"

// hostRootfs builds the per-architecture map an Engine carries since #388,
// with an entry for whatever architecture the test is running on. Keyed on
// runtime.GOARCH rather than a literal so these tests pass on the arm64 CI
// runner too, which is the machine that would notice if FromEngine started
// picking the wrong one.
func hostRootfs(url, sha string) map[string]release.Rootfs {
	return map[string]release.Rootfs{runtime.GOARCH: {URL: url, SHA256: sha}}
}

func TestFromEngineAndRoundTrip(t *testing.T) {
	e := &release.Engine{
		Version:    "29.7.2",
		Rootfs:     hostRootfs("https://example.com/rootfs.tar.gz", goodSHA),
		Components: map[string]string{"dockerd": "29.7.2", "runc": "1.3.0"},
	}
	l, err := FromEngine(e)
	if err != nil {
		t.Fatalf("FromEngine: %v", err)
	}
	if l.SchemaVersion != SchemaVersion || l.EngineVersion != "29.7.2" ||
		l.Rootfs.SHA256 != goodSHA || l.Components["runc"] != "1.3.0" {
		t.Fatalf("FromEngine wrong: %+v", l)
	}

	b, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if b[len(b)-1] != '\n' {
		t.Error("marshal should end in a newline")
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got.EngineVersion != l.EngineVersion || got.Rootfs.URL != l.Rootfs.URL ||
		got.Rootfs.SHA256 != l.Rootfs.SHA256 || got.Components["dockerd"] != "29.7.2" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// FromEngine must copy the components map, not alias the engine's.
func TestFromEngineCopiesComponents(t *testing.T) {
	comps := map[string]string{"dockerd": "29.7.2"}
	e := &release.Engine{Version: "v", Rootfs: hostRootfs("u", goodSHA), Components: comps}
	l, err := FromEngine(e)
	if err != nil {
		t.Fatal(err)
	}
	l.Components["dockerd"] = "mutated"
	if comps["dockerd"] != "29.7.2" {
		t.Error("FromEngine aliased the engine's components map")
	}
}

// A lock pins one architecture's bytes. An engine with no build for this host
// has nothing to pin, and FromEngine has to say so rather than emit a lock
// with an empty URL that fails much later, at the download.
func TestFromEngineRefusesAnEngineWithNoBuildForThisHost(t *testing.T) {
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	e := &release.Engine{
		Version: "29.7.2",
		Rootfs:  map[string]release.Rootfs{other: {URL: "https://x/y.tar.gz", SHA256: goodSHA}},
	}
	l, err := FromEngine(e)
	if err == nil {
		t.Fatalf("locked an engine built only for %s while running on %s: %+v",
			other, runtime.GOARCH, l)
	}
	var unsupported *release.ErrUnsupportedHostArch
	if !errors.As(err, &unsupported) {
		t.Errorf("want ErrUnsupportedHostArch, got %T: %v", err, err)
	}
}

// And the other failure: built for this host, release not cut yet. Different
// error, because the user's next move is different.
func TestFromEngineRefusesAnUnpublishedRootfs(t *testing.T) {
	e := &release.Engine{Version: "29.7.2", Rootfs: hostRootfs("", "")}
	if _, err := FromEngine(e); err == nil {
		t.Fatal("locked an engine with no published checksum")
	} else {
		var notPublished *release.ErrNotPublished
		if !errors.As(err, &notPublished) {
			t.Errorf("want ErrNotPublished, got %T: %v", err, err)
		}
	}
}

func TestValidate(t *testing.T) {
	base := func() Lock {
		return Lock{SchemaVersion: SchemaVersion, EngineVersion: "29.7.2",
			Rootfs: Rootfs{URL: "https://x/y.tar.gz", SHA256: goodSHA}}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid lock rejected: %v", err)
	}

	cases := map[string]func(*Lock){
		"bad schema":    func(l *Lock) { l.SchemaVersion = 99 },
		"no version":    func(l *Lock) { l.EngineVersion = "" },
		"no url":        func(l *Lock) { l.Rootfs.URL = "" },
		"short sha":     func(l *Lock) { l.Rootfs.SHA256 = "abc123" },
		"uppercase sha": func(l *Lock) { l.Rootfs.SHA256 = strings.ToUpper(goodSHA) },
		"non-hex sha":   func(l *Lock) { l.Rootfs.SHA256 = strings.Repeat("g", 64) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			l := base()
			mut(&l)
			if err := l.Validate(); err == nil {
				t.Errorf("expected %s to be rejected", name)
			}
		})
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	_, err := Parse([]byte(`{"schemaVersion":1,"engineVersion":"1","rootfs":{"url":"u","sha256":"` + goodSHA + `"},"extra":true}`))
	if err == nil {
		t.Fatal("unknown field should be rejected")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Fatal("garbage should fail to parse")
	}
}
