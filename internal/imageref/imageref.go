// Package imageref answers one question: which registry does an image
// reference name?
//
// It is its own package because that question is the load-bearing step in
// every registry allowlist, and it used to be answered twice — once in
// internal/wslc for the administrator's WSL policy, once in internal/policy for
// Skrog's own policy.yaml. Same rule, same explanatory comment, written twice,
// and already drifting: one trimmed surrounding whitespace and the other did
// not, one resolved an empty reference to "unknown" and the other to Docker Hub
// (#371).
//
// That is not a tidiness complaint. A gate that resolves a reference one way at
// pull and another at create is worse than either behaviour on its own, and the
// divergence is silent. Neither of the two packages could own this without the
// other importing it, so it lives here and both depend on it.
package imageref

import "strings"

// DockerHub is what an unqualified reference resolves to. An allowlist that
// does not name it therefore blocks `docker pull busybox`, which is usually the
// point of deploying one.
const DockerHub = "docker.io"

// Registry extracts the registry host from an image reference, applying
// Docker's own rule: the part before the first slash is a registry only if it
// looks like a host — it contains a dot or a colon, or is "localhost".
// Everything else is a Docker Hub short name, which is why "library/ubuntu"
// is Hub and not a registry called "library".
//
// An empty reference returns "", meaning **unknown** rather than Docker Hub.
// The distinction matters to a caller: "" is "I could not attribute this",
// and what to do about that is a policy decision, not a parsing one. The two
// gates legitimately answer it differently — WSL's own IsRegistryAllowed
// permits an unattributable server because WSL does not guess, while Skrog's
// policy.yaml refuses it. Returning "docker.io" here would quietly make that
// choice for both of them, and make it wrongly.
func Registry(image string) string {
	ref := strings.TrimSpace(image)
	if ref == "" {
		return ""
	}
	first, _, found := strings.Cut(ref, "/")
	if !found {
		return DockerHub
	}
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first
	}
	return DockerHub
}
