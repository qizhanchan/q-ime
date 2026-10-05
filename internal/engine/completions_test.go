package engine

import (
	"path/filepath"
	"testing"

	"github.com/qizhanchan/q-ime/internal/dict"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// buildWideDict compiles a lexicon whose [ni] node has more children than the
// completion pass may expand, with the only continuation a user would want
// sorting alphabetically LAST among them.
//
// This is the shape the shipped lexicon has everywhere — a common syllable's
// node has hundreds of children — shrunk to the smallest dictionary that still
// forces the choice: any picker that takes children in storage order fills its
// budget on the junk and never reaches the word, and a picker that ranks by
// weight cannot miss it.
func buildWideDict(t *testing.T) *dict.Reader {
	t.Helper()

	// junk syllables sort ahead of "zu"; there are more of them than the
	// expansion budget, so storage order alone never reaches the zu child.
	junk := []string{
		"an", "ba", "ca", "da", "e", "fa", "ga", "ha",
		"ji", "ka", "la", "ma", "pa", "qi", "sa", "ta",
	}
	if len(junk) <= 12 {
		t.Fatal("fixture is not adversarial: fewer junk children than the expansion budget")
	}
	syllables := append(append([]string{}, junk...), "ni", "zu")
	b := dict.NewBuilder(syllables)
	id := func(s string) uint16 {
		v, ok := b.SyllableID(s)
		if !ok {
			t.Fatalf("test syllable %q missing from alphabet", s)
		}
		return v
	}
	b.Add([]uint16{id("ni")}, "你", 1000)
	// One rare two-syllable word per junk syllable. The words are irrelevant;
	// only their existence as children of [ni] is.
	for _, s := range junk {
		b.Add([]uint16{id("ni"), id(s)}, "你"+s, 10)
	}
	// The continuation that matters: outweighing every junk sibling by orders
	// of magnitude — the 版权 shape, a phrase whose corpus weight dwarfs the
	// single character's — and filed under the syllable that sorts after all
	// of them.
	b.Add([]uint16{id("ni"), id("zu")}, "你祖", 200000)
	b.SetRefWeight(200000)

	path := filepath.Join(t.TempDir(), "wide.bin")
	if err := b.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r, err := dict.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// TestCompletionsPickChildrenByWeightNotAlphabet is the "你啊 but never 你们"
// bug. Children of a trie node are stored sorted by syllable ID — that is,
// alphabetically — and the completion pass used to expand the first twelve it
// walked past. Whether a continuation could be predicted at all therefore
// depended on where its next syllable sorted: on the shipped lexicon, "ni"
// predicted 你啊 and 你把 while 你们 and 你好 never appeared.
func TestCompletionsPickChildrenByWeightNotAlphabet(t *testing.T) {
	e := New(buildWideDict(t), nil, pinyin.Fuzzy{})

	got := e.Candidates("ni", 0)
	i := indexOf(got, "你祖")
	if i < 0 {
		t.Fatalf(`"ni" gives %v; the heaviest continuation is missing — `+
			`completions are being taken in alphabetical order`, words(got))
	}
	// It is a prediction, so it spans no typed input and consumes what WAS
	// typed — the invariants every completion carries.
	c := got[i]
	if c.Consumed != len("ni") {
		t.Errorf("你祖 consumed %d bytes, want %d", c.Consumed, len("ni"))
	}
	if len(c.Spans) != 1 || c.Spans[0] != 2 {
		t.Errorf("你祖 spans %v, want just the typed syllable [2]", c.Spans)
	}
	// And it must outrank every junk continuation it outweighs by orders of
	// magnitude. A prediction is recognizable by reading one syllable past
	// the input.
	for j, other := range got[:i] {
		if other.Source == SourceLexicon && len(other.Reading) == 2 {
			t.Errorf("junk continuation %q (rank %d) ranked above 你祖 (rank %d)",
				other.Word, j, i)
		}
	}
}

// TestPredictionNeverOutranksASpelledWord is the 版权-for-"ban" regression.
//
// 你祖 here outweighs 你 two hundred fold, the way 版权 outweighs 半 in the
// shipped corpus (boilerplate 版权所有 inflates it), which is more than
// completionPenalty can absorb. The alphabetical child scan used to hide
// this by never reaching the heavy child; the ranked scan surfaces it every
// time, so the promise completionPenalty documents — a prediction must never
// outrank something the user actually spelled — has to be enforced
// structurally: capped just under the best full-input reading, offered still.
func TestPredictionNeverOutranksASpelledWord(t *testing.T) {
	e := New(buildWideDict(t), nil, pinyin.Fuzzy{})

	got := e.Candidates("ni", 0)
	if len(got) == 0 || got[0].Word != "你" {
		t.Fatalf(`"ni" gives %v; the spelled 你 must hold the slot Space commits`, words(got))
	}
	if i := indexOf(got, "你祖"); i != 1 {
		t.Errorf(`"ni" gives %v; the heavy prediction belongs immediately below
the spelled word, not gone (%d)`, words(got), i)
	}
}
