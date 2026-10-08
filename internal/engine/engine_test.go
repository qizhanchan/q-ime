package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qizhanchan/q-ime/internal/dict"
	"github.com/qizhanchan/q-ime/internal/english"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// buildTestDict compiles a small lexicon in a temp dir.
//
// A hand-built dictionary rather than the real 77MB one: these tests are
// about the search, and asserting on the shipped lexicon would make them
// fail whenever a word's corpus frequency shifted. The behaviours below are
// properties of the engine, so they should be provable from a dozen entries.
func buildTestDict(t *testing.T) *dict.Reader {
	t.Helper()

	syllables := []string{
		"a", "an", "ban", "bei", "ce", "de", "ge", "guo", "hao", "hui",
		"jing", "me", "men", "mei", "ming", "na", "nian", "ni", "nin",
		"shan", "shang", "shi", "tian", "wo", "xi", "xian", "zhe", "zhong",
		// For the compose-vs-learned-words case below.
		"he", "li", "huo", "en", "lai", "o",
	}
	b := dict.NewBuilder(syllables)
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
	// Weights are chosen to encode the ratios the ranking rules have to
	// resolve, not to be realistic. In particular 年 outweighs 你, which is
	// true in the real corpus and is what partialPenalty exists to handle.
	add := func(w uint32, word string, syls ...string) { b.Add(ids(syls...), word, w) }
	add(3200000, "的", "de")
	add(1000000, "我", "wo")
	add(322000, "你", "ni")
	add(785000, "年", "nian")
	add(61000, "尼", "ni")
	add(900000, "我们", "wo", "men")
	add(25000, "我么", "wo", "me")
	add(800000, "你好", "ni", "hao")
	add(9000, "拟好", "ni", "hao")
	add(300000, "您好", "nin", "hao")
	add(700000, "中国", "zhong", "guo")
	add(650000, "这个", "zhe", "ge")
	add(400000, "北京", "bei", "jing")
	add(120000, "西安", "xi", "an")
	add(500000, "先", "xian")
	add(300000, "明天", "ming", "tian")
	add(250000, "测试", "ce", "shi")
	add(90000, "你们", "ni", "men")
	// coverBonus: 我们的 is 15x rarer than 我们, so for "womend" — the whole of
	// 我们 plus an onset for 的 — the extra letter has to be worth more than
	// ln15 ≈ 2.7 for the reading that explains it to win. See
	// TestExplainingEveryLetterBeatsAbandoningOne.
	add(60000, "我们的", "wo", "men", "de")
	// Three deliberately adversarial pairs, one per ranking rule. Each has
	// the WRONG answer winning on raw frequency, so the rule it targets is
	// the only thing keeping the right one on top — which is what makes the
	// corresponding test fail if that rule is weakened.
	add(200000, "山", "shan")        // partialPenalty: 上 is 10x commoner
	add(2000000, "上", "shang")      //
	add(300000, "半", "ban")         // syllablePenalty: 备案 is 10x commoner
	add(3000000, "备案", "bei", "an") //
	// coverBonus: 这 is commoner than 中国 and needs only two of "zhg".
	add(900000, "这", "zhe")
	// junctionPenalty: with 好 present, "nihao" can also be assembled as
	// 你 + 好, and 好 is commoner than the whole word 你好. Only the cost of
	// the word boundary keeps the real entry on top.
	add(3000000, "好", "hao")
	// A syllable's worth of homophones. Real Chinese syllables have dozens,
	// and that is the condition under which truncating a candidate list lands
	// on a low-scoring English word — see TestEnglishSurvivesTruncation. All
	// are ranked below 好 so nothing above depends on them.
	add(400000, "号", "hao")
	add(200000, "毫", "hao")
	add(150000, "豪", "hao")
	add(100000, "耗", "hao")
	// "hello" typed in Chinese mode: 合理 reads "hel" as he+li, while the
	// engine can also spell all five letters out as five single characters.
	// Those characters are individually common, which is what makes this the
	// shape that user-dictionary boosts used to amplify.
	add(400000, "合理", "he", "li")
	add(1500000, "或", "huo")
	add(1200000, "嗯", "en")
	add(1400000, "来", "lai")
	add(900000, "哦", "o")
	b.SetRefWeight(4000000)

	path := filepath.Join(t.TempDir(), "test.bin")
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

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return New(buildTestDict(t), nil, pinyin.Fuzzy{})
}

