package english

import (
	"strings"
	"testing"
)

const sample = `# a comment carrying provenance
# and a second one
and	and
hell	hell	he'll
hello	hello
iphone	iPhone	iPhone 17
zebra	zebra
`

func collect(t *testing.T, d *Dict, code string) []string {
	t.Helper()
	var got []string
	d.Words(code, func(w string) bool { got = append(got, w); return true })
	return got
}

func TestWordsLooksUpExactCodes(t *testing.T) {
	d := Load(sample)
	if d.Len() != 5 {
		t.Fatalf("indexed %d codes, want 5 (comments must not be entries)", d.Len())
	}
	cases := []struct {
		code string
		want []string
	}{
		{"hello", []string{"hello"}},
		{"hell", []string{"hell", "he'll"}},
		{"iphone", []string{"iPhone", "iPhone 17"}},
		{"and", []string{"and"}},
		{"zebra", []string{"zebra"}}, // last line, no trailing entry after it
		// A prefix of a real code is not a hit. This is the property that
		// keeps "nice" out of the candidate list while somebody is still
		// typing "ni", and it is worth asserting rather than assuming.
		{"hel", nil},
		{"h", nil},
		{"helloo", nil},
		{"", nil},
		{"aardvark", nil},
		{"zzz", nil},
		// Codes in the file are lowercase; callers pass the typed buffer,
		// which this input method only ever fills with a-z.
		{"HELLO", nil},
	}
	for _, tc := range cases {
		got := collect(t, d, tc.code)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("Words(%q) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestWordsStopsWhenCallbackDeclines(t *testing.T) {
	d := Load(sample)
	n := 0
	d.Words("iphone", func(string) bool { n++; return false })
	if n != 1 {
		t.Errorf("callback ran %d times after returning false, want 1", n)
	}
}

func TestHas(t *testing.T) {
	d := Load(sample)
	if !d.Has("hello") {
		t.Error(`Has("hello") = false`)
	}
	if d.Has("hel") {
		t.Error(`Has("hel") = true; a prefix is not a word`)
	}
}

// A nil Dict is the "no English list" case — the engine holds one as a plain
// field and must not have to nil-check before every lookup.
func TestNilDictIsUsable(t *testing.T) {
	var d *Dict
	if d.Len() != 0 || d.Has("hello") {
		t.Error("nil Dict claims to hold words")
	}
	d.Words("hello", func(string) bool { t.Error("nil Dict yielded a word"); return false })
}

// Load sorts a table whose codes are out of order, because the binary search
// would otherwise silently fail to find entries in a hand-edited file — the
// worst kind of failure, since most lookups would keep working.
func TestLoadRecoversFromUnsortedInput(t *testing.T) {
	d := Load("zebra\tzebra\nand\tand\nhello\thello\n")
	for _, code := range []string{"and", "hello", "zebra"} {
		if !d.Has(code) {
			t.Errorf("after loading unsorted input, %q not found", code)
		}
	}
}

func TestBuiltinLoads(t *testing.T) {
	d, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	// A floor, not the exact count: the table is regenerated from upstream
	// and asserting an exact size would make this fail on every dictionary
	// update for no reason.
	if d.Len() < 15000 {
		t.Errorf("embedded table holds %d codes, want at least 15000", d.Len())
	}
	// Spot checks of the three properties the generator is there to preserve.
	for _, tc := range []struct{ code, want string }{
		{"hello", "hello"},
		{"iphone", "iPhone"},            // canonical capitalisation
		{"buenosaires", "Buenos Aires"}, // a space in the text, not in the code
		{"github", "GitHub"},
	} {
		got := collect(t, d, tc.code)
		if len(got) == 0 || got[0] != tc.want {
			t.Errorf("Words(%q) = %v, want first word %q", tc.code, got, tc.want)
		}
	}
	// Single letters are dropped: they are how an abbreviated syllable is
	// spelled, and English there would be noise on the one keystroke where
	// the candidate list is least settled.
	for _, code := range []string{"a", "i", "j", "z"} {
		if d.Has(code) {
			t.Errorf("single-letter code %q is in the table", code)
		}
	}
	// Digits can never reach the buffer — they pick a candidate — so a code
	// containing one is dead weight.
	if d.Has("manifestv3") {
		t.Error("a code containing a digit is in the table")
	}
}
