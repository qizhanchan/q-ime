package main

import (
	"path/filepath"
	"testing"

	"github.com/qizhanchan/q-ime/internal/emoji"
	"github.com/qizhanchan/q-ime/internal/engine"
	"github.com/qizhanchan/q-ime/internal/english"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// newLearningSession is a session whose engine and history are the same
// dictionary, so a commit in one keystroke is visible to the next.
//
// The default test session shares no history at all (engine.New with a nil user
// dict), which is why "it learns" was testable nowhere: every test proved the
// candidate list for a user who had never typed anything.
func newLearningSession(t *testing.T) (*imeSession, *engine.UserDict) {
	t.Helper()
	d, err := openLexicon()
	if err != nil {
		// The lexicon is GPL data compiled on the developer's machine and is
		// not in the repo, so its absence is normal, not a failure.
		t.Skipf("no compiled lexicon: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	u := engine.NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	e := engine.New(d, u, pinyin.Fuzzy{})
	en, err := english.Builtin()
	if err != nil {
		t.Fatalf("english.Builtin: %v", err)
	}
	e.SetEnglish(en)
	em, err := emoji.Builtin()
	if err != nil {
		t.Fatalf("emoji.Builtin: %v", err)
	}
	e.SetEmoji(em, true)

	s, _ := newShiftTestSession(t)
	s.eng, s.user = e, u
	return s, u
}

// TestBrandTypedOnceIsOfferedNextTime is the reported bug, end to end: a term
// no dictionary can contain — a company name — typed the long way once, and
// then reachable from a prefix.
func TestBrandTypedOnceIsOfferedNextTime(t *testing.T) {
	s, _ := newLearningSession(t)

	typeKeys(s, "AfterShip")
	s.handleSpecialKey(noClient, keyReturn, 0)

	s.reset(noClient)
	typeKeys(s, "Aftersh")
	if indexOfWord(s.cands, "AfterShip") < 0 {
		t.Errorf("after typing it once, \"Aftersh\" offers %v, want AfterShip",
			candWords(s.cands))
	}
}

// TestCommittingAnEnglishCandidateLearnsItAsALiteral covers the other commit
// path — Space, a digit, a click on the panel — which goes through the
// candidate rather than through the typed letters.
//
// Tested at the seam because commit needs a live text client to insert into,
// and there is no such thing in a test. What the seam has to get right is
// WHICH namespace a pick lands in: an English candidate learned as a reading is
// how "AfterShip" ended up recorded and then never offered again.
func TestCommittingAnEnglishCandidateLearnsItAsALiteral(t *testing.T) {
	s, u := newLearningSession(t)

	s.learn(engine.Candidate{
		Word:     "AfterShip",
		Reading:  []string{"aftership"},
		Consumed: len("aftership"),
		Source:   engine.SourceEnglish,
	}, pickedByUser)
	if ls := u.LiteralsFor("aftership"); len(ls) != 1 || ls[0].Word != "AfterShip" {
		t.Errorf("an English pick recorded %v, want it in the literal namespace", ls)
	}

	// And a Chinese pick still goes where it always did.
	s.learn(engine.Candidate{
		Word:     "你好",
		Reading:  []string{"ni", "hao"},
		Consumed: 5,
		Source:   engine.SourceLexicon,
	}, pickedByUser)
	if _, ok := u.Boost([]string{"ni", "hao"}, "你好"); !ok {
		t.Error("a lexicon pick did not reach the reading namespace")
	}
	if ls := u.LiteralsFor("nihao"); len(ls) != 0 {
		t.Errorf("a lexicon pick leaked into the literal namespace: %v", ls)
	}
}

// TestEnterLearnsWhatWasTyped covers the OTHER way a term gets entered, and the
// one the user is most likely to reach for: type it, see nothing worth picking,
// press Enter.
//
// Enter is the escape hatch for whatever the lexicon cannot spell, which makes
// it the single best signal that a word is worth remembering — and it was the
// one commit path that recorded nothing at all.
func TestEnterLearnsWhatWasTyped(t *testing.T) {
	s, u := newLearningSession(t)

	typeKeys(s, "AfterShip")
	s.handleSpecialKey(noClient, keyReturn, 0)

	if ls := u.LiteralsFor("aftership"); len(ls) != 1 || ls[0].Word != "AfterShip" {
		t.Fatalf("Enter recorded %v, want AfterShip", ls)
	}

	// And it comes back in Chinese mode too: the capital is how an English word
	// starts, not a spelling the user has to repeat forever.
	s.reset(noClient)
	typeKeys(s, "aftership")
	if indexOfWord(s.cands, "AfterShip") < 0 {
		t.Errorf("lowercase \"aftership\" offers %v, want AfterShip", candWords(s.cands))
	}
}

// TestLearnedBrandIsOnTheFirstPage: a candidate the user has to page to is not
// offered in any sense they would recognise. The reserved English slot already
// promises this for the shipped table; a word they typed themselves has at
// least as good a claim on it.
func TestLearnedBrandIsOnTheFirstPage(t *testing.T) {
	s, _ := newLearningSession(t)

	typeKeys(s, "AfterShip")
	s.handleSpecialKey(noClient, keyReturn, 0)

	s.reset(noClient)
	typeKeys(s, "aftership")
	at := indexOfWord(s.cands, "AfterShip")
	if at < 0 {
		t.Fatalf("\"aftership\" offers %v, want AfterShip", candWords(s.cands))
	}
	if at >= s.pageSize() {
		t.Errorf("AfterShip is at position %d, off a %d-candidate page: %v",
			at, s.pageSize(), candWords(s.cands))
	}
}

// TestEnterOnPinyinDoesNotDisplaceTheChineseWord is the cost of Enter meaning
// "the letters" everywhere, and the bound on it.
//
// Pressing Enter on `nihao` now inserts "nihao" and — like every literal
// commit — teaches it, because a user who typed letters and refused every
// candidate is the clearest evidence this input method ever gets that the
// spelling matters. What that must NOT do is cost 你好 the top of the list: an
// Enter pressed to get past a composition would otherwise retrain the most
// common word in the language against itself.
func TestEnterOnPinyinDoesNotDisplaceTheChineseWord(t *testing.T) {
	s, _ := newLearningSession(t)

	typeKeys(s, "nihao")
	s.handleSpecialKey(noClient, keyReturn, 0)

	s.reset(noClient)
	typeKeys(s, "nihao")
	if len(s.cands) == 0 || s.cands[0].Word != "你好" {
		t.Errorf("after an Enter on \"nihao\", it gives %v, want 你好 first",
			candWords(s.cands))
	}
	// The letters do come back — that is what was learned — but in the reserved
	// English slot, not ahead of the Chinese.
	if i := indexOfWord(s.cands, "nihao"); i < 0 {
		t.Errorf("Enter taught nothing: %v", candWords(s.cands))
	} else if i == 0 {
		t.Errorf("the letters took the top slot from 你好: %v", candWords(s.cands))
	}
}
