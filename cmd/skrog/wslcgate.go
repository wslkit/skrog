package main

import (
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/wslc"
)

// combinedGate applies both admission policies on the wslc backend: the
// machine's deployed WSL container policy, and Skrog's own policy.yaml.
//
// They answer different questions and neither substitutes for the other. The
// WSL policy is the administrator's, deployed by GPO or Intune, and Skrog
// stands in for it because a direct docker.sock relay bypasses the enforcement
// in wslcsession (#322). policy.yaml is the machine owner's own rule set, the
// same one the distro backend applies (#120).
//
// Either may refuse. The WSL policy is consulted first so that when both would
// deny, the message names the one the user cannot simply edit.
type combinedGate struct {
	wsl   *wslc.PolicyGate
	skrog pipeproxy.Gate // policy.Watcher; nil when no policy.yaml is in use
}

func (g combinedGate) DenyCreate(body map[string]any) (string, bool) {
	if reason, denied := g.wsl.DenyCreate(body); denied {
		return reason, true
	}
	if g.skrog != nil {
		return g.skrog.DenyCreate(body)
	}
	return "", false
}

// DenyPull, DenyBuild and DenyPush currently consult only the WSL policy.
//
// Skrog's own policy.yaml has an allow-registries rule with the same shape of
// gap this closed for the WSL policy: it is evaluated on container create, so
// it stops a blocked image running without stopping it being fetched. Closing
// that is #334, and it applies equally to the distro backend — which is why it
// is not bolted on here.
func (g combinedGate) DenyPull(image string) (string, bool) { return g.wsl.DenyPull(image) }

func (g combinedGate) DenyBuild() (string, bool) { return g.wsl.DenyBuild() }

func (g combinedGate) DenyPush(image string) (string, bool) { return g.wsl.DenyPush(image) }

// Without these, dropping a method here would not fail the build — combinedGate
// would quietly stop satisfying ImageGate and every pull, build and push on
// this backend would pass unjudged.
var (
	_ pipeproxy.Gate      = combinedGate{}
	_ pipeproxy.ImageGate = combinedGate{}
)
