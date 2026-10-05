package engine

import (
	"strings"
	"testing"

	"github.com/qizhanchan/q-ime/internal/emoji"
)

// emojiTestDict is deliberately tiny and targeted: 你好 has one emoji, 中国 has
// one, 好 has three (the shape that must not spend three slots), and nothing
// else in the test lexicon has any.
func emojiTestDict() *emoji.Dict {
	return emoji.Load(strings.Join([]string{
		"中国\t🇨🇳",
		"你好\t👋",
		"好\t👌\t🙆\t🙋",
	}, "\n") + "\n")
}

func newEmojiEngine(t *testing.T, inline bool) *Engine {
	t.Helper()
	e := newTestEngine(t)
	e.SetEmoji(emojiTestDict(), inline)
	return e
}

// TestEmojiSitsBesideItsWord is the placement rule.
//
// Not a reserved slot at the back of the page, which is what English gets: an
// English candidate stands on its own, an emoji stands on a word. If the word
// is on the page, beside it is where a user will look; if it is not, the
// picture has no business being there ahead of it.
func TestEmojiSitsBesideItsWord(t *testing.T) {
	e := newEmojiEngine(t, true)
	cs := e.Candidates("nihao", 0)

	word := indexOf(cs, "你好")
	glyph := indexOf(cs, "👋")
	if word != 0 {
		t.Fatalf("\"nihao\" gives %v, want 你好 first", words(cs))
	}
	if glyph != word+1 {
		t.Errorf("👋 is at %d and 你好 at %d, want the emoji directly after its word",
			glyph, word)
	}
	if cs[glyph].Source != SourceEmoji {
		t.Errorf("emoji candidate has source %v, want SourceEmoji", cs[glyph].Source)
	}
}

// TestEmojiNeverTakesTheTopSlot: Space commits candidate 0, and it must commit
// the word. The picture is an offer, not an interpretation of the input.
func TestEmojiNeverTakesTheTopSlot(t *testing.T) {
	e := newEmojiEngine(t, true)
	for _, in := range []string{"nihao", "zhongguo", "hao"} {
		cs := e.Candidates(in, 0)
		if len(cs) == 0 {
			t.Errorf("%q returned nothing", in)
			continue
		}
		if cs[0].Source == SourceEmoji {
			t.Errorf("%q put %s first", in, cs[0].Word)
		}
	}
}

// TestEmojiInheritsTheReadingOfItsWord is what makes everything else work
// without a special case: the preedit segments from Spans, the user dictionary
// keys the pick from Reading, and the context memory needs both to offer it
// again. An emoji with no reading would be learnable by nothing.
func TestEmojiInheritsTheReadingOfItsWord(t *testing.T) {
	e := newEmojiEngine(t, true)
	cs := e.Candidates("nihao", 0)
	w, g := indexOf(cs, "你好"), indexOf(cs, "👋")
	if w < 0 || g < 0 {
		t.Fatalf("expected both 你好 and 👋 in %v", words(cs))
	}
	if strings.Join(cs[g].Reading, "'") != strings.Join(cs[w].Reading, "'") {
		t.Errorf("emoji reading %v, word reading %v — must match",
			cs[g].Reading, cs[w].Reading)
	}
	if cs[g].Consumed != cs[w].Consumed {
		t.Errorf("emoji consumed %d, word %d", cs[g].Consumed, cs[w].Consumed)
	}
	if len(cs[g].Spans) != len(cs[w].Spans) {
		t.Errorf("emoji spans %v, word spans %v", cs[g].Spans, cs[w].Spans)
	}
}

// TestEmojiOnlyAtFullCoverage: the same rule the user dictionary and the
// context memory obey. A picture for a word that explains half of what has been
// typed is a picture for something the user is still in the middle of saying.
func TestEmojiOnlyAtFullCoverage(t *testing.T) {
	e := newEmojiEngine(t, true)
	// "nihaom" — 你好 covers five of six letters.
	cs := e.Candidates("nihaom", 0)
	for _, c := range cs {
		if c.Source == SourceEmoji && c.Consumed < len("nihaom") {
			t.Errorf("%s offered for %q while covering only %d bytes",
				c.Word, "nihaom", c.Consumed)
		}
	}
}

