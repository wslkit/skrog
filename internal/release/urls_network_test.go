//go:build network

package release_test

// The guard that was missing.
//
// TestEmbeddedManifestMatchesRootfsPins checks the URL's *filename* against
// versions.env, and it passed all the way through the Skrog rename (#1) while
// the manifest pointed at assets that do not exist: the rename rewrote the
// published filenames from hawser-rootfs-* to skrog-rootfs-*, and rewrote that
// test's expectation to match. Two wrongs, one green run, and every `skrog
// install` and `skrog engine rollback` would have 404'd.
//
// Nothing offline can catch that -- the manifest names bytes published
// elsewhere, so the only honest check is to ask GitHub. Hence the build tag:
// `go test ./...` stays offline and deterministic, and CI runs this one
// explicitly with `-tags network`.

import (
	"net/http"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/release"
)

func TestPublishedRootfsURLsResolve(t *testing.T) {
	m, err := release.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	checked := 0
	for _, e := range m.Engines {
		// EVERY architecture, not just this runner's (#388).
		//
		// Published() and HostRootfs() are host-relative, and using either
		// here would leave the arm64 URLs unchecked on an amd64 runner --
		// which is precisely the 404-in-the-manifest class this test exists
		// for, just aimed at the architecture CI does not happen to be.
		for _, arch := range e.Architectures() {
			r := e.Rootfs[arch]

			// An empty checksum is the documented interim state between
			// cutting a rootfs release and copying its digest in
			// (RELEASING.md step 2). The URL genuinely does not exist yet,
			// and `skrog install` already refuses on it, so there is nothing
			// here to verify.
			if r.URL == "" || r.SHA256 == "" {
				t.Logf("engine %s (%s): not published yet, skipped", e.Version, arch)
				continue
			}
			checked++

			req, err := http.NewRequest(http.MethodHead, r.URL, nil)
			if err != nil {
				t.Errorf("engine %s (%s): bad URL %q: %v", e.Version, arch, r.URL, err)
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("engine %s (%s): %s: %v", e.Version, arch, r.URL, err)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("engine %s (%s): %s returned %s -- an install or rollback to this engine would fail",
					e.Version, arch, r.URL, resp.Status)
			}
		}
	}

	// A manifest where every entry is unpublished would otherwise pass this
	// test by checking nothing at all.
	if checked == 0 {
		t.Error("no published engine in the manifest, so nothing was verified")
	}
}
