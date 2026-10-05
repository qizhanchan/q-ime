package main

import (
	"testing"
	"time"
	"unsafe"

	"github.com/qizhanchan/q-ime/internal/engine"
	"github.com/qizhanchan/q-ime/internal/english"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// Key codes for the letters used below (Carbon kVK_ANSI_*). Spelled out
// because since the bridge moved to raw NSEvents the key code is what
// identifies a key, and a test that made them up would be testing nothing.
const (
	keyA = 0
	keyS = 1
	keyH = 4
)

// newShiftTestSession builds a session with no panel and no text client.
//
// Both are the OS's half: the panel's window methods no-op without a window
// (candidatePanel.windowless) and the ObjC bridge checks for a nil client on
// every call. What is left is exactly the part worth testing — the state
// machine deciding what each key means, which is where the backspace/arrow-key
// bug and the Shift bug both lived.
func newShiftTestSession(t *testing.T) (*imeSession, *fakeClock) {
	t.Helper()
	// buildTree, not newCandidatePanel: the widget tree is what applyState
	// mutates, and it needs no OS window or GPU context. Everything that does
	// need the window is guarded by candidatePanel.windowless.
	panel := &candidatePanel{}
	panel.buildTree()
	clock := &fakeClock{}
	return &imeSession{
		panel:     panel,
		eng:       nil, // set by tests that type letters
		punct:     newPunctuator(),
		prefs:     prefs{PageSize: 5},
		chinese:   true,
		deferToUI: clock.schedule,
		// No test may write the developer's real preferences file.
		save: func(prefs) {},
		// Vacuously true of a composition with no pieces yet — see coinPhrase.
		pendingPicked: true,
	}, clock
}

// fakeClock stands in for the timer plus the hop back to the UI thread.
//
// The delay itself is not what needs testing — time.AfterFunc works — and a
// test that slept 60ms to find out would be slow and flaky. What needs testing
// is whether the scheduled switch still runs once other events have happened,
// so the test decides when the timer fires.
type fakeClock struct{ pending []func() }

func (c *fakeClock) schedule(_ time.Duration, fn func()) {
	c.pending = append(c.pending, fn)
}

// fire runs everything scheduled so far, in order. Cancelled work is still
// "fired" here — cancellation is the callback deciding to do nothing, exactly
// as with a real timer that cannot be un-scheduled.
func (c *fakeClock) fire() {
	due := c.pending
	c.pending = nil
	for _, fn := range due {
		fn()
	}
}

var noClient = unsafe.Pointer(nil)

// testEngineOrSkip opens the compiled lexicon, or skips. The tests that need
// it are about what a keystroke DOES, not about ranking, so any real
// dictionary will do — and its absence is normal, since it is GPL data built
// on the developer's machine and not committed.
//
// Wired the same way newEngine wires the real one, English list included. An
// engine assembled differently here would be a test of something the app does
// not run: leaving SetEnglish out made "She" look like it offered no
// alternatives when in fact the helper had no dictionary to offer them from.
func testEngineOrSkip(t *testing.T) *engine.Engine {
	t.Helper()
	d, err := openLexicon()
	if err != nil {
		t.Skipf("no compiled lexicon: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	e := engine.New(d, nil, pinyin.Fuzzy{})
	en, err := english.Builtin()
	if err != nil {
		t.Fatalf("english.Builtin: %v", err)
	}
	e.SetEnglish(en)
	return e
}

// tapShift is Shift pressed and released with nothing in between.
func tapShift(s *imeSession) {
	s.handleFlags(noClient, modShift, keyLeftShift)
	s.handleFlags(noClient, 0, keyLeftShift)
}

// TestShiftTapTogglesLanguage is the gesture as documented: Shift on its own,
// nothing in between.
func TestShiftTapTogglesLanguage(t *testing.T) {
	s, clock := newShiftTestSession(t)

	tapShift(s)
	// Deliberately checked BEFORE the timer: the switch is scheduled, not done.
	// That delay is the whole mechanism, so a test that could not tell the two
	// apart would not be testing it.
	if !s.chinese {
		t.Error("the switch happened immediately; it must wait out shiftToggleDelay")
	}
	clock.fire()
	if s.chinese {
		t.Error("a bare Shift tap did not leave English mode")
	}

	tapShift(s)
	clock.fire()
	if !s.chinese {
		t.Error("a second Shift tap did not come back to Chinese mode")
	}
}

// TestShiftTapIsCancelledByAKeyRightAfterIt is what the delay is for.
//
// A fast typist aiming for a capital sometimes releases Shift a hair BEFORE
// pressing the letter. The letter then arrives lowercase and the sequence is
// indistinguishable from a deliberate tap followed by a letter — so the mode
// used to flip, which is a sticky error: every key after it is wrong until the
// user notices.
//
// Waiting lets the letter call the switch off. The letter is still lowercase —
// nothing can conjure the capital that was wanted — but it lands as pinyin in
// the mode the user was already in, which the candidate panel shows
// immediately and one backspace undoes.
func TestShiftTapIsCancelledByAKeyRightAfterIt(t *testing.T) {
	s, clock := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	tapShift(s)                          // Shift down, Shift up — sloppily early
	s.handleText(noClient, "a", keyA, 0) // the letter the user meant to capitalise
	clock.fire()                         // the timer comes due

	if !s.chinese {
		t.Error("a key arriving inside the delay did not call off the language switch")
	}
	if s.buffer != "a" {
		t.Errorf("buffer is %q; the letter should have gone in as pinyin", s.buffer)
	}
}

// TestShiftTapSurvivesAKeyArrivingAfterTheWindow: the cancel must be scoped to
// the delay. Tapping Shift and then typing later is two separate intentions,
// and the switch has already happened by then.
func TestShiftTapSurvivesAKeyArrivingAfterTheWindow(t *testing.T) {
	s, clock := newShiftTestSession(t)

	tapShift(s)
	clock.fire() // the delay elapses first, with nothing in it
	if s.chinese {
		t.Fatal("the tap did not switch language")
	}

	s.handleText(noClient, "a", keyA, 0)
	if s.chinese {
		t.Error("a key typed after the switch undid it")
	}
}

// TestOtherModifierCancelsAPendingTap: Shift-then-Cmd is a shortcut being
// assembled. Chording is slow enough that the Cmd often lands after Shift is
// already up, so the cancel has to reach a switch that is already scheduled.
func TestOtherModifierCancelsAPendingTap(t *testing.T) {
	s, clock := newShiftTestSession(t)

	tapShift(s)
	s.handleFlags(noClient, modCommand, 55) // kVK_Command
	clock.fire()

	if !s.chinese {
		t.Error("Shift then Cmd switched language; that is a shortcut, not a tap")
	}
}

// TestFocusLeavingCancelsAPendingTap: the switch would flush a composition
// through a text client that is going away, and would land in whatever gets
// focus next.
func TestFocusLeavingCancelsAPendingTap(t *testing.T) {
	s, clock := newShiftTestSession(t)

	tapShift(s)
	s.handleCommand(noClient, "commitComposition:")
	clock.fire()

	if !s.chinese {
		t.Error("a pending switch survived focus leaving the field")
	}

	s2, clock2 := newShiftTestSession(t)
	tapShift(s2)
	s2.deactivate(noClient)
	clock2.fire()
	if !s2.chinese {
		t.Error("a pending switch survived deactivateServer:")
	}
}

// TestShiftLetterDoesNotSwitchLanguage: Shift+A is a capital, not a mode
// switch.
//
// The full macOS event sequence, in order, because the ordering is the whole
// mechanism: the press only ARMS the gesture, the letter disarms it, and the
// release then has nothing to fire.
//
// What the capital DOES with the buffer is english_mode_test.go's business;
// this one is only about the language mode surviving it.
func TestShiftLetterDoesNotSwitchLanguage(t *testing.T) {
	s, clock := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	s.handleFlags(noClient, modShift, keyLeftShift) // Shift down
	handled := s.handleText(noClient, "A", keyA, modShift)
	s.handleFlags(noClient, 0, keyLeftShift) // Shift up
	clock.fire()                             // and nothing was left scheduled

	if !s.chinese {
		t.Error("Shift+A switched to English mode; it must only type a capital")
	}
	if !handled {
		t.Error("Shift+A was passed to the host; it opens an English word instead")
	}
}

// TestShiftHeldAcrossSeveralLettersKeepsTheMode: holding Shift to type several
// capitals produces one press and one release with several letters between
// them, so the gesture must stay disarmed the whole time.
func TestShiftHeldAcrossSeveralLettersKeepsTheMode(t *testing.T) {
	s, clock := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	s.handleFlags(noClient, modShift, keyLeftShift)
	for _, k := range []struct {
		text string
		code int
	}{{"A", keyA}, {"S", keyS}, {"H", keyH}} {
		s.handleText(noClient, k.text, k.code, modShift)
	}
	s.handleFlags(noClient, 0, keyLeftShift)
	clock.fire()

	if !s.chinese {
		t.Error("holding Shift for three capitals switched to English mode")
	}
	// They accumulate as an English WORD, not as pinyin. "ASH" is what was
	// pressed, so "ASH" is what the buffer holds.
	if !s.englishMode || s.buffer != "ASH" {
		t.Errorf("buffer %q englishMode %v; want \"ASH\" as an English word",
			s.buffer, s.englishMode)
	}
}

// TestShiftLetterMidCompositionFlushesThePinyin is the interleaving that
// happens in real typing: some pinyin, then a capital.
//
// The capital must not join the pinyin buffer and must not switch language.
// What was already composed belongs BEFORE the English word, so the pinyin is
// committed first and the capital opens a fresh English buffer — "ni" then
// Shift+A commits 你 and starts on "A", rather than leaving a buffer of "niA"
// that spells nothing in either language.
func TestShiftLetterMidCompositionFlushesThePinyin(t *testing.T) {
	s, clock := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	// Type the pinyin the ordinary way, so the candidate list is real and
	// commitAll has something to commit.
	s.handleText(noClient, "n", 45, 0)
	s.handleText(noClient, "i", 34, 0)
	if s.buffer != "ni" {
		t.Fatalf("buffer is %q after typing \"ni\"", s.buffer)
	}

	s.handleFlags(noClient, modShift, keyLeftShift)
	handled := s.handleText(noClient, "A", keyA, modShift)
	s.handleFlags(noClient, 0, keyLeftShift)
	clock.fire()

	if !s.chinese {
		t.Error("Shift+A after typing pinyin switched to English mode")
	}
	// The pinyin is gone from the buffer — committed, not carried along — and
	// what is left is the English word just begun.
	if !s.englishMode || s.buffer != "A" {
		t.Errorf("buffer %q englishMode %v; want the pinyin flushed and \"A\" begun",
			s.buffer, s.englishMode)
	}
	if !handled {
		t.Error("the capital was passed to the host without flushing the pinyin first, " +
			"so it would land BEFORE the Chinese text")
	}
}

// TestCapitalIsNeverPinyin: a capital must never be read as pinyin. The
// lowercase branch is spelled r >= 'a' && r <= 'z' precisely so this holds; a
// widened range would spell readings nobody typed.
func TestCapitalIsNeverPinyin(t *testing.T) {
	s, clock := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)

	for _, capital := range []string{"A", "N", "I", "Z"} {
		s.reset(noClient)
		s.handleText(noClient, capital, keyA, modShift)
		clock.fire()
		if !s.englishMode {
			t.Errorf("%q did not open an English word", capital)
		}
		if s.buffer != capital {
			t.Errorf("%q left the buffer as %q", capital, s.buffer)
		}
		// Nothing Chinese may be offered for a capital.
		for _, c := range s.cands {
			if c.Source != engine.SourceEnglish {
				t.Errorf("%q offered a non-English candidate %q", capital, c.Word)
			}
		}
	}
}

// typeLetters feeds a run of lowercase letters through the key handler.
//
// Key codes are not meaningful for letters here — handleText dispatches
// letters on their text, and the special keys it dispatches on keyCode are all
// non-letters — so one placeholder code keeps the call readable.
func typeLetters(s *imeSession, letters string) {
	for _, r := range letters {
		s.handleText(noClient, string(r), keyA, 0)
	}
}

// TestShiftTapMidCompositionEmitsTheLettersAndSwitches is the "go" case.
//
// Typing "go" in Chinese mode offers 公 and its homophones, which is right —
// those letters ARE pinyin. But when they were an English word, a bare Shift
// tap has to be retroactive: the letters go in as letters, and the mode
// follows. Committing the candidate instead left 公 in the document and the
// word to be retyped, which is exactly what the gesture is reached for.
func TestShiftTapMidCompositionEmitsTheLettersAndSwitches(t *testing.T) {
	s, clock := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)
	var got []string
	s.insert = func(_ unsafe.Pointer, text string) { got = append(got, text) }

	typeLetters(s, "go")
	// The engine really does read those letters as Chinese, so the assertion
	// below is about a choice and not about an empty candidate list.
	if len(s.cands) == 0 || s.cands[0].Word == "go" {
		t.Fatalf("expected Chinese candidates for \"go\", got %v", candWords(s.cands))
	}
	tapShift(s)
	clock.fire()

	if len(got) != 1 || got[0] != "go" {
		t.Errorf("emitted %q, want exactly [\"go\"] — the letters, not the candidate", got)
	}
	if s.chinese {
		t.Error("the language mode did not switch to English")
	}
	if s.composing() {
		t.Errorf("composition survived the switch: buffer=%q", s.buffer)
	}
}

// TestShiftTapDropsSyllableSeparators: an apostrophe in a pinyin buffer is a
// syllable boundary, not a letter, so it must not reach the document.
func TestShiftTapDropsSyllableSeparators(t *testing.T) {
	s, clock := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)
	var got []string
	s.insert = func(_ unsafe.Pointer, text string) { got = append(got, text) }

	typeLetters(s, "xi")
	s.handleText(noClient, "'", keyA, 0)
	typeLetters(s, "an")
	if s.buffer != "xi'an" {
		t.Fatalf("buffer is %q, want %q", s.buffer, "xi'an")
	}
	tapShift(s)
	clock.fire()

	if len(got) != 1 || got[0] != "xian" {
		t.Errorf("emitted %q, want [\"xian\"]: the separator is not part of the text", got)
	}
}

