package engine

import (
	"path/filepath"
	"testing"

	"github.com/qizhanchan/q-ime/internal/dict"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// buildTypoDict is a lexicon built for one question: when the letters a user
// typed are not a syllable, but swapping two of them makes one, what happens?
//
// Separate from buildTestDict because the interesting cases need syllable
// PAIRS that are transpositions of each other — diu/dui, an/na — and adding
// those to the shared fixture would change the rankings every other test
// asserts on.
//
// The weights are the point: 对 is six times commoner than 丢, and 那 as
// common as 安. So any test below that passes because the "wrong" answer was
// rare would pass for the wrong reason; here the wrong answer is the one raw
// frequency favours.
func buildTypoDict(t *testing.T) *dict.Reader {
	t.Helper()

	b := dict.NewBuilder([]string{"an", "diu", "dui", "guo", "hao", "na", "ni", "ping", "zhong"})
	ids := func(ss ...string) []uint16 {
		out := make([]uint16, len(ss))
		for i, s := range ss {
			id, ok := b.SyllableID(s)
			if !ok {
				t.Fatalf("test syllable %q missing from alphabet", s)
			}
			out[i] = id
		}
		return out
	}
	add := func(w uint32, word string, syls ...string) { b.Add(ids(syls...), word, w) }
	add(3000000, "对", "dui")
	add(500000, "丢", "diu")
	add(1000000, "安", "an")
	add(1000000, "那", "na")
	add(800000, "平", "ping")
	add(300000, "你", "ni")
	add(3000000, "好", "hao")
	add(800000, "你好", "ni", "hao")
	add(700000, "中国", "zhong", "guo")
	b.SetRefWeight(4000000)

	path := filepath.Join(t.TempDir(), "typo.bin")
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

func newTypoEngine(t *testing.T) *Engine {
	t.Helper()
	e := New(buildTypoDict(t), nil, pinyin.Fuzzy{})
	e.SetTypoCorrection(true)
	return e
}

// TestTypoCorrectionRecoversTransposedSyllable is the feature itself: a
// syllable typed with one adjacent pair swapped still finds its word.
func TestTypoCorrectionRecoversTransposedSyllable(t *testing.T) {
	e := newTypoEngine(t)
	cases := []struct {
		in   string
		want string
		why  string
	}{
		{"pign", "平", "the g and n of ping swapped — the commonest slip in Chinese typing"},
		{"pnig", "平", "the i and n swapped, same syllable"},
		{"nihoa", "你好", "the slip is in the second syllable of a two-syllable word"},
		{"nhoa", "你好", "a correction and an abbreviation in the same reading"},
	}
	for _, c := range cases {
		cands := e.Candidates(c.in, 10)
		if i := indexOf(cands, c.want); i < 0 {
			t.Errorf("%q: %q not offered at all (%s); got %v", c.in, c.want, c.why, words(cands))
		}
	}
}

// TestTypoCorrectionRecoversDroppedAndExtraLetters covers the other two edit
// kinds, which share the transposition machinery — same flag, same penalty,
// same one-slip lane — and differ only in the arcs the lattice offers.
func TestTypoCorrectionRecoversDroppedAndExtraLetters(t *testing.T) {
	e := newTypoEngine(t)
	cases := []struct {
		in   string
		want string
		why  string
	}{
		{"zhonguo", "中国", "the second g dropped: zhon repairs to zhong mid-input"},
		{"zhogguo", "中国", "the n dropped instead"},
		{"zhongguuo", "中国", "the u doubled: guuo repairs to guo"},
		{"zzhongguo", "中国", "the z doubled at the very front"},
	}
	for _, c := range cases {
		cands := e.Candidates(c.in, 10)
		i := indexOf(cands, c.want)
		if i < 0 {
			t.Errorf("%q: %q not offered (%s); got %v", c.in, c.want, c.why, words(cands))
			continue
		}
		if !cands[i].Corrected {
			t.Errorf("%q: %q not marked Corrected", c.in, c.want)
		}
	}

	// One slip per reading holds ACROSS edit kinds: a dropped letter in one
	// syllable plus an extra one in the next is two slips.
	if i := indexOf(e.Candidates("zhoguuo", 20), "中国"); i >= 0 {
		t.Errorf("\"zhoguuo\" offered 中国 at %d: a deletion and an insertion were both allowed", i)
	}
}

// TestDroppedLetterRepairDoesNotShadowCheaperReadings pins the arc ordering
// the lattice comment calls load-bearing. The chunks below are all reachable
// through the deletion index, but each has a cheaper explanation that must
// win the (position, node) memo: "zh" is an onset the abbreviation arc
// already covers, and a trailing "zhon" is a half-typed zhong before it is a
// mistyped one.
func TestDroppedLetterRepairDoesNotShadowCheaperReadings(t *testing.T) {
	e := newTypoEngine(t)
	for _, in := range []string{"zh", "zhon", "zhongg"} {
		for _, c := range e.Candidates(in, 20) {
			if c.Corrected {
				t.Errorf("%q offered %q as a CORRECTION; a cheaper reading exists and must win",
					in, c.Word)
			}
		}
	}
}

// TestTypoCorrectionMarksTheCandidate keeps the correction visible to the UI.
//
// The preedit shows what was TYPED, so a corrected candidate is the one place
// the user could be surprised; Corrected is how a caller can say so.
func TestTypoCorrectionMarksTheCandidate(t *testing.T) {
	e := newTypoEngine(t)
	for _, c := range e.Candidates("pign", 10) {
		if c.Word == "平" && !c.Corrected {
			t.Errorf("平 for \"pign\" is not marked Corrected")
		}
	}
	// And the flag is not set for input that needed no correction.
	for _, c := range e.Candidates("ping", 10) {
		if c.Corrected {
			t.Errorf("%q for \"ping\" marked Corrected though nothing was swapped", c.Word)
		}
	}
}

// TestTypoCorrectionSkipsRealSyllables is the load-bearing test.
//
// "diu" is a syllable, and swapping its last two letters gives "dui", whose
// word is six times commoner. Correcting here would mean a user who typed 丢
// correctly gets 对 — the exact failure that makes typo correction feel like
// the input method fighting back, and the reason rime-ice leaves d out of its
// ui/iu rule by hand.
//
// The guard is not "charge more for it": it is that the arc does not exist.
// A completed syllable is the user having said what they meant.
func TestTypoCorrectionSkipsRealSyllables(t *testing.T) {
	e := newTypoEngine(t)
	cands := e.Candidates("diu", 10)
	if len(cands) == 0 || cands[0].Word != "丢" {
		t.Fatalf("\"diu\": want 丢 first, got %v", words(cands))
	}
	if i := indexOf(cands, "对"); i >= 0 {
		t.Errorf("\"diu\" offered 对 at %d: a real syllable was re-read as another one", i)
	}
	// Same in the other direction, and for a pair where the two words are
	// equally common so nothing but the guard separates them.
	if i := indexOf(e.Candidates("an", 10), "那"); i >= 0 {
		t.Errorf("\"an\" offered 那 at %d", i)
	}
	if i := indexOf(e.Candidates("na", 10), "安"); i >= 0 {
		t.Errorf("\"na\" offered 安 at %d", i)
	}
}

// TestTypoCorrectionAllowsOneSlipPerReading pins maxTypos.
//
// Two transpositions is not reading the input any more, it is searching for
// what the input could have been. It also multiplies the arcs at every
// position, on the keystroke path.
func TestTypoCorrectionAllowsOneSlipPerReading(t *testing.T) {
	e := newTypoEngine(t)
	// "inhoa" is "nihao" with BOTH syllables transposed.
	if i := indexOf(e.Candidates("inhoa", 20), "你好"); i >= 0 {
		t.Errorf("\"inhoa\" offered 你好 at %d: two transpositions were allowed", i)
	}
	// One of the two is still enough.
	if i := indexOf(e.Candidates("nihoa", 20), "你好"); i < 0 {
		t.Errorf("\"nihoa\" lost 你好; one transposition must still work")
	}
}

// TestTypoCorrectionCostsExactlyOnePenalty checks the score arithmetic rather
// than a ranking, so it stays true if the lexicon or the other constants move.
//
// It deliberately does NOT pin the VALUE of typoPenalty — setting the constant
// to zero leaves this test green, which was confirmed by mutation. What it
// catches is the penalty being charged twice, or not at all, for one swap. The
// value itself cannot be defended from a dictionary this size; it comes from
// the two-sided audit against the shipped lexicon that the README records.
func TestTypoCorrectionCostsExactlyOnePenalty(t *testing.T) {
	e := newTypoEngine(t)
	exact := scoreOf(t, e.Candidates("ping", 10), "平")
	typo := scoreOf(t, e.Candidates("pign", 10), "平")
	if got := exact - typo; got < typoPenalty-1e-9 || got > typoPenalty+1e-9 {
		t.Errorf("correcting 平 cost %.4f, want exactly typoPenalty (%.4f)", got, typoPenalty)
	}
}

// TestTypoCorrectionIsOffUntilAskedFor keeps the engine's default honest: the
// app turns this on from prefs, and a caller that never mentions it gets the
// old behaviour exactly.
func TestTypoCorrectionIsOffUntilAskedFor(t *testing.T) {
	e := New(buildTypoDict(t), nil, pinyin.Fuzzy{})
	if e.TypoCorrection() {
		t.Errorf("transposition correction defaults to on")
	}
	if i := indexOf(e.Candidates("pign", 10), "平"); i >= 0 {
		t.Errorf("\"pign\" found 平 at %d with correction off", i)
	}
	if i := indexOf(e.Candidates("zhonguo", 10), "中国"); i >= 0 {
		t.Errorf("\"zhonguo\" found 中国 at %d with correction off — one flag gates every edit kind", i)
	}
}

func scoreOf(t *testing.T, cs []Candidate, word string) float64 {
	t.Helper()
	i := indexOf(cs, word)
	if i < 0 {
		t.Fatalf("%q not among %v", word, words(cs))
	}
	return cs[i].Score
}
