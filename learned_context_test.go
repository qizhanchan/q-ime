package main

import (
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/qizhanchan/q-ime/internal/engine"
)

// commitWord stands in for picking a candidate off the panel.
//
// The real path needs a live text client to insert into and there is no such
// thing in a test, so these drive the learning seam directly. What that leaves
// untested is the cgo insert; what it covers is every rule about WHAT gets
// remembered, which is where the bugs are.
func commitWord(s *imeSession, word string, reading ...string) {
	s.learn(engine.Candidate{
		Word:     word,
		Reading:  reading,
		Consumed: len(reading),
		Source:   engine.SourceLexicon,
	}, pickedByUser)
}

// TestTypingAPhraseTeachesItsContinuation is the feature end to end: say
// something once, and the second time an initial is enough.
func TestTypingAPhraseTeachesItsContinuation(t *testing.T) {
	s, _ := newLearningSession(t)

	// First time round, spelled out in full.
	commitWord(s, "真", "zhen")
	commitWord(s, "不错", "bu", "cuo")

	// A new sentence, and the same opening.
	s.endSentence()
	commitWord(s, "真", "zhen")

	s.reset(noClient)
	typeKeys(s, "b")
	if i := indexOfWord(s.cands, "不错"); i != 0 {
		t.Errorf("after 真, \"b\" gives %v, want 不错 first", candWords(s.cands[:min(6, len(s.cands))]))
	}
}

// TestCommittedWordOffersDictionaryPhraseContinuation covers the cold-start
// path. The user has committed 吃饭, so a following `l` should be able to reach
// the suffix of dictionary phrases such as 吃饭了吗 even before that exact
// pair has been learned in the user history.
func TestCommittedWordOffersDictionaryPhraseContinuation(t *testing.T) {
	s, _ := newLearningSession(t)
	commitWord(s, "吃饭", "chi", "fan")

	s.reset(noClient)
	typeKeys(s, "l")
	i := indexOfWord(s.cands, "了吗")
	if i < 0 {
		t.Fatalf("after 吃饭, `l` gives %v, want suffix candidate 了吗",
			candWords(s.cands[:min(8, len(s.cands))]))
	}
	if i >= s.pageSize() {
		t.Errorf("了吗 is at %d, off the first page of %d candidates", i, s.pageSize())
	}
	c := s.cands[i]
	if c.Consumed != 1 {
		t.Errorf("context suffix consumed %d bytes, want 1", c.Consumed)
	}
	if len(c.Reading) != 2 || c.Reading[0] != "le" || c.Reading[1] != "ma" {
		t.Errorf("context suffix reading = %v, want [le ma]", c.Reading)
	}
}

func TestCommittedWordOffersSingleCharacterContinuation(t *testing.T) {
	s, _ := newLearningSession(t)
	commitWord(s, "长", "chang")

	s.reset(noClient)
	typeKeys(s, "c")
	i := indexOfWord(s.cands, "城")
	if i < 0 {
		t.Fatalf("after 长, `c` gives %v, want 城", candWords(s.cands[:min(8, len(s.cands))]))
	}
	if i >= s.pageSize() {
		t.Errorf("城 is at %d, off the first page of %d candidates", i, s.pageSize())
	}
}

func TestCommittedWordOffersBadaLingContinuation(t *testing.T) {
	s, _ := newLearningSession(t)
	commitWord(s, "八达", "ba", "da")

	s.reset(noClient)
	typeKeys(s, "l")
	i := indexOfWord(s.cands, "岭")
	if i < 0 {
		t.Fatalf("after 八达, `l` gives %v, want 岭", candWords(s.cands[:min(8, len(s.cands))]))
	}
	if i >= s.pageSize() {
		t.Errorf("岭 is at %d, off the first page of %d candidates", i, s.pageSize())
	}
}

