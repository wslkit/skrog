// Package policy is local, scriptable admission control for the docker API
// (#120): a small set of rules evaluated on each container-affecting call,
// denying the ones a machine's owner has ruled out.
//
// Skrog already sits in the request path — every `docker` call crosses the
// named-pipe bridge, which rewrites bind-mount paths — so the same seam can
// judge a request before it reaches the engine. That is what makes this cheap:
// no elevation, no engine change, no daemon plugin.
//
// # What this is not
//
// Not a security boundary against a hostile local user. They own the machine:
// they can edit the rules file, point DOCKER_HOST elsewhere, or talk to the
// engine directly. It is a guardrail against mistakes and a policy surface for
// shared and CI machines, and claiming more would be dishonest.
//
// Not an OPA/Rego engine either. The rules are a fixed, small vocabulary
// chosen so that a reader can tell at a glance what is forbidden. A rule
// language is a much bigger promise than this needs to make.
//
// # Failure direction
//
// A rule set that cannot be read is an error the caller surfaces, not an
// empty policy: silently allowing everything because a file has a typo is the
// one failure mode a guardrail must not have. An empty or absent file, by
// contrast, means exactly what it says — no rules, allow everything — because
// that is the default state of a machine nobody has configured.
package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wslkit/skrog/internal/apibody"
	"github.com/wslkit/skrog/internal/imageref"
	"github.com/wslkit/skrog/internal/pipeproxy"
)

// FileName is the rule set's name inside the state dir.
const FileName = "policy.yaml"

// Path is where the rule set lives for a given install.
func Path(stateDir string) string { return filepath.Join(stateDir, FileName) }

// Rules is the declarative rule set, in the same kebab-case YAML shape as
// skrog.yaml so the two read alike.
type Rules struct {
	// DenyPrivileged refuses `--privileged`, which disables essentially every
	// container isolation at once.
	DenyPrivileged bool `yaml:"deny-privileged,omitempty" json:"denyPrivileged,omitempty"`

	// DenyAddedCapabilities refuses any `--cap-add`. Capabilities are the
	// piecemeal version of --privileged, so a rule set that denies one and
	// ignores the other is not worth much.
	DenyAddedCapabilities bool `yaml:"deny-added-capabilities,omitempty" json:"denyAddedCapabilities,omitempty"`

	// DenyCapabilities refuses only these, for a rule set that wants
	// SYS_ADMIN gone without banning the harmless ones. Names match however
	// docker spells them; comparison is case-insensitive and ignores a CAP_
	// prefix, since both spellings are common.
	DenyCapabilities []string `yaml:"deny-capabilities,omitempty" json:"denyCapabilities,omitempty"`

	// DenyHostNamespaces refuses --network=host, --pid=host, --ipc=host and
	// --uts=host, each of which reaches straight out of the container.
	DenyHostNamespaces bool `yaml:"deny-host-namespaces,omitempty" json:"denyHostNamespaces,omitempty"`

	// AllowBindSources limits where a bind mount may come from. Empty means
	// no restriction. Paths are Windows-side as the user typed them, matched
	// as path prefixes, because that is what a person writing this rule is
	// thinking about.
	AllowBindSources []string `yaml:"allow-bind-sources,omitempty" json:"allowBindSources,omitempty"`

	// AllowRegistries limits which registries an image may come from. Empty
	// means no restriction. A bare name like "ubuntu" is Docker Hub, so
	// allowing only an internal registry also stops Hub pulls.
	AllowRegistries []string `yaml:"allow-registries,omitempty" json:"allowRegistries,omitempty"`

	// RequireDigest refuses an image reference that is not pinned by digest,
	// which is the only reference that cannot change under you.
	RequireDigest bool `yaml:"require-digest,omitempty" json:"requireDigest,omitempty"`

	// DenyUnattributableBuilds refuses `docker build` while AllowRegistries is
	// in force (#376).
	//
	// Opt-in, and the name says what it does rather than how: a build cannot
	// be attributed to a registry in advance, because a Dockerfile's FROM and
	// any RUN can reach anywhere and at the pipe a BuildKit build is an opaque
	// gRPC stream. So the only two honest positions are refuse, or allow and
	// say so. See DenyBuild for why "allow and say so" is the default here,
	// where an administrator's deployed WSL policy refuses instead.
	//
	// It does nothing on its own: with no AllowRegistries there is no rule for
	// a build to get around, and refusing every build on a machine with no
	// registry restriction would be superstition rather than policy.
	DenyUnattributableBuilds bool `yaml:"deny-unattributable-builds,omitempty" json:"denyUnattributableBuilds,omitempty"`

	// DenyUnattributableImages refuses to run an image whose provenance this
	// machine did not record (#343).
	//
	// The allowlist judges the REFERENCE in a request, and a reference is a
	// mutable label: `docker load` an image, `docker tag` it into an allowed
	// name, and the check passes on a name that was never fetched from
	// anywhere. Provenance judges the image ID instead, which is
	// content-addressed and is what a container create resolves to.
	//
	// Opt-in, for the reason DenyUnattributableBuilds is: every `docker build`
	// produces an image with no provenance, so refusing by default would break
	// every local build and be turned off within a day. Allowing by default
	// closes nothing, which is the state before this existed and is honest
	// about what it is. The strict reading is available to whoever wants it.
	//
	// Like the rest, it needs AllowRegistries to bite. Refusing unattributable
	// images on a machine that does not restrict registries would be closing a
	// hole that is not open.
	//
	// It is not a boundary: see internal/provenance, and #418 route 3.
	DenyUnattributableImages bool `yaml:"deny-unattributable-images,omitempty" json:"denyUnattributableImages,omitempty"`
}

