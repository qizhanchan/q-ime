package dict

import (
	"os"
	"path/filepath"
	"testing"
)

var testSyllables = []string{
	"a", "an", "ang", "bei", "guo", "hao", "na", "nai", "nan", "ni",
	"nian", "wo", "zhong",
}

func build(t *testing.T, fn func(b *Builder)) *Reader {
	t.Helper()
	b := NewBuilder(testSyllables)
	b.SetRefWeight(1000)
	fn(b)
	path := filepath.Join(t.TempDir(), "d.bin")
	if err := b.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func ids(t *testing.T, r *Reader, ss ...string) []uint16 {
	t.Helper()
	out := make([]uint16, len(ss))
	for i, s := range ss {
		id, ok := r.SyllableID(s)
		if !ok {
			t.Fatalf("syllable %q not in alphabet", s)
		}
		out[i] = id
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	r := build(t, func(b *Builder) {
		add := func(w uint32, word string, syls ...string) {
			out := make([]uint16, len(syls))
			for i, s := range syls {
				id, _ := b.SyllableID(s)
				out[i] = id
			}
			b.Add(out, word, w)
		}
		add(500, "你", "ni")
		add(300, "尼", "ni")
		add(900, "你好", "ni", "hao")
		add(700, "中国", "zhong", "guo")
	})

	got := r.Lookup(ids(t, r, "ni"), 0)
	if len(got) != 2 {
		t.Fatalf("Lookup(ni) returned %d candidates, want 2", len(got))
	}
	// Postings must come back weight-descending — the reader relies on that
	// to take "the best few" as a plain prefix.
	if got[0].Word != "你" || got[0].Weight != 500 {
		t.Errorf("first = %+v, want 你/500", got[0])
	}
	if got[1].Word != "尼" || got[1].Weight != 300 {
		t.Errorf("second = %+v, want 尼/300", got[1])
	}

	if got := r.Lookup(ids(t, r, "ni", "hao"), 0); len(got) != 1 || got[0].Word != "你好" {
		t.Errorf("Lookup(ni hao) = %+v, want 你好", got)
	}
	if got := r.Lookup(ids(t, r, "wo"), 0); got != nil {
		t.Errorf("Lookup of an absent key = %+v, want nil", got)
	}
	if r.RefWeight() != 1000 {
		t.Errorf("RefWeight = %d, want 1000", r.RefWeight())
	}
}

func TestLookupLimit(t *testing.T) {
	r := build(t, func(b *Builder) {
		id, _ := b.SyllableID("ni")
		for i, w := range []uint32{9, 8, 7, 6, 5} {
			b.Add([]uint16{id}, string(rune('a'+i)), w)
		}
	})
	if got := r.Lookup(ids(t, r, "ni"), 2); len(got) != 2 {
		t.Errorf("limit 2 returned %d", len(got))
	}
	if got := r.Lookup(ids(t, r, "ni"), 0); len(got) != 5 {
		t.Errorf("limit 0 returned %d, want all 5", len(got))
	}
}

// TestPrefixRangeIsContiguous is the property the whole abbreviation scheme
// rests on: because syllable IDs are assigned in ascending byte order,
// "every syllable starting with n" is one unbroken ID range, so the engine
// can find it with a binary search instead of scanning the alphabet.
func TestPrefixRangeIsContiguous(t *testing.T) {
	r := build(t, func(b *Builder) {})
	lo, hi := r.PrefixRange("n")
	if hi <= lo {
		t.Fatal("PrefixRange(n) is empty")
	}
	for id := lo; id < hi; id++ {
		s := r.Syllable(id)
		if len(s) == 0 || s[0] != 'n' {
			t.Errorf("id %d = %q is inside the n range but does not start with n", id, s)
		}
	}
	// And nothing starting with n may fall outside it.
	for id, s := range r.Syllables() {
		if s[0] == 'n' && (uint16(id) < lo || uint16(id) >= hi) {
			t.Errorf("%q (id %d) starts with n but is outside [%d,%d)", s, id, lo, hi)
		}
	}
}

func TestPrefixRangeExactAndMissing(t *testing.T) {
	r := build(t, func(b *Builder) {})
	// A full syllable's own prefix range includes itself and its extensions.
	lo, hi := r.PrefixRange("na")
	found := map[string]bool{}
	for id := lo; id < hi; id++ {
		found[r.Syllable(id)] = true
	}
	for _, want := range []string{"na", "nai", "nan"} {
		if !found[want] {
			t.Errorf("PrefixRange(na) missing %q", want)
		}
	}
	if lo, hi := r.PrefixRange("qq"); hi != lo {
		t.Errorf("PrefixRange of an impossible prefix = [%d,%d), want empty", lo, hi)
	}
}

func TestBuilderKeepsHigherWeightOnDuplicate(t *testing.T) {
	// Merging several source dictionaries relies on this: the same word at
	// the same reading keeps the strongest claim rather than the last one in.
	r := build(t, func(b *Builder) {
		id, _ := b.SyllableID("ni")
		b.Add([]uint16{id}, "你", 100)
		b.Add([]uint16{id}, "你", 900)
		b.Add([]uint16{id}, "你", 400)
	})
	got := r.Lookup(ids(t, r, "ni"), 0)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want the duplicate collapsed to 1", len(got))
	}
	if got[0].Weight != 900 {
		t.Errorf("weight = %d, want the highest (900)", got[0].Weight)
	}
}

func TestTrimPostings(t *testing.T) {
	b := NewBuilder(testSyllables)
	id, _ := b.SyllableID("ni")
	for i := 0; i < 50; i++ {
		b.Add([]uint16{id}, string(rune('A'+i)), uint32(i))
	}
	if dropped := b.TrimPostings(10); dropped != 40 {
		t.Errorf("dropped %d, want 40", dropped)
	}
	path := filepath.Join(t.TempDir(), "d.bin")
	if err := b.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := r.Lookup(ids(t, r, "ni"), 0)
	if len(got) != 10 {
		t.Fatalf("kept %d, want 10", len(got))
	}
	// Trimming must keep the HEAVIEST, not an arbitrary ten.
	if got[0].Weight != 49 {
		t.Errorf("heaviest kept = %d, want 49", got[0].Weight)
	}
}

func TestChildrenInRange(t *testing.T) {
	r := build(t, func(b *Builder) {
		for _, s := range []string{"na", "nai", "nan", "ni", "wo"} {
			id, _ := b.SyllableID(s)
			b.Add([]uint16{id}, "x"+s, 1)
		}
	})
	lo, hi := r.PrefixRange("n")
	var seen []string
	r.ChildrenInRange(r.Root(), lo, hi, func(syl uint16, child Node) bool {
		seen = append(seen, r.Syllable(syl))
		return true
	})
	if len(seen) != 4 {
		t.Errorf("walked %v, want the four n-syllables", seen)
	}
	for _, s := range seen {
		if s[0] != 'n' {
			t.Errorf("walk escaped the range: %q", s)
		}
	}

	// Returning false must stop the walk.
	count := 0
	r.ChildrenInRange(r.Root(), lo, hi, func(uint16, Node) bool {
		count++
		return false
	})
	if count != 1 {
		t.Errorf("early stop visited %d children, want 1", count)
	}
}

func TestRejectsForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(path, make([]byte, 128), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("Open accepted a file that is not a dictionary")
	}
}