// TestOnlyOneEmojiPerWordInline: 好 has three, and a five-candidate page cannot
// spend three of them on pictures of one word.
func TestOnlyOneEmojiPerWordInline(t *testing.T) {
	e := newEmojiEngine(t, true)
	cs := e.Candidates("hao", 0)
	n := 0
	for _, c := range cs {
		if c.Source == SourceEmoji {
			n++
		}
	}
	if n > maxEmojiDecorations {
		t.Errorf("\"hao\" produced %d emoji, want at most %d: %v",
			n, maxEmojiDecorations, words(cs))
	}
	if indexOf(cs, "👌") < 0 {
		t.Errorf("the first emoji for 好 is missing from %v", words(cs))
	}
}

// TestInlineEmojiCanBeTurnedOff: the switch exists because the cost lands on
// every keystroke while only some people want it — the same argument that keeps
// fuzzy matching off. Turning it off must not take the explicit mode away.
func TestInlineEmojiCanBeTurnedOff(t *testing.T) {
	e := newEmojiEngine(t, false)
	for _, c := range e.Candidates("nihao", 0) {
		if c.Source == SourceEmoji {
			t.Fatalf("emoji %s appeared with the decoration switched off", c.Word)
		}
	}
	if got := e.EmojiWords("nihao", 8); len(got) == 0 || got[0].Word != "👋" {
		t.Errorf("EmojiWords still has to work: got %v", words(got))
	}
}

// TestEmojiModeReachesThroughAbbreviations is the payoff of keying by word
// rather than by pinyin: "nh" finds 你好 through machinery that already exists,
// and the emoji comes along for free. A table keyed by pinyin would have had to
// reimplement abbreviation matching to do this.
func TestEmojiModeReachesThroughAbbreviations(t *testing.T) {
	e := newEmojiEngine(t, true)
	for _, in := range []string{"nihao", "nih", "nh"} {
		got := e.EmojiWords(in, 8)
		if len(got) == 0 || got[0].Word != "👋" {
			t.Errorf("EmojiWords(%q) = %v, want 👋 first", in, words(got))
		}
	}
}

// TestEmojiModeOffersEveryEmojiOfAWord: the user who asked for emoji is not
// spending a slot they wanted for something else, so the one-per-word rule that
// governs the ordinary list does not apply here.
func TestEmojiModeOffersEveryEmojiOfAWord(t *testing.T) {
	e := newEmojiEngine(t, true)
	got := words(e.EmojiWords("hao", 12))
	for _, want := range []string{"👌", "🙆", "🙋"} {
		if indexOfString(got, want) < 0 {
			t.Errorf("EmojiWords(\"hao\") = %v, want %s among them", got, want)
		}
	}
}

// TestEmojiModeOffersNothingElse: whatever else "hao" means, somebody who typed
// the v is not asking for 好.
func TestEmojiModeOffersNothingElse(t *testing.T) {
	e := newEmojiEngine(t, true)
	for _, c := range e.EmojiWords("hao", 12) {
		if c.Source != SourceEmoji {
			t.Errorf("EmojiWords returned %q (source %v)", c.Word, c.Source)
		}
	}
}

// TestNoEmojiTableIsNotAnError: the table is embedded, so a failure to load it
// means a corrupt binary — and an input method that cannot offer 😄 must still
// type Chinese.
func TestNoEmojiTableIsNotAnError(t *testing.T) {
	e := newTestEngine(t) // no SetEmoji at all
	if cs := e.Candidates("nihao", 0); len(cs) == 0 || cs[0].Word != "你好" {
		t.Errorf("without an emoji table, \"nihao\" gives %v", words(cs))
	}
	if got := e.EmojiWords("nihao", 8); got != nil {
		t.Errorf("EmojiWords without a table = %v, want nothing", words(got))
	}
}
