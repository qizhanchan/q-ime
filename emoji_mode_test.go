package main

import (
	"testing"

	"github.com/qizhanchan/q-ime/internal/engine"
)

// TestLeadingVOpensEmojiMode is the trigger, and the reason it can be a bare
// letter at all: v spells ü in this input method ("lv"→绿, "nv"→女) but only
// ever after an initial, so nothing can begin with one. Verified against the
// shipped lexicon — "v" and "vk" return no candidates today.
func TestLeadingVOpensEmojiMode(t *testing.T) {
	s, _ := newLearningSession(t)

	typeKeys(s, "vkaixin")

	if !s.emojiMode {
		t.Fatal("a leading v did not open emoji mode")
	}
	if s.buffer != "vkaixin" {
		t.Errorf("buffer is %q, want \"vkaixin\"", s.buffer)
	}
	// The preedit is a record of what was pressed, the v included.
	if got := s.preedit(); got != "vkaixin" {
		t.Errorf("preedit is %q, want \"vkaixin\"", got)
	}
	if len(s.cands) == 0 || s.cands[0].Word != "😄" {
		t.Fatalf("candidates are %v, want 😄 first", candWords(s.cands))
	}
	for _, c := range s.cands {
		if c.Source != engine.SourceEmoji {
			t.Errorf("candidate %q is not an emoji (source %v)", c.Word, c.Source)
		}
	}
}

// TestVInsideACompositionIsStillU: this is the whole risk of using v as the
// trigger, and it is why the branch is guarded on the buffer being empty.
func TestVInsideACompositionIsStillU(t *testing.T) {
	s, _ := newLearningSession(t)

	typeKeys(s, "lv")

	if s.emojiMode {
		t.Fatal("a v after an initial opened emoji mode; lv must stay 绿")
	}
	if len(s.cands) == 0 || s.cands[0].Word != "绿" {
		t.Errorf("\"lv\" gives %v, want 绿 first", candWords(s.cands))
	}

	s.reset(noClient)
	typeKeys(s, "nv")
	if s.emojiMode || len(s.cands) == 0 || s.cands[0].Word != "女" {
		t.Errorf("\"nv\" gives %v (emojiMode=%v), want 女 first",
			candWords(s.cands), s.emojiMode)
	}
}

// TestEmojiModeTakesAbbreviations: the mode is the ordinary query with a filter
// on the answers, so everything the engine can already do comes along.
func TestEmojiModeTakesAbbreviations(t *testing.T) {
	s, _ := newLearningSession(t)

	typeKeys(s, "vkx")

	if len(s.cands) == 0 || s.cands[0].Word != "😄" {
		t.Errorf("\"vkx\" gives %v, want 😄 first", candWords(s.cands))
	}
}

// TestBackspaceLeavesEmojiMode: deleting the v away must put the next letter
// back on the pinyin path, or one stray v would strand the user in emoji mode.
func TestBackspaceLeavesEmojiMode(t *testing.T) {
	s, _ := newLearningSession(t)

	typeKeys(s, "vk")
	for i := 0; i < 2; i++ {
		s.handleSpecialKey(noClient, keyBackspace, 0)
	}
	if s.emojiMode || s.composing() {
		t.Fatalf("after deleting it all: buffer %q emojiMode %v", s.buffer, s.emojiMode)
	}

	typeKeys(s, "nihao")
	if s.emojiMode {
		t.Fatal("still in emoji mode after the v was deleted")
	}
	if len(s.cands) == 0 || s.cands[0].Word != "你好" {
		t.Errorf("after leaving emoji mode, \"nihao\" gives %v", candWords(s.cands))
	}
}

// TestBareVCommitsSomething: with nothing after it there is nothing to look up,
// and an empty candidate list is how text vanishes — Space would commit nothing
// at all. The letters as typed are the fallback, exactly as on the pinyin path.
func TestBareVCommitsSomething(t *testing.T) {
	s, _ := newLearningSession(t)

	typeKeys(s, "v")

	if len(s.cands) == 0 {
		t.Fatal("a bare v produced an empty candidate list")
	}
	if s.cands[0].Word != "v" {
		t.Errorf("candidate 0 is %q, want the letter as typed", s.cands[0].Word)
	}
}

// TestEmojiEndsThePhrase: an emoji finishes a thought the way a full stop does.
// The pair INTO it is worth keeping — 开心 is often followed by 😄 — but pairing
// the next word with a picture would train a phrase nobody said.
func TestEmojiEndsThePhrase(t *testing.T) {
	s, u := newLearningSession(t)

	commitWord(s, "开心", "kai", "xin")
	s.learn(engine.Candidate{
		Word:     "😄",
		Reading:  []string{"kai", "xin"},
		Consumed: 6,
		Source:   engine.SourceEmoji,
	}, pickedByUser)

	if s.lastWord != "" {
		t.Errorf("context after an emoji is %q, want it cleared", s.lastWord)
	}
	// The pair leading into the emoji is still learned.
	if ss := u.Successors("开心"); len(ss) != 1 || ss[0].Word != "😄" {
		t.Errorf("Successors(\"开心\") = %v, want the emoji recorded", ss)
	}
}

// TestPickingAnEmojiPromotesIt: an emoji candidate carries the reading of the
// word it decorates, so the ordinary learning path applies with no special
// case. By default 😄 sits just behind 开心 and never ahead of it — but somebody
// who keeps choosing the picture is saying what they mean, and "picked even
// once for this reading jumps the list" is the promise the user dictionary
// makes to every other candidate.
//
// This was silently broken when written: the emoji's score is DERIVED from its
// word's, so its own history was never read and picking it a hundred times did
// nothing at all.
func TestPickingAnEmojiPromotesIt(t *testing.T) {
	s, u := newLearningSession(t)

	cs := s.eng.Candidates("kaixin", 0)
	if len(cs) == 0 || cs[0].Word != "开心" || indexOfWord(cs, "😄") != 1 {
		t.Fatalf("\"kaixin\" starts as %v, want 开心 then 😄", candWords(cs[:min(4, len(cs))]))
	}

	for i := 0; i < 3; i++ {
		s.learn(engine.Candidate{
			Word:     "😄",
			Reading:  []string{"kai", "xin"},
			Consumed: 6,
			Source:   engine.SourceEmoji,
		}, pickedByUser)
	}
	if _, ok := u.Boost([]string{"kai", "xin"}, "😄"); !ok {
		t.Fatal("picking an emoji recorded nothing")
	}

	cs = s.eng.Candidates("kaixin", 0)
	if len(cs) == 0 || cs[0].Word != "😄" {
		t.Errorf("after three picks \"kaixin\" gives %v, want 😄 first",
			candWords(cs[:min(4, len(cs))]))
	}
}