func TestContinuationsStripTheCommittedPrefix(t *testing.T) {
	e := newTestEngine(t)
	cs := e.Continuations("你", []string{"ni"}, "m", 10)
	i := indexOf(cs, "们")
	if i < 0 {
		t.Fatalf("after 你, `m` gives %v, want suffix 们 from 你们", words(cs))
	}
	c := cs[i]
	if c.Consumed != 1 {
		t.Errorf("Consumed = %d, want only the newly typed byte", c.Consumed)
	}
	if len(c.Reading) != 1 || c.Reading[0] != "men" {
		t.Errorf("Reading = %v, want [men]", c.Reading)
	}
	if len(c.Spans) != 1 || c.Spans[0] != 1 {
		t.Errorf("Spans = %v, want [1] relative to the new input", c.Spans)
	}
	if c.Source != SourceContext {
		t.Errorf("Source = %v, want SourceContext", c.Source)
	}
}

// TestContinuationsCarryTheLearnedPair pins the key the pair memory is read
// with. Picking a continuation commits only its suffix, so that is what the
// session records after the last word; looking the pair up under the whole
// phrase found nothing, and a continuation the user had picked ranked exactly as
// one they never had.
func TestContinuationsCarryTheLearnedPair(t *testing.T) {
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	e := New(buildTestDict(t), u, pinyin.Fuzzy{})
	score := func() float64 {
		t.Helper()
		e.SetContext("你")
		cs := e.Continuations("你", []string{"ni"}, "m", 10)
		i := indexOf(cs, "们")
		if i < 0 {
			t.Fatalf("after 你, `m` gives %v, want suffix 们", words(cs))
		}
		return cs[i].Score
	}
	before := score()
	u.RecordBigram("你", []string{"men"}, "们")
	if after := score(); after <= before {
		t.Errorf("们 after 你 scores %.2f with the pair recorded, %.2f without; want it higher",
			after, before)
	}
}

// words extracts just the candidate text, for readable assertions.
func words(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Word
	}
	return out
}

func indexOf(cs []Candidate, word string) int {
	for i, c := range cs {
		if c.Word == word {
			return i
		}
	}
	return -1
}

func TestTopCandidate(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		in   string
		want string
		why  string
	}{
		{"nihao", "你好", "the ordinary full-pinyin case"},
		{"wo", "我", "a single syllable resolves to its most common word"},
		{"women", "我们", "two full syllables"},
		{"zhg", "中国", "both syllables given as bare onsets"},
		{"beij", "北京", "second syllable abbreviated to its onset"},
		{"nih", "你好", "trailing onset completes the word"},
		{"niha", "你好", "trailing half-syllable completes the word"},
		{"xi'an", "西安", "an explicit separator forces the two-syllable reading"},
	}
	for _, tc := range cases {
		got := e.Candidates(tc.in, 8)
		if len(got) == 0 {
			t.Errorf("%s: %q returned nothing", tc.why, tc.in)
			continue
		}
		if got[0].Word != tc.want {
			t.Errorf("%s: %q → %v, want %q first", tc.why, tc.in, words(got), tc.want)
		}
	}
}

// TestPartialPenaltyBeatsFrequency covers the an/ang, en/eng and in/ing
// confusions, where the longer syllable is usually the commoner word. 上 is
// ten times commoner than 山, so the only thing keeping "shan" from
// resolving to 上 — which is not what was typed — is what it costs to
// override a syllable the user already finished.
func TestPartialPenaltyBeatsFrequency(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct{ in, want, loser string }{
		{"shan", "山", "上"},
		{"ni", "你", "年"},
	} {
		got := e.Candidates(tc.in, 10)
		if len(got) == 0 || got[0].Word != tc.want {
			t.Errorf("%q → %v, want %s first (%s is commoner but needs a partial reading)",
				tc.in, words(got), tc.want, tc.loser)
			continue
		}
		if i, j := indexOf(got, tc.want), indexOf(got, tc.loser); j >= 0 && j < i {
			t.Errorf("%s ranked above %s for %q", tc.loser, tc.want, tc.in)
		}
	}
}

// TestSyllablePenaltyPrefersTheSimplerReading: "ban" reads as one syllable
// (半) or as ba+n (备案), both covering all three bytes. 备案 is ten times
// commoner, so only the cost of positing an extra syllable keeps the simple
// reading on top.
func TestSyllablePenaltyPrefersTheSimplerReading(t *testing.T) {
	e := newTestEngine(t)
	got := e.Candidates("ban", 10)
	if len(got) == 0 || got[0].Word != "半" {
		t.Fatalf(`"ban" → %v, want 半 first (备案 is commoner but needs two syllables)`, words(got))
	}
}

// TestCoverageDominates: 中国 explains all of "zhg", 这 explains only "zh"
// and is the commoner word. Reading everything the user typed has to win.
func TestCoverageDominates(t *testing.T) {
	e := newTestEngine(t)
	got := e.Candidates("zhg", 10)
	if len(got) == 0 || got[0].Word != "中国" {
		t.Fatalf(`"zhg" → %v, want 中国 first (这 is commoner but leaves "g" unexplained)`, words(got))
	}
}

