package wslc

import (
	"fmt"
	"strings"

	"github.com/wslkit/skrog/internal/apibody"
)

// PolicyGate enforces the administrator's WSL container policy at Skrog's pipe,
// standing in for the checks that live in wslcsession and that a direct
// docker.sock relay bypasses (#322).
//
// It implements pipeproxy.Gate and pipeproxy.ImageGate. The rules are WSL's,
// read from WSL's own configuration — the aim is that a deployed allowlist
// means the same thing through this pipe as through `wslc`, not that Skrog
// invents a second policy language next to it. Skrog's own policy.yaml is a
// separate, additional gate; this one exists so an Intune deployment is not
// silently voided by installing Skrog.
type PolicyGate struct {
	// Policies is the policy to enforce when Source is nil. Tests set it
	// directly; production wires Source instead.
	Policies Policies

	// Source returns the policy to enforce for THIS request, re-reading it if
	// it has changed on disk.
	//
	// It exists because a fixed snapshot is wrong here: the supervisor runs for
	// months, so a policy deployed or tightened after it started would never be
	// enforced (#354). Every judgement below goes through policies() rather
	// than touching the field, so there is one place this can be got wrong.
	Source func() Policies
}

// policies is the policy in force for the request being judged.
func (g *PolicyGate) policies() Policies {
	if g.Source != nil {
		return g.Source()
	}
	return g.Policies
}

// DenyCreate judges `docker create` / `docker run`.
//
// Two checks: the image's registry against the allowlist, and --privileged
// against AllowWSLContainerPrivileged.
func (g *PolicyGate) DenyCreate(body map[string]any) (string, bool) {
	// Read once for the whole judgement: both checks below must apply the same
	// policy, and re-reading between them could straddle a GPO refresh.
	p := g.policies()

	if image := apibody.String(body, "Image"); image != "" {
		if server := RegistryServer(image); !p.RegistryAllowed(server) {
			return fmt.Sprintf(
				"WSLContainerRegistryAllowlist does not permit registry %q (image %q); "+
					"this machine's WSL policy allows: %s",
				server, image, strings.Join(p.RegistryAllowlist, ", ")), true
		}
	}

	if !p.PrivilegedAllowed {
		if hc, ok := apibody.Map(body, "HostConfig"); ok {
			if priv, _ := apibody.Field(hc, "Privileged"); truthyPriv(priv) {
				return "AllowWSLContainerPrivileged denies privileged containers on this machine", true
			}
		}
	}
	return "", false
}

// DenyPull judges `docker pull`, and the implicit pull inside `docker run`.
//
// This is the check that makes the allowlist mean what an administrator thinks
// it means. Gating only container creation would stop a blocked image running
// while still fetching it onto the machine, which is not the guarantee WSL
// gives: it applies the allowlist when wslcsession handles the pull, before the
// bytes are requested.
func (g *PolicyGate) DenyPull(image string) (string, bool) {
	p := g.policies()
	server := RegistryServer(image)
	if p.RegistryAllowed(server) {
		return "", false
	}
	return fmt.Sprintf(
		"WSLContainerRegistryAllowlist does not permit pulling from %q (image %q); "+
			"this machine's WSL policy allows: %s",
		server, image, strings.Join(p.RegistryAllowlist, ", ")), true
}

// DenyBuild judges `docker build`.
//
// Refused outright whenever an allowlist is active: a Dockerfile's FROM and any
// RUN can reach any registry, and at the pipe a BuildKit build is an opaque
// gRPC stream, so the traffic cannot be attributed to an allowed registry.
// Allowing builds through would be the easiest possible way around the
// allowlist, so this fails closed.
//
// wslpolicies.h anticipates exactly this, for callers "that cannot attribute
// traffic to a specific registry and must therefore refuse the operation
// whenever any allowlist restriction is active", naming `wslc image build` as
// the example.
//
// The shipped CLI is better than its own header, though, and it is worth being
// straight about the difference: `wslc image build` is NOT refused. It runs,
// and the allowlist is enforced per source inside BuildKit — a blocked base
// image fails with `source "docker-image://docker.io/library/busybox:latest"
// denied by policy`. wslcsession can do that because it drives BuildKit
// directly and attaches a source policy to the solve request. That policy is
// client-side, not daemon configuration: a build sent straight to this
// session's dockerd inherits nothing, which is measured, not assumed.
//
// Matching it would mean parsing and rewriting protobuf inside a hijacked HTTP/2
// stream. Until that exists, Skrog is stricter than WSL here rather than
// looser, and says so in the refusal.
func (g *PolicyGate) DenyBuild() (string, bool) {
	if !g.policies().HasRegistryAllowlist() {
		return "", false
	}
	return "WSLContainerRegistryAllowlist is in force on this machine, and a build " +
		"can pull from any registry, so it cannot be attributed to an allowed one. " +
		"Skrog refuses the build; `wslc image build` instead enforces the allowlist " +
		"per source inside BuildKit, which Skrog cannot do at the pipe", true
}

// RegistryServer extracts the registry host from an image reference, the way
// WSL's RepositoryReference does before checking the allowlist.
//
// The rule is Docker's: the part before the first slash is a registry only if
// it looks like a host — it contains a dot or a colon, or is "localhost".
// Otherwise the reference is a Docker Hub short name ("busybox",
// "library/busybox").
func RegistryServer(image string) string {
	ref := strings.TrimSpace(image)
	if ref == "" {
		return ""
	}
	first, _, found := strings.Cut(ref, "/")
	if !found {
		return DockerHubServer
	}
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first
	}
	return DockerHubServer
}

// DockerHubServer is what an unqualified image reference resolves to. An
// allowlist that does not name it therefore blocks `docker pull busybox`, which
// is the point of deploying one.
const DockerHubServer = "docker.io"

// truthyPriv accepts the shapes a JSON decode can produce for a boolean the
// daemon will read as true. A bare type assertion to bool missed `"Privileged":
// 1`, which dockerd's decoder rejects outright -- but it also missed nothing
// else, so this stays deliberately narrow rather than inventing coercions the
// engine does not perform.
func truthyPriv(v any) bool {
	b, _ := v.(bool)
	return b
}