// Empty reports whether the rule set forbids nothing, so callers can skip the
// work and say "no policy" rather than "policy that allows everything".
func (r Rules) Empty() bool {
	return !r.DenyPrivileged && !r.DenyAddedCapabilities && len(r.DenyCapabilities) == 0 &&
		!r.DenyHostNamespaces && len(r.AllowBindSources) == 0 &&
		len(r.AllowRegistries) == 0 && !r.RequireDigest &&
		// Counted even though it only bites alongside allow-registries: a file
		// that sets it is a configured file, and reporting "no policy" for it
		// would be a lie of the kind this package exists to avoid.
		!r.DenyUnattributableBuilds && !r.DenyUnattributableImages
}

// Load reads the rule set for an install. A missing file is an empty rule set,
// not an error: no policy is the default state.
func Load(stateDir string) (Rules, error) {
	b, err := os.ReadFile(Path(stateDir))
	if os.IsNotExist(err) {
		return Rules{}, nil
	}
	if err != nil {
		return Rules{}, fmt.Errorf("policy: reading %s: %w", Path(stateDir), err)
	}
	return Parse(b)
}

// Parse decodes a rule set. Unknown fields are refused rather than ignored: a
// misspelled rule that silently does nothing is the worst outcome a guardrail
// can have, and it would look identical to a rule that is working.
func Parse(b []byte) (Rules, error) {
	var r Rules
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		// An empty document decodes to io.EOF; that is an empty rule set.
		if err.Error() == "EOF" {
			return Rules{}, nil
		}
		return Rules{}, fmt.Errorf("policy: %w", err)
	}
	return r, nil
}

// Decision is the verdict on one request.
type Decision struct {
	// Denied is whether the request must be refused.
	Denied bool
	// Reason is shown to the user verbatim, through the docker CLI, so it
	// names the rule and what tripped it rather than saying "denied".
	Reason string
	// Rule is the rule that denied, for the log.
	Rule string
}

// Allow is the verdict when nothing objects.
var Allow = Decision{}

func deny(rule, format string, args ...any) Decision {
	return Decision{Denied: true, Rule: rule, Reason: fmt.Sprintf(format, args...)}
}

