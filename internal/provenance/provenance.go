// Package provenance records where an image on this machine came from (#343).
//
// # The hole it closes
//
// allow-registries judges the REFERENCE in a request. Nothing recorded where
// an image actually came from, so any image could be renamed into an allowed
// reference and run:
//
//	docker load -i bad.tar
//	docker tag bad contoso.azurecr.io/anything:1
//	docker run contoso.azurecr.io/anything:1     # passes
//
// # Why entries are keyed by image ID
//
// The cheap version of this records "reference R was pulled from registry X"
// and judges by reference. It closes the example above and dies to a one-line
// variation: pull something legitimate at that reference first, creating a
// record, then load and tag over it. A reference is a mutable label, so
// provenance bound to one attests to a name that anything can be renamed to --
// which is the mistake the allowlist already makes, one level down.
//
// The image ID is the config digest: content-addressed, and what a container
// create ultimately resolves to. Renaming an image keeps its ID, so a record
// follows the content rather than the label. That is also why tag is not
// gated: renaming an image with a record keeps the record, and renaming one
// without does not create one.
//
// # What this is NOT
//
// It is not a boundary against someone who sets out to get around it. The
// engine distro is registered in the user's own WSL installation, so
// `wsl -d skrog-engine -u root docker load -i bad.tar` never touches the pipe
// and cannot be gated here (#418, route 3).
//
// It protects against the accident and the drift: a base image that arrived
// through a docker load in a script, a CI job importing a tarball, a mirror
// nobody vetted. Those are real, and they are what an allowlist is for on a
// cooperating machine. Enforcement is opt-in for the same reason -- see
// policy.Rules.DenyUnattributableImages.
//
// # Storage
//
// An append-only JSONL file. Appends rather than read-modify-write because the
// writer is a long-lived bridge handling concurrent connections, and because
// nothing stops a second skrog process from touching the same state dir. A
// read-modify-write would lose records silently under either; an append
// cannot. Compaction rewrites the file when it grows, keeping the newest entry
// per ID.
package provenance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileName is the store, inside the state dir.
const FileName = "image-provenance.jsonl"

// Source says how an image came to be on this machine.
type Source string

const (
	// SourcePull is an image the gate allowed and watched arrive.
	//
	// This covers more than a user typing `docker pull`. `skrog prewarm`
	// drives the docker CLI, so its pulls cross the bridge like any other and
	// are recorded here with no special case.
	//
	// There is deliberately no "bundle" source. The air-gap bundle (#75)
	// carries a rootfs, not container images, so nothing loads images from it
	// and a source with no producer would be dead code claiming a guarantee.
	SourcePull Source = "pull"
	// SourcePreExisting is an image that was already here when provenance
	// recording started.
	//
	// Seeded rather than left unknown, and named rather than disguised as a
	// pull: on a machine with images predating the feature, refusing the whole
	// cache is not a defensible default, and "everything before today is
	// trusted" is a smaller lie when it says so out loud. `skrog policy show`
	// reports how many entries are of this kind.
	SourcePreExisting Source = "pre-existing"
)

// Entry is one image's provenance.
type Entry struct {
	// ID is the image ID (config digest), the content identity a container
	// create resolves to.
	ID string `json:"id"`
	// Registry is the host the image was fetched from. Empty for a source
	// that is not a registry.
	Registry string `json:"registry,omitempty"`
	// Ref is the reference as it was requested, kept for reporting. It is
	// never what a judgement is made on.
	Ref string `json:"ref,omitempty"`
	// Source is how it arrived.
	Source Source `json:"source"`
	// At is when the record was made.
	At time.Time `json:"at"`
}

// maxEntries and maxAge bound the store. Image IDs accumulate on a machine
// that pulls a lot, and a file nobody prunes is a file that eventually becomes
// the problem.
const (
	maxEntries = 4096
	maxAge     = 180 * 24 * time.Hour
)

// mu serialises this process's writes. It does not coordinate with another
// process -- the append is what makes that safe.
var mu sync.Mutex

func storePath(stateDir string) string { return filepath.Join(stateDir, FileName) }

// Record appends an entry. A record for an ID that already has one is kept:
// Lookup takes the newest, and compaction drops the rest.
func Record(stateDir string, e Entry) error {
	if e.ID == "" {
		return fmt.Errorf("provenance: an entry needs an image ID")
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}

	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("provenance: creating state dir: %w", err)
	}
	f, err := os.OpenFile(storePath(stateDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("provenance: opening the store: %w", err)
	}
	// Closed explicitly, and its error returned: a deferred Close on a
	// WRITABLE file throws away the one error that says the record did not
	// reach the disk. Write can succeed into a buffer that Close then fails to
	// flush, and a provenance store that silently lost its last append is the
	// exact failure this package exists to make visible.
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("provenance: appending: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("provenance: closing the store: %w", err)
	}
	return nil
}

