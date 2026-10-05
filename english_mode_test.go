package main

import (
	"strings"
	"testing"

	"github.com/qizhanchan/q-ime/internal/engine"
)

// typeKeys presses a run of characters, one handleText call each, the way a
// user would. Key codes are not meaningful here — none of these are special
// keys, and the branches under test dispatch on the character.
func typeKeys(s *imeSession, keys string) {
	for _, r := range keys {
		s.handleText(noClient, string(r), keyA, 0)
	}
}

func candWords(cs []engine.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Word
	}
	return out
}

// TestCapitalStartsAnEnglishWord is the reported bug, keystroke for keystroke.
//
// Typing Shift+S then "he" used to produce a capital S handed straight to the
// host, followed by a FRESH PINYIN composition on "he" — which 或 won. The
// letters after the capital were not continuous with it, so "She" was
// unreachable.
//
// Pressing Shift is the clearest signal available that the next word is not
// Chinese, so it now switches the buffer over instead of ending it.
func TestCapitalStartsAnEnglishWord(t *testing.T) {
	s, _ := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	typeKeys(s, "She")

	if !s.englishMode {
		t.Fatal("typing She did not open an English word")
	}
	if s.buffer != "She" {
		t.Errorf("buffer is %q, want \"She\"", s.buffer)
	}
	// The preedit is a record of what was pressed, capitals included, with no
	// syllable separators invented for it.
	if got := s.preedit(); got != "She" {
		t.Errorf("preedit is %q, want \"She\"", got)
	}
	// Candidate 0 is what Space commits, and it must be exactly what was
	// typed — the whole point being that "She" is reachable at all.
	if len(s.cands) == 0 || s.cands[0].Word != "She" {
		t.Fatalf("candidates are %v, want \"She\" first", candWords(s.cands))
	}
	// Nothing Chinese may appear: the user has said this word is English.
	for _, c := range s.cands {
		if c.Source != engine.SourceEnglish {
			t.Errorf("candidate %q is not English (source %v)", c.Word, c.Source)
		}
	}
	// And the dictionary's own spelling is offered as an alternative.
	if i := indexOfWord(s.cands, "she"); i < 0 {
		t.Errorf("candidates are %v, want \"she\" offered too", candWords(s.cands))
	}
}

// TestEnglishWordOffersCanonicalCasing is the payoff of keying the table by
// what you TYPE rather than by spelling: the capitalisation nobody wants to
// reach for is one candidate away.
func TestEnglishWordOffersCanonicalCasing(t *testing.T) {
	s, _ := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	for _, tc := range []struct{ typed, want string }{
		{"Iphone", "iPhone"},
		{"Github", "GitHub"},
	} {
		s.reset(noClient)
		typeKeys(s, tc.typed)
		if s.cands[0].Word != tc.typed {
			t.Errorf("%q: candidate 0 is %q, want the literal text", tc.typed, s.cands[0].Word)
		}
		if indexOfWord(s.cands, tc.want) < 0 {
			t.Errorf("%q offered %v, want %q among them", tc.typed, candWords(s.cands), tc.want)
		}
	}
}

// TestApostropheIsLiteralInAnEnglishWord: inside pinyin the apostrophe forces a
// syllable boundary, which is meaningless in English and would break the
// contractions people actually type.
func TestApostropheIsLiteralInAnEnglishWord(t *testing.T) {
	s, _ := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	typeKeys(s, "Don't")

	if s.buffer != "Don't" {
		t.Errorf("buffer is %q, want \"Don't\"", s.buffer)
	}
	if got := s.preedit(); got != "Don't" {
		t.Errorf("preedit is %q, want \"Don't\" — the apostrophe is a letter here", got)
	}
	// typed() is what Enter inserts and what commit measures against. Stripping
	// the apostrophe there would insert "Dont".
	if got := s.typed(); got != "Don't" {
		t.Errorf("typed() is %q, want \"Don't\"", got)
	}
	if s.cands[0].Word != "Don't" {
		t.Errorf("candidate 0 is %q, want \"Don't\"", s.cands[0].Word)
	}
}