// EvaluateCreate judges a container-create body, decoded as the bridge decodes
// it: a generic map, so that fields this code does not know about are neither
// required nor disturbed.
//
// The first rule to object wins. Reporting only one reason is deliberate — a
// list of everything wrong with a request is harder to act on than the first
// thing to fix, and the user re-runs anyway.
func (r Rules) EvaluateCreate(body map[string]any) Decision {
	if r.Empty() {
		return Allow
	}
	hc, _ := apibody.Map(body, "HostConfig")

	if priv, _ := apibody.Field(hc, "Privileged"); r.DenyPrivileged && truthy(priv) {
		return deny("deny-privileged",
			"policy denies --privileged: it turns off container isolation wholesale")
	}

	capAdd, _ := apibody.Field(hc, "CapAdd")
	if added := stringsOf(capAdd); len(added) > 0 {
		if r.DenyAddedCapabilities {
			return deny("deny-added-capabilities",
				"policy denies added capabilities (--cap-add %s)", strings.Join(added, ", "))
		}
		for _, c := range added {
			for _, bad := range r.DenyCapabilities {
				if normalizeCap(c) == normalizeCap(bad) {
					return deny("deny-capabilities",
						"policy denies the %s capability", strings.ToUpper(normalizeCap(c)))
				}
			}
		}
	}

	if r.DenyHostNamespaces {
		for _, ns := range []struct{ field, flag string }{
			{"NetworkMode", "--network=host"},
			{"PidMode", "--pid=host"},
			{"IpcMode", "--ipc=host"},
			{"UTSMode", "--uts=host"},
		} {
			if s, _ := hc[ns.field].(string); strings.EqualFold(s, "host") {
				return deny("deny-host-namespaces",
					"policy denies %s: it shares the host namespace with the container", ns.flag)
			}
		}
	}

	if len(r.AllowBindSources) > 0 {
		for _, src := range bindSources(hc) {
			if !underAny(src, r.AllowBindSources) {
				return deny("allow-bind-sources",
					"policy does not allow bind mounts from %s (allowed: %s)",
					src, strings.Join(r.AllowBindSources, ", "))
			}
		}
	}

	image := apibody.String(body, "Image")
	if image != "" {
		if len(r.AllowRegistries) > 0 {
			reg, ok := imageref.Registry(image)
			if !ok {
				// Unparseable, so it cannot be attributed to an allowed
				// registry. Refuse rather than guess: dockerd rejects this
				// reference too, and guessing Docker Hub for it would permit it
				// on any allowlist that names Hub.
				return deny("allow-registries",
					"policy cannot attribute %q to a registry (allowed: %s)",
					image, strings.Join(r.AllowRegistries, ", "))
			}
			if !matchesAny(reg, r.AllowRegistries) {
				return deny("allow-registries",
					"policy does not allow images from %s (allowed: %s)",
					reg, strings.Join(r.AllowRegistries, ", "))
			}
		}
		if r.RequireDigest && !strings.Contains(image, "@sha256:") {
			return deny("require-digest",
				"policy requires an image pinned by digest; %s is not (use image@sha256:...)", image)
		}
	}

	return Allow
}

// truthy reads a JSON boolean that may have arrived as a bool or, from a
// hand-written body, as a string.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

// stringsOf reads a JSON array of strings, tolerating a single string.
func stringsOf(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	}
	return nil
}

// normalizeCap makes NET_ADMIN, net_admin and CAP_NET_ADMIN the same name.
func normalizeCap(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "CAP_"))
}