// TestASuffixTheUserHasPickedBeforeKeepsItsSlot is the reported bug.
//
// The list below is the real one, scores included, from a user dictionary that
// had already learned 来, 了, 理 and 岭 for `l`. 岭 arrives twice — as the suffix
// of 八达岭 (3.0) and as that learned candidate (5.7) — and the merge keeps the
// higher score, and with it SourceUser. Reading the label off the merged list
// therefore handed the slot to 岭镇, whose move pushed 岭 from the last place on
// the page to the first place off it: 八达岭 was one keystroke away when typed
// as `badal` and unreachable a keystroke after committing 八达.
func TestASuffixTheUserHasPickedBeforeKeepsItsSlot(t *testing.T) {
	base := []engine.Candidate{
		{Word: "来", Score: 14.4, Source: engine.SourceUser},
		{Word: "l", Score: 12.2, Source: engine.SourceUser},
		{Word: "了", Score: 11.7, Source: engine.SourceUser},
		{Word: "理", Score: 9.0, Source: engine.SourceUser},
		{Word: "岭", Score: 5.7, Source: engine.SourceUser},
		{Word: "里", Score: 4.0},
		{Word: "老", Score: 3.4},
	}
	cont := []engine.Candidate{
		{Word: "岭", Score: 3.0, Source: engine.SourceContext},
		{Word: "岭镇", Score: 2.9, Source: engine.SourceContext},
		{Word: "岭站", Score: 2.8, Source: engine.SourceContext},
	}

	cands := reserveContextSlot(mergeCandidates(base, cont), cont, 5)

	want := []string{"来", "l", "了", "岭", "岭镇"}
	if got := candWords(cands[:5]); !slices.Equal(got, want) {
		t.Fatalf("first page = %v, want %v", got, want)
	}
}

// TestTheSlotGoesToTheBestSuffixNotTheBestWord: 长 + `c` reaches eight
// single-character suffixes, and 长 itself (长长) outranks all of them in the
// merged list on its own merits as a candidate for `c`. Treating "some
// continuation is already visible" as good enough would leave 城 — the one
// anybody typing 长 then `c` means — off the page.
func TestTheSlotGoesToTheBestSuffixNotTheBestWord(t *testing.T) {
	base := []engine.Candidate{
		{Word: "成", Score: 3.28}, {Word: "从", Score: 3.18},
		{Word: "长", Score: 3.16}, {Word: "才", Score: 3.05},
		{Word: "此", Score: 3.03}, {Word: "城", Score: 3.00},
		{Word: "春", Score: 2.90}, {Word: "次", Score: 2.87},
	}
	cont := []engine.Candidate{
		{Word: "城", Score: 3.0, Source: engine.SourceContext},
		{Word: "春", Score: 2.9, Source: engine.SourceContext},
		{Word: "长", Score: 2.7, Source: engine.SourceContext},
	}

	cands := reserveContextSlot(mergeCandidates(base, cont), cont, 5)

	want := []string{"成", "从", "长", "城", "才"}
	if got := candWords(cands[:5]); !slices.Equal(got, want) {
		t.Fatalf("first page = %v, want %v", got, want)
	}
}

// TestAPhraseTypedInPiecesStillContinues is the case that stayed broken after
// the scoring fix: 博物 committed as ONE word could reach 博物馆, and the same
// three characters committed as 博 then 物 could not, because the only context
// the session kept was the last word — 物, which no phrase reads as 物馆.
//
// Continuity within one field is the whole promise. How many commits it took to
// put the text there is not something the user is thinking about.
func TestAPhraseTypedInPiecesStillContinues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts [][]string
		key   string
		want  string
	}{
		{"博 then 物", [][]string{{"博", "bo"}, {"物", "wu"}}, "guan", "馆"},
		{"天 then 安", [][]string{{"天", "tian"}, {"安", "an"}}, "men", "门"},
		{"八 then 达", [][]string{{"八", "ba"}, {"达", "da"}}, "ling", "岭"},
		{"图 then 书", [][]string{{"图", "tu"}, {"书", "shu"}}, "guan", "馆"},
		// Three pieces, so the longest run is only reachable with the joined
		// context — and 长城站 is the phrase, not 站馆.
		{"长 城 站", [][]string{{"长", "chang"}, {"城", "cheng"}, {"站", "zhan"}}, "t", "台"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newLearningSession(t)
			for _, p := range tc.parts {
				commitWord(s, p[0], p[1:]...)
			}
			s.reset(noClient)
			typeKeys(s, tc.key)
			i := indexOfWord(s.cands, tc.want)
			if i < 0 || i >= s.pageSize() {
				t.Errorf("%s + %q gives %v, want %s on the first page",
					tc.name, tc.key, candWords(s.cands[:min(s.pageSize(), len(s.cands))]), tc.want)
			}
		})
	}
}

// fakeClient is a non-nil stand-in for a text client. Never dereferenced: with
// s.insert and s.mark installed, nothing in the commit path reaches cgo, and the
// pointer only has to get past commit()'s "is there a client" guard.
var fakeClient = unsafe.Pointer(&struct{ _ byte }{})

