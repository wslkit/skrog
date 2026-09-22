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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

			resp, err := headWithRetry(client, r.URL, 3)
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

// headWithRetry issues a HEAD, retrying only TRANSPORT failures.
//
// The distinction is the whole point, and collapsing it is what made this test
// redden main for a reason that had nothing to do with the change:
//
//	a non-200 status   the asset is missing or renamed. That is the bug this
//	                   test exists for (#1), it is deterministic, and it is
//	                   returned immediately without a retry.
//	a transport error  the connection was reset, timed out or never opened.
//	                   That says nothing about whether the asset exists, so
//	                   failing on it asserts something the test did not learn.
//
// The observed failure was `wsarecv: An existing connection was forcibly
// closed by the remote host` on a HEAD to GitHub's release-asset CDN, on a
// commit whose URLs had been downloaded and hashed by hand hours earlier. A
// re-run with no change passed.
//
// Three attempts, not more: this is a gate on every push to main, and a
// genuinely unreachable CDN should still fail the run rather than be retried
// into a pass over minutes.
func headWithRetry(client *http.Client, url string, attempts int) (*http.Response, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * time.Second)
		}
		req, err := http.NewRequest(http.MethodHead, url, nil)
		if err != nil {
			return nil, err // a malformed URL is not worth retrying
		}
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}

// A reset connection is retried, because it is not evidence about the asset.
func TestHeadWithRetrySurvivesAResetConnection(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			// Hijack and close without a response: the transport sees the
			// connection go away mid-request, which is the observed failure.
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := headWithRetry(srv.Client(), srv.URL, 3)
	if err != nil {
		t.Fatalf("gave up on a resettable connection: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %s", resp.Status)
	}
}

// A 404 is NOT retried. It is the answer this test exists to catch, it will
// not change on a second ask, and retrying it would turn a fast, clear failure
// into a slow one.
func TestHeadWithRetryDoesNotRetryA404(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	resp, err := headWithRetry(srv.Client(), srv.URL, 3)
	if err != nil {
		t.Fatalf("a 404 must come back as a response, not an error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %s, want 404", resp.Status)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("asked %d times; a missing asset is deterministic and must not be retried", got)
	}
}