// A dictionary built by a different version of the format must be rejected
// loudly. Silently misreading it would produce candidates that look almost
// right, which is far harder to diagnose than a refusal to start.
func TestRejectsWrongVersion(t *testing.T) {
	b := NewBuilder(testSyllables)
	id, _ := b.SyllableID("ni")
	b.Add([]uint16{id}, "你", 1)
	path := filepath.Join(t.TempDir(), "d.bin")
	if err := b.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	le.PutUint32(data[hVersion:], FormatVersion+1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("Open accepted a dictionary from a different format version")
	}
}

func TestEmptyDictionary(t *testing.T) {
	r := build(t, func(b *Builder) {})
	if got := r.Lookup(ids(t, r, "ni"), 0); got != nil {
		t.Errorf("empty dictionary returned %+v", got)
	}
	if r.PostCount(r.Root()) != 0 {
		t.Error("empty dictionary has postings at the root")
	}
	if len(r.Syllables()) != len(testSyllables) {
		t.Errorf("alphabet has %d entries, want %d", len(r.Syllables()), len(testSyllables))
	}
}

func TestMaxSyllablesRejected(t *testing.T) {
	b := NewBuilder(testSyllables)
	id, _ := b.SyllableID("ni")
	long := make([]uint16, MaxSyllables+1)
	for i := range long {
		long[i] = id
	}
	before, _ := b.Stats()
	b.Add(long, "toolong", 1)
	after, _ := b.Stats()
	if after != before {
		t.Errorf("an over-long key added %d nodes; it should have been refused", after-before)
	}
}
