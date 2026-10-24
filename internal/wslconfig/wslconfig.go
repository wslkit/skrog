// Package wslconfig edits the sizing keys in ~/.wslconfig with consent (#148).
//
// A runner VM with 8 GB should not let the engine take four of them, and WSL2's
// sizing lives in ~/.wslconfig -- which is **global**: every WSL2 distro on the
// machine shares it, Docker Desktop's and Rancher's included. So nothing here
// writes without being asked to, and `skrog wsl-config apply` shows the exact
// diff first. It is the same consent shape doctor already uses for the VPN
// remedies (#63), for the same reason.
//
// Three properties matter and are what the tests pin:
//
//   - Nothing else in the file moves. Unknown sections, unknown keys, comments,
//     blank lines and even key order survive a write, because a file this
//     global belongs to the user and not to us.
//   - Applying twice changes nothing (the headless `--yes` path has to be
//     idempotent, or a converge run rewrites a file every time).
//   - A restart is REPORTED, never performed. Sizing takes effect when the WSL
//     VM next starts, and the only way to force that is `wsl --shutdown`, which
//     stops every distro on the machine -- someone else's containers included.
//     Skrog stops its own distro and nothing else, ever.
package wslconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Section is where a managed key lives unless SectionOf says otherwise.
const Section = "wsl2"

// SectionExperimental holds the keys WSL has not promoted to [wsl2].
const SectionExperimental = "experimental"

// SectionOf is the section WSL reads a managed key from.
//
// autoMemoryReclaim is `experimental.autoMemoryReclaim` in WSL's own config
// table, with no `wsl2.` alias (networkingMode has both). Written under [wsl2]
// it is an "Unknown key" WSL ignores, which is what every Skrog release before
// #550 did.
func SectionOf(key string) string {
	if strings.EqualFold(key, KeyAutoMemoryReclaim) {
		return SectionExperimental
	}
	return Section
}

// staleSection is where an earlier Skrog wrote a key WSL does not read there,
// or "" when the key was always written to the right place. Plan moves such a
// line rather than leaving a dead one behind.
func staleSection(key string) string {
	if strings.EqualFold(key, KeyAutoMemoryReclaim) {
		return Section
	}
	return ""
}

// Keys are the settings Skrog manages, in the order a diff shows them.
const (
	KeyMemory            = "memory"
	KeyProcessors        = "processors"
	KeySwap              = "swap"
	KeyAutoMemoryReclaim = "autoMemoryReclaim"
	// KeyVirtiofs switches how WSL2 mounts Windows drives into every distro:
	// virtiofs instead of 9p for /mnt/c (#327). Present from WSL 2.9.x.
	//
	// It belongs here rather than in doctor's advice, next to the networking
	// keys, because it is the same KIND of setting as memory and processors:
	// machine-wide, deliberate, and about how the VM performs rather than how
	// it reaches the network. Measured on 2.9.11, a Windows folder
	// bind-mounted into a container reads about 4x faster and lists a
	// directory about 3x faster, with writes a modest and noisy gain.
	KeyVirtiofs = "virtiofs"
)

// Managed lists them in display order. Deliberately short: these are the knobs
// a runner owner sets deliberately. Networking keys (networkingMode,
// dnsTunneling, autoProxy) stay doctor's advice rather than something an
// install writes, because they change how every distro on the machine reaches
// the network.
func Managed() []string {
	return []string{KeyMemory, KeyProcessors, KeySwap, KeyAutoMemoryReclaim, KeyVirtiofs}
}

// Path is ~/.wslconfig.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the home directory: %w", err)
	}
	return filepath.Join(home, ".wslconfig"), nil
}

// File is a parsed ~/.wslconfig that remembers its own layout.
type File struct {
	// lines is the file verbatim, so a write can put back everything it did
	// not deliberately change.
	lines []string
	// path is where it came from; empty for a file that does not exist yet.
	path string
	// existed records whether there was a file at all, which changes the
	// wording of a diff ("create" versus "edit").
	existed bool
}

// Load reads ~/.wslconfig. A missing file is an empty one, not an error: a
// machine that has never been tuned is the common case.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &File{path: path}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	f := &File{path: path, existed: true}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	f.lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	return f, nil
}

// Existed reports whether the file was there before.
func (f *File) Existed() bool { return f.existed }

