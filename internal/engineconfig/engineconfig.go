// Package engineconfig is a safe, curated surface over the engine's
// daemon.json (#68). Docker Desktop offers a raw JSON textbox that can brick
// the daemon; Skrog instead exposes a validated allowlist of keys, refuses
// unknown ones loudly (the same philosophy as the `idle-timeout` config), and
// runs `dockerd --validate` on every candidate before it replaces the live
// file — the trick that already guards the rootfs pipeline.
package engineconfig

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Prefix marks an engine (daemon.json) key on the `skrog config` surface, e.g.
// "engine.registry-mirrors". It keeps engine settings namespaced away from
// Skrog's own settings so one command can route both.
const Prefix = "engine."

// Kind is how a key's raw CLI value is parsed and marshaled into daemon.json.
type Kind int

const (
	// KindStringList is a comma-separated list -> JSON array of strings.
	KindStringList Kind = iota
	// KindString is a single string.
	KindString
	// KindStringMap is comma-separated k=v pairs -> JSON object of strings.
	KindStringMap
	// KindInt is an integer.
	KindInt
	// KindBool is true/false.
	KindBool
)

// Key is one allowlisted daemon.json setting. Name is both the CLI suffix and
// the daemon.json field, so there is nothing to translate.
type Key struct {
	Name string
	Kind Kind
	Help string
}

// keys is the curated allowlist. Deliberately small: each entry is a setting
// with a clear, safe use and a stable daemon.json shape. Growth is a code
// change, not user-supplied JSON.
var keys = []Key{
	{"registry-mirrors", KindStringList, "pull-through mirror URLs, tried before Docker Hub"},
	{"insecure-registries", KindStringList, "registries reachable over HTTP or with an untrusted cert (host[:port] or CIDR); loopback stays insecure too"},
	{"dns", KindStringList, "DNS servers for containers"},
	{"dns-search", KindStringList, "DNS search domains for containers"},
	{"log-driver", KindString, "default container logging driver, e.g. json-file or local"},
	{"log-opts", KindStringMap, "logging driver options, e.g. max-size=10m,max-file=3"},
	{"max-concurrent-downloads", KindInt, "parallel layer pulls per image"},
	{"max-concurrent-uploads", KindInt, "parallel layer pushes per image"},
	{"mtu", KindInt, "MTU for the default bridge network -- lower it under a VPN that clamps the tunnel MTU, or pulls hang mid-layer (#63)"},
	{"userland-proxy", KindBool, "relay published ports through docker-proxy instead of iptables NAT (Skrog defaults this to false: NAT is what makes -p ports reachable from Windows under mirrored networking)"},
}

// Defaults are the daemon.json keys Skrog has an opinion about on an engine
// that does not (the key is absent). They are written before dockerd launches,
// so an install that predates a default still gets it, and a key the user set
// explicitly -- `skrog config set engine.<key>` -- is never overridden.
//
// userland-proxy=false: with the proxy on, a connection from Windows to a
// published port is DNAT'd to the container and, under mirrored networking,
// arrives with a 127.0.0.1 source -- so the container answers into its own
// loopback and the connection hangs (#163). With the proxy off, dockerd installs
// the LOCAL-source MASQUERADE that makes that return path work. Mirrored
// networking is what `skrog doctor` itself recommends for VPNs, so this is the
// configuration Skrog has to be correct in.
var Defaults = map[string]string{
	"userland-proxy": "false",
}

// LoopbackRegistries are the addresses dockerd treated as insecure on its own,
// whatever insecure-registries said, until engine 29.10.0. From 29.10.0 it adds
// them only when insecure-registries is absent (moby/moby#53868), so setting
// the key for one corporate registry silently made every plain-HTTP registry
// on loopback secure -- including `skrog cache`, which dockerd reaches at
// http://127.0.0.1:5000. Skrog keeps them in any list it writes, so the key
// means what it meant before the engine upgrade: "these, as well".
var LoopbackRegistries = []string{"127.0.0.0/8", "::1/128"}