// bindSources collects every host path a create request would mount, from both
// spellings the API accepts: the legacy HostConfig.Binds strings and the
// structured HostConfig.Mounts entries.
func bindSources(hc map[string]any) []string {
	var out []string
	binds, _ := apibody.Field(hc, "Binds")
	for _, b := range stringsOf(binds) {
		// "src:dst[:opts]" — and src may be a Windows path with a drive
		// letter, so the split cannot simply take the first field.
		//
		// A source with no separator is a NAMED VOLUME, not a host path
		// ("myvol:/data"), and has nothing for a bind rule to restrict.
		// Treating it as a path would deny every volume mount on a machine
		// with an allowlist — which is not what the rule says.
		if src := bindSource(b); src != "" && isHostPath(src) {
			out = append(out, src)
		}
	}
	if mounts, ok := apibody.Slice(hc, "Mounts"); ok {
		for _, m := range mounts {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			// npipe counts as a bind here, because the bridge makes it one.
			//
			// Immediately after this gate returns, pipeproxy's rewriter turns
			// Type "npipe" into "bind" and maps the source through ToWSL,
			// where a pipe path resolves to the engine's own socket (#164,
			// deliberate). Skipping it meant the legacy spelling of that mount
			// (HostConfig.Binds) was correctly denied while
			// `--mount type=npipe,...` was never examined at all — the same
			// request, two spellings, opposite verdicts, and the permissive one
			// a first-class docker CLI flag (#256).
			//
			// Only these two types are admitted, so volume and tmpfs mounts —
			// whose Source is a name, not a path — are still not judged as
			// bind sources. Deliberately no isHostPath filter on this branch:
			// under an allowlist a source that is skipped is a source that is
			// allowed, so dropping anything here could only loosen the rule.
			t, _ := mm["Type"].(string)
			if !strings.EqualFold(t, "bind") && !strings.EqualFold(t, "npipe") {
				continue
			}
			if s, _ := mm["Source"].(string); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// bindSource extracts the host side of a "src:dst[:opts]" bind. A Windows
// source carries its own colon (C:\src), so the split cannot simply take the
// first field.
func bindSource(b string) string {
	parts := strings.Split(b, ":")
	switch {
	case len(parts) < 2:
		return ""
	case len(parts[0]) == 1 && len(parts) >= 3:
		// Drive letter: "C:\src:/dst" -> "C:\src".
		return parts[0] + ":" + parts[1]
	default:
		return parts[0]
	}
}

// underAny reports whether a path sits under one of the allowed roots. Compared
// case-insensitively with separators normalized, because these are Windows
// paths written by hand and "C:/src" and `c:\src` mean the same directory.
func underAny(path string, roots []string) bool {
	p := normPath(path)
	for _, root := range roots {
		r := strings.TrimSuffix(normPath(root), "/")
		if r == "" {
			continue
		}
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// normPath lowercases, unifies separators and — the part that matters —
// resolves . and .. before anything is compared.
//
// Without that, a source spelled as an allowed root followed by
// parent-directory segments satisfied the prefix test and then mounted
// somewhere else entirely: winpath.ToWSL passes the remainder through
// verbatim, so the traversal survived into the mount the kernel resolved.
// docs/policy.md describes this rule as a boundary-respecting prefix match,
// which was true only of already-normalised input (#256).
//
// path.Clean rather than filepath.Clean: separators are unified to forward
// slashes first, and this must behave identically wherever the tests run
// rather than following the host's rules.
func normPath(p string) string {
	s := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"))
	if s == "" {
		return ""
	}
	// Keep a drive letter attached to its root: path.Clean("c:/a/../..") would
	// otherwise walk above "c:" and produce "..", which no root can match —
	// failing closed, but confusingly.
	cleaned := path.Clean(s)
	if strings.HasPrefix(cleaned, "..") {
		// Escaped its own root: cannot be under any allowed root.
		return "\x00escaped"
	}
	return cleaned
}

// matchesAny compares a registry against the allowlist, case-insensitively,
// supporting a leading "*." wildcard for a whole domain.
func matchesAny(reg string, allowed []string) bool {
	r := strings.ToLower(strings.TrimSpace(reg))
	// A registry written without a port means that host on any port: the port
	// is a deployment detail, and a rule set that allows
	// registry.example.com but not registry.example.com:5000 would surprise
	// everyone. An entry that DOES name a port is matched exactly.
	host := r
	if h, _, found := strings.Cut(r, ":"); found {
		host = h
	}
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		target := r
		if !strings.Contains(a, ":") {
			target = host
		}
		switch {
		case a == target:
			return true
		case strings.HasPrefix(a, "*."):
			if strings.HasSuffix(target, a[1:]) {
				return true
			}
		}
	}
	return false
}

// DecodeCreateBody parses a container-create body the way the bridge does, so
// `skrog policy test` judges exactly what the bridge would.
func DecodeCreateBody(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("policy: parsing the create body: %w", err)
	}
	return body, nil
}

// DenyCreate adapts Rules to the bridge's structural Gate interface, so
// internal/pipeproxy never has to import this package — the same shape the
// audit sink uses, and for the same reason.
func (r Rules) DenyCreate(body map[string]any) (reason string, denied bool) {
	d := r.EvaluateCreate(body)
	return d.Reason, d.Denied
}

// DenyPull applies the image rules to `docker pull` and to the implicit pull
// inside `docker run` (#334).
//
// Until this existed, allow-registries was evaluated only on container create,
// so a blocked image could not RUN but could still be FETCHED onto the machine
// — the same gap #322 closed for the administrator's WSL policy. require-digest
// had it too: an unpinned image landed in the local store and only the create
// was refused.
//
// Note this is a behaviour change: a `docker pull` that worked yesterday on a
// machine with allow-registries set is refused now.
// That is the rule doing what it says, but it is a change in a shipping
// product, which is why it is documented in docs/policy.md rather than slipped
// in.
func (r Rules) DenyPull(image string) (reason string, denied bool) {
	if image == "" {
		return "", false
	}
	if len(r.AllowRegistries) > 0 {
		reg, ok := imageref.Registry(image)
		if !ok {
			return fmt.Sprintf("policy cannot attribute %q to a registry (allowed: %s)",
				image, strings.Join(r.AllowRegistries, ", ")), true
		}
		if !matchesAny(reg, r.AllowRegistries) {
			return fmt.Sprintf("policy does not allow images from %s (allowed: %s)",
				reg, strings.Join(r.AllowRegistries, ", ")), true
		}
	}
	if r.RequireDigest && !strings.Contains(image, "@sha256:") {
		return fmt.Sprintf("policy requires an image pinned by digest; %s is not (use image@sha256:...)", image), true
	}
	return "", false
}

// DenyBuild implements pipeproxy.ImageGate. policy.yaml does NOT refuse builds,
// and that is a decision rather than an omission.
//
// The case for refusing is real: a Dockerfile's FROM and any RUN can reach any
// registry, so a build cannot be attributed in advance, and allow-registries is
// therefore bypassable by anyone who writes a Dockerfile. An administrator's
// deployed WSL policy does refuse, per wslpolicies.h.
//
// It is not followed here because the two rule sets answer to different people.
// The WSL policy is an ADMINISTRATOR's, deployed by GPO against a user who
// cannot edit it, so failing closed is the only coherent choice. policy.yaml is
// the machine owner's own file; refusing every build on a machine that merely
// lists its registries would break working setups today to close a hole its own
// author can walk around by editing one line.
//
// So it stays open and documented rather than closed and surprising — unless
// the rule set asks for the strict reading with deny-unattributable-builds
// (#376), which is the same verdict the administrator's policy reaches by
// default, reached here only because someone chose it.
//
// The rule needs an allowlist to bite. Refusing builds on a machine with no
// registry restriction would close a hole that is not open.
func (r Rules) DenyBuild() (reason string, denied bool) {
	if !r.DenyUnattributableBuilds || len(r.AllowRegistries) == 0 {
		return "", false
	}
	return fmt.Sprintf(
		"a build cannot be attributed to a registry (its FROM and any RUN may reach "+
			"anywhere), and deny-unattributable-builds is set with allow-registries "+
			"active: %s. Pull the image you need instead, or unset "+
			"deny-unattributable-builds to allow builds through",
		strings.Join(r.AllowRegistries, ", ")), true
}

// DenyPush applies the registry allowlist to `docker push`, matching what the
// WSL gate does (#353): an allowlist that governs only inbound says nothing
// about what leaves the machine.
//
// require-digest deliberately does NOT apply here. It exists to stop unpinned
// images being CONSUMED; a push is publishing something built locally, and
// demanding a digest for it would refuse every ordinary `docker push app:v1`.
func (r Rules) DenyPush(image string) (reason string, denied bool) {
	if image == "" || len(r.AllowRegistries) == 0 {
		return "", false
	}
	reg, ok := imageref.Registry(image)
	if !ok {
		return fmt.Sprintf("policy cannot attribute %q to a registry (allowed: %s)",
			image, strings.Join(r.AllowRegistries, ", ")), true
	}
	if !matchesAny(reg, r.AllowRegistries) {
		return fmt.Sprintf("policy does not allow pushing to %s (allowed: %s)",
			reg, strings.Join(r.AllowRegistries, ", ")), true
	}
	return "", false
}

// isHostPath distinguishes a bind source from a named volume in the legacy
// "src:dst" form. Docker's rule: a source with no separator is a volume name.
// A Windows drive letter counts as a path even though it carries a colon.
func isHostPath(s string) bool {
	if strings.ContainsAny(s, `/\`) {
		return true
	}
	// "C:" on its own, i.e. a bare drive root.
	return len(s) == 2 && s[1] == ':'
}

// Watcher is the Gate the supervisor installs: it re-reads the rules file when
// it changes, so editing policy.yaml takes effect without restarting anything.
//
// The first cut read the file once at supervisor start, and documented that a
// change needed `skrog restart`. That was wrong twice over. `restart` bounces
// the ENGINE; the supervisor process — which holds the rules — keeps running,
// so the advice did not work even when followed. And a policy file created
// after the supervisor started installed no gate at all, silently. Both were
// caught by running a real `docker run --privileged` against a machine that
// had just been told to deny it, and watching it succeed.
//
// So: stat on each judged request, re-read when it changed. A container create
// is a human-scale event and a stat is microseconds, which buys the behaviour
// `skrog config` already promises — settings apply live.
type Watcher struct {
	stateDir string

	mu      sync.Mutex
	rules   Rules
	modTime time.Time
	size    int64
	loaded  bool
	// machine mirrors the fields above for the machine-wide layer (#386). It
	// is watched the same way and on the same schedule: a fleet policy that
	// only took effect after a logon would be a fleet policy nobody trusted.
	machineRules   Rules
	machineModTime time.Time
	machineSize    int64
	machineLoaded  bool
	// machineUnknown mirrors `unknown` for the machine layer: a file exists
	// and has never parsed, so what the administrator asked for is not known.
	// Separate lastErr too -- sharing one field let two layers erroring
	// alternately defeat the once-only OnError dedupe, and leaked a machine
	// error into the user-file message.
	machineUnknown bool
	machineLastErr string
	// unknown is set when a rule file exists but has never parsed. The rules
	// are then neither "empty" nor known, and requests are refused.
	unknown bool
	// lastErr is remembered so a file that breaks mid-edit is reported once
	// rather than on every request.
	lastErr string

	// OnError reports a rule file that stopped parsing. Optional.
	OnError func(err error)
}

// NewWatcher returns a Gate over the install's rule file. It reads nothing
// yet: the first judged request loads it, so constructing one is free even on
// a machine with no policy.
func NewWatcher(stateDir string) *Watcher { return &Watcher{stateDir: stateDir} }

// Rules returns the current EFFECTIVE rule set — the machine layer tightened
// by the user's (#386) — re-reading either file if it changed.
func (w *Watcher) Rules() Rules {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refreshLocked()
	w.refreshMachineLocked()
	return Merge(w.machineRules, w.rules)
}

// refreshMachineLocked re-reads the machine layer when its stamp has moved.
//
// A machine file that stops parsing keeps the rules that were working, exactly
// as the user layer does, and for a sharper reason: dropping a fleet policy
// because somebody deployed a typo is how a managed estate silently stops
// being managed.
func (w *Watcher) refreshMachineLocked() {
	path := MachinePath()
	if path == "" {
		return
	}
	fi, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		w.machineRules = Rules{}
		w.machineLoaded, w.machineModTime, w.machineSize = true, time.Time{}, 0
		return
	case err != nil:
		return
	}
	if w.machineLoaded && fi.ModTime().Equal(w.machineModTime) && fi.Size() == w.machineSize {
		return
	}
	rules, err := LoadMachine()
	if err != nil {
		// FAIL CLOSED, exactly as the user layer does (#254) — and this half
		// did not, which was worse. A machine file exists, so an administrator
		// deployed something; if it has never parsed we do not know what, and
		// judging requests as though no fleet policy existed is the one
		// outcome the layer was built to prevent.
		//
		// `Parse` uses KnownFields(true), so a misspelled rule is a hard error
		// rather than an ignored key: one typo in an Intune deployment used to
		// mean every machine that received it ran unenforced, with a single
		// log line, while `skrog policy show` reported the file as broken.
		// The two disagreeing in that direction is the worst possible pair of
		// answers.
		if !w.machineLoaded {
			w.machineUnknown = true
		}
		if w.OnError != nil && err.Error() != w.machineLastErr {
			w.OnError(err)
		}
		w.machineLastErr = err.Error()
		w.machineModTime, w.machineSize = fi.ModTime(), fi.Size()
		return
	}
	w.machineRules, w.machineLoaded, w.machineUnknown = rules, true, false
	w.machineLastErr = ""
	w.machineModTime, w.machineSize = fi.ModTime(), fi.Size()
}

// refreshLocked re-reads the file when its mtime or size has moved.
func (w *Watcher) refreshLocked() {
	fi, err := os.Stat(Path(w.stateDir))
	switch {
	case os.IsNotExist(err):
		// Deleting the file removes the rules, which is the obvious meaning
		// and the only way to turn policy off without editing YAML.
		w.rules, w.loaded, w.modTime, w.size = Rules{}, true, time.Time{}, 0
		return
	case err != nil:
		return // unreadable right now; keep what we have
	}
	if w.loaded && fi.ModTime().Equal(w.modTime) && fi.Size() == w.size {
		return
	}

	rules, err := Load(w.stateDir)
	if err != nil {
		// Keep the rules that were working. A typo saved mid-edit must not
		// silently drop the guardrail — that is the failure direction this
		// package exists to avoid.
		//
		// Unless there are none to keep. On the FIRST load there is no
		// previous rule set, so "keep what we have" kept the zero Rules{} —
		// which Empty() reports as "no policy" and EvaluateCreate allows
		// everything through. That is the exact failure this package's doc
		// comment names as the one a guardrail must not have, and it survives
		// a reboot: save a typo, restart, and admission control is off while
		// the file on disk still looks enforced (#254).
		//
		// So when nothing has ever parsed, the rules are not empty — they are
		// unknown, and DenyCreate refuses rather than guessing.
		if !w.loaded {
			w.unknown = true
		}
		if w.OnError != nil && err.Error() != w.lastErr {
			w.OnError(err)
		}
		w.lastErr = err.Error()
		// Record the stamp anyway so a broken file is not re-read and
		// re-reported on every single request.
		w.modTime, w.size = fi.ModTime(), fi.Size()
		return
	}
	w.rules, w.loaded, w.unknown = rules, true, false
	w.modTime, w.size = fi.ModTime(), fi.Size()
	w.lastErr = ""
}

// Unavailable reports that a rule file exists but has never been read
// successfully, so what the operator asked for is unknown.
//
// Distinct from "no policy": an absent file means "no rules, allow
// everything", which is the default state of a machine nobody has configured.
// A file that is present and unreadable means the opposite — someone
// configured something, and we cannot tell what.
func (w *Watcher) Unavailable() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refreshLocked()
	w.refreshMachineLocked()

	// The machine layer first: it is an administrator's rule set, so "we
	// cannot read what the administrator deployed" is the more serious of the
	// two and should be the message the user sees.
	if w.machineUnknown {
		return fmt.Errorf("the machine-wide policy at %s could not be read: %s",
			MachinePath(), w.machineLastErr)
	}
	if !w.unknown {
		return nil
	}
	return fmt.Errorf("%s could not be read: %s", Path(w.stateDir), w.lastErr)
}

// DenyCreate implements the bridge's Gate.
//
// A rule file that has never parsed refuses here rather than falling through
// to an empty rule set: the operator configured something, and allowing
// everything because we cannot read it is the one failure direction this
// package exists to avoid (#254).
func (w *Watcher) DenyCreate(body map[string]any) (string, bool) {
	if err := w.Unavailable(); err != nil {
		return "policy is configured but its rule file cannot be read, so this request is refused " +
			"rather than allowed unjudged (" + err.Error() + "). Fix the file, or remove it to run without rules.", true
	}
	return w.Rules().DenyCreate(body)
}

// DenyPull, DenyBuild and DenyPush make the Watcher a pipeproxy.ImageGate, so
// the image rules reach pulls and pushes on BOTH backends (#334).
//
// Each carries the same unreadable-file refusal as DenyCreate: a rule file the
// operator wrote and we cannot parse must not silently allow the very requests
// it was written to judge (#254).
func (w *Watcher) DenyPull(image string) (string, bool) {
	if err := w.Unavailable(); err != nil {
		return unreadableRules(err), true
	}
	return w.Rules().DenyPull(image)
}

func (w *Watcher) DenyBuild() (string, bool) {
	// This returned a hardcoded ("", false) until #376 gave Rules.DenyBuild
	// something to say, and the comment explaining the hardcode outlived the
	// reason for it -- so the rule shipped, was documented, reported active by
	// `policy show`, and did nothing. Consult the rules.
	//
	// The unreadable-file refusal still does NOT apply here, and that part was
	// always deliberate: builds are allowed by default, so refusing them
	// because a file will not parse would impose the strict reading on people
	// who never asked for it. An operator who wants builds refused sets
	// deny-unattributable-builds, and then a broken file leaves the previous
	// rules in force like every other rule.
	return w.Rules().DenyBuild()
}

func (w *Watcher) DenyPush(image string) (string, bool) {
	if err := w.Unavailable(); err != nil {
		return unreadableRules(err), true
	}
	return w.Rules().DenyPush(image)
}

// DenyUnattributableImage makes the Watcher a pipeproxy.ProvenanceGate (#343).
//
// Without this method the whole feature is a no-op in the product: the bridge
// finds its gate through a type assertion, and the Watcher -- not Rules -- is
// what the supervisor installs. That is exactly how deny-unattributable-builds
// shipped documented, reported active, and doing nothing; see DenyBuild.
//
// No unreadable-file refusal here, for the same reason DenyBuild has none, and
// because it would be redundant anyway: DenyCreate judges the same request
// first and already refuses when the rule file cannot be read.
func (w *Watcher) DenyUnattributableImage(ref, id string, known bool) (string, bool) {
	return w.Rules().DenyUnattributableImage(ref, id, known)
}

func unreadableRules(err error) string {
	return "policy is configured but its rule file cannot be read, so this request is refused " +
		"rather than allowed unjudged (" + err.Error() + "). Fix the file, or remove it to run without rules."
}

// Without these the Watcher could quietly stop being an ImageGate and every
// pull and push would pass unjudged, which is indistinguishable from a rule set
// that allows everything (#353).
var (
	_ pipeproxy.Gate      = (*Watcher)(nil)
	_ pipeproxy.ImageGate = (*Watcher)(nil)
	_ pipeproxy.Gate      = Rules{}
	_ pipeproxy.ImageGate = Rules{}

	// Same hazard, one release later (#343): the provenance check reaches the
	// gate through its own type assertion, so dropping this method would turn
	// deny-unattributable-images off without a single test failing.
	_ pipeproxy.ProvenanceGate = (*Watcher)(nil)
	_ pipeproxy.ProvenanceGate = Rules{}
)

// DenyMirror judges a registry MIRROR host against the allowlist (#421).
//
// A mirror is not an image reference, which is why it needed its own door.
// `allow-registries` judges the reference in a request, and dockerd applies
// `registry-mirrors` to Docker Hub pulls without changing the reference at
// all -- so on the common allowlist of docker.io plus a corporate registry,
// every unpinned `docker pull ubuntu` fetched its bytes from whatever host the
// mirror named, and the reference check passed because the reference was
// untouched.
//
// Three places in this codebase asserted that could not happen, on the ground
// that a mirror "changes where bytes come from, not which image was asked
// for". Both halves of that sentence are true and the conclusion does not
// follow: where the bytes come from is the thing an allowlist exists to
// constrain.
//
// Empty allowlist means no opinion, as everywhere else: a rule set that does
// not restrict registries has no view on mirrors either.
func (r Rules) DenyMirror(host string) (reason string, denied bool) {
	host = strings.TrimSpace(host)
	if host == "" || len(r.AllowRegistries) == 0 {
		return "", false
	}
	if !matchesAny(host, r.AllowRegistries) {
		return fmt.Sprintf(
			"policy does not allow %s as a registry, so it may not serve as a mirror either "+
				"(allowed: %s)", host, strings.Join(r.AllowRegistries, ", ")), true
	}
	return "", false
}

// DenyUnattributableImage judges a container create against what this machine
// recorded about where the image came from (#343).
//
// It takes the lookup RESULT rather than doing the lookup, so this package
// stays pure and testable: the caller owns the state dir and the engine query
// that resolves a reference to an ID.
//
// An empty id means the reference could not be resolved at all -- the engine
// did not answer, or the image is not present locally yet. That is NOT treated
// as unattributable. A create for an image docker is about to pull implicitly
// is judged by DenyPull on the pull itself, and refusing here as well would
// refuse the ordinary `docker run` of an image nobody has yet, which is not
// what this rule is for.
func (r Rules) DenyUnattributableImage(ref, id string, known bool) (reason string, denied bool) {
	if !r.DenyUnattributableImages || len(r.AllowRegistries) == 0 {
		return "", false
	}
	if id == "" || known {
		return "", false
	}
	return fmt.Sprintf(
		"policy refuses %s: this machine has no record of where it came from. "+
			"An image that was built locally, loaded from a tarball or imported "+
			"outside Skrog carries no provenance, and deny-unattributable-images "+
			"is set. Pull it from an allowed registry (%s), or clear the rule",
		displayRef(ref, id), strings.Join(r.AllowRegistries, ", ")), true
}

// displayRef names the image in a refusal the way the user typed it, falling
// back to the ID when a create carried no readable reference.
func displayRef(ref, id string) string {
	if strings.TrimSpace(ref) != "" {
		return ref
	}
	if len(id) > 19 {
		return id[:19] + "..."
	}
	return id
}