// supersedes reports whether a LATER LINE should replace the current winner
// for an image ID.
//
// "Not before" rather than "after", and the difference is the whole point: the
// store is append-only, so line order is arrival order, and a strict `After`
// makes two records written in the same clock tick a tie that resolves to
// whichever the loop saw first -- the OLDER one. That is not hypothetical on
// Windows, where the wall clock granularity is coarse enough that a seed and
// the pull immediately after it share a timestamp; it turned a pulled image
// back into a pre-existing one, and CI caught it on main (#343).
//
// Position is the tie-break because position is the fact: a line further down
// the file was appended later, whatever the clock said at the time.
func supersedes(candidate, current Entry) bool {
	return !candidate.At.Before(current.At)
}

// Lookup returns the newest record for an image ID.
func Lookup(stateDir, id string) (Entry, bool) {
	if id == "" {
		return Entry{}, false
	}
	var found Entry
	var ok bool
	for _, e := range read(stateDir) {
		if e.ID == id && (!ok || supersedes(e, found)) {
			found, ok = e, true
		}
	}
	return found, ok
}

// Stats is what `skrog policy show` and doctor report: how much of this
// machine's image set can be attributed, before anything is refused for not
// being.
type Stats struct {
	Total       int `json:"total"`
	Pulled      int `json:"pulled"`
	PreExisting int `json:"preExisting"`
}

// Summarize counts the newest record per ID.
func Summarize(stateDir string) Stats {
	newest := map[string]Entry{}
	for _, e := range read(stateDir) {
		if prev, ok := newest[e.ID]; !ok || supersedes(e, prev) {
			newest[e.ID] = e
		}
	}
	var s Stats
	for _, e := range newest {
		s.Total++
		switch e.Source {
		case SourcePull:
			s.Pulled++
		case SourcePreExisting:
			s.PreExisting++
		}
	}
	return s
}

// Seed records images that were already present, once. It is a no-op when the
// store already exists, so it cannot relabel a machine's history on every
// start.
func Seed(stateDir string, ids []string) (seeded int, err error) {
	mu.Lock()
	_, statErr := os.Stat(storePath(stateDir))
	mu.Unlock()
	if statErr == nil {
		return 0, nil
	}

	now := time.Now()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if err := Record(stateDir, Entry{ID: id, Source: SourcePreExisting, At: now}); err != nil {
			return seeded, err
		}
		seeded++
	}
	return seeded, nil
}

// Compact rewrites the store with one entry per ID, dropping anything older
// than maxAge and keeping the newest maxEntries. Safe to call at any time; it
// is a no-op while the file is small.
func Compact(stateDir string) error {
	mu.Lock()
	defer mu.Unlock()

	entries := readLocked(stateDir)
	if len(entries) <= maxEntries {
		return nil
	}
	// Winners and the cap are both decided by POSITION, not by the clock.
	//
	// In an append-only log the order records arrived in is a fact, and it is
	// the one thing a coarse clock cannot blur -- which matters here for the
	// same reason it matters in supersedes: a seed and the pull right after it
	// routinely share a timestamp on Windows. Comparing timestamps to pick a
	// winner, or sorting by them to apply the cap, resolves those ties
	// arbitrarily and can drop the record that is actually current.
	newestIdx := map[string]int{}
	cutoff := time.Now().Add(-maxAge)
	for i, e := range entries {
		if e.At.Before(cutoff) {
			continue
		}
		if j, ok := newestIdx[e.ID]; !ok || supersedes(e, entries[j]) {
			newestIdx[e.ID] = i
		}
	}
	idxs := make([]int, 0, len(newestIdx))
	for _, i := range newestIdx {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	// The newest are furthest down the file, so the cap takes the TAIL.
	if len(idxs) > maxEntries {
		idxs = idxs[len(idxs)-maxEntries:]
	}
	// Written back in file order. Nothing reads a tie out of the compacted
	// block -- there is one line per ID by construction -- but keeping the
	// order means the file still reads as the log it is, and a record appended
	// afterwards is still the last line.
	kept := make([]Entry, 0, len(idxs))
	for _, i := range idxs {
		kept = append(kept, entries[i])
	}

	var b strings.Builder
	for _, e := range kept {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp := storePath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("provenance: writing the compacted store: %w", err)
	}
	if err := os.Rename(tmp, storePath(stateDir)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("provenance: committing the compacted store: %w", err)
	}
	return nil
}

func read(stateDir string) []Entry {
	mu.Lock()
	defer mu.Unlock()
	return readLocked(stateDir)
}

// readLocked parses the store, skipping lines it cannot parse.
//
// A truncated final line is expected rather than exceptional: an append
// interrupted by a power loss leaves one, and losing one record is not a
// reason to refuse to read the rest.
func readLocked(stateDir string) []Entry {
	f, err := os.Open(storePath(stateDir))
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.ID == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}