// TestBackspaceLeavesEnglishMode: deleting the word away must put the next
// letter back on the pinyin path, or one stray capital would strand the user in
// English until they committed something.
func TestBackspaceLeavesEnglishMode(t *testing.T) {
	s, _ := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	typeKeys(s, "She")
	for i := 0; i < 3; i++ {
		s.handleSpecialKey(noClient, keyBackspace, 0)
	}
	if s.englishMode || s.composing() {
		t.Fatalf("after deleting it all: buffer %q englishMode %v", s.buffer, s.englishMode)
	}

	// Back to pinyin: "ni" must find 你 again.
	typeKeys(s, "ni")
	if s.englishMode {
		t.Fatal("still in English mode after the word was deleted")
	}
	if len(s.cands) == 0 || s.cands[0].Word != "你" {
		t.Errorf("after leaving English mode, \"ni\" gives %v, want 你 first",
			candWords(s.cands))
	}
}

// TestEscapeLeavesEnglishMode: abandoning the word is not the same as
// committing it, and must not leave the mode behind either.
func TestEscapeLeavesEnglishMode(t *testing.T) {
	s, _ := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	typeKeys(s, "She")
	s.handleSpecialKey(noClient, keyEscape, 0)
	if s.englishMode || s.composing() {
		t.Errorf("after Escape: buffer %q englishMode %v", s.buffer, s.englishMode)
	}
}

// TestSpaceCommitsTheEnglishWordAndReturnsToPinyin: an English word is one
// word, not a mode the user has to leave by hand.
func TestSpaceCommitsTheEnglishWordAndReturnsToPinyin(t *testing.T) {
	s, _ := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	typeKeys(s, "She")
	s.handleText(noClient, " ", 49, 0)

	if s.englishMode || s.composing() {
		t.Fatalf("after Space: buffer %q englishMode %v", s.buffer, s.englishMode)
	}
	typeKeys(s, "hao")
	if s.englishMode {
		t.Error("the next word is still being treated as English")
	}
	if len(s.cands) == 0 || s.cands[0].Word != "好" {
		t.Errorf("after committing an English word, \"hao\" gives %v, want 好 first",
			candWords(s.cands))
	}
}

// TestLowercaseStartAloneIsStillPinyin is the boundary that keeps all of this
// safe. Only a CAPITAL opens an English word, so nothing about ordinary
// lowercase typing changes — and lowercase is every keystroke of Chinese input.
func TestLowercaseStartAloneIsStillPinyin(t *testing.T) {
	s, _ := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	for _, in := range []string{"nihao", "zhg", "women", "she", "hello"} {
		s.reset(noClient)
		typeKeys(s, in)
		if s.englishMode {
			t.Errorf("%q opened an English word; only a capital may do that", in)
		}
		if s.buffer != in {
			t.Errorf("%q left the buffer as %q", in, s.buffer)
		}
	}
	// And the pinyin ranking is untouched: lowercase "she" is still Chinese
	// first, with the English word merely offered.
	s.reset(noClient)
	typeKeys(s, "she")
	if s.cands[0].Source == engine.SourceEnglish {
		t.Errorf("lowercase \"she\" put English first: %v", candWords(s.cands))
	}
}

// TestCapitalMidWordJustExtends: "McDonald" has a capital in the middle, and it
// must not restart the word or flush what is already there.
func TestCapitalMidWordJustExtends(t *testing.T) {
	s, _ := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	typeKeys(s, "McD")
	if s.buffer != "McD" {
		t.Errorf("buffer is %q, want \"McD\"", s.buffer)
	}
	if strings.Count(s.preedit(), "M") != 1 {
		t.Errorf("preedit is %q; the word restarted instead of extending", s.preedit())
	}
}

func indexOfWord(cs []engine.Candidate, word string) int {
	for i, c := range cs {
		if c.Word == word {
			return i
		}
	}
	return -1
}
