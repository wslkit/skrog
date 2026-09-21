// Package imageref answers one question: which registry does an image
// reference name?
//
// It is its own package because that question is the load-bearing step in
// every registry allowlist, and it used to be answered twice — once for the
// administrator's deployed WSL policy, once in internal/policy for Skrog's
// own policy.yaml. Same rule, written twice, and already drifting (#371).
//
// The rule itself is not ours. It is Docker's, via
// distribution/reference.ParseNormalizedNamed — the function the daemon uses.
// Hand-parsing it was a standing source of divergence: a gate that resolves a
// reference differently from the daemon judges one registry while the daemon
// contacts another, which is the same class of bug as the two bypasses fixed in
// #344 (#355).
package imageref

import (
	"strings"

	"github.com/distribution/reference"
)

// DockerHub is what an unqualified reference normalises to. An allowlist that
// does not name it therefore blocks `docker pull busybox`, which is usually the
// point of deploying one.
const DockerHub = "docker.io"

// Registry names the registry an image reference points at.
//
// ok is false when the reference cannot be parsed at all. That is deliberately
// distinct from "Docker Hub" and from "empty", and callers must not collapse it
// into either: dockerd rejects such a reference too, so a gate with an active
// allowlist should REFUSE it rather than guess a registry for it. Guessing is
// how a gate ends up permitting something the daemon then resolves elsewhere.
//
// Cases worth knowing, all verified against v0.6.0 rather than assumed:
//
//	busybox                        -> docker.io   (normalised to library/busybox)
//	library/ubuntu                 -> docker.io   (a Hub namespace, not a registry)
//	ghcr.io/o/r                    -> ghcr.io
//	localhost/x                    -> localhost   (a host despite having no dot)
//	registry:5000/app              -> registry:5000
//	MYREG/img                      -> MYREG       (uppercase implies a domain)
//	index.docker.io/library/x      -> docker.io   (canonicalised)
//	registry-1.docker.io/library/x -> registry-1.docker.io  (NOT canonicalised)
//	user@host/img, /leading, ""    -> ok == false
//
// The MYREG case is the parity gap #355 was filed for: Docker's
// splitDockerDomain treats a first component that is not all-lowercase as a
// domain, because Hub namespaces may not contain uppercase. Hand-parsing
// resolved it to docker.io, so an allowlist permitting Docker Hub also
// permitted a pull from a single-label host called MYREG.
//
// registry-1.docker.io is NOT folded into docker.io, and that is correct rather
// than an oversight. Docker itself does not fold it. #355 suggested it should
// be; doing so would make the gate disagree with the daemon in the opposite
// direction, which is the exact failure this parser exists to remove.
func Registry(image string) (host string, ok bool) {
	// Trim first: ParseNormalizedNamed rejects surrounding whitespace, and a
	// value arriving from a registry key or a YAML file can carry it. Refusing
	// " ghcr.io/x" outright would be a parsing decision masquerading as a
	// policy one.
	ref := strings.TrimSpace(image)
	if ref == "" {
		return "", false
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", false
	}
	return reference.Domain(named), true
}