// Get returns a key's current value in the section WSL reads it from, and
// whether it was set there. A copy in any other section is not reported: WSL
// ignores it, so it is not the effective value.
func (f *File) Get(key string) (string, bool) {
	i := f.find(key)
	if i < 0 {
		return "", false
	}
	_, v := splitKV(f.lines[i])
	return v, true
}

// All returns every managed key currently set, for reporting the effective
// configuration.
func (f *File) All() map[string]string {
	out := map[string]string{}
	for _, k := range Managed() {
		if v, ok := f.Get(k); ok {
			out[k] = v
		}
	}
	return out
}

// find locates a key inside the section WSL reads it from, or -1.
func (f *File) find(key string) int {
	return f.findIn(SectionOf(key), key)
}

// findIn locates a key inside the named section, or -1. Comparison is
// case-insensitive on both the section and the key, because WSL is.
func (f *File) findIn(section, key string) int {
	inSection := false
	for i, line := range f.lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			inSection = strings.EqualFold(strings.Trim(t, "[]"), section)
			continue
		}
		if !inSection || t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		if k, _ := splitKV(line); strings.EqualFold(k, key) {
			return i
		}
	}
	return -1
}

func splitKV(line string) (key, value string) {
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return strings.TrimSpace(line), ""
	}
	return strings.TrimSpace(line[:eq]), strings.TrimSpace(line[eq+1:])
}

// Change is one line a write would alter.
type Change struct {
	Key string
	// Old is the current value; empty with Added means the key was absent.
	Old string
	New string
	// Added is true when the key is not in the file yet.
	Added bool
	// StaleSection names the section a dead copy of the key is removed from
	// (an earlier Skrog wrote it where WSL does not read it); empty otherwise.
	StaleSection string
	// Stale is the value on that dead line.
	Stale string
}

// String renders a change as a diff line, which is what consent is given to.
// A key outside [wsl2] names its section, and a moved line says why, because
// "- x / + x" with the same value would otherwise read as a no-op.
func (c Change) String() string {
	where := ""
	if s := SectionOf(c.Key); s != Section {
		where = "  (under [" + s + "])"
	}
	var b strings.Builder
	if c.StaleSection != "" {
		fmt.Fprintf(&b, "- %s=%s  (under [%s], where WSL ignores it)\n", c.Key, c.Stale, c.StaleSection)
	}
	if c.Added {
		fmt.Fprintf(&b, "+ %s=%s%s", c.Key, c.New, where)
		return b.String()
	}
	if strings.EqualFold(c.Old, c.New) {
		// Only the dead copy goes; the live line is already right.
		return strings.TrimSuffix(b.String(), "\n")
	}
	fmt.Fprintf(&b, "- %s=%s%s\n+ %s=%s%s", c.Key, c.Old, where, c.Key, c.New, where)
	return b.String()
}

// Plan returns the changes needed to make the managed keys in the file match
// desired, and nothing else. A key whose value already matches produces no
// change, which is what makes `apply --yes` idempotent.
func (f *File) Plan(desired map[string]string) []Change {
	var out []Change
	for _, k := range Managed() {
		want, ok := desired[k]
		if !ok || strings.TrimSpace(want) == "" {
			continue // unset in Skrog's config: not ours to touch
		}
		cur, present := f.Get(k)
		c := Change{Key: k, Old: cur, New: want, Added: !present}
		if s := staleSection(k); s != "" {
			if i := f.findIn(s, k); i >= 0 {
				c.StaleSection = s
				_, c.Stale = splitKV(f.lines[i])
			}
		}
		if present && strings.EqualFold(cur, want) && c.StaleSection == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Apply writes the changes, preserving everything else in the file byte for
// byte apart from the lines it edits. The write is atomic: a crash mid-write
// must not leave a global config file truncated.
func (f *File) Apply(changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	for _, c := range changes {
		if c.StaleSection != "" {
			if i := f.findIn(c.StaleSection, c.Key); i >= 0 {
				f.lines = append(f.lines[:i], f.lines[i+1:]...)
			}
		}
		if i := f.find(c.Key); i >= 0 {
			// Keep any inline comment: someone wrote it for a reason.
			f.lines[i] = c.Key + "=" + c.New + inlineComment(f.lines[i])
			continue
		}
		f.insertIntoSection(SectionOf(c.Key), c.Key+"="+c.New)
	}
	return f.write()
}

// inlineComment returns the trailing "  # ..." of a line, if any.
func inlineComment(line string) string {
	if i := strings.IndexAny(line, "#;"); i >= 0 {
		return "  " + strings.TrimSpace(line[i:])
	}
	return ""
}

// insertIntoSection appends a line at the end of the named section, creating
// the section when the file has none. Appending rather than sorting keeps the
// user's own ordering intact.
func (f *File) insertIntoSection(section, line string) {
	start := -1
	end := len(f.lines)
	for i, l := range f.lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "[") || !strings.HasSuffix(t, "]") {
			continue
		}
		if strings.EqualFold(strings.Trim(t, "[]"), section) {
			start = i
			end = len(f.lines)
			continue
		}
		if start >= 0 && i > start {
			end = i
			break
		}
	}
	if start < 0 {
		// No such section at all: add one, separated from whatever precedes it.
		if len(f.lines) > 0 && strings.TrimSpace(f.lines[len(f.lines)-1]) != "" {
			f.lines = append(f.lines, "")
		}
		f.lines = append(f.lines, "["+section+"]", line)
		return
	}
	// Insert after the section's last non-blank line, so a trailing blank line
	// stays trailing.
	insert := end
	for insert > start+1 && strings.TrimSpace(f.lines[insert-1]) == "" {
		insert--
	}
	rest := append([]string{line}, f.lines[insert:]...)
	f.lines = append(f.lines[:insert], rest...)
}

