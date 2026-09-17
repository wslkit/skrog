package main

import (
	"testing"

	"github.com/wslkit/skrog/internal/regcache"
)

// The busy probe must skip Skrog's own infrastructure and nothing else.
//
// Both directions of this are quiet failures, which is why it is pinned:
// counting the cache container holds the engine awake forever and suppresses
// every scheduled prune (so enabling a cache would silently disable two other
// features), while skipping a user's container stops the engine out from under
// real work.
func TestProbedContainerIsInfra(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
		want  bool
	}{
		{"the cache, as the API returns it", []string{"/" + regcache.ContainerName}, true},
		{"the cache, unslashed", []string{regcache.ContainerName}, true},
		{"a user's container", []string{"/my-postgres"}, false},
		{"no names at all", nil, false},
		// Substring matches must NOT count: someone else's container that
		// merely mentions the name is their work, not our plumbing.
		{"a lookalike prefix", []string{"/skrog-cache-of-mine"}, false},
		{"a lookalike suffix", []string{"/not-skrog-cache"}, false},
		// A container with several names counts if any of them is ours.
		{"aliased", []string{"/other", "/" + regcache.ContainerName}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probedContainer{Names: tc.names}.isInfra()
			if got != tc.want {
				t.Errorf("isInfra(%v) = %v, want %v", tc.names, got, tc.want)
			}
		})
	}
}
