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
}

// Empty reports whether the rule set forbids nothing, so callers can skip the
// work and say "no policy" rather than "policy that allows everything".
func (r Rules) Empty() bool {
	return !r.DenyPrivileged && !r.DenyAddedCapabilities && len(r.DenyCapabilities) == 0 &&
		!r.DenyHostNamespaces && len(r.AllowBindSources) == 0 &&
		len(r.AllowRegistries) == 0 && !r.RequireDigest
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
			reg := imageref.Registry(image)
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

// Rules returns the current rule set, re-reading if the file changed.
func (w *Watcher) Rules() Rules {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refreshLocked()
	return w.rules
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
