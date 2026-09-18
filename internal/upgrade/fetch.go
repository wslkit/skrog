package upgrade

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// DefaultAssetBase is where release assets live. A var so a test can point it
// at a local server rather than the network.
var DefaultAssetBase = "https://github.com/wslkit/skrog/releases/download"

// Fetcher downloads and verifies a release's binaries.
//
// Verification is not optional and not a flag. The whole reason an in-process
// upgrade is defensible while the binaries are unsigned (#77) is that the
// checksum list is fetched alongside the zip and checked before anything on
// disk is touched -- a tighter loop than the documented `irm … | iex`, which
// asks the user to run a script from the internet with no check at all.
type Fetcher struct {
	// Base defaults to DefaultAssetBase.
	Base string
	// Client defaults to http.DefaultClient.
	Client *http.Client
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return http.DefaultClient
}

func (f *Fetcher) base() string {
	if f.Base != "" {
		return f.Base
	}
	return DefaultAssetBase
}

// Stage downloads the release asset for version/arch, checks it against the
// release's SHA256SUMS, and extracts the binaries into dir.
//
// Nothing is extracted until the hash matches. A download that fails
// verification leaves dir empty and the caller with an error, which is the only
// safe order: a swap cannot un-apply a bad binary once it has started.
func (f *Fetcher) Stage(ctx context.Context, version, arch, dir string) error {
	asset := AssetName(version, arch)
	tag := "v" + strings.TrimPrefix(version, "v")

	sums, err := f.get(ctx, fmt.Sprintf("%s/%s/SHA256SUMS", f.base(), tag))
	if err != nil {
		return fmt.Errorf("fetching SHA256SUMS: %w", err)
	}
	want, ok := sumFor(string(sums), asset)
	if !ok {
		return fmt.Errorf("SHA256SUMS for %s does not list %s", tag, asset)
	}

	zipBytes, err := f.get(ctx, fmt.Sprintf("%s/%s/%s", f.base(), tag, asset))
	if err != nil {
		return fmt.Errorf("downloading %s: %w", asset, err)
	}
	got := sha256.Sum256(zipBytes)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s failed verification: SHA256SUMS says %s, the download is %s",
			asset, want, hex.EncodeToString(got[:]))
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, asset)
	if err := os.WriteFile(tmp, zipBytes, 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return extractBinaries(tmp, dir)
}

func (f *Fetcher) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	// Bounded: a release zip is ~8 MB, and an unbounded read from a redirected
	// URL is how a download turns into a memory exhaustion bug.
	return io.ReadAll(io.LimitReader(resp.Body, 128<<20))
}

// sumFor finds the hash for a file in a SHA256SUMS body.
//
// The format is "<hash>  <name>", and the name may carry a leading * for the
// binary-mode marker sha256sum writes. Lines are split on whitespace rather
// than a fixed offset so a CRLF file -- which this project shipped until
// v0.4.1 (#248) -- still parses.
func sumFor(body, name string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// matchBinary returns the entry of Binaries equal to base, or false. The
// returned string is the constant, not the caller's: that is the point.
func matchBinary(base string) (string, bool) {
	for _, b := range Binaries {
		if b == base {
			return b, true
		}
	}
	return "", false
}

// extractBinaries pulls just the executables out of the release zip. The zip
// also carries LICENSE and README, which an upgrade has no business writing
// over the installed copies.
func extractBinaries(zipPath, dir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	found := 0
	for _, entry := range zr.File {
		// The archive's own string is used to CHOOSE a binary and never to
		// build the path we write. filepath.Base plus the allowlist already
		// stopped a "../" entry escaping dir, but the name still flowed from
		// the zip into filepath.Join, so nothing in the code said so -- not to
		// a reader, and not to CodeQL (go/zipslip).
		//
		// Resolving to the constant from Binaries makes the destination
		// provably ours: entry.Name reaches a comparison and stops there.
		name, ok := matchBinary(filepath.Base(entry.Name))
		if !ok {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 256<<20))
		rc.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o755); err != nil {
			return err
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("%s contained none of %v", filepath.Base(zipPath), Binaries)
	}
	return nil
}