// TestDeadEndReadingsDropped covers the "ni" → 那 artifact: reading only the
// onset "n" strands an "i" that no syllable can start with, so that parse can
// never become a word and must not be offered.
func TestDeadEndReadingsDropped(t *testing.T) {
	e := newTestEngine(t)
	for _, c := range e.Candidates("ni", 20) {
		if c.Consumed < len("ni") && c.Source != SourceSentence {
			// Any surviving partial reading must leave input that could
			// still begin a syllable.
			t.Errorf("candidate %q consumes only %d of 2 bytes; the remainder cannot start a syllable",
				c.Word, c.Consumed)
		}
	}
}

// TestSentenceComposition is the property that separates this engine from a
// dictionary lookup: text no single entry contains, assembled correctly.
func TestSentenceComposition(t *testing.T) {
	e := newTestEngine(t)
	got := e.Candidates("womenmingtianbeijing", 5)
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	const want = "我们明天北京"
	if got[0].Word != want {
		t.Fatalf("→ %v, want %q first", words(got), want)
	}
	if got[0].Source != SourceSentence {
		t.Errorf("source = %v, want SourceSentence", got[0].Source)
	}
	if got[0].Consumed != len("womenmingtianbeijing") {
		t.Errorf("consumed %d bytes, want all %d", got[0].Consumed, len("womenmingtianbeijing"))
	}
}

// TestSentenceOffersHomophoneAlternates: a composed sentence with one wrong
// homophone used to be all-or-nothing — the Viterbi emits exactly one chain,
// and the user's only recourse was to abandon it and commit word by word.
// The alternates swap a single position to a runner-up at the same node, so
// 我们好 is followed by 我们号, sharing its segmentation and ranked strictly
// below it.
func TestSentenceOffersHomophoneAlternates(t *testing.T) {
	e := newTestEngine(t)
	got := e.Candidates("womenhao", 0)

	mi := indexOf(got, "我们好")
	if mi < 0 {
		t.Fatalf(`"womenhao" gives %v, want the sentence 我们好 offered`, words(got))
	}
	ai := indexOf(got, "我们号")
	if ai < 0 {
		t.Fatalf(`"womenhao" gives %v, want the homophone variant 我们号 offered`, words(got))
	}
	if ai < mi {
		t.Errorf("the variant (rank %d) outranked the chain it came from (rank %d)", ai, mi)
	}
	main, alt := got[mi], got[ai]
	if alt.Source != SourceSentence {
		t.Errorf("variant source = %v, want SourceSentence", alt.Source)
	}
	if alt.Score >= main.Score {
		t.Errorf("variant scored %.2f, not below the chain's %.2f", alt.Score, main.Score)
	}
	// Same segmentation: the variant varies a WORD, never the cut.
	if len(alt.Spans) != len(main.Spans) || len(alt.Reading) != len(main.Reading) {
		t.Errorf("variant segmentation %v/%v differs from the chain's %v/%v",
			alt.Reading, alt.Spans, main.Reading, main.Spans)
	}
	if len(alt.Words) != 2 || alt.Words[0] != "我们" || alt.Words[1] != "号" {
		t.Errorf("variant Words = %v, want [我们 号]", alt.Words)
	}
}

// TestSingleWordBeatsComposition guards the junction penalty. 你好 exists as
// one lexicon entry, and "nihao" also assembles as 你 + 好 out of two
// commoner pieces; the real word has to win.
func TestSingleWordBeatsComposition(t *testing.T) {
	e := newTestEngine(t)
	got := e.Candidates("nihao", 5)
	if len(got) == 0 || got[0].Word != "你好" {
		t.Fatalf(`"nihao" → %v, want the single entry 你好 first`, words(got))
	}
	if got[0].Source == SourceSentence {
		t.Errorf("你好 came back as an assembled sentence, not as the lexicon entry it is")
	}
}

func TestNoDuplicateWords(t *testing.T) {
	e := newTestEngine(t)
	// "nihao" reaches 你好 as an exact parse and again through the trailing
	// prefix arc; the accumulator must collapse those to one entry.
	seen := map[string]bool{}
	for _, c := range e.Candidates("nihao", 50) {
		if seen[c.Word] {
			t.Errorf("duplicate candidate %q", c.Word)
		}
		seen[c.Word] = true
	}
}

func TestReadingMatchesCandidate(t *testing.T) {
	e := newTestEngine(t)
	got := e.Candidates("nihao", 1)
	if len(got) != 1 {
		t.Fatal("expected one candidate")
	}
	want := []string{"ni", "hao"}
	if len(got[0].Reading) != len(want) {
		t.Fatalf("reading %v, want %v", got[0].Reading, want)
	}
	for i := range want {
		if got[0].Reading[i] != want[i] {
			t.Fatalf("reading %v, want %v", got[0].Reading, want)
		}
	}
}

