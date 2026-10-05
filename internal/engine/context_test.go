package engine

import (
	"path/filepath"
	"testing"

	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// The context memory is the transition probability the Viterbi never had: the
// junction between two words has always cost a flat junctionPenalty, which says
// a boundary is expensive but nothing about WHICH words meet at one.
//
// It is the user's own, not a corpus. That makes it free of a second data
// source and a second licence, and useless the first time — both of which are
// the point, and both of which these tests pin.

// The fixture pair is a word the test lexicon DOES NOT CONTAIN, spelled out of
// syllables it does — the shape a user produces by committing a stitched
// sentence.
//
// Chosen that way so "the context produced this" cannot be mistaken for "the
// search would have found it anyway". In the shipped lexicon the interesting
// case is subtler — 不错 is a real entry that the completion fan-out simply
// never reaches from `b`, verified at sixty results — but a hand-built
// dictionary of thirty words cannot reproduce a fan-out limit, and a test that
// quietly stops testing its own premise is worse than one that overstates it.
const (
	fixtureWord = "你们好"
	fixturePrev = "我"
)

var fixtureReading = []string{"ni", "men", "hao"}

func learnFixture(u *UserDict) { u.RecordBigram(fixturePrev, fixtureReading, fixtureWord) }

func newContextEngine(t *testing.T, learn func(*UserDict)) *Engine {
	t.Helper()
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	learn(u)
	return New(buildTestDict(t), u, pinyin.Fuzzy{})
}

// TestContextOffersAWordTheSearchCannotReach is the case the whole design turns
// on, and the reason readings are stored with the pair.
//
// Typing one letter after a word asks for a three-syllable continuation out of
// a single onset. No search here can produce it, so the context has to OFFER
// the word rather than reorder it — which means knowing how it is spelled.
func TestContextOffersAWordTheSearchCannotReach(t *testing.T) {
	e := newContextEngine(t, learnFixture)

	if i := indexOf(e.Candidates("n", 0), fixtureWord); i >= 0 {
		t.Fatalf("%q already reachable at %d; the test proves nothing", fixtureWord, i)
	}

	e.SetContext(fixturePrev)
	cs := e.Candidates("n", 0)
	i := indexOf(cs, fixtureWord)
	if i < 0 {
		t.Fatalf("after %s, \"n\" gives %v, want %s offered", fixturePrev, words(cs), fixtureWord)
	}
	if cs[i].Source != SourceUser {
		t.Errorf("context candidate has source %v, want SourceUser", cs[i].Source)
	}
	if cs[i].Consumed != 1 {
		t.Errorf("Consumed is %d, want the one letter typed", cs[i].Consumed)
	}
	// The preedit is built from Spans and must show the letter that was
	// pressed, never the syllables it was understood as.
	if len(cs[i].Spans) != 1 || cs[i].Spans[0] != 1 {
		t.Errorf("Spans are %v, want just the typed letter [1]", cs[i].Spans)
	}
}

// TestContextCandidateVanishesWhenTheInputContradictsIt is the escape hatch,
// and it is what makes offering a guess near the top defensible: one more
// letter and the guess is gone, with no gesture to learn.
func TestContextCandidateVanishesWhenTheInputContradictsIt(t *testing.T) {
	e := newContextEngine(t, learnFixture)
	e.SetContext(fixturePrev)

	for _, typed := range []string{"n", "ni", "nim", "nimen", "nimenh", "nimenhao"} {
		if indexOf(e.Candidates(typed, 0), fixtureWord) < 0 {
			t.Errorf("%q does not offer %s, but it is consistent with ni'men'hao",
				typed, fixtureWord)
		}
	}
	for _, typed := range []string{"h", "hao", "nih", "na", "nimeng"} {
		if i := indexOf(e.Candidates(typed, 0), fixtureWord); i >= 0 {
			t.Errorf("%q still offers %s at %d; the input rules that word out",
				typed, fixtureWord, i)
		}
	}
}

// TestContextRanksWhatWasActuallyTyped: the boost is not a licence to ignore
// the keyboard. A remembered continuation must lose to a word the user has
// spelled out in full.
func TestContextRanksWhatWasActuallyTyped(t *testing.T) {
	e := newContextEngine(t, learnFixture)
	e.SetContext(fixturePrev)

	cs := e.Candidates("nihao", 0)
	if len(cs) == 0 || cs[0].Word != "你好" {
		t.Errorf("after %s, \"nihao\" gives %v, want 你好 first", fixturePrev, words(cs))
	}
}

// TestContextIsClearedAtASentenceBoundary: "" means the start of a sentence,
// and a sentence start continues nothing.
func TestContextIsClearedAtASentenceBoundary(t *testing.T) {
	e := newContextEngine(t, learnFixture)

	e.SetContext(fixturePrev)
	if indexOf(e.Candidates("n", 0), fixtureWord) < 0 {
		t.Fatal("context did not take effect")
	}
	e.SetContext("")
	if i := indexOf(e.Candidates("n", 0), fixtureWord); i >= 0 {
		t.Errorf("\"n\" still offers %s at %d with no context", fixtureWord, i)
	}
}

// TestContextSeesAPairRecordedUnderTheSameWord is the generation check.
//
// The successor cache is refreshed when the context CHANGES, and committing the
// same word twice running does not change it. Without a generation counter the
// pair just recorded would be invisible until some other word intervened —
// which for a user typing 好, 好 is exactly the moment they are looking.
func TestContextSeesAPairRecordedUnderTheSameWord(t *testing.T) {
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	e := New(buildTestDict(t), u, pinyin.Fuzzy{})

	e.SetContext(fixturePrev)
	if indexOf(e.Candidates("n", 0), fixtureWord) >= 0 {
		t.Fatalf("%s offered before anything was recorded", fixtureWord)
	}
	learnFixture(u)
	e.SetContext(fixturePrev) // same word: only the generation says anything changed

	if indexOf(e.Candidates("n", 0), fixtureWord) < 0 {
		t.Error("a pair recorded under the current context stayed invisible")
	}
}

// TestContextSurvivesAReload: the whole point is a memory that outlives the
// process. The successor index is derived, so this also pins that it is rebuilt
// from the file.
func TestContextSurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	u := NewUserDict(path)
	learnFixture(u)
	if err := u.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ss := NewUserDict(path).Successors(fixturePrev)
	if len(ss) != 1 || ss[0].Word != fixtureWord {
		t.Fatalf("after reload Successors = %v, want %s", ss, fixtureWord)
	}
	if got := ss[0].Reading; len(got) != 3 || got[0] != "ni" || got[2] != "hao" {
		t.Errorf("reading came back as %v, want [ni men hao]", got)
	}
}