func (f *File) write() error {
	content := strings.Join(f.lines, "\r\n") + "\r\n"
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".wslconfig-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", f.path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", f.path, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, f.path); err != nil {
		return fmt.Errorf("replacing %s: %w", f.path, err)
	}
	f.existed = true
	return nil
}

// Validate checks one managed key's value the way WSL would read it, so a typo
// fails at `skrog config set` rather than silently making the VM's sizing
// something other than what was asked for.
func Validate(key, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", nil // clearing is always allowed
	}
	switch key {
	case KeyMemory, KeySwap:
		return validateSizeValue(key, v)
	case KeyProcessors:
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return "", fmt.Errorf("%s: %q is not a positive whole number of CPUs", key, value)
		}
		return strconv.Itoa(n), nil
	case KeyAutoMemoryReclaim:
		switch strings.ToLower(v) {
		case "gradual", "dropcache", "disabled":
			return strings.ToLower(v), nil
		}
		return "", fmt.Errorf("%s: %q is not gradual, dropcache or disabled", key, value)
	case KeyVirtiofs:
		switch strings.ToLower(v) {
		case "true", "false":
			return strings.ToLower(v), nil
		}
		return "", fmt.Errorf("%s: %q is not true or false", key, value)
	}
	return "", fmt.Errorf("%q is not a managed .wslconfig key (managed: %s)",
		key, strings.Join(Managed(), ", "))
}

// validateSizeValue accepts WSL's size syntax: a number with an optional
// MB/GB suffix. `swap=0` is meaningful (no swap file at all), so zero is
// allowed here where it is not for processors.
func validateSizeValue(key, v string) (string, error) {
	up := strings.ToUpper(v)
	num := strings.TrimRight(up, "BKMGT")
	unit := strings.TrimPrefix(up, num)
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return "", fmt.Errorf("%s: %q is not a size like 4GB, 512MB or 0", key, v)
	}
	switch unit {
	case "", "B":
		if f == 0 {
			return "0", nil
		}
		// A bare number in .wslconfig is bytes, which is almost never what
		// someone means when they type 4.
		return "", fmt.Errorf("%s: %q has no unit; write %sGB or %sMB", key, v, num, num)
	case "KB", "MB", "GB", "TB":
		return strings.TrimSuffix(num, ".0") + unit, nil
	}
	return "", fmt.Errorf("%s: %q is not a size like 4GB, 512MB or 0", key, v)
}

// Bytes parses a managed size value into bytes, for reporting and for the
// small-host warning. Returns 0 for an unset or unparseable value.
func Bytes(v string) uint64 {
	up := strings.ToUpper(strings.TrimSpace(v))
	if up == "" {
		return 0
	}
	num := strings.TrimRight(up, "BKMGT")
	unit := strings.TrimPrefix(up, num)
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0
	}
	mult := map[string]float64{"": 1, "B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}[unit]
	if mult == 0 {
		return 0
	}
	return uint64(f * mult)
}

// SortedKeys is a small helper for stable reporting.
func SortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