func TestEmptyInput(t *testing.T) {
	e := newTestEngine(t)
	for _, in := range []string{"", "   ", "'"} {
		if got := e.Candidates(in, 5); len(got) != 0 {
			t.Errorf("Candidates(%q) = %v, want none", in, words(got))
		}
	}
}

func TestSeparatorForcesBoundary(t *testing.T) {
	e := newTestEngine(t)
	// Without the apostrophe "xian" prefers the single syllable 先.
	if got := e.Candidates("xian", 5); got[0].Word != "先" {
		t.Errorf(`"xian" → %v, want 先 first`, words(got))
	}
	// With it, the only legal reading is xi + an.
	got := e.Candidates("xi'an", 5)
	if got[0].Word != "西安" {
		t.Errorf(`"xi'an" → %v, want 西安 first`, words(got))
	}
	if indexOf(got, "先") >= 0 {
		t.Errorf(`"xi'an" offered 先, but the separator rules out the xian reading`)
	}
}

func TestUserDictPromotes(t *testing.T) {
	d := buildTestDict(t)
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	e := New(d, u, pinyin.Fuzzy{})

	// 拟好 is far rarer than 你好 and starts well below it.
	before := e.Candidates("nihao", 10)
	if indexOf(before, "拟好") <= 0 {
		t.Fatalf("expected 拟好 to start below the top; got %v", words(before))
	}
	for i := 0; i < 6; i++ {
		u.Record([]string{"ni", "hao"}, "拟好")
	}
	after := e.Candidates("nihao", 10)
	if after[0].Word != "拟好" {
		t.Errorf("after learning, %q → %v, want 拟好 first", "nihao", words(after))
	}
	if after[0].Source != SourceUser {
		t.Errorf("source = %v, want SourceUser", after[0].Source)
	}
}

func TestUserDictRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	u := NewUserDict(path)
	u.Record([]string{"ni", "hao"}, "你好")
	u.Record([]string{"ni", "hao"}, "你好")
	if err := u.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewUserDict(path)
	if reloaded.Len() != 1 {
		t.Fatalf("reloaded %d entries, want 1", reloaded.Len())
	}
	boost, ok := reloaded.Boost([]string{"ni", "hao"}, "你好")
	if !ok || boost <= 0 {
		t.Errorf("Boost = %v, %v; want a positive boost", boost, ok)
	}
	// A different reading for the same word must not inherit the history —
	// this is what keeps 行 learned as háng from affecting 行 as xíng.
	if _, ok := reloaded.Boost([]string{"ni", "hao", "a"}, "你好"); ok {
		t.Error("boost leaked across readings")
	}
}

func TestUserDictSurvivesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Must start empty rather than fail: losing learned frequencies is a far
	// better outcome than an input method that will not launch.
	if u := NewUserDict(path); u.Len() != 0 {
		t.Errorf("Len = %d, want 0", u.Len())
	}
}

func TestFuzzyMatching(t *testing.T) {
	d := buildTestDict(t)
	strict := New(d, nil, pinyin.Fuzzy{})
	// "zhonguo" misspells zhong as zhon... use the initial confusion instead:
	// "zongguo" should not reach 中国 without fuzzy rules on.
	if got := strict.Candidates("zongguo", 5); indexOf(got, "中国") >= 0 {
		t.Errorf("strict engine matched 中国 for %q: %v", "zongguo", words(got))
	}
	fuzzy := New(d, nil, pinyin.Fuzzy{ZhZ: true})
	if got := fuzzy.Candidates("zongguo", 5); indexOf(got, "中国") < 0 {
		t.Errorf("fuzzy engine missed 中国 for %q: %v", "zongguo", words(got))
	}
}

func TestLimitRespected(t *testing.T) {
	e := newTestEngine(t)
	if got := e.Candidates("ni", 3); len(got) > 3 {
		t.Errorf("got %d candidates, want at most 3", len(got))
	}
}

// TestCandidatesAreDeterministic guards against the list reshuffling between
// identical queries, which a map-ordered tiebreak would cause and which users
// experience as candidates jumping under their fingers.
func TestCandidatesAreDeterministic(t *testing.T) {
	e := newTestEngine(t)
	first := words(e.Candidates("nihao", 20))
	for i := 0; i < 5; i++ {
		got := words(e.Candidates("nihao", 20))
		if len(got) != len(first) {
			t.Fatalf("run %d returned %d candidates, first returned %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d differs at %d: %q vs %q", i, j, got[j], first[j])
			}
		}
	}
}