// TestContextDoesNotLeakIntoTheOtherNamespaces: three kinds of memory share one
// map and one file, and a key that lands in the wrong one is a word offered for
// something the user never typed.
func TestContextDoesNotLeakIntoTheOtherNamespaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	u := NewUserDict(path)
	u.Record([]string{"ni"}, "你")
	u.RecordLiteral("ok", "OK")
	u.RecordBigram("你", []string{"ok"}, "OK") // an ASCII word in a bigram key
	if err := u.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reloading runs the literal migration, which rewrites ASCII-worded reading
	// keys. It must not touch this one.
	r := NewUserDict(path)
	if ss := r.Successors("你"); len(ss) != 1 || ss[0].Word != "OK" {
		t.Errorf("Successors(\"你\") = %v, want the recorded pair", ss)
	}
	if ls := r.LiteralsFor("ok"); len(ls) != 1 || ls[0].Word != "OK" {
		t.Errorf("LiteralsFor(\"ok\") = %v, want just the literal", ls)
	}
	if _, ok := r.Boost([]string{"ni"}, "你"); !ok {
		t.Error("the reading entry did not survive")
	}
}

func TestMatchReadingAcceptsExactlyWhatTheLatticeWould(t *testing.T) {
	reading := []string{"bu", "cuo"}
	for _, tc := range []struct {
		in              string
		ok              bool
		abbrev, partial int
		spans           []int
		name            string
	}{
		{"b", true, 1, 0, []int{1}, "onset standing in for the first syllable"},
		{"bu", true, 0, 0, []int{2}, "first syllable whole, second predicted"},
		{"buc", true, 1, 0, []int{2, 3}, "second syllable as an onset"},
		{"bucu", true, 0, 1, []int{2, 4}, "second syllable half typed"},
		{"bucuo", true, 0, 0, []int{2, 5}, "the whole word"},
		{"bc", true, 2, 0, []int{1, 2}, "both syllables as onsets"},
		{"bcuo", true, 1, 0, []int{1, 4}, "onset then a whole syllable"},
		{"ba", false, 0, 0, nil, "a letter the word cannot explain"},
		{"bucuoa", false, 0, 0, nil, "input past the end of the word"},
		{"bcu", true, 1, 1, []int{1, 3}, "onset then a half-typed syllable"},
		{"c", false, 0, 0, nil, "starts somewhere else entirely"},
	} {
		spans, abbrev, partial, ok := matchReading(tc.in, reading)
		if ok != tc.ok {
			t.Errorf("%s: matchReading(%q) ok=%v, want %v", tc.name, tc.in, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if abbrev != tc.abbrev || partial != tc.partial {
			t.Errorf("%s: %q gave abbrev=%d partial=%d, want %d/%d",
				tc.name, tc.in, abbrev, partial, tc.abbrev, tc.partial)
		}
		if len(spans) != len(tc.spans) {
			t.Errorf("%s: %q gave spans %v, want %v", tc.name, tc.in, spans, tc.spans)
			continue
		}
		for i := range spans {
			if spans[i] != tc.spans[i] {
				t.Errorf("%s: %q gave spans %v, want %v", tc.name, tc.in, spans, tc.spans)
				break
			}
		}
	}
}

// TestMatchReadingPerSyllableInitials: `ywc` has to reach a word read
// ye'wu'ce, because that is the abbreviation the lattice grants every lexicon
// word and a learned word must be offered under the same rules. Also pins the
// two-letter onsets (zh/ch/sh) and the backtracking they need: reading "zg"
// against zhong'guo only works if the failed whole-syllable attempt at "z…"
// gives the onset reading a turn.
func TestMatchReadingPerSyllableInitials(t *testing.T) {
	for _, tc := range []struct {
		reading         []string
		in              string
		ok              bool
		abbrev, partial int
		spans           []int
		name            string
	}{
		{[]string{"ye", "wu", "ce"}, "ywc", true, 3, 0, []int{1, 2, 3}, "one initial per syllable"},
		{[]string{"ye", "wu", "ce"}, "ywce", true, 2, 0, []int{1, 2, 4}, "initials then a whole syllable"},
		{[]string{"ye", "wu", "ce"}, "yewc", true, 2, 0, []int{2, 3, 4}, "whole syllable then initials"},
		{[]string{"ye", "wu", "ce"}, "ywa", false, 0, 0, nil, "a letter no syllable explains"},
		{[]string{"zhong", "guo"}, "zhg", true, 2, 0, []int{2, 3}, "two-letter onset"},
		{[]string{"zhong", "guo"}, "zg", true, 2, 0, []int{1, 2}, "one letter of a two-letter onset"},
		{[]string{"zhong", "guo"}, "zhongg", true, 1, 0, []int{5, 6}, "whole then onset"},
		{[]string{"ya", "an"}, "yan", true, 1, 0, []int{1, 3}, "backtracking: ya+n dead-ends, y+an reads"},
	} {
		spans, abbrev, partial, ok := matchReading(tc.in, tc.reading)
		if ok != tc.ok {
			t.Errorf("%s: matchReading(%q, %v) ok=%v, want %v",
				tc.name, tc.in, tc.reading, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if abbrev != tc.abbrev || partial != tc.partial {
			t.Errorf("%s: %q gave abbrev=%d partial=%d, want %d/%d",
				tc.name, tc.in, abbrev, partial, tc.abbrev, tc.partial)
		}
		if len(spans) != len(tc.spans) {
			t.Errorf("%s: %q gave spans %v, want %v", tc.name, tc.in, spans, tc.spans)
			continue
		}
		for i := range spans {
			if spans[i] != tc.spans[i] {
				t.Errorf("%s: %q gave spans %v, want %v", tc.name, tc.in, spans, tc.spans)
				break
			}
		}
	}
}

// TestContextCandidateReachableByPerSyllableInitials extends the escape-hatch
// test above to full abbreviations: after 我, typing "nmh" is consistent with
// ni'men'hao syllable by syllable, so the remembered word has to be offered —
// and one letter that contradicts any syllable still rules it out.
func TestContextCandidateReachableByPerSyllableInitials(t *testing.T) {
	e := newContextEngine(t, learnFixture)
	e.SetContext(fixturePrev)

	for _, typed := range []string{"nm", "nmh", "nimh", "nmhao", "nmha"} {
		if indexOf(e.Candidates(typed, 0), fixtureWord) < 0 {
			t.Errorf("%q does not offer %s, but it abbreviates ni'men'hao legally",
				typed, fixtureWord)
		}
	}
	for _, typed := range []string{"nmg", "nhm", "mnh"} {
		if i := indexOf(e.Candidates(typed, 0), fixtureWord); i >= 0 {
			t.Errorf("%q still offers %s at %d; the input rules that word out",
				typed, fixtureWord, i)
		}
	}
}

// TestLearnedPhraseReachableByPerSyllableInitials is the 业务侧 case at the
// engine seam: a word the lexicon does not contain, coined by the user, must
// answer to the same full abbreviation any lexicon word would.
func TestLearnedPhraseReachableByPerSyllableInitials(t *testing.T) {
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	u.RecordCoined([]string{"ye", "wu", "ce"}, "业务侧")
	e := New(buildTestDict(t), u, pinyin.Fuzzy{})

	for _, typed := range []string{"ywc", "yewuc", "ywce"} {
		cs := e.Candidates(typed, 0)
		i := indexOf(cs, "业务侧")
		if i < 0 {
			t.Errorf("%q gives %v, want the coined 业务侧 offered", typed, words(cs))
			continue
		}
		if cs[i].Source != SourceUser {
			t.Errorf("%q: coined word has source %v, want SourceUser", typed, cs[i].Source)
		}
	}
	if i := indexOf(e.Candidates("ywa", 0), "业务侧"); i >= 0 {
		t.Errorf(`"ywa" offers 业务侧 at %d; the a contradicts every syllable`, i)
	}
}
