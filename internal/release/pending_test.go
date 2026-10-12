package release_test

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/wslkit/skrog/internal/release"
)

// skipWhilePending turns "the default engine's rootfs is not released yet"
// into a skip, on a pull request only.
//
// An engine bump is red by construction until its rootfs is published: the
// release is cut from the PR's own head, and its checksums land in the same PR
// (docs/bumping-upstream.md, step 7). A red the author expects is a red every
// reviewer learns to scroll past, which is the habit that lets a real failure
// through. So while the checksum is the ONLY thing missing, the PR says so
// plainly instead of failing.
//
// Narrow on purpose:
//   - only ErrNotPublished, i.e. a well-formed entry with an empty checksum.
//     A missing architecture, a bad URL or a wrong engine still fail.
//   - only when GITHUB_EVENT_NAME is pull_request. A push to main and any local
//     run still fail, so an empty checksum cannot merge and stay unnoticed.
func isPending(err error) bool {
	var notPublished *release.ErrNotPublished
	return errors.As(err, &notPublished)
}

func skipWhilePending(t *testing.T, err error) {
	t.Helper()
	if err == nil || !isPending(err) || os.Getenv("GITHUB_EVENT_NAME") != "pull_request" {
		return
	}
	msg := fmt.Sprintf("default engine rootfs is not released yet (expected on an engine-bump PR until its checksums are added): %v", err)
	fmt.Printf("::warning title=engine rootfs pending::%s\n", msg)
	t.Skip(msg)
}
