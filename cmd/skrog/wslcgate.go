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

// DenyPull and DenyPush consult both policies, in the same order and for the
// same reason as DenyCreate: when both would refuse, the message should name
// the one the user cannot simply edit (#334).
//
// The skrog gate is only asked if it is an ImageGate. It is — policy.Watcher
// implements it — but the field is typed as the narrower pipeproxy.Gate, so the
// assertion is what bridges that, exactly as the handler does.
func (g combinedGate) DenyPull(image string) (string, bool) {
	if reason, denied := g.wsl.DenyPull(image); denied {
		return reason, true
	}
	if ig, ok := g.skrog.(pipeproxy.ImageGate); ok && g.skrog != nil {
		return ig.DenyPull(image)
	}
	return "", false
}

func (g combinedGate) DenyPush(image string) (string, bool) {
	if reason, denied := g.wsl.DenyPush(image); denied {
		return reason, true
	}
	if ig, ok := g.skrog.(pipeproxy.ImageGate); ok && g.skrog != nil {
		return ig.DenyPush(image)
	}
	return "", false
}

// DenyBuild consults only the WSL policy, because policy.yaml deliberately does
// not refuse builds. See policy.Rules.DenyBuild for that decision, and #376 for
// making it opt-in.
func (g combinedGate) DenyBuild() (string, bool) { return g.wsl.DenyBuild() }

// Without these, dropping a method here would not fail the build — combinedGate
// would quietly stop satisfying ImageGate and every pull, build and push on
// this backend would pass unjudged.
var (
	_ pipeproxy.Gate      = combinedGate{}
	_ pipeproxy.ImageGate = combinedGate{}
)