// withFakeClient makes the commit path runnable in a test and records what was
// inserted into the document.
func withFakeClient(s *imeSession) *[]string {
	var got []string
	s.insert = func(_ unsafe.Pointer, text string) { got = append(got, text) }
	s.mark = func(unsafe.Pointer, string) {}
	s.retain = func(c unsafe.Pointer) unsafe.Pointer { return c }
	s.release = func(unsafe.Pointer) {}
	s.caret = func(unsafe.Pointer) (int, int, int, int, bool) { return 0, 0, 0, 0, false }
	s.setClient(fakeClient)
	return &got
}

// TestAPhraseBuiltInPiecesIsOfferedNextTime is the reported case end to end, and
// it is the pair of changes together: coining writes a word the lexicon does not
// have, and LearnedPhrases is what can read one back.
//
// 业务侧 was structurally unreachable when this was written. The stitched path
// offered exactly ONE candidate and 测 beats 侧 for `ce` by 0.37, so 业务侧
// never appeared; and every path that recorded anything recorded the PIECES
// (业务 under ye'wu, 侧 under ce), which no query for "yewuce" can assemble.
// Picking it the long way taught nothing that could ever be read back — so no
// number of repetitions helped.
//
// The sentence homophone alternates have since narrowed that wall: 业务侧 now
// shows up as a VARIANT of the 业务测 chain, a few slots down. What coining
// still uniquely provides — and what this test pins — is the other side:
// first place after building it once, and the learned boost that keeps it
// there.
func TestAPhraseBuiltInPiecesIsOfferedNextTime(t *testing.T) {
	s, u := newLearningSession(t)
	inserted := withFakeClient(s)

	// Before: not the answer. (It may appear further down as a sentence
	// variant — that is the alternates feature, not this one.)
	s.reset(noClient)
	typeKeys(s, "yewuce")
	if len(s.cands) > 0 && s.cands[0].Word == "业务侧" {
		t.Fatal("setup: 业务侧 already first, so this proves nothing")
	}

	// Build it the long way: pick 业务, which consumes four of six bytes and
	// leaves "ce" composing, then pick 侧.
	s.setClient(fakeClient)
	i := indexOfWord(s.cands, "业务")
	if i < 0 {
		t.Fatalf("no 业务 candidate for yewuce: %v", candWords(s.cands[:min(8, len(s.cands))]))
	}
	s.commit(i, pickedByUser)
	if s.buffer != "ce" {
		t.Fatalf("after committing 业务 the buffer is %q, want %q", s.buffer, "ce")
	}
	i = indexOfWord(s.cands, "侧")
	if i < 0 {
		t.Fatalf("no 侧 candidate for ce: %v", candWords(s.cands[:min(8, len(s.cands))]))
	}
	s.commit(i, pickedByUser)

	if got := strings.Join(*inserted, ""); got != "业务侧" {
		t.Errorf("document got %q, want 业务侧", got)
	}
	if _, ok := u.Boost([]string{"ye", "wu", "ce"}, "业务侧"); !ok {
		t.Fatal("the composition was not coined as a phrase")
	}

	// After: one long-way commit is enough.
	s.reset(noClient)
	typeKeys(s, "yewuce")
	if s.cands[0].Word != "业务侧" {
		t.Errorf("yewuce now gives %v, want 业务侧 first",
			candWords(s.cands[:min(6, len(s.cands))]))
	}
	// A half-typed last syllable reaches it too (the index is keyed by the
	// first letter), and so do per-syllable initials — matchReading grants a
	// learned word the same abbreviation rules the lattice grants every
	// lexicon word, so "ywc" works exactly like "zhg" does for 中国.
	for _, typed := range []string{"yewuc", "ywc"} {
		s.reset(noClient)
		typeKeys(s, typed)
		if indexOfWord(s.cands, "业务侧") < 0 {
			t.Errorf("%s gives %v, want 业务侧 among them", typed,
				candWords(s.cands[:min(8, len(s.cands))]))
		}
	}
}