// TestLearnedWordsDoNotCompoundIntoSentences is the 或+嗯+来+来+哦 bug.
//
// A user-dictionary boost is calibrated to lift ONE word above its
// same-reading rivals. Applying one per word inside a composed sentence let a
// five-character chain collect five of them, and a handful of individually
// reasonable learned characters summed into a sentence nobody would want:
// with 39 real learned words, "hello" scored 或+嗯+来+来+哦 above 合理 by more
// than 2x.
func TestLearnedWordsDoNotCompoundIntoSentences(t *testing.T) {
	d := buildTestDict(t)
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	e := New(d, u, pinyin.Fuzzy{})

	before := e.Candidates("hello", 8)
	if len(before) == 0 || before[0].Word != "合理" {
		t.Fatalf(`"hello" → %v, want 合理 first even before learning`, words(before))
	}

	// Learn every single character the junk chain is made of. A realistic
	// handful of picks each — enough that the old code summed them into a
	// winning sentence, not so many that any one of them is expected to
	// outrank a longer match on its own.
	for i := 0; i < 3; i++ {
		u.Record([]string{"huo"}, "或")
		u.Record([]string{"en"}, "嗯")
		u.Record([]string{"lai"}, "来")
		u.Record([]string{"o"}, "哦")
	}

	after := e.Candidates("hello", 8)
	if len(after) == 0 {
		t.Fatal("no candidates after learning")
	}
	if after[0].Source == SourceSentence {
		t.Errorf("top candidate is an assembled sentence (%v), built by summing the boosts "+
			"of its learned characters", after[0].Words)
	}
	if after[0].Word != "合理" {
		t.Errorf("after learning single characters, %q → %v; the real word must still win",
			"hello", words(after))
	}
}

// TestSingletonWordsArePenalizedInSentences: a chain word that explains a
// single byte of input is one bare onset standing in for a whole word, which
// is no evidence at all. "zhg" → 中国 is two characters confirmed by two
// onsets; "t" → 他 is one character confirmed by nothing.
func TestSingletonWordsArePenalizedInSentences(t *testing.T) {
	e := newTestEngine(t)
	// Every word here is reachable, so the composer has a choice of cuts; the
	// one that leans on single-byte words must not be the one it picks.
	got := e.Candidates("hello", 8)
	for _, c := range got {
		if c.Source != SourceSentence {
			continue
		}
		singles := 0
		prev := 0
		for _, end := range c.Spans {
			if end-prev <= 1 {
				singles++
			}
			prev = end
		}
		if singles >= 3 && c.Score >= got[0].Score {
			t.Errorf("a chain of %d single-byte words (%v) tied or beat the top candidate",
				singles, c.Words)
		}
	}
}

// --- English candidates ----------------------------------------------

// englishTestDict is small on purpose. Each code is here to exercise one rule:
//
//	hello  — no clean Chinese reading exists, so English must win
//	hao    — a clean single-syllable reading exists, so English must lose
//	women  — a clean two-syllable reading exists, so English must lose
//	hell   — two words share one code, so their order must be preserved
func englishTestDict() *english.Dict {
	return english.Load(strings.Join([]string{
		"hao\thao",
		"hell\thell\the'll",
		"hello\thello",
		"women\twomen",
	}, "\n") + "\n")
}

func newEnglishEngine(t *testing.T) *Engine {
	t.Helper()
	e := newTestEngine(t)
	e.SetEnglish(englishTestDict())
	return e
}

// TestEnglishWinsWhenChineseMustBeForced is the feature: letters that spell an
// English word and do not spell any decent pinyin should come out as that
// English word.
//
// "hello" is the case that motivated this. Its best Chinese reading is 合理,
// which explains three of the five letters as an abbreviated he+li; the rest
// is force-fitted. Before the English list, the top of this list was whichever
// of those force-fits happened to score highest.
func TestEnglishWinsWhenChineseMustBeForced(t *testing.T) {
	e := newEnglishEngine(t)
	got := e.Candidates("hello", 5)
	if len(got) == 0 || got[0].Word != "hello" {
		t.Fatalf(`Candidates("hello") = %v; want "hello" first`, words(got))
	}
	if got[0].Source != SourceEnglish {
		t.Errorf("source = %v, want SourceEnglish", got[0].Source)
	}
	if got[0].Consumed != 5 {
		t.Errorf("consumed %d of 5 bytes", got[0].Consumed)
	}
}

