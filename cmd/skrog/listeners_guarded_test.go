package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every listener that serves the engine must install the guarded handler.
//
// Only the supervisor did. `skrog serve --tcp` — whose own help text says it
// exists so "a teammate or a CI runner" can drive this engine — used the bare
// rewriter, so every rule the machine's owner wrote was unenforced for remote
// clients and none of their calls were audited (#257). `skrog proxy` dropped
// auditing the same way.
//
// This is a source check rather than a behavioural one on purpose: the failure
// was a listener being *added* without the gate, and what needs pinning is
// that no file wires a Server.Handler to the unguarded rewriter. A test that
// drove each listener would prove today's three correct and say nothing about
// the fourth.
func TestNoListenerUsesTheUnguardedRewriter(t *testing.T) {
	// `Handler: pipeproxy.RewriteBinds` or `srv.Handler = pipeproxy.RewriteBinds`,
	// but not RewriteBindsGuarded / RewriteBindsAudited.
	bare := regexp.MustCompile(`Handler\s*[:=]\s*pipeproxy\.RewriteBinds\b`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if bare.MatchString(line) {
				t.Errorf("%s:%d installs the unguarded rewriter, so this listener enforces no policy and audits nothing:\n  %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// And every listener must install the PROVENANCED handler, for the same
// reason one release later (#343).
//
// deny-unattributable-images is judged inside the handler, so a listener still
// on RewriteBindsGuarded serves the same engine with that rule not applying --
// and unlike a missing gate, nothing about it is visible: `skrog policy show`
// reports the rule active, because it is, on the listener that has the hook.
//
// A source check for the reason the one above gives: what needs pinning is
// that a listener added later cannot quietly opt out.
func TestEveryListenerInstallsTheProvenancedHandler(t *testing.T) {
	guardedOnly := regexp.MustCompile(`Handler\s*[:=]\s*pipeproxy\.RewriteBindsGuarded\b`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if guardedOnly.MatchString(line) {
				t.Errorf("%s:%d installs the handler without provenance, so deny-unattributable-images does not apply to this listener:\n  %s",
					name, i+1, strings.TrimSpace(line))
			}
			if strings.Contains(line, "pipeproxy.RewriteBindsProvenanced") {
				found++
			}
		}
	}
	// A regex that matches nothing passes vacuously; this is the half that
	// notices the handler wiring disappearing altogether.
	if found < 3 {
		t.Errorf("found %d provenanced handlers, expected one per listener (supervisor, serve, proxy)", found)
	}
}

// Every listener also seeds, for a reason that only shows up in the wrong
// order (#343).
//
// Seeding is what records the images already on the machine as pre-existing,
// and it runs once. If only the supervisor did it, a machine where `skrog
// proxy` or `skrog serve` came up first with deny-unattributable-images set
// would refuse its entire existing image cache -- correct by the rule, and
// indistinguishable from the feature being broken.
//
// Seed is a no-op once the store exists, so calling it from all three is free.
func TestEveryListenerSeedsProvenance(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	handlers, seeds := 0, 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "pipeproxy.RewriteBindsProvenanced") {
				handlers++
			}
			// The declaration in provenance.go is not a call site.
			if strings.Contains(line, "prov.SeedExisting(") {
				seeds++
			}
		}
	}
	if seeds != handlers {
		t.Errorf("%d listeners install the provenanced handler but only %d seed; "+
			"a listener that judges provenance without seeding refuses every pre-existing image",
			handlers, seeds)
	}
}