// TestAFlushedCompositionIsNotCoined: the same rule as everywhere else. A
// composition that got out of the way of a comma is not a word anybody coined,
// and coining it would be the self-training loop again, one level up.
func TestAFlushedCompositionIsNotCoined(t *testing.T) {
	s, u := newLearningSession(t)
	withFakeClient(s)

	s.reset(noClient)
	typeKeys(s, "yewuce")
	s.setClient(fakeClient)
	i := indexOfWord(s.cands, "业务")
	if i < 0 {
		t.Fatal("no 业务 candidate")
	}
	s.commit(i, pickedByUser)
	// The rest of the buffer is flushed rather than picked.
	s.commit(s.sel, flushed)

	for _, w := range []string{"业务测", "业务侧", "业务册", "业务策"} {
		if _, ok := u.Boost([]string{"ye", "wu", "ce"}, w); ok {
			t.Errorf("%q was coined from a flushed composition", w)
		}
	}
}

// TestACoinedPhraseStopsAtFourSyllables: the cap. Without it a sentence
// committed in eight pieces becomes an eight-syllable "word" and outranks
// everything for that reading.
func TestACoinedPhraseStopsAtFourSyllables(t *testing.T) {
	s, u := newLearningSession(t)
	s.pendingPicked = true
	s.pending = []committedRun{
		{word: "我们", reading: []string{"wo", "men"}},
		{word: "明天", reading: []string{"ming", "tian"}},
		{word: "去", reading: []string{"qu"}},
	}
	s.coinPhrase(engine.Candidate{
		Word: "北京", Reading: []string{"bei", "jing"},
	}, pickedByUser)

	if _, ok := u.Boost([]string{"wo", "men", "ming", "tian", "qu", "bei", "jing"},
		"我们明天去北京"); ok {
		t.Error("a seven-syllable run was coined as one word")
	}
}

// TestShiftEnterRecordsAWordThePinyinSwallows is the reported dead end.
//
// `bigquery` reads as bi'g'qu'er'y and 比过去而言 covers all eight letters, so
// plain Enter confirms THAT — leaving no way to put the word in the document,
// let alone teach it. Shift+Enter is the unconditional escape hatch, and the
// literal it records is now offered back (see englishExact) rather than skipped
// for being spelled the same as what was typed.
func TestShiftEnterRecordsAWordThePinyinSwallows(t *testing.T) {
	s, _ := newLearningSession(t)
	inserted := withFakeClient(s)

	s.reset(noClient)
	typeKeys(s, "bigquery")
	if s.cands[0].Word == "bigquery" {
		t.Fatal("setup: the letters are already first, so this proves nothing")
	}

	s.setClient(fakeClient)
	s.handleText(fakeClient, "", keyReturn, modShift)

	if got := strings.Join(*inserted, ""); got != "bigquery" {
		t.Fatalf("Shift+Enter put %q in the document, want the letters", got)
	}

	s.reset(noClient)
	typeKeys(s, "bigquery")
	if s.cands[0].Word != "bigquery" {
		t.Errorf("after teaching it once, bigquery gives %v, want the word first",
			candWords(s.cands[:min(5, len(s.cands))]))
	}
}

// TestPlainEnterCommitsTheTypedLetters pins the rule that decides what every
// Enter in this input method does: the letters go in AS TYPED, whatever the
// candidate list is offering.
//
// `bada` is the case that used to go the other way — 八达 covers the whole
// buffer, so the old rule confirmed it — and it is exactly why the rule is
// gone. Whether a run of letters was meant as Chinese is not something the
// letters can tell you, so it is a key that says it: Space and the digits
// confirm Chinese, Enter does not.
func TestPlainEnterCommitsTheTypedLetters(t *testing.T) {
	s, _ := newLearningSession(t)
	inserted := withFakeClient(s)

	s.reset(noClient)
	typeKeys(s, "bada")
	if s.cands[0].Word == "bada" {
		t.Fatal("setup: the letters are already the top candidate, so this proves nothing")
	}

	s.setClient(fakeClient)
	s.handleText(fakeClient, "", keyReturn, 0)

	if got := strings.Join(*inserted, ""); got != "bada" {
		t.Fatalf("Enter put %q in the document, want the letters as typed", got)
	}
}

// TestSpaceStillConfirmsAChineseWord is the other half of the split, and the
// one that has to keep working: Enter giving up the Chinese path only makes
// sense if Space still has it, with the reading attached — 八达 followed by
// Space and then `l` still has to reach 岭, which needs the context.
func TestSpaceStillConfirmsAChineseWord(t *testing.T) {
	s, _ := newLearningSession(t)
	inserted := withFakeClient(s)

	s.reset(noClient)
	typeKeys(s, "bada")
	s.setClient(fakeClient)
	s.handleText(fakeClient, " ", keySpace, 0)

	got := strings.Join(*inserted, "")
	if got == "bada" {
		t.Fatalf("Space emitted the letters; Enter's rule leaked into it")
	}
	if s.lastWord != got || len(s.lastReading) == 0 {
		t.Errorf("after Space the context is %q/%v, want the committed word and its reading",
			s.lastWord, s.lastReading)
	}
}

