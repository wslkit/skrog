package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultReleasesURL is the one endpoint this package ever contacts.
const DefaultReleasesURL = "https://api.github.com/repos/wslkit/skrog/releases?per_page=30"

// GitHubReleases reads the newest published app release from the GitHub API.
//
// It is the only network access in `skrog upgrade`, it happens only when a
// user runs the command, and it sends nothing but the request itself — no
// machine identifier, no version, no telemetry. The server learns an IP made
// a request, which is what any download would tell it anyway.
type GitHubReleases struct {
	// URL defaults to DefaultReleasesURL.
	URL string
	// Client defaults to a client with a short timeout: a version check must
	// never be the reason a command hangs.
	Client *http.Client
}

func (g *GitHubReleases) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (g *GitHubReleases) url() string {
	if g.URL != "" {
		return g.URL
	}
	return DefaultReleasesURL
}

// release is the subset of the API response this needs.
type release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// LatestApp returns the newest app version published, without its leading v.
//
// The repository publishes two kinds of release from one tag namespace: the
// app (v0.3.0) and the engine rootfs (rootfs-v29.8.0-1). Filtering to the
// app's shape matters more than it looks — the rootfs versions are numerically
// far higher, so taking the newest release of any kind would report that
// skrog 0.3.0 should upgrade to 29.8.0.
//
// Pre-releases count, and that is now a deliberate divergence rather than an
// accident of history.
//
// It was written when every release was flagged pre-release, so a check that
// ignored them would have told every user they were current forever. From
// v0.6.0 that premise is gone: stable releases exist. scripts/install.ps1
// makes the opposite choice — it prefers the newest NON-prerelease — because
// a one-liner that pipes a URL into a shell should land people on the stable
// build.
//
// Both are right for what they do, and the split is the policy:
//
//	install.ps1  -> stable by default. First contact; least surprise.
//	upgrade      -> reports everything, including previews.
//
// So `skrog upgrade --check` can report a preview as available on a machine
// install.ps1 put on the stable build. That is intended: someone who has the
// tool installed and asks what is newer should be told what is newer. What
// `--apply` then installs is their choice, not the checker's.
//
// Written down in RELEASING.md under "A normal release" so the next person to
// touch either side finds the reasoning before changing one of them.
func (g *GitHubReleases) LatestApp(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.url(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "skrog")

	resp, err := g.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Rate limiting is the common one and deserves to be recognizable
		// rather than surfacing as a bare 403.
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return "", fmt.Errorf("the releases API is rate-limiting this address (HTTP %d); try again later", resp.StatusCode)
		}
		return "", fmt.Errorf("the releases API answered HTTP %d", resp.StatusCode)
	}

	var rels []release
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return "", fmt.Errorf("reading the releases API response: %w", err)
	}

	best := ""
	for _, r := range rels {
		if r.Draft {
			continue
		}
		v, ok := appVersion(r.TagName)
		if !ok {
			continue
		}
		if best == "" || Compare(v, best) > 0 {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("no app release found among %d releases", len(rels))
	}
	return best, nil
}

// appVersion accepts an app tag (v0.3.0) and rejects everything else,
// including the rootfs tags that share the namespace.
func appVersion(tag string) (string, bool) {
	tag = strings.TrimSpace(tag)
	if !strings.HasPrefix(tag, "v") {
		return "", false
	}
	v := strings.TrimPrefix(tag, "v")
	if v == "" || v[0] < '0' || v[0] > '9' {
		return "", false
	}
	return v, true
}
