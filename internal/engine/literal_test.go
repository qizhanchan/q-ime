package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// The reported bug: typing a brand or a piece of jargon "does not seem to be
// remembered". It was recorded and then never consulted, and learning that
// produces no candidate is indistinguishable from not learning at all.
//
// These tests attach no English table on purpose. A word the shipped table
// contains would prove nothing here: the case that matters is a word no fixed
// list can hold, which is exactly why the table cannot be what supplies it.

// newLearnedEngine is an engine whose only English knowledge is what this user
// has typed.
func newLearnedEngine(t *testing.T, learn func(*UserDict)) *Engine {
	t.Helper()
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	learn(u)
	return New(buildTestDict(t), u, pinyin.Fuzzy{})
}

// TestLearnedLiteralComesBackForTheSameLetters is the minimum the user asked
// for: type it once, and typing it again offers the spelling back.
func TestLearnedLiteralComesBackForTheSameLetters(t *testing.T) {
	e := newLearnedEngine(t, func(u *UserDict) {
		u.RecordLiteral("aftership", "AfterShip")
	})

	// Typed with the capital that opens English mode, but with the ordinary
	// capitalisation a user reaches for — the value here is not having to
	// remember where the second capital goes.
	got := words(e.EnglishWords("Aftership", 12))
	if indexOfString(got, "AfterShip") < 0 {
		t.Errorf("EnglishWords(\"Aftership\") = %v, want \"AfterShip\" offered", got)
	}
	if got[0] != "Aftership" {
		t.Errorf("candidate 0 is %q; the literal as typed must stay first", got[0])
	}
}

// TestLearnedLiteralCompletesFromAPrefix: the payoff of learning is typing
// less, and that only happens if a couple of letters are enough.
func TestLearnedLiteralCompletesFromAPrefix(t *testing.T) {
	e := newLearnedEngine(t, func(u *UserDict) {
		u.RecordLiteral("aftership", "AfterShip")
	})

	got := words(e.EnglishWords("Af", 12))
	if indexOfString(got, "AfterShip") < 0 {
		t.Errorf("EnglishWords(\"Af\") = %v, want \"AfterShip\" among them", got)
	}

	// One letter is not a prefix worth completing, here as in the table: it
	// matches everything the user has ever typed, in no useful order.
	if got := words(e.EnglishWords("A", 12)); indexOfString(got, "AfterShip") >= 0 {
		t.Errorf("EnglishWords(\"A\") = %v, want no completion from one letter", got)
	}
}

// TestLearnedLiteralIsOfferedInPinyinMode: the capital is how an English word
// STARTS, not how it has to be typed forever. Somebody who types "aftership"
// lowercase — most people, most of the time — must still be offered it.
func TestLearnedLiteralIsOfferedInPinyinMode(t *testing.T) {
	e := newLearnedEngine(t, func(u *UserDict) {
		u.RecordLiteral("aftership", "AfterShip")
	})

	cs := e.Candidates("aftership", 0)
	i := indexOf(cs, "AfterShip")
	if i < 0 {
		t.Fatalf("Candidates(\"aftership\") = %v, want \"AfterShip\" among them", words(cs))
	}
	// Marked English so the UI's reserved slot keeps it on the first page —
	// the same promise the table's words get, for a word that earned it.
	if cs[i].Source != SourceEnglish {
		t.Errorf("learned literal has source %v, want SourceEnglish", cs[i].Source)
	}
	if cs[i].Consumed != len("aftership") {
		t.Errorf("Consumed is %d, want the whole input accounted for", cs[i].Consumed)
	}
}

// TestAnIdenticalLiteralRanksBehindCleanPinyin is the guard that keeps literals
// from leaking into Chinese typing.
//
// The rule used to be "never offer a literal whose spelling equals the input",
// and that turned out to be too blunt: `bigquery` reads as bi'g'qu'er'y and
// offers 比过去而言 at full coverage, so a user who had committed the word four
// times was still offered nothing. The literals ARE offered now — what has to
// hold is that they lose to a clean reading.
//
// Ranking rather than exclusion, and the margin is why the boost is withheld
// from identical literals: without it "nihao" scores 24.5 against 你好's 30.49,
// with it 30.5 — a coin toss. See englishExact.
func TestAnIdenticalLiteralRanksBehindCleanPinyin(t *testing.T) {
	e := newLearnedEngine(t, func(u *UserDict) {
		u.RecordLiteral("nihao", "nihao")
	})

	cs := e.Candidates("nihao", 0)
	if len(cs) == 0 || cs[0].Word != "你好" {
		t.Fatalf("Candidates(\"nihao\") = %v, want 你好 first", words(cs))
	}
	// Offered, just not first: the letters have to be reachable, because Enter
	// confirms 你好 and the empty-list fallback never fires here.
	if indexOf(cs, "nihao") < 0 {
		t.Errorf("Candidates(\"nihao\") = %v, want the letters reachable too", words(cs))
	}
}

// TestLearnedLiteralSurvivesAReload: learning that lasts until the input method
// restarts is not learning. The code index is derived data, so this also pins
// that it is rebuilt from the FILE and not only by the calls that filled it.
func TestLearnedLiteralSurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	u := NewUserDict(path)
	u.RecordLiteral("aftership", "AfterShip")
	if err := u.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewUserDict(path)
	ls := reloaded.LiteralsFor("aftership")
	if len(ls) != 1 || ls[0].Word != "AfterShip" {
		t.Fatalf("after reload LiteralsFor = %v, want AfterShip", ls)
	}
	if ls := reloaded.LiteralsWithPrefix("af", 4); len(ls) != 1 {
		t.Errorf("after reload LiteralsWithPrefix = %v, want one hit", ls)
	}
}