// TestOnlyAPickTrainsTheDictionary is the rule: text that reached the document
// because the composition was in the way is not evidence of a preference.
//
// The two halves matter separately. A flush must NOT write to the dictionary —
// otherwise the top candidate trains itself every time a comma or a focus change
// interrupts a composition, which is how 博物→关 got recorded twice without
// anybody choosing 关. But a flush must still become the CONTEXT: the characters
// are on screen either way, and the next query has to be ranked against what is
// actually there.
//
// Driven at the learn seam rather than through the keys, for the reason given on
// commitWord: commit() bails without a live text client and a test has none. The
// wiring — which key passes which commitKind — is readable at the six call sites
// and is what a keyboard test would add.
func TestOnlyAPickTrainsTheDictionary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  commitKind
		learn bool
	}{
		{"Space, Enter, Tab, a digit, a click", pickedByUser, true},
		{"a punctuation mark or a focus change", flushed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, u := newLearningSession(t)
			commitWord(s, "博物", "bo", "wu")

			s.learn(engine.Candidate{
				Word: "关", Reading: []string{"guan"}, Consumed: 4,
				Source: engine.SourceLexicon,
			}, tc.kind)

			if _, ok := u.Boost([]string{"guan"}, "关"); ok != tc.learn {
				t.Errorf("guan→关 recorded = %v, want %v", ok, tc.learn)
			}
			pair := false
			for _, succ := range u.Successors("博物") {
				if succ.Word == "关" {
					pair = true
				}
			}
			if pair != tc.learn {
				t.Errorf("bigram 博物→关 recorded = %v, want %v", pair, tc.learn)
			}
			// Either way the text is on screen, so it is the context.
			if s.lastWord != "关" {
				t.Errorf("context after the commit is %q, want 关", s.lastWord)
			}
		})
	}
}

// TestShiftBackspaceForgetsTheHighlightedCandidate is the escape hatch for a
// wrong entry, and it is driven through the real key path — Shift+⌫ arriving at
// handleText — because the whole point is that a user can reach it.
//
// The scenario is the reported one, reproduced from its causes: 关 committed
// twice after 博物 records BOTH guan→关 and 博物→关, which is enough to put 关
// ahead of 馆 and keep it there. Outvoting that takes five deliberate picks; this
// takes one keystroke.
func TestShiftBackspaceForgetsTheHighlightedCandidate(t *testing.T) {
	s, u := newLearningSession(t)
	for i := 0; i < 2; i++ {
		commitWord(s, "博物", "bo", "wu")
		commitWord(s, "关", "guan")
		s.endSentence()
	}
	before := u.Len()

	commitWord(s, "博物", "bo", "wu")
	s.reset(noClient)
	typeKeys(s, "guan")
	if s.cands[0].Word != "关" {
		t.Fatalf("setup failed: 博物 + guan gives %v, want 关 first",
			candWords(s.cands[:min(5, len(s.cands))]))
	}

	// The key itself, with the buffer intact afterwards: this is not a delete.
	s.handleText(noClient, "", keyBackspace, modShift)

	if s.buffer != "guan" {
		t.Errorf("Shift+backspace ate the buffer: %q, want %q", s.buffer, "guan")
	}
	if s.cands[0].Word != "馆" {
		t.Errorf("after forgetting 关, 博物 + guan gives %v, want 馆 first",
			candWords(s.cands[:min(5, len(s.cands))]))
	}
	// Both namespaces, or 关 survives on its unigram boost.
	if _, ok := u.Boost([]string{"guan"}, "关"); ok {
		t.Error("guan→关 survived the forget")
	}
	for _, succ := range u.Successors("博物") {
		if succ.Word == "关" {
			t.Error("bigram 博物→关 survived the forget")
		}
	}
	if got := u.Len(); got != before-2 {
		t.Errorf("entries went from %d to %d, want %d removed", before, got, 2)
	}
}

