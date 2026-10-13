// Package skrogfile is the declarative install format (#69): one YAML file
// describing a whole install — distro, data dir, engine version, idle timeout,
// engine daemon.json keys, lifecycle hooks, WSL integrations, autostart — so a
// fleet or CI runner is provisioned from a checked-in file instead of a pile of
// flags. The schema invents nothing: every field maps to an existing flag or
// config key.
package skrogfile

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/engineconfig"
	"github.com/wslkit/skrog/internal/target"
	"gopkg.in/yaml.v3"
)

// File is the parsed skrog.yaml. Every field is optional; an absent field means
// "leave it at the default / don't touch it," so a partial file is valid and
// `config export` can omit what is unset.
//
// JSON tags mirror the YAML keys, so `profile show --json` and `config export
// --json` emit the same document a skrog.yaml holds (#137).
type File struct {
	// Target names the local engine this workspace uses (#539): `skrog install
	// --config` creates it when missing, converges its settings, and points
	// docker at it. Absent means the default shared engine. It is `target`, not
	// `engine`, because `engine:` below already holds daemon.json settings.
	Target string `yaml:"target,omitempty" json:"target,omitempty"`
	// Distro is the WSL distro name (install --distro).
	Distro string `yaml:"distro,omitempty" json:"distro,omitempty"`
	// DataDir is where the distro's VHDX lives (install --data-dir).
	DataDir string `yaml:"data-dir,omitempty" json:"data-dir,omitempty"`
	// EngineVersion pins the engine (install --engine-version).
	EngineVersion string `yaml:"engine-version,omitempty" json:"engine-version,omitempty"`
	// IdleTimeout is the `idle-timeout` setting (a duration or "off").
	IdleTimeout string `yaml:"idle-timeout,omitempty" json:"idle-timeout,omitempty"`
	// Autostart controls the logon autostart. A pointer so "unset" (leave as-is)
	// is distinct from "false" (disable).
	Autostart *bool `yaml:"autostart,omitempty" json:"autostart,omitempty"`
	// Engine holds daemon.json settings, keyed by the engine.<key> suffix
	// (e.g. registry-mirrors). Values are the CLI string form.
	Engine map[string]string `yaml:"engine,omitempty" json:"engine,omitempty"`
	// Hooks maps lifecycle event -> script path (e.g. post-start).
	Hooks map[string]string `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	// Integrations lists distros to `wsl-integrate`.
	Integrations []string `yaml:"integrations,omitempty" json:"integrations,omitempty"`
}

// Load reads and strictly parses a skrog.yaml from disk.
func Load(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("reading %s: %w", path, err)
	}
	return Parse(b)
}

// Parse strictly decodes a skrog.yaml. Unknown fields fail loudly (a typo'd
// key is a mistake, not something to silently ignore), and the result is
// validated against the known config surface.
func Parse(b []byte) (File, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		// io.EOF means an empty file: an empty install spec is valid (nothing to do).
		if err.Error() == "EOF" {
			return File{}, nil
		}
		return File{}, fmt.Errorf("parsing skrog.yaml: %w", err)
	}
	if err := f.Validate(); err != nil {
		return File{}, err
	}
	return f, nil
}

// Validate checks the sub-map keys the YAML decoder cannot: engine.<key> and
// hooks.<event> must name real settings, so a misspelled key is rejected at load
// rather than silently dropped at apply.
func (f File) Validate() error {
	if f.Target != "" {
		if err := target.ValidName(f.Target); err != nil {
			return fmt.Errorf("target: %w", err)
		}
		if f.Distro != "" || f.DataDir != "" {
			return fmt.Errorf("target: a named engine's distro and data dir are derived from its name; " +
				"remove distro: and data-dir: from a file that sets target:")
		}
	}
	engineKeys := toSet(engineconfig.Keys())
	for k := range f.Engine {
		if !engineKeys[k] {
			return fmt.Errorf("unknown engine key %q (known: %s)", k, strings.Join(engineconfig.Keys(), ", "))
		}
	}
	hookEvents := hookEventSet()
	for k := range f.Hooks {
		if !hookEvents[k] {
			return fmt.Errorf("unknown hook event %q (known: %s)", k, strings.Join(hookEventNames(), ", "))
		}
	}
	return nil
}

// Marshal renders the file back to YAML for `config export`.
func (f File) Marshal() ([]byte, error) {
	return yaml.Marshal(f)
}

// hookEventNames are the lifecycle events usable under `hooks:` — the config
// hook keys with their "hook." prefix stripped.
func hookEventNames() []string {
	keys := config.HookKeys()
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = strings.TrimPrefix(k, "hook.")
	}
	sort.Strings(out)
	return out
}

func hookEventSet() map[string]bool { return toSet(hookEventNames()) }

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
