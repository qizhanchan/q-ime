package emoji

import (
	"strings"
	"testing"
)

func testDict() *Dict {
	return Load(strings.Join([]string{
		"# a comment the loader must skip",
		"加油\t💪\t⛽",
		"好\t👌\t🙆‍♂️\t🙆‍♀️",
		"开心\t😄",
		"笑\t😄\t😊",
	}, "\n") + "\n")
}

func TestEmojiLookup(t *testing.T) {
	d := testDict()
	if got := d.Len(); got != 4 {
		t.Fatalf("Len = %d, want 4 (the comment must not be indexed)", got)
	}
	for _, tc := range []struct {
		word string
		want []string
	}{
		{"开心", []string{"😄"}},
		{"笑", []string{"😄", "😊"}},
		{"加油", []string{"💪", "⛽"}},
		{"好", []string{"👌", "🙆‍♂️", "🙆‍♀️"}},
		{"没有这个词", nil},
		{"", nil},
	} {
		var got []string
		d.Emoji(tc.word, func(e string) bool { got = append(got, e); return true })
		if strings.Join(got, "") != strings.Join(tc.want, "") {
			t.Errorf("Emoji(%q) = %v, want %v", tc.word, got, tc.want)
		}
	}
}

// TestFirstTakesOnlyOne: the main candidate list gets one slot, and a word with
// three emoji must not spend three of the five on a page.
func TestFirstTakesOnlyOne(t *testing.T) {
	d := testDict()
	if got := d.First("好"); got != "👌" {
		t.Errorf("First(\"好\") = %q, want 👌", got)
	}
	if got := d.First("没有这个词"); got != "" {
		t.Errorf("First on a missing word = %q, want empty", got)
	}
	if !d.Has("笑") || d.Has("没有这个词") {
		t.Error("Has disagrees with First")
	}
}

// TestEmojiStopsEarly pins the fn-returns-false contract, which First relies on
// and which is the difference between reading one emoji and reading all of a
// word's.
func TestEmojiStopsEarly(t *testing.T) {
	d := testDict()
	n := 0
	d.Emoji("好", func(string) bool { n++; return false })
	if n != 1 {
		t.Errorf("walked %d emoji after returning false, want 1", n)
	}
}

// TestUnsortedFileStillWorks: the generator sorts, but a hand-edited table must
// not silently lose lookups.
func TestUnsortedFileStillWorks(t *testing.T) {
	d := Load("笑\t😄\n开心\t😄\n加油\t💪\n")
	for _, w := range []string{"笑", "开心", "加油"} {
		if !d.Has(w) {
			t.Errorf("%q lost in an unsorted table", w)
		}
	}
}

// TestBuiltinLoads is the smoke test for the embedded table: it is generated,
// so a broken generator would otherwise surface as "emoji just do not work".
func TestBuiltinLoads(t *testing.T) {
	d, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	if d.Len() < 4000 {
		t.Errorf("built-in table holds %d words, want the full map", d.Len())
	}
	for _, tc := range []struct{ word, want string }{
		{"开心", "😄"},
		{"火", "🔥"},
		{"中国", "🇨🇳"},
		{"谢谢", "🙏"},
	} {
		if got := d.First(tc.word); got != tc.want {
			t.Errorf("First(%q) = %q, want %q", tc.word, got, tc.want)
		}
	}
	// Words the lexicon is full of must NOT be in here, or every other
	// candidate would sprout a picture.
	for _, w := range []string{"的", "了", "我们", "这个"} {
		if d.Has(w) {
			t.Errorf("%q has an emoji; the map is supposed to be selective", w)
		}
	}
}