// TestPlainBackspaceStillDeletesALetter guards the key this shares: without
// Shift it must stay an ordinary backspace.
func TestPlainBackspaceStillDeletesALetter(t *testing.T) {
	s, _ := newLearningSession(t)
	typeKeys(s, "guan")

	s.handleText(noClient, "", keyBackspace, 0)

	if s.buffer != "gua" {
		t.Errorf("backspace left %q, want %q", s.buffer, "gua")
	}
}

// TestAContinuationDoesNotReparseTheCommittedReading: 西 was typed as xi, so
// typing "an" after it means xi + an, never xian. Asking the lexicon about a
// bare "xian" ranks 先 现 显 线 above 西安 and spends the result window on
// candidates that cannot continue 西 at all.
func TestAContinuationDoesNotReparseTheCommittedReading(t *testing.T) {
	s, _ := newLearningSession(t)
	commitWord(s, "西", "xi")

	s.reset(noClient)
	typeKeys(s, "an")

	i := indexOfWord(s.cands, "安")
	if i < 0 || i >= s.pageSize() {
		t.Errorf("西 + \"an\" gives %v, want 安 on the first page",
			candWords(s.cands[:min(s.pageSize(), len(s.cands))]))
	}
}

func TestContextAndEnglishReservedSlotsCompose(t *testing.T) {
	cands := []engine.Candidate{
		{Word: "一"}, {Word: "二"}, {Word: "三"}, {Word: "四"}, {Word: "五"},
		{Word: "了吗", Source: engine.SourceContext},
		{Word: "hello", Source: engine.SourceEnglish},
		{Word: "六"},
	}
	cont := []engine.Candidate{{Word: "了吗", Source: engine.SourceContext}}
	cands = reserveContextSlot(cands, cont, 5)
	cands = reserveEnglishSlot(cands, 5)
	if cands[0].Word != "一" {
		t.Errorf("context reservation changed the Space candidate to %q", cands[0].Word)
	}
	if cands[3].Word != "了吗" || cands[4].Word != "hello" {
		t.Errorf("first page = %v, want context then English in the reserved slots",
			candWords(cands[:5]))
	}
}

func TestSingleCharacterContextSuffixIsVisible(t *testing.T) {
	cands := []engine.Candidate{
		{Word: "成"}, {Word: "从"}, {Word: "长"}, {Word: "才"}, {Word: "此"},
		{Word: "城", Source: engine.SourceContext},
	}
	cands = reserveContextSlot(cands, []engine.Candidate{{Word: "城"}}, 5)
	if cands[3].Word != "城" {
		t.Errorf("first page = %v, want single-character context suffix 城 in slot 4",
			candWords(cands[:5]))
	}
}

// TestAParticleFollowsItsWord is the cold-start case the lexicon's phrase
// weights get wrong: 删除了 is an entry, but at the floor weight of an n-gram
// list, so read as a transition probability it said 了 almost never follows 删除
// and the bare-letter favourite 来 took first.
func TestAParticleFollowsItsWord(t *testing.T) {
	cases := []struct {
		word    string
		reading []string
		typed   string
		want    string
	}{
		{"删除", []string{"shan", "chu"}, "l", "了"},
		{"修改", []string{"xiu", "gai"}, "l", "了"},
		{"完成", []string{"wan", "cheng"}, "l", "了"},
		{"去", []string{"qu"}, "g", "过"},
		{"看", []string{"kan"}, "z", "着"},
		{"慢慢", []string{"man", "man"}, "d", "地"},
		{"跑", []string{"pao"}, "de", "得"},
		{"好", []string{"hao"}, "b", "吧"},
		{"走", []string{"zou"}, "b", "吧"},
		{"好", []string{"hao"}, "n", "呢"},
	}
	for _, c := range cases {
		s, _ := newLearningSession(t)
		commitWord(s, c.word, c.reading...)
		s.reset(noClient)
		typeKeys(s, c.typed)
		if len(s.cands) == 0 || s.cands[0].Word != c.want {
			t.Errorf("after %s, %q gives %v, want %s first",
				c.word, c.typed, candWords(s.cands[:min(6, len(s.cands))]), c.want)
		}
	}
}