// TestChineseWinsWhenTheReadingIsClean is the other half, and the one that
// makes the feature safe to have on all the time. Every short pinyin string is
// somebody's English word — hen, ban, song, men, man, women — and a user in
// Chinese mode typing those means the Chinese.
//
// This is what englishBase is calibrated against. Raise it far enough and this
// test fails while the one above still passes, which is precisely the
// regression to catch: the feature looks like it works and quietly ruins
// ordinary typing.
func TestChineseWinsWhenTheReadingIsClean(t *testing.T) {
	e := newEnglishEngine(t)
	cases := []struct{ input, want string }{
		{"hao", "好"},    // one syllable, no penalties, a common word
		{"women", "我们"}, // two syllables, exact, a common word
	}
	for _, tc := range cases {
		got := e.Candidates(tc.input, 8)
		if len(got) == 0 || got[0].Word != tc.want {
			t.Errorf("Candidates(%q) = %v; want %q first", tc.input, words(got), tc.want)
			continue
		}
		// Losing the top slot is right; vanishing is not. It has to be in the
		// list for the UI to be able to reserve it a place.
		if indexOf(got, tc.input) < 0 {
			t.Errorf("Candidates(%q) = %v; the English word is missing entirely",
				tc.input, words(got))
		}
	}
}

// TestEnglishMatchesOnlyWholeInput: a prefix is not a hit. While somebody is
// typing "hell" toward "hello", the candidate list must not fill up with
// English words that merely start with what they have typed.
func TestEnglishMatchesOnlyWholeInput(t *testing.T) {
	e := newEnglishEngine(t)
	for _, input := range []string{"hel", "hellox", "wom"} {
		for _, c := range e.Candidates(input, 20) {
			if c.Source == SourceEnglish {
				t.Errorf("Candidates(%q) offered English %q", input, c.Word)
			}
		}
	}
}

// TestEnglishAlternatesKeepTheirOrder: "hell" and "he'll" share a code, and
// the order rime-ice files them in — the weighted one first — has to survive
// the score sort.
func TestEnglishAlternatesKeepTheirOrder(t *testing.T) {
	e := newEnglishEngine(t)
	got := e.Candidates("hell", 20)
	first, second := indexOf(got, "hell"), indexOf(got, "he'll")
	if first < 0 || second < 0 {
		t.Fatalf(`Candidates("hell") = %v; want both "hell" and "he'll"`, words(got))
	}
	if first > second {
		t.Errorf(`"he'll" (%d) ranked above "hell" (%d)`, second, first)
	}
}

// TestEnglishCandidateDoesNotSegmentThePreedit guards the rule from the
// preedit fix: the underlined text is a record of what was pressed. An English
// candidate read nothing as a syllable, so it must claim no syllable
// boundaries — Spans nil, which is what makes the preedit show "hello" rather
// than a segmentation of it.
func TestEnglishCandidateDoesNotSegmentThePreedit(t *testing.T) {
	e := newEnglishEngine(t)
	for _, c := range e.Candidates("hello", 5) {
		if c.Source != SourceEnglish {
			continue
		}
		if c.Spans != nil {
			t.Errorf("English candidate %q carries spans %v", c.Word, c.Spans)
		}
		// Reading is the user-dictionary key, so it has to be the letters
		// themselves — there is no pinyin reading of "hello" to record.
		if len(c.Reading) != 1 || c.Reading[0] != "hello" {
			t.Errorf("English candidate reading = %v, want [hello]", c.Reading)
		}
	}
}

// TestNoEnglishListMeansNoEnglishCandidates: the list is optional, and an
// engine without one must behave exactly as it did before the feature.
func TestNoEnglishListMeansNoEnglishCandidates(t *testing.T) {
	e := newTestEngine(t) // no SetEnglish
	for _, c := range e.Candidates("hello", 20) {
		if c.Source == SourceEnglish {
			t.Fatalf("engine with no English list offered %q", c.Word)
		}
	}
	// And the pre-existing behaviour is unchanged: 合理 is still the best
	// Chinese reading of those letters.
	if got := e.Candidates("hello", 1); len(got) == 0 || got[0].Word != "合理" {
		t.Errorf(`without English, Candidates("hello") = %v, want 合理 first`, words(got))
	}
}

