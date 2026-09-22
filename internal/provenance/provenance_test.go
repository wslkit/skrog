package provenance

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordAndLookup(t *testing.T) {
	dir := t.TempDir()
	if _, ok := Lookup(dir, "sha256:aaa"); ok {
		t.Fatal("found a record in an empty store")
	}
	if err := Record(dir, Entry{ID: "sha256:aaa", Registry: "ghcr.io", Ref: "ghcr.io/x:1", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	got, ok := Lookup(dir, "sha256:aaa")
	if !ok {
		t.Fatal("recorded entry not found")
	}
	if got.Registry != "ghcr.io" || got.Source != SourcePull {
		t.Errorf("entry = %+v", got)
	}
	if got.At.IsZero() {
		t.Error("a record with no timestamp cannot be compacted or aged out")
	}
}

// The store is append-only, so an ID can have several records. The newest
// wins: an image retagged and repulled from somewhere else is now from
// somewhere else.
func TestLookupTakesTheNewestRecord(t *testing.T) {
	dir := t.TempDir()
	older := time.Now().Add(-time.Hour)
	if err := Record(dir, Entry{ID: "sha256:a", Registry: "old.example.com", Source: SourcePull, At: older}); err != nil {
		t.Fatal(err)
	}
	if err := Record(dir, Entry{ID: "sha256:a", Registry: "new.example.com", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	got, _ := Lookup(dir, "sha256:a")
	if got.Registry != "new.example.com" {
		t.Errorf("registry = %q, want the newest record", got.Registry)
	}
}

// An entry with no ID is meaningless: the ID is the whole point, because a
// reference is a mutable label.
func TestRecordRefusesAnEntryWithNoID(t *testing.T) {
	if err := Record(t.TempDir(), Entry{Registry: "ghcr.io", Source: SourcePull}); err == nil {
		t.Error("recorded an entry with no image ID")
	}
}

// A truncated final line is what an interrupted append leaves. Losing one
// record is not a reason to refuse to read the rest.
func TestReadSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	if err := Record(dir, Entry{ID: "sha256:good", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, FileName), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"sha256:trunc","sou`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, ok := Lookup(dir, "sha256:good"); !ok {
		t.Error("a truncated trailing line hid the records before it")
	}
}

// Seeding is once. A machine's history must not be relabelled on every
// supervisor start -- that would quietly turn every image ever loaded into a
// trusted one.
func TestSeedOnlyRunsOnce(t *testing.T) {
	dir := t.TempDir()
	n, err := Seed(dir, []string{"sha256:a", "sha256:b"})
	if err != nil || n != 2 {
		t.Fatalf("first seed: n=%d err=%v", n, err)
	}
	n, err = Seed(dir, []string{"sha256:c"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("seeded %d on a store that already exists", n)
	}
	if _, ok := Lookup(dir, "sha256:c"); ok {
		t.Error("a second seed added records")
	}
}

// Seeded images say what they are. Disguising them as pulls would make the
// report claim an origin nobody recorded.
func TestSeededEntriesAreNamedPreExisting(t *testing.T) {
	dir := t.TempDir()
	if _, err := Seed(dir, []string{"sha256:a"}); err != nil {
		t.Fatal(err)
	}
	got, _ := Lookup(dir, "sha256:a")
	if got.Source != SourcePreExisting {
		t.Errorf("source = %q, want %q", got.Source, SourcePreExisting)
	}
	if got.Registry != "" {
		t.Errorf("registry = %q; a pre-existing image has no known origin", got.Registry)
	}
}

func TestSummarizeCountsEachIDOnce(t *testing.T) {
	dir := t.TempDir()
	if _, err := Seed(dir, []string{"sha256:a", "sha256:b"}); err != nil {
		t.Fatal(err)
	}
	// A repull of one of them: still one image.
	if err := Record(dir, Entry{ID: "sha256:a", Registry: "ghcr.io", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	st := Summarize(dir)
	if st.Total != 2 {
		t.Errorf("total = %d, want 2", st.Total)
	}
	if st.Pulled != 1 || st.PreExisting != 1 {
		t.Errorf("pulled=%d preExisting=%d; the newest record per ID decides", st.Pulled, st.PreExisting)
	}
}

// The store must not grow without bound on a machine that pulls constantly.
func TestCompactBoundsTheStore(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxEntries+50; i++ {
		if err := Record(dir, Entry{ID: "sha256:" + string(rune('a'+i%26)) + itoa(i), Source: SourcePull}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Compact(dir); err != nil {
		t.Fatal(err)
	}
	if got := len(read(dir)); got > maxEntries {
		t.Errorf("store holds %d entries after compaction, cap is %d", got, maxEntries)
	}
}

// Compaction keeps the newest record per ID, so it must not lose a lookup.
func TestCompactKeepsTheNewestPerID(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxEntries+10; i++ {
		if err := Record(dir, Entry{ID: "sha256:filler" + itoa(i), Source: SourcePull}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Record(dir, Entry{ID: "sha256:keep", Registry: "ghcr.io", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	if err := Compact(dir); err != nil {
		t.Fatal(err)
	}
	got, ok := Lookup(dir, "sha256:keep")
	if !ok || got.Registry != "ghcr.io" {
		t.Errorf("compaction dropped the newest record: %+v ok=%v", got, ok)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
