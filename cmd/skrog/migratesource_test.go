package main

import (
	"strings"
	"testing"
)

func TestResolveMigrateSource(t *testing.T) {
	for _, tc := range []struct {
		name        string
		selected    map[string]bool
		ctxOverride string
		wantContext string
		wantHost    string
	}{
		{"desktop", map[string]bool{"from-desktop": true}, "", "desktop-linux", ""},
		{"rancher", map[string]bool{"from-rancher": true}, "", "rancher-desktop", ""},
		// Podman registers no docker context, so it is addressed by pipe.
		{"podman", map[string]bool{"from-podman": true}, "", "", `npipe:////./pipe/podman-machine-default`},
		{"explicit context wins", map[string]bool{"from-desktop": true}, "my-ctx", "my-ctx", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMigrateSource(tc.selected, tc.ctxOverride, "")
			if err != nil {
				t.Fatalf("resolveMigrateSource: %v", err)
			}
			if got.Context != tc.wantContext {
				t.Errorf("Context = %q, want %q", got.Context, tc.wantContext)
			}
			if got.Host != tc.wantHost {
				t.Errorf("Host = %q, want %q", got.Host, tc.wantHost)
			}
		})
	}
}

// No source must not default to one: guessing which engine to copy from costs
// a long transfer of the wrong data before anyone notices.
func TestResolveMigrateSourceRequiresOne(t *testing.T) {
	_, err := resolveMigrateSource(map[string]bool{}, "", "")
	if err == nil {
		t.Fatal("no source was accepted; it must be required")
	}
	for _, want := range []string{"--from-desktop", "--from-rancher", "--from-podman"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s: %q", want, err)
		}
	}
}

// Two sources is a mistake worth catching before a transfer starts, not a
// silent first-wins.
func TestResolveMigrateSourceRefusesTwo(t *testing.T) {
	_, err := resolveMigrateSource(map[string]bool{
		"from-desktop": true,
		"from-podman":  true,
	}, "", "")
	if err == nil {
		t.Fatal("two sources were accepted")
	}
	if !strings.Contains(err.Error(), "--from-desktop") || !strings.Contains(err.Error(), "--from-podman") {
		t.Errorf("the error does not name both: %q", err)
	}
}

// --from-host is how a non-default podman machine is reached, so it has to
// win over the menu rather than be ignored.
func TestResolveMigrateSourceHostOverride(t *testing.T) {
	got, err := resolveMigrateSource(map[string]bool{"from-podman": true},
		"", `npipe:////./pipe/podman-machine-dev`)
	if err != nil {
		t.Fatalf("resolveMigrateSource: %v", err)
	}
	if got.Host != `npipe:////./pipe/podman-machine-dev` {
		t.Errorf("Host = %q; the override was not applied", got.Host)
	}
	if got.Context != "" {
		t.Errorf("Context = %q; a host override must not also carry a context", got.Context)
	}
}

// Every source needs a hint: when planning fails, "what should I check" is the
// whole value, and a source without one fails with a bare docker error.
func TestEverySourceHasAHint(t *testing.T) {
	for _, s := range migrateSources() {
		if strings.TrimSpace(s.Hint) == "" {
			t.Errorf("%s has no hint", s.Flag)
		}
		if strings.TrimSpace(s.Label) == "" {
			t.Errorf("%s has no label", s.Flag)
		}
		if s.Context == "" && s.Host == "" {
			t.Errorf("%s addresses no engine", s.Flag)
		}
	}
}

func TestJoinWords(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a and b"},
		{[]string{"a", "b", "c"}, "a, b and c"},
	} {
		if got := joinWords(tc.in); got != tc.want {
			t.Errorf("joinWords(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