// TestTheParticleCreditSurvivesHistory is the same case for a user who has
// already typed both pairs: 删除→来 as often as 删除→了, and 来 for `l` more
// often overall. The pair memory is a tie and the lexicon's 删除了 is what should
// break it — which it did not while the credit was computed without the pair.
func TestTheParticleCreditSurvivesHistory(t *testing.T) {
	s, _ := newLearningSession(t)
	for i := 0; i < 3; i++ {
		commitWord(s, "删除", "shan", "chu")
		commitWord(s, "来", "lai")
		s.endSentence()
		commitWord(s, "删除", "shan", "chu")
		commitWord(s, "了", "le")
		s.endSentence()
		commitWord(s, "来", "lai")
		s.endSentence()
	}
	commitWord(s, "删除", "shan", "chu")
	s.reset(noClient)
	typeKeys(s, "l")
	if len(s.cands) == 0 || s.cands[0].Word != "了" {
		t.Errorf("after 删除, \"l\" gives %v, want 了 first", candWords(s.cands[:min(6, len(s.cands))]))
	}
}

// TestAPronounIsNotReadAsAPredicate is the other half: 我了 and 我们了 are
// lexicon entries too, and the particle credit must not turn 我 + `l` into 我了.
func TestAPronounIsNotReadAsAPredicate(t *testing.T) {
	for _, c := range []struct {
		word    string
		reading []string
	}{
		{"我", []string{"wo"}},
		{"他", []string{"ta"}},
		{"我们", []string{"wo", "men"}},
	} {
		s, _ := newLearningSession(t)
		commitWord(s, c.word, c.reading...)
		s.reset(noClient)
		typeKeys(s, "l")
		if len(s.cands) == 0 || s.cands[0].Word != "来" {
			t.Errorf("after %s, \"l\" gives %v, want 来 first",
				c.word, candWords(s.cands[:min(6, len(s.cands))]))
		}
	}
}

// TestOneMoreLetterDropsAContinuationThatWasWrong is the escape hatch, and the
// reason a remembered continuation is allowed near the top at all: it costs one
// keystroke to be rid of, and no gesture anyone has to learn.
func TestOneMoreLetterDropsAContinuationThatWasWrong(t *testing.T) {
	s, _ := newLearningSession(t)

	commitWord(s, "真", "zhen")
	commitWord(s, "不错", "bu", "cuo")
	s.endSentence()
	commitWord(s, "真", "zhen")

	s.reset(noClient)
	typeKeys(s, "ba")
	if i := indexOfWord(s.cands, "不错"); i >= 0 {
		t.Errorf("\"ba\" still offers 不错 at %d; 不错 does not begin that way", i)
	}
	if len(s.cands) == 0 || s.cands[0].Word != "把" {
		t.Errorf("\"ba\" gives %v, want 把 first", candWords(s.cands[:min(6, len(s.cands))]))
	}
}

// TestContextSurvivesTheResetThatEndsACommit is the wiring detail everything
// else depends on.
//
// reset() runs at the end of every commit — it is what clears the buffer and
// hides the panel. If it cleared the context too, the memory would be wiped a
// microsecond after being written, at the exact moment it becomes useful, and
// every test above would pass while the running input method learned nothing.
func TestContextSurvivesTheResetThatEndsACommit(t *testing.T) {
	s, _ := newLearningSession(t)

	commitWord(s, "真", "zhen")
	s.reset(noClient)
	if s.lastWord != "真" {
		t.Errorf("context after reset is %q, want 真", s.lastWord)
	}
}

// TestASentenceEndingMarkClearsTheContext: 。 finishes the thought, and pairing
// the next word with the last one before it would train a phrase that never
// existed. A comma does not — "我们，明天…" is one sentence.
func TestASentenceEndingMarkClearsTheContext(t *testing.T) {
	for _, tc := range []struct {
		mark  string
		clear bool
	}{
		{".", true},
		{"?", true},
		{",", false},
	} {
		s, _ := newLearningSession(t)
		s.prefs.ChinesePunctuation = true
		commitWord(s, "真", "zhen")

		s.handleText(noClient, tc.mark, keyA, 0)

		if got := s.lastWord == ""; got != tc.clear {
			t.Errorf("after %q the context is %q (cleared=%v), want cleared=%v",
				tc.mark, s.lastWord, got, tc.clear)
		}
	}
}

// TestFocusLeavingClearsTheContext: the next thing typed is in another
// document, and a pair spanning two of them is not a phrase.
func TestFocusLeavingClearsTheContext(t *testing.T) {
	s, _ := newLearningSession(t)
	commitWord(s, "真", "zhen")

	s.deactivate(noClient)

	if s.lastWord != "" {
		t.Errorf("context after focus left is %q, want it cleared", s.lastWord)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