// keepLoopbackInsecure appends LoopbackRegistries to a non-empty
// insecure-registries list that lacks them, and reports whether cfg changed.
// An absent or empty key is left alone: dockerd applies the loopback default
// itself then, and writing the list would turn that default into a choice.
// The value is []string when it came from parseValue and []any when it was
// decoded from daemon.json; both are handled.
func keepLoopbackInsecure(cfg map[string]any) bool {
	const name = "insecure-registries"
	var list []string
	switch v := cfg[name].(type) {
	case []string:
		list = append(list, v...)
	case []any:
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return false // not a shape Skrog wrote; leave it for dockerd to judge
			}
			list = append(list, s)
		}
	default:
		return false
	}
	if len(list) == 0 {
		return false
	}
	changed := false
	for _, lb := range LoopbackRegistries {
		if !containsString(list, lb) {
			list = append(list, lb)
			changed = true
		}
	}
	if changed {
		cfg[name] = list
	}
	return changed
}

func containsString(list []string, s string) bool {
	for _, e := range list {
		if strings.TrimSpace(e) == s {
			return true
		}
	}
	return false
}

func keyByName(name string) (Key, bool) {
	for _, k := range keys {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

// Keys returns the allowlisted key names, sorted, for help and listing.
func Keys() []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.Name
	}
	sort.Strings(out)
	return out
}

// KeyHelp returns the allowlist with help, sorted, for command help text.
func KeyHelp() []Key {
	out := append([]Key(nil), keys...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsEngineKey reports whether a full config key targets the engine surface.
func IsEngineKey(full string) bool { return strings.HasPrefix(full, Prefix) }

// StripPrefix returns the daemon.json field name for a full engine key.
func StripPrefix(full string) string { return strings.TrimPrefix(full, Prefix) }

// parseValue turns a raw CLI value into the Go value marshaled into daemon.json.
// An empty raw value returns (nil, true): the caller clears the key.
func parseValue(k Key, raw string) (value any, clear bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true, nil
	}
	switch k.Kind {
	case KindStringList:
		list := splitList(raw)
		if len(list) == 0 {
			return nil, true, nil
		}
		return list, false, nil
	case KindString:
		return raw, false, nil
	case KindStringMap:
		m := map[string]string{}
		for _, pair := range splitList(raw) {
			eq := strings.IndexByte(pair, '=')
			if eq < 1 {
				return nil, false, fmt.Errorf("%q is not k=v", pair)
			}
			m[strings.TrimSpace(pair[:eq])] = strings.TrimSpace(pair[eq+1:])
		}
		if len(m) == 0 {
			return nil, true, nil
		}
		return m, false, nil
	case KindInt:
		n, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return nil, false, fmt.Errorf("%q is not an integer", raw)
		}
		if n < 0 {
			return nil, false, fmt.Errorf("%q is negative", raw)
		}
		return n, false, nil
	case KindBool:
		b, convErr := strconv.ParseBool(raw)
		if convErr != nil {
			return nil, false, fmt.Errorf("%q is not true or false", raw)
		}
		return b, false, nil
	default:
		return nil, false, fmt.Errorf("unhandled kind for %s", k.Name)
	}
}

func splitList(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// renderValue turns a daemon.json value (as decoded from JSON, so lists are
// []any and maps are map[string]any) back into the CLI string form, so `get`
// round-trips what `set` accepts.
func renderValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64: // JSON numbers decode as float64
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int:
		return strconv.Itoa(t)
	case []string:
		return strings.Join(t, ",")
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = renderValue(e)
		}
		return strings.Join(parts, ",")
	case map[string]string:
		return renderMap(mapAny(t))
	case map[string]any:
		return renderMap(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func mapAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func renderMap(m map[string]any) string {
	kv := make([]string, 0, len(m))
	for k := range m {
		kv = append(kv, k)
	}
	sort.Strings(kv)
	for i, k := range kv {
		kv[i] = k + "=" + renderValue(m[k])
	}
	return strings.Join(kv, ",")
}