// TestShiftTapEmitsAnEnglishWordVerbatim: the other composition kind, entered
// by a capital letter, is already literal text — capitals included.
func TestShiftTapEmitsAnEnglishWordVerbatim(t *testing.T) {
	s, clock := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)
	var got []string
	s.insert = func(_ unsafe.Pointer, text string) { got = append(got, text) }

	s.handleText(noClient, "S", keyS, modShift)
	s.handleText(noClient, "h", keyH, 0)
	if !s.englishMode || s.buffer != "Sh" {
		t.Fatalf("englishMode=%v buffer=%q, want an English word in progress", s.englishMode, s.buffer)
	}
	tapShift(s)
	clock.fire()

	if len(got) != 1 || got[0] != "Sh" {
		t.Errorf("emitted %q, want [\"Sh\"]", got)
	}
}

// TestShiftTapEmitsNothingWithNothingComposing keeps the plain mode switch
// silent — it must not put a stray empty insert into the document.
func TestShiftTapEmitsNothingWithNothingComposing(t *testing.T) {
	s, clock := newShiftTestSession(t)
	var got []string
	s.insert = func(_ unsafe.Pointer, text string) { got = append(got, text) }

	tapShift(s)
	clock.fire()

	if len(got) != 0 {
		t.Errorf("emitted %q with an empty buffer", got)
	}
	if s.chinese {
		t.Error("the mode did not switch")
	}
}

// TestShiftTapFlushIsCancelledByAKey: the flush rides the same delayed,
// cancellable path as the switch itself, so a letter arriving inside the window
// calls off both — and must not have emitted anything on the way.
func TestShiftTapFlushIsCancelledByAKey(t *testing.T) {
	s, clock := newShiftTestSession(t)
	s.eng = testEngineOrSkip(t)
	var got []string
	s.insert = func(_ unsafe.Pointer, text string) { got = append(got, text) }

	typeLetters(s, "go")
	tapShift(s)
	typeLetters(s, "u") // sloppy Shift released just before the letter
	clock.fire()

	if len(got) != 0 {
		t.Errorf("emitted %q; the flush should have been called off", got)
	}
	if !s.chinese {
		t.Error("the language mode switched despite the key")
	}
	if s.buffer != "gou" {
		t.Errorf("buffer is %q, want %q: the letter belongs to the pinyin", s.buffer, "gou")
	}
}
