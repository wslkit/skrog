package main

import (
	"sort"
	"testing"
)

// Every key in subcommands must name a real command, or completion offers
// words for something that does not exist and the map rots unnoticed.
func TestSubcommandsNameRealCommands(t *testing.T) {
	known := map[string]bool{}
	for _, c := range commands() {
		known[c.name] = true
	}
	for name := range subcommands {
		if !known[name] {
			t.Errorf("subcommands has %q, which is not a command", name)
		}
	}
}

// No empty lists and no duplicates: an empty entry means "has subcommands"
// to a generator while offering none, which is worse than being absent.
func TestSubcommandsAreWellFormed(t *testing.T) {
	for name, subs := range subcommands {
		if len(subs) == 0 {
			t.Errorf("%q has an empty subcommand list; remove the entry instead", name)
		}
		seen := map[string]bool{}
		for _, s := range subs {
			if s == "" {
				t.Errorf("%q has an empty subcommand", name)
			}
			if seen[s] {
				t.Errorf("%q lists %q twice", name, s)
			}
			seen[s] = true
		}
	}
}

// helpIndex is what the completion generator reads, so assert the subcommands
// actually reach it rather than only living in the map.
func TestHelpIndexCarriesSubcommands(t *testing.T) {
	byName := map[string]helpEntry{}
	for _, e := range helpIndex() {
		byName[e.Name] = e
	}

	if len(byName) != len(commands()) {
		t.Fatalf("helpIndex has %d entries, commands() has %d", len(byName), len(commands()))
	}

	got := byName["snapshot"].Subs
	want := []string{"list", "save", "restore", "delete"}
	if len(got) != len(want) {
		t.Fatalf("snapshot subs = %v, want %v", got, want)
	}
	gotSorted, wantSorted := append([]string(nil), got...), append([]string(nil), want...)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)
	for i := range gotSorted {
		if gotSorted[i] != wantSorted[i] {
			t.Errorf("snapshot subs = %v, want %v", got, want)
			break
		}
	}

	// A command with no subcommands must omit the field entirely, so a
	// consumer can tell "takes subcommands" from "takes arguments".
	if subs := byName["start"].Subs; subs != nil {
		t.Errorf("start has subs %v; it takes none", subs)
	}
}