// TestEnglishBaseSitsBetweenForcedAndCleanReadings tests the reasoning behind
// englishBase rather than a behaviour that happens to follow from it.
//
// The claim in that constant's comment is that coverage cancels when an
// English hit is compared against a Chinese reading of the same letters, so
// what remains on the Chinese side — ln P(word) minus the force-fitting it
// took — is a single number that englishBase has to sit between: below a clean
// reading, above a forced one.
//
// Measuring both sides here is what makes the constant checkable. The
// behavioural tests above pass across a wide range of values; this one reports
// the actual headroom, and fails the moment englishBase drifts far enough in
// either direction to change which regime it belongs to.
func TestEnglishBaseSitsBetweenForcedAndCleanReadings(t *testing.T) {
	e := newTestEngine(t) // deliberately without the English list

	// residual strips the coverage term, leaving the quantity englishBase is
	// weighed against.
	residual := func(input string) float64 {
		got := e.Candidates(input, 1)
		if len(got) == 0 {
			t.Fatalf("Candidates(%q) found nothing", input)
		}
		return got[0].Score - coverBonus*float64(len(input))
	}

	clean := residual("women")  // 我们: exact, two syllables, common
	forced := residual("hello") // 合理: an abbreviated he+li explaining 3 of 5

	if forced >= clean {
		t.Fatalf("test lexicon is not adversarial: forced %.2f >= clean %.2f", forced, clean)
	}
	if englishBase <= forced || englishBase >= clean {
		t.Errorf("englishBase %.2f is outside (%.2f, %.2f): English would %s",
			englishBase, forced, clean,
			map[bool]string{true: "lose even where Chinese was forced",
				false: "beat clean Chinese readings"}[englishBase <= forced])
	}
	t.Logf("headroom: forced %.2f < englishBase %.2f < clean %.2f (%.2f / %.2f to spare)",
		forced, englishBase, clean, englishBase-forced, clean-englishBase)
}

// TestEnglishSurvivesTruncation: the candidate list is cut to a limit, and an
// exact English match must not be what gets cut.
//
// For a short input there are more Chinese readings above the English word
// than the limit allows, so the cut lands right on it. This was not
// hypothetical — 15 of the shipped list's short codes, including "js", "ssh"
// and "cd", vanished from a 60-candidate list this way, which also silently
// defeated the UI's reserved slot: it can only move a candidate that came back.
func TestEnglishSurvivesTruncation(t *testing.T) {
	e := newTestEngine(t)
	// "hao" has several Chinese candidates and one English word scoring far
	// below all of them — the shape of the problem, in miniature.
	e.SetEnglish(english.Load("hao\thao\n"))

	full := e.Candidates("hao", 0)
	deep := indexOf(full, "hao")
	if deep < 0 {
		t.Fatal(`the English word is missing from the unlimited list`)
	}
	if deep < 2 {
		t.Fatalf("test is vacuous: English already ranks %d, so no limit can cut it", deep)
	}

	for limit := 2; limit <= deep; limit++ {
		got := e.Candidates("hao", limit)
		if len(got) != limit {
			t.Errorf("limit %d returned %d candidates", limit, len(got))
		}
		if indexOf(got, "hao") < 0 {
			t.Errorf("limit %d dropped the English word: %v", limit, words(got))
		}
		// The slot given up is the last one, never the first: candidate 0 is
		// what Space commits.
		if len(got) > 0 && got[0].Source == SourceEnglish {
			t.Errorf("limit %d promoted English to the committed slot: %v", limit, words(got))
		}
	}

	// A caller asking for one candidate is asking which ranked highest, so it
	// must get the ranking's answer and not the English word.
	if got := e.Candidates("hao", 1); len(got) != 1 || got[0].Source == SourceEnglish {
		t.Errorf("limit 1 returned %v; want the top-ranked Chinese candidate", words(got))
	}
}

// --- user-dictionary boosts vs coverage ------------------------------

// TestLearnedShortWordDoesNotHijackLongerInput is the "sz" report: typing sz
// for 深圳 gave 是, because 是 had been picked eleven times.
//
//	是    shi        cover=1/2  score=12.03   boost 8.05
//	深圳  shen'zhen  cover=2/2  score=11.66   boost 6.46
//
// The boost on 是 was earned by typing the whole syllable "shi". Here the
// engine reached that reading from a bare "s" and then abandoned the "z", so it
// explained half the input using history gathered when it explained all of it.
//
// Reproduced in miniature: "wm" reads as wo+men (我们, all of it) or as wo
// alone (我, half of it), and 我 is the word with the history.
func TestLearnedShortWordDoesNotHijackLongerInput(t *testing.T) {
	e := newTestEngine(t)
	e.user = NewUserDict(filepath.Join(t.TempDir(), "user.json"))

	// Without any history 我们 leads, which is what makes this meaningful:
	// only the boost could overturn it.
	if got := e.Candidates("wm", 4); len(got) == 0 || got[0].Word != "我们" {
		t.Fatalf(`before learning, "wm" gives %v; want 我们 first`, words(got))
	}

	// Picked many times, the way a common single character actually gets used.
	// The count matters: the boost grows with it without bound, which is why
	// no fixed discount is safe and the rule is all-or-nothing.
	for i := 0; i < 200; i++ {
		e.user.Record([]string{"wo"}, "我")
	}

	got := e.Candidates("wm", 4)
	if len(got) == 0 || got[0].Word != "我们" {
		t.Errorf(`"wm" gives %v; want 我们 first — 我 explains only half of it`, words(got))
	}
	// Still offered, just not promoted.
	if indexOf(got, "我") < 0 {
		t.Errorf(`"wm" gives %v; the learned word must still be in the list`, words(got))
	}
}

