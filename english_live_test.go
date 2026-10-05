package main

import (
	"testing"

	"github.com/qizhanchan/q-ime/internal/engine"
	"github.com/qizhanchan/q-ime/internal/english"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// TestEnglishIsVisibleAgainstTheRealLexicon checks the two visibility rules
// COMPOSED, against the shipped dictionary.
//
// Both halves are unit-tested — truncateKeepingEnglish in the engine,
// reserveEnglishSlot here — and both passed while the feature was still
// broken, because the bug was in how they met: the engine truncated the
// English candidate away at 60, so the UI's reserved slot had nothing to move.
// Fifteen codes, "js" and "ssh" among them, were silently unreachable.
//
// Deliberately asserts VISIBILITY and not rank. What rank a candidate gets
// depends on corpus frequencies that shift with every dictionary update, and a
// test that pinned those would fail for reasons nobody cares about. That an
// exact English match is reachable without paging is a promise the UI makes,
// and it should hold whatever the corpus says.
func TestEnglishIsVisibleAgainstTheRealLexicon(t *testing.T) {
	d, err := openLexicon()
	if err != nil {
		// The lexicon is GPL data compiled on the developer's machine and is
		// not in the repo, so its absence is normal, not a failure.
		t.Skipf("no compiled lexicon: %v", err)
	}
	defer d.Close()

	en, err := english.Builtin()
	if err != nil {
		t.Fatalf("english.Builtin: %v", err)
	}
	e := engine.New(d, nil, pinyin.Fuzzy{})
	e.SetEnglish(en)

	// Driven through the session rather than by composing the two rules by
	// hand: doing it by hand would still pass if refresh forgot to apply the
	// reserved slot at all. No panel and no text client are needed, which is
	// why setCandidates is split out of refresh.
	const pageSize = 5
	s := &imeSession{eng: e, prefs: prefs{PageSize: pageSize}}
	inputs := []string{
		// Force-fitted Chinese: these should win outright.
		"hello", "world", "python", "thanks", "github", "install",
		// Short codes that lost to real pinyin AND used to fall off the end of
		// the 60-candidate list entirely. The reason this test exists.
		"js", "ssh", "zsh", "cd", "my", "she", "bus", "by", "yin",
		// Clean pinyin readings that are also English words. Chinese should
		// keep the top slot; English still has to be on the page.
		"song", "men", "taiwan", "women", "dance", "the", "and", "he",
	}
	for _, in := range inputs {
		if !en.Has(in) {
			t.Errorf("%q is not in the English table; the case is not being tested", in)
			continue
		}
		s.buffer = in
		s.setCandidates()
		at := -1
		for i, c := range s.cands {
			if c.Source == engine.SourceEnglish {
				at = i
				break
			}
		}
		switch {
		case at < 0:
			t.Errorf("%q: no English candidate in %d results", in, len(s.cands))
		case at >= pageSize:
			t.Errorf("%q: English at position %d, off a %d-candidate page",
				in, at, pageSize)
		}
	}
}