// TestLiteralsRankByUse: two spellings under one code is the ordinary case —
// "Aftership" typed in haste, "AfterShip" typed deliberately — and the one used
// more, and more recently, is the one to offer first.
func TestLiteralsRankByUse(t *testing.T) {
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	u.RecordLiteral("aftership", "Aftership")
	for i := 0; i < 5; i++ {
		u.RecordLiteral("aftership", "AfterShip")
	}
	ls := u.LiteralsFor("aftership")
	if len(ls) != 2 || ls[0].Word != "AfterShip" {
		t.Errorf("LiteralsFor = %v, want the five-times spelling first", ls)
	}
}

// TestLiteralsAreNotConfusedWithReadings: one map holds both namespaces, and a
// pinyin reading is free to spell an English code ("ni", "shang"). A Chinese
// word leaking into the literal side would be offered as an English candidate.
func TestLiteralsAreNotConfusedWithReadings(t *testing.T) {
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	u.Record([]string{"ni"}, "你")
	u.RecordLiteral("ni", "Ni")

	if ls := u.LiteralsFor("ni"); len(ls) != 1 || ls[0].Word != "Ni" {
		t.Errorf("LiteralsFor(\"ni\") = %v, want only the literal", ls)
	}
	if _, ok := u.Boost([]string{"ni"}, "你"); !ok {
		t.Error("the reading entry lost its boost")
	}
	if _, ok := u.Boost([]string{"ni"}, "Ni"); ok {
		t.Error("a literal answered a reading lookup")
	}
}

// TestPickingATableEnglishWordStillSticks guards the boundary between the two
// namespaces from the inside.
//
// Every English pick is now recorded as a literal, including the ones the
// shipped table already contains. If the table's own candidates kept looking
// their boost up under a reading key, choosing "hao" over 好 twenty times would
// go on recording and stop counting — the same bug this change exists to fix,
// moved to a different word.
func TestPickingATableEnglishWordStillSticks(t *testing.T) {
	e := newEnglishEngine(t)
	e.user = NewUserDict(filepath.Join(t.TempDir(), "user.json"))

	before := indexOf(e.Candidates("hao", 0), "hao")
	if before <= 0 {
		t.Fatalf("\"hao\" starts at %d; the case needs Chinese ahead of it", before)
	}
	for i := 0; i < 5; i++ {
		e.user.RecordLiteral("hao", "hao")
	}
	if after := indexOf(e.Candidates("hao", 0), "hao"); after >= before {
		t.Errorf("after five picks \"hao\" is at %d, was at %d — the picks did not count",
			after, before)
	}
}

// TestOldEnglishPicksAreMigrated: the English candidate path has been recording
// picks under a reading key since it shipped, where nothing ever read them back.
// Those are real picks by a real user, and a fix that ignores them says "your
// history is fine, now type each of these once more".
func TestOldEnglishPicksAreMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	u := NewUserDict(path)
	u.Record([]string{"goland"}, "GoLand") // as the old English path wrote it
	u.Record([]string{"ni"}, "你")          // a reading, which must not move
	if err := u.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewUserDict(path)
	if ls := reloaded.LiteralsFor("goland"); len(ls) != 1 || ls[0].Word != "GoLand" {
		t.Errorf("LiteralsFor(\"goland\") = %v, want the migrated pick", ls)
	}
	if _, ok := reloaded.Boost([]string{"goland"}, "GoLand"); ok {
		t.Error("the old key survived the migration; the entry is now in both")
	}
	if _, ok := reloaded.Boost([]string{"ni"}, "你"); !ok {
		t.Error("a Chinese pick was migrated out of the reading namespace")
	}
	if ls := reloaded.LiteralsFor("ni"); len(ls) != 0 {
		t.Errorf("a Chinese pick reached the literal namespace: %v", ls)
	}
}

// TestLiteralIndexSurvivesEviction: the index is an accelerator over entries,
// and an accelerator that disagrees with its source grows without bound — every
// eviction would leave a word behind in it, for a process that runs for weeks.
func TestLiteralIndexSurvivesEviction(t *testing.T) {
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	now := int64(1_700_000_000)
	u.clock = func() time.Time { return time.Unix(now, 0) }

	u.RecordLiteral("aftership", "AfterShip")
	// Everything else is written an hour later, so the literal is unambiguously
	// the oldest entry and therefore the first thing eviction reaches. Without
	// the gap they all share a timestamp and which one goes is arbitrary — the
	// test would pass by not exercising anything.
	now += 3600
	for i := 0; i < maxUserEntries; i++ {
		u.Record([]string{"ni"}, string(rune(0x4e00+i)))
	}

	if _, ok := u.entries[literalKey("aftership", "AfterShip")]; ok {
		t.Fatal("eviction never reached the literal; this test proves nothing")
	}
	for code, ws := range u.literals {
		for _, w := range ws {
			if _, ok := u.entries[literalKey(code, w)]; !ok {
				t.Errorf("index still lists %q under %q, gone from entries", w, code)
			}
		}
	}
}

func indexOfString(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