// TestLearnedWordKeepsItsBoostAtFullCoverage is the other half. The rule must
// not quietly disable user adaptation, which is the case it exists for: a
// learned word that explains everything typed has to win even against a much
// commoner competitor.
//
// 你好 outweighs 拟好 by 89x here, so only the boost can flip it.
func TestLearnedWordKeepsItsBoostAtFullCoverage(t *testing.T) {
	e := newTestEngine(t)
	e.user = NewUserDict(filepath.Join(t.TempDir(), "user.json"))

	if got := e.Candidates("nihao", 4); len(got) == 0 || got[0].Word != "你好" {
		t.Fatalf(`before learning, "nihao" gives %v; want 你好 first`, words(got))
	}
	e.user.Record([]string{"ni", "hao"}, "拟好")

	got := e.Candidates("nihao", 4)
	if len(got) == 0 || got[0].Word != "拟好" {
		t.Errorf(`"nihao" gives %v; want the learned 拟好 first at full coverage`,
			words(got))
	}
}

// TestBoostIsLostPastTheInput pins the KNOWN COST of the all-or-nothing rule,
// so it is a decision on record rather than a surprise.
//
// One stray letter and a learned word stops being boosted: "nihaoo" leaves 拟好
// covering 5 of 6 bytes, so its history no longer applies and the commoner 你好
// takes the slot back. Accepted because the alternative — any fixed discount —
// stops working for whichever words a user types most, and stops working
// silently. See explainsEverything.
func TestBoostIsLostPastTheInput(t *testing.T) {
	e := newTestEngine(t)
	e.user = NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	e.user.Record([]string{"ni", "hao"}, "拟好")

	got := e.Candidates("nihaoo", 4)
	if len(got) == 0 || got[0].Word != "你好" {
		t.Errorf(`"nihaoo" gives %v; want 你好 first — the boost does not reach `+
			`a candidate that leaves input unexplained`, words(got))
	}
	if indexOf(got, "拟好") < 0 {
		t.Errorf(`"nihaoo" gives %v; 拟好 must still be offered`, words(got))
	}
}

// TestExplainsEverything pins the predicate, including the completion case,
// where a candidate runs PAST the input and must not be treated as partial.
func TestExplainsEverything(t *testing.T) {
	cases := []struct {
		consumed, total int
		want            bool
	}{
		{5, 5, true},  // exactly the input
		{6, 5, true},  // a prediction past it — 你们 for "ni"
		{1, 2, false}, // the "sz" case
		{2, 3, false},
		{5, 6, false}, // one stray letter; the documented cost
		{0, 1, false},
	}
	for _, tc := range cases {
		if got := explainsEverything(tc.consumed, tc.total); got != tc.want {
			t.Errorf("explainsEverything(%d, %d) = %v, want %v",
				tc.consumed, tc.total, got, tc.want)
		}
	}
}

// TestExplainingEveryLetterBeatsAbandoningOne is the reported case: typing
// "lihail" — 厉害 plus an onset for 了 — answered 厉害 and left the "l"
// unexplained.
//
// Both readings are legal and both stay in the list; what is at stake is which
// one leads. The letter was typed on purpose, so the reading that accounts for
// it should win unless explaining it took real force — which is what coverBonus
// is for, and at 5.0 it was not enough to overcome an ordinary corpus frequency
// gap. See the note on coverBonus for how the value was measured.
//
// Pinned here because the failure is silent: every other test passes at either
// value, and the difference only shows up as an input method that declines to
// read the last letter you typed.
func TestExplainingEveryLetterBeatsAbandoningOne(t *testing.T) {
	e := newTestEngine(t)

	cs := e.Candidates("womend", 0)
	if len(cs) == 0 {
		t.Fatal("no candidates for womend")
	}
	if cs[0].Word != "我们的" {
		t.Errorf("\"womend\" gives %v, want 我们的 first: the d was typed on purpose",
			words(cs[:min(5, len(cs))]))
	}
	// The abandoning reading is not thrown away — the user may still be
	// mid-syllable, and committing 我们 leaves "d" composing.
	if indexOf(cs, "我们") < 0 {
		t.Error("我们 disappeared; a partial reading is still a legal one")
	}
	// And with nothing extra typed, the plain word is still the answer.
	if cs := e.Candidates("women", 0); len(cs) == 0 || cs[0].Word != "我们" {
		t.Errorf("\"women\" gives %v, want 我们 first", words(cs))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
