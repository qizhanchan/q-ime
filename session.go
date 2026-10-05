package main

import (
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/qizhanchan/q-ime/internal/dict"
	"github.com/qizhanchan/q-ime/internal/engine"
)

// imeSession is the whole input method's state: one raw-ASCII buffer, one
// candidate list, one panel.
//
// Deliberately a process singleton. IMK hands out one IMKInputController per
// text client, and putting the buffer there would give every text field its
// own composition — type "ni" in one field, click into another, and the
// first field's preedit would be stranded.
type imeSession struct {
	panel *candidatePanel
	eng   *engine.Engine
	user  *engine.UserDict
	punct *punctuator
	prefs prefs

	// buffer is the raw ASCII typed so far. Empty means "not composing".
	buffer string
	cands  []engine.Candidate
	// sel is the highlighted candidate, an absolute index into cands.
	sel int
	// pageStart is the first candidate shown. Paging moves this by
	// prefs.PageSize; it is kept rather than derived from sel so that
	// arrowing back and forth does not reflow the page under the user.
	pageStart int

	// chinese is the input mode. Off means every key passes straight through
	// to the host — see toggleLanguage.
	chinese bool
	// lastWord is the word committed most recently, which the engine ranks the
	// next query against. Empty at the start of a sentence.
	//
	// Deliberately NOT cleared by reset: reset ends a composition, and a
	// composition ending is exactly when this becomes useful. It is cleared by
	// the two things that really do end a context — a sentence-ending mark, and
	// focus leaving the field.
	lastWord string
	// lastReading is the pinyin reading of lastWord. Keeping it lets the next
	// query look up dictionary phrases that begin with the committed word, so
	// `吃饭` followed by `l` can offer the suffixes of 吃饭了 / 吃饭了吗 even
	// before the user has learned that exact pair.
	lastReading []string
	// recent is the tail of what this session has committed into the current
	// field, oldest first, with the reading each piece was typed as.
	//
	// Separate from lastWord because the two answer different questions.
	// lastWord is a BIGRAM KEY — what the user dictionary recorded a pair
	// against — and must stay exactly one committed word or the learned data
	// stops matching. This is the TEXT ON SCREEN, which is what a dictionary
	// phrase has to be looked up against: 博物 typed as one word and 博物 typed
	// as 博 then 物 leave the same three characters in the document, and only
	// the first could ever reach 博物馆 from lastWord alone.
	//
	// Cleared by exactly the same two things as lastWord — a sentence-ending
	// mark and focus leaving. Not by reset: reset ends a composition, and that
	// is when this becomes useful. Forgetting on a focus change is deliberate
	// and matches what other input methods do; the promise here is only that
	// continuous typing WITHIN one field stays continuous.
	recent []committedRun
	// pending is the pieces of the composition CURRENTLY in flight, committed
	// one at a time while the buffer still has letters left. Distinct from
	// recent, which spans compositions: only pieces of ONE buffer are evidence
	// that the user meant them as a single word. Cleared by reset, which is what
	// ends a composition.
	pending []committedRun
	// pendingPicked stays true only while every piece of this composition was
	// deliberately picked. One flush anywhere in it and the whole thing stops
	// being a coinage — see coinPhrase.
	pendingPicked bool
	// englishMode says the buffer holds a literal English word rather than
	// pinyin. Entered by a capital letter and left when the word is committed
	// or deleted away — see the capital-letter branch in handleText.
	englishMode bool
	// emojiMode says the buffer opened with "v" and holds a pinyin query whose
	// answers are emoji rather than words.
	//
	// A leading v is free real estate: v spells ü in this input method
	// ("lv"→绿, "nv"→女) but only ever AFTER an initial, so no syllable — and
	// therefore no composition — can begin with one. Verified against the
	// shipped lexicon: "v" and "vk" return nothing at all today.
	emojiMode bool
	// shiftArmed tracks a Shift press not yet disqualified by another key.
	shiftArmed bool
	// shiftSeq invalidates a language switch that has been scheduled but not
	// yet run. Bumped by anything that says the Shift tap was not a bare tap
	// after all — see scheduleLanguageToggle.
	shiftSeq uint64
	// pendingShiftClient is the text client a scheduled switch will flush
	// through. RETAINED, because it has to outlive the IMK callback that
	// produced it; see setPendingShiftClient.
	pendingShiftClient unsafe.Pointer
	// deferToUI schedules fn on the UI thread after d. A field rather than a
	// direct call so the Shift timing can be tested without a window and
	// without waiting on a wall clock.
	deferToUI func(d time.Duration, fn func())
	// insert puts committed text into the host document; nil means the real
	// bridge. A field for the same reason deferToUI is one, and it earns it:
	// this is the state machine's only OUTPUT, and several of the rules here
	// are about WHICH text goes in rather than about internal state — a Shift
	// tap emits the letters and not the candidate, punctuation lands after the
	// flush and not before it. A test with no text client can watch this; it
	// cannot watch cgo.
	insert func(client unsafe.Pointer, text string)
	// mark shows the in-progress preedit in the host document; nil means the
	// real bridge. The companion to insert, and it earns its keep for the same
	// reason plus one more: commit() refuses to run without a live text client,
	// so until this existed the ENTIRE commit path — partial commits, the pieces
	// a coined word is built from, which candidate a key picks — could only be
	// tested by calling learn() directly and pretending. See setMarked.
	mark func(client unsafe.Pointer, text string)
	// retain and release manage the cached client's reference count; nil means
	// the real bridge. The last two of the four cgo calls the commit path makes,
	// hooked for the same reason as the other two: an IMK client pointer is an
	// ObjC object and CFBridgingRetain on anything else is an immediate crash,
	// so without these a test cannot hold a client at all — and commit() will
	// not run without one.
	retain  func(client unsafe.Pointer) unsafe.Pointer
	release func(client unsafe.Pointer)
	// caret asks the host where its insertion point is; nil means the real
	// bridge.
	//
	// These five — insert, mark, retain, release, caret — are the session's
	// ENTIRE cgo surface, and they are hooked as a set rather than one at a time
	// because they fail as a set: every one of them dereferences an IMK client
	// pointer, so a test that has no such pointer cannot call any of them, and
	// commit() refuses to run without one. Behind the seam the whole commit path
	// is exercisable — partial commits, which candidate a key picks, the pieces a
	// coined word is built from. In front of it, none of it was.
	caret func(client unsafe.Pointer) (x, y, w, h int, ok bool)

	// save persists the preferences; nil means the real file. A seam of the
	// same kind as the five above, for a smaller but sharper reason: the only
	// setting that changes at runtime (the candidate count, from the
	// input-source menu) writes ~/Library/Application Support/QuiIME/prefs.json,
	// and a test exercising that path must not rewrite the developer's own.
	save func(p prefs)

	// lastCaret* is where the host's insertion point was last seen, so the
	// mode indicator has somewhere to appear even when the toggle happens
	// outside a composition.
	lastCaretX, lastCaretY, lastCaretH int

	// debugKeys logs every key; loggedFirstKey makes the confirmation line
	// fire exactly once. See logKey.
	debugKeys      bool
	loggedFirstKey bool

	// client is the text client of the CURRENT composition, kept so a mouse
	// click on a candidate chip — which arrives outside any IMK callback —
	// knows where to commit. RETAINED while held (see setClient): ARC's
	// __bridge cast does not take a reference, and a host app closing a text
	// field mid-composition would otherwise leave this dangling.
	client unsafe.Pointer
}

// maxCandidates bounds what the engine is asked for.
//
// Enough to page through comfortably, far short of everything that matches:
// nobody pages 500 deep, and the cost of ranking candidates nobody will see
// is paid on every keystroke.
const maxCandidates = 60

// setClient replaces the cached client, retaining the new one and releasing
// the old. Funnelling every assignment through here is what keeps the
// retain/release balanced across the several paths that set it.
func (s *imeSession) setClient(client unsafe.Pointer) {
	if s.client == client {
		return
	}
	if s.release != nil {
		s.release(s.client)
	} else {
		releaseClient(s.client)
	}
	if s.retain != nil {
		s.client = s.retain(client)
	} else {
		s.client = retainClient(client)
	}
}

func newIMESession(panel *candidatePanel, eng *engine.Engine, user *engine.UserDict, p prefs) *imeSession {
	s := &imeSession{
		panel:     panel,
		eng:       eng,
		user:      user,
		punct:     newPunctuator(),
		prefs:     p,
		chinese:   true,
		deferToUI: panel.deferToUI,
		insert:    insertText,
		debugKeys: os.Getenv("QUI_IME_DEBUG_KEYS") != "",
		// Vacuously true of a composition with no pieces yet.
		pendingPicked: true,
	}
	panel.onPick = s.pickVisible
	return s
}

// --- IMK callbacks ---------------------------------------------------

// NSEventModifierFlag bit positions, spelled out rather than imported so the
// key-filtering rules are readable without cross-referencing AppKit.
const (
	modShift   = 1 << 17
	modControl = 1 << 18
	modOption  = 1 << 19
	modCommand = 1 << 20
)

// Virtual key codes (Carbon kVK_*). Since the bridge moved to raw NSEvents,
// the key code is the ONLY thing that identifies a non-text key: backspace
// arrives as U+007F, the arrows as private-use code points, and neither is
// something to match text against.
const (
	keyReturn = 36
	keyTab    = 48
	// Space is matched as TEXT, not by code — it is the one commit key that
	// carries a character. Named here anyway because it is now the key that
	// owns the Chinese-commit path Enter used to share (see keyReturn), and a
	// bare 49 in a test says nothing about that.
	keySpace       = 49
	keyBackspace   = 51 // kVK_Delete — the key labelled ⌫
	keyEscape      = 53
	keyKeypadEnter = 76
	keyPageUp      = 116
	keyPageDown    = 121
	keyLeftArrow   = 123
	keyRightArrow  = 124
	keyDownArrow   = 125
	keyUpArrow     = 126

	// Left and right Shift. Modifier keys have no character at all, so the
	// language-switch gesture is recognized purely by these.
	keyLeftShift  = 56
	keyRightShift = 60
)

// handleText is IMK's inputText:key:modifiers:client:. Returning true
// swallows the key.
func (s *imeSession) handleText(client unsafe.Pointer, text string, keyCode int, mods uint) bool {
	// Never swallow a shortcut. Cmd-C has to reach the host app even
	// mid-composition, or the input method breaks every app it is used in.
	if mods&(modCommand|modControl|modOption) != 0 {
		return false
	}
	s.logKey(text, keyCode, mods)

	// A key arrived, so any Shift that is down is a modifier being held, not
	// the tap-on-its-own gesture.
	s.disarmShift()

	if !s.chinese {
		return false // English mode: every key belongs to the host
	}

	// Non-text keys, by key code. Must come before any text matching: these
	// keys DO carry characters (backspace is U+007F, the arrows are
	// private-use code points), and letting them reach the punctuation or
	// passthrough branches below is what made backspace look like it
	// abandoned the composition.
	if handled, done := s.handleSpecialKey(client, keyCode, mods); done {
		return handled
	}

	if text == "" {
		return false
	}
	r := []rune(text)[0]

	// Digits select a candidate on the current page while composing.
	if len(text) == 1 && r >= '1' && r <= '9' && s.composing() {
		s.setClient(client)
		s.pickVisible(int(r - '1'))
		return true
	}

	// Paging. Both conventions are wired up because both are muscle memory
	// for somebody: -/= on the number row, and ,/. next to the space bar.
	if s.composing() {
		switch text {
		case "-", ",":
			s.setClient(client)
			s.pageBy(-1)
			return true
		case "=", ".":
			s.setClient(client)
			s.pageBy(+1)
			return true
		}
	}

	// Space commits the highlighted candidate, the way every pinyin IME does.
	if text == " " {
		if !s.composing() {
			return false
		}
		s.setClient(client)
		s.commit(s.sel, pickedByUser)
		return true
	}

	// An English word already in progress swallows letters and the apostrophe
	// literally — that is what "Sh" + "e" being She rather than 或 depends on.
	// Must come before the pinyin branch below, which would otherwise take the
	// lowercase letters back.
	if s.englishMode && len(text) == 1 && (isASCIILetter(r) || r == '\'') {
		s.buffer += text
		s.refresh(client)
		return true
	}

	// A leading "v" opens emoji mode. Only leading: inside a composition v is
	// ü, and "lv" must stay 绿.
	if !s.englishMode && !s.composing() && text == "v" {
		s.emojiMode = true
		s.buffer += text
		s.refresh(client)
		return true
	}

	// a-z extends a PINYIN composition. Lowercase only, deliberately: a
	// capital is not pinyin.
	//
	// Guarded on englishMode so that the branch above OWNS the letters while an
	// English word is in progress. Without the guard both branches append the
	// same byte and the English one is dead code — which is how mutation
	// testing found it: deleting it changed no behaviour and failed no test,
	// while leaving the file claiming a distinction it did not make.
	//
	// A capital also must not switch language. The Shift-tap gesture is a
	// press and a release with NOTHING in between, and this key is the
	// something in between: disarmShift ran at the top of this function, so
	// the release that follows has nothing left to fire. That ordering is
	// guaranteed and not a race — for the letter to arrive capitalised at all,
	// Shift must still have been down when it was pressed, so its key-down
	// always precedes the modifier's release.
	if !s.englishMode && len(text) == 1 && r >= 'a' && r <= 'z' {
		s.buffer += text
		s.refresh(client)
		return true
	}

	// A capital letter STARTS an English word.
	//
	// Typing a capital and letting the following letters fall back to pinyin
	// is the shape that made "She" impossible: the S went to the host as
	// literal text, then "he" opened a fresh pinyin composition and 或 won it.
	// Pressing Shift is the clearest signal a user can give that the next word
	// is not Chinese, so it switches the buffer over rather than ending it.
	if len(text) == 1 && r >= 'A' && r <= 'Z' {
		// Only ever reached with no English word in progress — a capital
		// arriving mid-word was taken by the branch above, which is what makes
		// "McD" extend rather than restart.
		//
		// Pinyin already in flight belongs BEFORE the English word, so it is
		// committed here. commitAll ends with a reset, which clears the way.
		if s.composing() {
			s.setClient(client)
			s.commitAll()
		}
		s.englishMode = true
		s.buffer += text
		s.refresh(client)
		return true
	}

	// An apostrophe forces a syllable boundary, but only inside a PINYIN
	// composition — on its own it is a quote mark, and inside an English word
	// it is a letter, taken literally by the branch above ("Don't").
	if !s.englishMode && text == "'" && s.composing() {
		s.buffer += text
		s.refresh(client)
		return true
	}

	// Punctuation. Flushes any composition first so the marks land in the
	// order they were typed, then goes in as its Chinese form.
	//
	// Inserts through `client`, not s.client: flushing ends the composition,
	// which clears the cached client, and inserting through the cleared one
	// would drop the punctuation on the floor.
	if s.prefs.ChinesePunctuation {
		if converted := s.punct.convert(r); converted != "" {
			s.setClient(client)
			s.commitAll()
			s.emit(client, converted)
			s.reset(client)
			if strings.ContainsRune(sentenceEnders, r) {
				s.endSentence()
			}
			return true
		}
	}

	// Anything else — capitals, digits outside a composition, the symbols the
	// punctuation table leaves alone — goes through as literal text.
	return s.passLiteral(client, text)
}

// passLiteral ends any composition and then lets text through unchanged, so it
// lands after what was already being typed rather than before it.
//
// With nothing composing it returns false and inserts nothing: there is no
// ordering left to enforce, so the host app should type the key itself. That
// keeps its own behaviour intact — autocorrect, undo grouping, key repeat —
// none of which an input method should be reimplementing for a plain letter.
func (s *imeSession) passLiteral(client unsafe.Pointer, text string) bool {
	if !s.composing() {
		return false
	}
	s.setClient(client)
	s.commitAll()
	s.emit(client, text)
	s.reset(client)
	// Whatever this was — a digit, a symbol the punctuation table leaves alone —
	// it is not a word, so it ends the phrase rather than joining it.
	s.endSentence()
	return true
}

// emit puts text into the host document, through the seam so tests can see it.
func (s *imeSession) emit(client unsafe.Pointer, text string) {
	if s.insert != nil {
		s.insert(client, text)
		return
	}
	insertText(client, text)
}

// commitLiteral sends the letters AS TYPED and ends the composition.
//
// Shift tap always uses this path to say the letters were English. Enter uses
// it only when no complete Chinese candidate can be confirmed; that keeps the
// raw-text escape hatch for URLs, passwords and unknown words while allowing a
// normal Chinese Enter to preserve the committed word's reading/context.
func (s *imeSession) commitLiteral(client unsafe.Pointer) {
	s.setClient(client)
	typed := s.typed()
	// The strongest signal this input method ever gets that a word is worth
	// remembering. The user looked at what the engine offered, rejected all of
	// it, and typed the spelling out themselves — which is precisely the case a
	// fixed lexicon cannot cover and precisely what learning is for. Recording
	// candidate picks but not this one was why a brand or a piece of jargon
	// never stuck: the way you enter it is the one way that did not learn.
	code := strings.ToLower(typed)
	s.user.RecordLiteral(code, typed)
	s.user.RecordBigram(s.lastWord, []string{code}, typed)
	// The reading of a literal is the letters themselves: that IS how it is
	// typed, which is exactly what the context memory needs to offer it again.
	s.noteCommitted([]string{code}, typed)
	s.emit(client, typed)
	s.reset(client)
}

// forgetSelected drops what the user dictionary has learned about the
// highlighted candidate, so it falls back to whatever the lexicon says.
//
// The context passed in is s.lastWord, which is the key the pair was recorded
// under — forgetting 关 while 博物 is on screen has to reach 博物→关, not just
// guan→关, or the candidate that is bothering the user right now survives.
//
// A candidate the history says nothing about is a silent no-op rather than a
// fallthrough to ordinary backspace: the user asked to forget it, and eating a
// letter instead would be a surprising way to say "there was nothing to forget".
func (s *imeSession) forgetSelected() {
	c, ok := s.selected()
	if !ok || len(c.Reading) == 0 {
		return
	}
	gone := s.user.Forget(s.lastWord, c.Reading, c.Word)
	logf("forget %q (reading %v, after %q): %d entries removed",
		c.Word, c.Reading, s.lastWord, gone)
}

// caretAt reports the host's insertion point, through the test hook when one is
// installed.
func (s *imeSession) caretAt(client unsafe.Pointer) (x, y, w, h int, ok bool) {
	if s.caret != nil {
		return s.caret(client)
	}
	return caretRect(client)
}

// setMarked shows text as the in-progress preedit, through the test hook when
// one is installed.
func (s *imeSession) setMarked(client unsafe.Pointer, text string) {
	if s.mark != nil {
		s.mark(client, text)
		return
	}
	setMarkedText(client, text)
}

// commitKind says whether text reached the document because the user picked it,
// or because the composition was in the way of something else.
//
// Only a pick is evidence of preference, and only evidence of preference belongs
// in the user dictionary. Before this distinction existed, every path that put
// text on screen wrote to it at full weight, and the two most common ones are
// not choices at all: Space commits whatever is HIGHLIGHTED, which is candidate
// #1 unless the user moved the selection, and commitAll flushes every piece of
// the buffer when a capital letter, a punctuation mark or a focus change
// interrupts a composition.
//
// That made the ranking self-reinforcing. Measured on the real profile that
// reported it: 关 is the top candidate for `guan`, so committing after 博物 and
// carrying on recorded 博物→关 — which made 关 more firmly the top candidate. Two
// such commits were enough to bury 馆, and undoing them takes five deliberate
// picks, because userScale·ln(count+1) is flat and 关 leads on lexicon frequency
// by 1.96 besides. A wrong default that trains itself is much harder to leave
// than one that just sits there.
type commitKind int

const (
	// pickedByUser is a key whose whole meaning is "commit this": Space, Enter,
	// Tab, a digit, a click on the panel, a Shift tap.
	pickedByUser commitKind = iota
	// flushed is the composition being got out of the way — a capital letter
	// starting an English word, a punctuation mark, any other literal key, or
	// focus leaving the field. The text is still committed, and still becomes
	// the CONTEXT for what follows; it just does not claim to be a preference.
	flushed
)

// learn records a committed candidate.
//
// English candidates go in the literal namespace, keyed by the letters that
// were typed rather than by a reading. "AfterShip" is not a reading of
// anything, and what has to come back next time is its SPELLING — see
// engine.UserDict's literalKey.
//
// A flush updates the context and nothing else: what is on screen is a fact and
// the next query should see it, but nobody chose it.
func (s *imeSession) learn(c engine.Candidate, kind commitKind) {
	if kind == pickedByUser {
		if c.Source == engine.SourceEnglish && len(c.Reading) == 1 {
			s.user.RecordLiteral(c.Reading[0], c.Word)
		} else {
			s.user.Record(c.Reading, c.Word)
		}
		// Before noteCommitted, which is what moves lastWord on.
		s.user.RecordBigram(s.lastWord, c.Reading, c.Word)
	}
	s.noteCommitted(c.Reading, c.Word)
	if c.Source == engine.SourceEmoji {
		// An emoji ends the thought the way a full stop does. The pair INTO it
		// is worth keeping — 开心 is often followed by 😄 — but pairing the next
		// word with a picture would train a phrase nobody said.
		s.endSentence()
	}
}

// noteCommitted makes this word the context for whatever comes next.
//
// Context only — the user dictionary is written by the callers, and only when
// the commit was a pick. What ends up on screen decides what the next query is
// ranked against whether or not anybody chose it.
//
// Called from every path that puts text in the document, including the literal
// ones: what matters for context is what ended up in the document, not which
// branch produced it.
func (s *imeSession) noteCommitted(reading []string, word string) {
	if word == "" {
		return
	}
	s.lastWord = word
	s.lastReading = append(s.lastReading[:0], reading...)

	// The reading is copied rather than retained: it comes off a Candidate, and
	// the engine reuses its scratch between queries.
	run := committedRun{word: word, reading: append([]string(nil), reading...)}
	if len(s.recent) == maxRecentRuns {
		copy(s.recent, s.recent[1:])
		s.recent[len(s.recent)-1] = run
		return
	}
	s.recent = append(s.recent, run)
}

// endSentence drops the context. A word after a full stop continues nothing,
// and carrying the previous clause's last word across would train pairs that
// were never a phrase.
func (s *imeSession) endSentence() {
	s.lastWord = ""
	s.lastReading = nil
	s.recent = s.recent[:0]
}

// committedRun is one piece of text this session put in the document, with the
// reading it was typed as.
type committedRun struct {
	word    string
	reading []string
}

// maxRecentRuns is how many committed pieces may be joined when looking for a
// phrase continuation.
//
// Three, which is what it costs. Each length tried is another full lexicon
// query on the keystroke path, so this is a budget rather than a belief about
// language — and the guard in Engine.MayContinue means the lengths that could
// not possibly extend into a phrase are rejected by a few trie steps instead.
// Three covers the way phrases actually get typed a piece at a time: 博 + 物,
// 中华 + 人民, 长 + 城 + 站.
const maxRecentRuns = 3

// sentenceEnders are the marks that finish a thought. Commas and the like are
// deliberately absent: "我们，明天…" is one sentence, and the words on either
// side of that comma go together.
const sentenceEnders = "。！？；.!?;\n\r"

// logKey records the very first key of a session, and every key when
// QUI_IME_DEBUG_KEYS is set.
//
// The first-key line is not optional noise: which IMKServerInput path IMK
// actually drives is not knowable from the headers, and this one line in the
// log is how anyone confirms keys are arriving at all — in a process with no
// console, attached to an app it must not disturb. Getting this wrong is
// exactly how backspace ended up handled by code that never ran.
func (s *imeSession) logKey(text string, keyCode int, mods uint) {
	if s.debugKeys {
		logf("key: code=%d text=%q mods=%#x composing=%v", keyCode, text, mods, s.composing())
		return
	}
	if !s.loggedFirstKey {
		s.loggedFirstKey = true
		logf("first key received: code=%d text=%q — the event path is live", keyCode, text)
	}
}

// handleSpecialKey dispatches the non-text keys by key code.
//
// Returns (handled, done): done says this key code is one this function owns,
// handled says whether to swallow it. The two are separate because a key can
// be ours to decide about and still belong to the app — Escape with nothing
// composing must reach the host, or the input method breaks every dialog.
func (s *imeSession) handleSpecialKey(client unsafe.Pointer, keyCode int, mods uint) (handled, done bool) {
	switch keyCode {
	case keyBackspace:
		if !s.composing() {
			return false, true
		}
		s.setClient(client)
		if mods&modShift != 0 {
			// Shift+⌫ forgets the highlighted candidate rather than deleting a
			// letter. Same gesture as Rime, and the reason it is worth a key at
			// all is that a wrong entry is otherwise permanent in practice:
			// userScale·ln(count+1) is flat enough that outvoting one takes five
			// deliberate picks, and until then it keeps being offered first.
			s.forgetSelected()
			s.refresh(client)
			return true, true
		}
		// Buffer is always ASCII (a-z and the apostrophe), so trimming one
		// byte is trimming one typed character.
		s.buffer = s.buffer[:len(s.buffer)-1]
		s.refresh(client)
		return true, true

	case keyEscape:
		if !s.composing() {
			return false, true
		}
		s.reset(client)
		return true, true

	case keyReturn, keyKeypadEnter:
		if !s.composing() {
			return false, true
		}
		// Enter is the raw-text escape hatch, UNCONDITIONALLY: it puts the
		// letters exactly as typed into the document, whatever the candidate
		// list is offering. `nihao` + Enter is "nihao", not 你好.
		//
		// The keys that confirm Chinese are Space and the digits, and nothing
		// else — that split is the whole point. Enter used to confirm the
		// highlighted candidate whenever one covered the buffer, which made it a
		// second Space with a dead end behind it: a run of letters that is not
		// Chinese at all usually has SOME reading covering every one of them, so
		// Enter confirmed that instead. `bigquery` reads as bi'g'qu'er'y and
		// offers 比过去而言 at full coverage — leaving no way to put the word
		// itself in the document, let alone teach it.
		//
		// A separate key rather than a smarter rule, because the input cannot
		// tell you which was meant. Force-fitting is not the signal: `zhg` →
		// 中国 also spends two bare initials, and it is exactly right. Neither is
		// being a stitched sentence: `womenqu` → 我们去 is stitched and also
		// right. The only thing that knows is the user, and now the answer is a
		// key apart instead of a heuristic.
		//
		// Shift+Enter stays wired to the same thing. It was the escape hatch
		// before this, so somebody has it in their fingers, and a modifier that
		// silently stops working is worse than one that is merely redundant.
		s.commitLiteral(client)
		return true, true

	case keyLeftArrow:
		if !s.composing() {
			return false, true
		}
		s.moveSelection(-1)
		return true, true

	case keyRightArrow:
		if !s.composing() {
			return false, true
		}
		s.moveSelection(+1)
		return true, true

	case keyUpArrow, keyPageUp:
		if !s.composing() {
			return false, true
		}
		s.setClient(client)
		s.pageBy(-1)
		return true, true

	case keyDownArrow, keyPageDown:
		if !s.composing() {
			return false, true
		}
		s.setClient(client)
		s.pageBy(+1)
		return true, true

	case keyTab:
		// Swallowed while composing so Tab cannot move focus out of a field
		// that still has an unfinished preedit in it.
		if !s.composing() {
			return false, true
		}
		s.setClient(client)
		s.commit(s.sel, pickedByUser)
		return true, true
	}
	return false, false
}

// handleCommand is IMK's commitComposition:, which is a direct call on the
// controller rather than part of the event dispatch — so unlike
// didCommandBySelector: it is still delivered under the raw-NSEvent approach.
func (s *imeSession) handleCommand(client unsafe.Pointer, selector string) bool {
	switch selector {
	case "commitComposition:":
		// Focus is leaving. Flush EVERYTHING so the host app is not left
		// with a marked range nothing will finish.
		if s.composing() {
			s.setClient(client)
			s.commitAll()
		}
		s.reset(client)
		// A Shift tap waiting to switch language belonged to the field being
		// left, so it must not land in whatever gets focus next.
		s.cancelLanguageToggle()
		return true
	}
	return false
}

// handleFlags implements "tap Shift on its own to switch language".
//
// Modifier transitions have no character and no keybinding, so they are only
// visible through the raw-NSEvent path — which is the reason this input
// method uses it. See bridge_darwin.m.
//
// The gesture is a Shift press and release with NOTHING in between. Shift
// held down to type a capital must not toggle anything, so the press only
// ARMS the gesture and any other key disarms it (disarmShift, called from
// handleText).
//
// ALWAYS returns false: the host app needs to see modifier changes, and
// swallowing them would break shift-click, drag modifiers, and every
// hold-to-modify behaviour in whatever is being typed into.
func (s *imeSession) handleFlags(client unsafe.Pointer, mods uint, keyCode int) bool {
	if keyCode != keyLeftShift && keyCode != keyRightShift {
		// Some other modifier moved. That disqualifies the gesture, whether it
		// is still being armed or already scheduled: Shift-Cmd is a shortcut
		// being assembled, not a language switch.
		s.shiftArmed = false
		s.cancelLanguageToggle()
		return false
	}
	if mods&modShift != 0 {
		// Press. Only arm when Shift is alone.
		s.shiftArmed = mods&(modCommand|modControl|modOption) == 0
		return false
	}
	// Release.
	if s.shiftArmed {
		s.shiftArmed = false
		s.scheduleLanguageToggle(client)
	}
	return false
}

// shiftToggleDelay is how long a Shift tap waits before it actually switches
// language, so a key arriving in the meantime can call it off.
//
// This exists for one specific mis-fire. Shift+A is unambiguous — for the
// letter to arrive capitalised, Shift must still have been down when it was
// pressed, so its key-down always precedes the modifier's release and always
// disarms the gesture. But a fast typist sometimes releases Shift a hair
// BEFORE pressing the letter. The letter then comes out lowercase and, to the
// input method, the sequence is indistinguishable from a deliberate tap
// followed by a letter — so the mode flipped, which is a STICKY error: every
// key after it is wrong until the user notices.
//
// Waiting turns that into a much smaller error. The letter still arrives
// lowercase — nothing here can conjure the capital the user wanted — but it
// lands as pinyin in the mode they were already in, which the candidate panel
// makes obvious immediately and one backspace undoes.
//
// 60ms is chosen to sit well under the gap between separate keystrokes:
// typing at 100 WPM is about 125ms per character, while the sloppy Shift and
// its letter are one intended keystroke and land within a few milliseconds of
// each other. The cost is the reverse mistake — deliberately tapping Shift and
// then typing within 60ms cancels the switch — which is why the window is this
// short rather than the 200ms that would catch every sloppy press.
const shiftToggleDelay = 60 * time.Millisecond

// scheduleLanguageToggle commits to switching language shiftToggleDelay from
// now, unless something disqualifies it first.
func (s *imeSession) scheduleLanguageToggle(client unsafe.Pointer) {
	s.shiftSeq++
	seq := s.shiftSeq
	s.setPendingShiftClient(client)

	s.deferToUI(shiftToggleDelay, func() {
		// Runs on the UI thread, so this reads the same shiftSeq the key
		// handlers write; no locking, and no toggle racing a keystroke.
		if s.shiftSeq != seq {
			return // a key arrived: that was not a bare Shift tap
		}
		s.toggleLanguage(s.pendingShiftClient)
		s.setPendingShiftClient(nil)
	})
}

// cancelLanguageToggle calls off a scheduled switch. Cheap and idempotent, so
// it can sit on every path that means "the gesture is over".
func (s *imeSession) cancelLanguageToggle() {
	s.shiftSeq++
	s.setPendingShiftClient(nil)
}

// setPendingShiftClient replaces the retained client a scheduled switch will
// use. Funnelled like setClient, and for the same reason: the retain and the
// release have to stay balanced across the several paths that clear it.
func (s *imeSession) setPendingShiftClient(client unsafe.Pointer) {
	if s.pendingShiftClient == client {
		return
	}
	releaseClient(s.pendingShiftClient)
	s.pendingShiftClient = retainClient(client)
}

// toggleLanguage switches between Chinese input and pass-through English.
func (s *imeSession) toggleLanguage(client unsafe.Pointer) {
	// Anything half-composed goes in AS LETTERS, not as its top candidate.
	//
	// This used to commit the candidate, and that made the gesture unusable for
	// the case it is most wanted in: typing "go" offers 公 (correctly — in
	// Chinese mode those letters are pinyin), and tapping Shift to say "that
	// was English" produced 公 followed by an English mode. The letters were
	// gone and had to be retyped.
	//
	// A Shift tap is a statement about the letters already typed, so it has to
	// be retroactive: the same escape hatch as Enter, reached by the key that
	// also says what to do next. See commitLiteral.
	if s.composing() {
		s.commitLiteral(client)
	}
	s.chinese = !s.chinese
	logf("language mode: chinese=%v", s.chinese)

	// Say so on screen. A mode that changes what every key does, with no
	// visible sign of which one is current, is a mode users fight instead of
	// use.
	if client != nil {
		if x, y, _, h, ok := s.caretAt(client); ok {
			s.lastCaretX, s.lastCaretY, s.lastCaretH = x, y, h
		}
	}
	if s.lastCaretH == 0 {
		// Never seen an insertion point — toggled before typing anything, or
		// a host that will not report one. Better to say nothing than to
		// flash a panel in a screen corner unrelated to where they are working.
		return
	}
	label := "英 / EN"
	if s.chinese {
		label = "中 / CN"
	}
	s.panel.showNotice(label, s.lastCaretX, s.lastCaretY, s.lastCaretH)
}

// disarmShift cancels the Shift gesture, at whichever of its two stages it has
// reached: still being armed (Shift is down), or already scheduled and waiting
// out shiftToggleDelay.
//
// Called whenever a real key arrives, which is what separates "tapped Shift"
// from "held Shift to type a capital letter" — and, thanks to the scheduling
// delay, also from "released Shift a moment too early".
func (s *imeSession) disarmShift() {
	s.shiftArmed = false
	s.cancelLanguageToggle()
}

func (s *imeSession) deactivate(client unsafe.Pointer) {
	s.reset(client)
	s.punct.reset()
	s.shiftArmed = false
	// Focus is leaving this field. Whatever is typed next is somewhere else, and
	// pairing it with the last word from here would be inventing a phrase that
	// spans two documents.
	s.endSentence()
	// The client this would have flushed through is going away, so a switch
	// still waiting to run has nowhere to put a half-typed composition.
	s.cancelLanguageToggle()
}

// --- state -----------------------------------------------------------

func (s *imeSession) composing() bool { return s.buffer != "" }

// typed is the buffer as the engine and the host see it: separators stripped
// for pinyin, verbatim for an English word, where an apostrophe is a letter of
// the word ("Don't") rather than a syllable boundary.
func (s *imeSession) typed() string {
	if s.englishMode {
		return s.buffer
	}
	return strings.ReplaceAll(s.buffer, "'", "")
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// refresh recomputes candidates for the current buffer and moves the panel
// under the caret. The panel is shown from here rather than from the key
// handler so every path that changes the buffer gets the same treatment.
func (s *imeSession) refresh(client unsafe.Pointer) {
	s.setClient(client)
	if !s.composing() {
		s.reset(client)
		return
	}
	s.setCandidates()

	s.setMarked(client, s.preedit())
	s.updatePanel()

	if x, y, _, h, ok := s.caretAt(client); ok {
		s.lastCaretX, s.lastCaretY, s.lastCaretH = x, y, h
		s.panel.showBelowCaret(x, y, h)
		return
	}
	// Some hosts (a few Electron and Java apps) refuse to report a caret.
	// Showing the panel at the origin would be worse than leaving it where
	// it was, so keep the last position and just make sure it is visible.
	logf("client reported no caret rect; keeping previous panel position")
	s.panel.show()
}

// setCandidates rebuilds the candidate list for the current buffer and resets
// the selection to the top.
//
// Split out of refresh so the whole list-building path — engine, the English
// reserved slot, the empty-list fallback — is reachable without a panel, a
// window or a text client. refresh cannot be called in a test; this can, which
// is the difference between testing the rules and testing that they are
// actually wired up.
func (s *imeSession) setCandidates() {
	// The engine ranks against whatever was committed last; it is set here
	// rather than at commit time so every path that rebuilds the list — including
	// the tests, which drive setCandidates directly — sees the same context.
	s.eng.SetContext(s.lastWord)
	if s.emojiMode {
		// Everything after the "v" is an ordinary pinyin query; only the
		// answers differ. An empty query (just "v") has nothing to look up yet.
		s.cands = s.eng.EmojiWords(strings.TrimPrefix(s.typed(), "v"), maxCandidates)
		if len(s.cands) == 0 {
			// Same reasoning as the pinyin fallback below: Space must commit
			// something rather than making the text vanish. The letters as
			// typed, minus nothing — the v included, because that is what was
			// pressed.
			clean := s.typed()
			s.cands = []engine.Candidate{{Word: clean, Consumed: len(clean)}}
		}
		s.sel, s.pageStart = 0, 0
		return
	}
	if s.englishMode {
		// No pinyin work at all, and no reserved slot: every candidate here is
		// already English, and the first is the text exactly as typed.
		s.cands = s.eng.EnglishWords(s.buffer, maxCandidates)
		s.sel, s.pageStart = 0, 0
		return
	}
	s.cands = s.eng.Candidates(s.buffer, maxCandidates)
	// The ordinary query remains the source of truth for ranking. These phrase
	// suffixes are an additional path for the common case where the previous
	// text is already committed and the user starts a continuation. A learned
	// context candidate still wins through the normal accumulator; this path
	// only supplies dictionary phrases the new input cannot reach by itself.
	cont := s.continuations()
	if len(cont) > 0 {
		s.cands = mergeCandidates(s.cands, cont)
	}
	s.cands = reserveContextSlot(s.cands, cont, s.pageSize())
	s.cands = reserveEnglishSlot(s.cands, s.pageSize())
	if len(s.cands) == 0 {
		// Nothing matched. Offer the letters themselves rather than an empty
		// list: no syllable begins with i, u or v, so typing one of those —
		// or any typo — would otherwise leave Space with nothing to commit
		// and the text would simply vanish.
		clean := strings.ReplaceAll(s.buffer, "'", "")
		s.cands = []engine.Candidate{{Word: clean, Consumed: len(clean)}}
	}
	s.sel, s.pageStart = 0, 0
}

// continuations asks for dictionary-phrase suffixes of the text this session
// has committed into the current field, trying the longest run it remembers as
// well as the shorter tails inside it.
//
// More than the last word, because a phrase gets typed a piece at a time. 博物
// committed as one word and 博物 committed as 博 then 物 put the same three
// characters on screen; asking only about 物 finds 物馆, which is not a word, and
// 博物馆 is never reached. Every length is tried rather than just the longest
// because the pieces may not belong together at all — 我 then 说, and there is no
// phrase 我说… to continue, only whatever follows 说.
//
// The lengths that cannot possibly extend into a phrase never reach the lexicon
// query: MayContinue rejects them with a few trie steps.
//
// Results merge by score, and the scores are comparable across context lengths —
// a continuation is always scored for the input it explains, never for the text
// already on screen. So the winner is the best suffix, whichever run produced it.
//
// # Two queries, not one per length
//
// Engine.Continuations costs a full ranked lookup over the joined reading plus
// the input, and that is the expensive half of a keystroke: measured on the
// shipped lexicon, three lengths took 1.5ms per keystroke against 90µs with no
// context at all. So only two lengths are asked about — the last committed word,
// which is the behaviour this feature already had and must not lose, and the
// LONGEST joined run the lexicon could extend. The lengths in between are the
// least informative: whatever a middle run can continue, the longest run
// usually continues too, and more specifically.
func (s *imeSession) continuations() []engine.Candidate {
	if s.buffer == "" || len(s.recent) == 0 {
		return nil
	}
	out := s.continueFrom(s.recent[len(s.recent)-1:])
	for n := len(s.recent); n >= 2; n-- {
		if runs := s.recent[len(s.recent)-n:]; s.canContinue(runs) {
			out = mergeCandidates(out, s.continueFrom(runs))
			break
		}
	}
	return out
}

// canContinue is the cheap gate: a few trie steps that say whether asking the
// lexicon about this run could possibly return anything.
func (s *imeSession) canContinue(runs []committedRun) bool {
	_, reading := joinRuns(runs)
	return s.eng.MayContinue(reading)
}

func (s *imeSession) continueFrom(runs []committedRun) []engine.Candidate {
	word, reading := joinRuns(runs)
	if !s.eng.MayContinue(reading) {
		return nil
	}
	return s.eng.Continuations(word, reading, s.buffer, 8)
}

// joinRuns concatenates committed pieces into the one word and one reading the
// lexicon has to be asked about.
func joinRuns(runs []committedRun) (string, []string) {
	if len(runs) == 1 {
		return runs[0].word, runs[0].reading
	}
	var word strings.Builder
	reading := make([]string, 0, 4*len(runs))
	for _, r := range runs {
		word.WriteString(r.word)
		reading = append(reading, r.reading...)
	}
	return word.String(), reading
}

// reserveContextSlot keeps phrase-like continuations visible without promoting
// them to the Space-committed position. The first reserved item is the best
// direct suffix (岭 for 八达 + `l`); when there is no English candidate, a
// second slot also carries the best longer phrase suffix (了吗 for 吃饭 + `l`).
//
// The second-last slot is deliberate: English owns the last slot, and the two
// presentation guarantees should compose rather than overwrite each other.
//
// WHICH suffix deserves the slot is decided by `cont` — the continuation list in
// the order the engine ranked it — and never by scanning the merged list for a
// SourceContext label. Two things go wrong with the label:
//
//   - It does not survive the merge. 八达 + `l` reaches 岭 as a suffix of 八达岭
//     (score 3.0) and, for a user who has picked 岭 before, as a learned
//     candidate for `l` (score 5.7). The higher score rightly wins, and takes
//     SourceUser with it — so the slot went to the next-best suffix 岭镇, whose
//     move pushed 岭 off the end of the page. 八达岭 was one keystroke away when
//     typed as `badal` and unreachable a keystroke after committing 八达.
//   - Merged order is not continuation order. 长 + `c` reaches eight
//     single-character suffixes; the one worth showing is 城 (长城), but 长
//     (长长) sits higher in the merged list on its own merits and would satisfy
//     an "is a continuation already visible" test while 城 stayed off-page.
func reserveContextSlot(cands, cont []engine.Candidate, pageSize int) []engine.Candidate {
	if pageSize < 3 || len(cands) <= pageSize || len(cont) == 0 {
		return cands
	}
	slot := pageSize - 2
	// cont is ranked, so its head is the suffix the phrase ranking liked best.
	firstWord := cont[0].Word
	// The longest suffix is the phrase-shaped one — 了吗 rather than 了 — and it
	// is worth a slot of its own because no amount of ranking on a single letter
	// will surface a two-character continuation.
	longestWord, longestLen := "", 1
	for _, c := range cont {
		if l := utf8.RuneCountInString(c.Word); l > longestLen {
			longestWord, longestLen = c.Word, l
		}
	}
	hasEnglish := false
	for _, c := range cands {
		if c.Source == engine.SourceEnglish {
			hasEnglish = true
			break
		}
	}
	move := func(word string, target int) {
		if word == "" || target < 0 || target >= len(cands) {
			return
		}
		idx := -1
		for i, c := range cands {
			if c.Word == word {
				idx = i
				break
			}
		}
		// Already on the page under its own steam: leave it where it earned.
		if idx < 0 || idx <= target {
			return
		}
		moved := cands[idx]
		copy(cands[target+1:idx+1], cands[target:idx])
		cands[target] = moved
	}
	move(firstWord, slot)
	if !hasEnglish && longestWord != firstWord {
		move(longestWord, slot+1)
	}
	return cands
}

// mergeCandidates folds phrase continuations into the ordinary ranking, keeping
// the better-scoring version of a word that both lists contain.
//
// A word that wins on the base score keeps the base score AND the base label —
// so a continuation the user has picked before comes out of here marked
// SourceUser, not SourceContext. That is why reserveContextSlot is handed the
// continuation list separately instead of looking for the label in here.
func mergeCandidates(base, extra []engine.Candidate) []engine.Candidate {
	if len(extra) == 0 {
		return base
	}
	byWord := make(map[string]engine.Candidate, len(base)+len(extra))
	for _, c := range base {
		byWord[c.Word] = c
	}
	for _, c := range extra {
		if old, ok := byWord[c.Word]; !ok || c.Score > old.Score {
			byWord[c.Word] = c
		}
	}
	out := make([]engine.Candidate, 0, len(byWord))
	for _, c := range byWord {
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Word < out[j].Word
	})
	return out
}

// reserveEnglishSlot guarantees that an exact English match is on the first
// page, by moving it to the last slot of that page if it did not get there on
// score alone.
//
// This is a presentation rule, which is why it lives here and not in the
// ranking: the engine has no idea how many candidates fit on a page.
//
// The ranking on its own is not enough. It correctly puts "hello" first —
// nothing Chinese explains those letters without force-fitting them — and it
// correctly refuses to put "song" first, because 送 and 松 are what somebody
// typing "song" in Chinese mode almost always means. But "correctly refuses to
// win" came out as rank 26, and a candidate on page six is not offered in any
// sense the user would recognise. So the score decides whether English is
// FIRST, and this decides that it is at least VISIBLE.
//
// The displaced candidate is not lost, only pushed one place back, and the top
// pick — the one Space commits — is never touched: the slot taken is the last
// one on the page, never the first.
func reserveEnglishSlot(cands []engine.Candidate, pageSize int) []engine.Candidate {
	if pageSize < 2 || len(cands) <= pageSize {
		return cands // everything is already on the first page
	}
	slot := pageSize - 1
	for i, c := range cands {
		if c.Source != engine.SourceEnglish {
			continue
		}
		if i <= slot {
			return cands // it earned a place on the page; leave the order alone
		}
		// Only the best English word gets the reserved slot. Any alternates
		// sharing the same code ("he'll" for "hell") keep the rank they
		// scored, so one guaranteed slot does not turn into four.
		moved := cands[i]
		copy(cands[slot+1:i+1], cands[slot:i])
		cands[slot] = moved
		return cands
	}
	return cands
}

// preedit is what the host app shows underlined while composing.
//
// The TYPED characters, with a separator at each syllable boundary the
// highlighted candidate implies: "nihao" shows as "ni'hao". Segmenting is
// what lets a user spot a mis-reading ("xian" taken as one syllable when they
// meant xi'an) and fix it with an apostrophe rather than deleting everything
// and wondering what went wrong.
//
// It shows the typed letters and NOTHING ELSE. An earlier version spelled out
// the candidate's reading instead, which put letters on screen that were
// never pressed — type "h" and the preedit read "huo", because that is how
// the engine understood it. The engine's interpretation belongs in the
// candidate list, which is where the user can accept or reject it; the
// preedit is a record of what they typed.
func (s *imeSession) preedit() string {
	if s.englishMode || s.emojiMode {
		// Literal text: there are no syllables to segment and no separators to
		// strip, and the capitals — or the leading v — are part of what was
		// typed.
		return s.buffer
	}
	clean := strings.ReplaceAll(s.buffer, "'", "")
	c, ok := s.selected()
	if !ok || len(c.Spans) == 0 {
		return clean
	}

	var b strings.Builder
	b.Grow(len(clean) + len(c.Spans))
	prev := 0
	for _, end := range c.Spans {
		if end <= prev || end > len(clean) {
			continue // a predicted syllable, or a span past what was typed
		}
		if prev > 0 {
			b.WriteByte('\'')
		}
		b.WriteString(clean[prev:end])
		prev = end
	}
	// Whatever the candidate does not account for still has to be visible —
	// it is text the user typed and is presumably still working on.
	if prev < len(clean) {
		if prev > 0 {
			b.WriteByte('\'')
		}
		b.WriteString(clean[prev:])
	}
	return b.String()
}

func (s *imeSession) selected() (engine.Candidate, bool) {
	if s.sel < 0 || s.sel >= len(s.cands) {
		return engine.Candidate{}, false
	}
	return s.cands[s.sel], true
}

// pageSize is how many candidates are visible at once.
func (s *imeSession) pageSize() int {
	if s.prefs.PageSize > 0 {
		return s.prefs.PageSize
	}
	return defaultPrefs().PageSize
}

// setPageSize changes how many candidates are offered and remembers it.
//
// Called from the input-source menu, which is reachable WHILE composing —
// macOS does not close the composition to open that menu. So the current list
// is rebuilt rather than merely redrawn: the reserved English and context slots
// are positioned relative to the page size (reserveEnglishSlot puts the English
// candidate at pageSize-1), and a list built for a page of five shows the
// reservation in the wrong place on a page of eight.
//
// Persisted immediately. The alternative is writing at exit, and an input
// method exits by being killed at logout as often as not.
func (s *imeSession) setPageSize(n int) {
	n = clampPageSize(n)
	if n == s.pageSize() {
		return
	}
	s.prefs.PageSize = n
	if s.save != nil {
		s.save(s.prefs)
	} else {
		writePrefs(s.prefs)
	}
	if !s.composing() {
		return
	}
	s.setCandidates()
	s.syncPreedit()
	s.updatePanel()
}

// visible returns the candidates on the current page.
func (s *imeSession) visible() []engine.Candidate {
	if s.pageStart >= len(s.cands) {
		return nil
	}
	end := s.pageStart + s.pageSize()
	if end > len(s.cands) {
		end = len(s.cands)
	}
	return s.cands[s.pageStart:end]
}

// pageBy moves the page by delta pages, clamped. Returns without redrawing
// if the move would go past either end, so holding the key does not flicker.
func (s *imeSession) pageBy(delta int) {
	if !s.composing() {
		return
	}
	next := s.pageStart + delta*s.pageSize()
	if next < 0 || next >= len(s.cands) {
		return
	}
	s.pageStart = next
	s.sel = next // land on the first candidate of the new page
	s.syncPreedit()
	s.updatePanel()
}

// moveSelection walks the highlight, paging when it runs off either edge.
func (s *imeSession) moveSelection(delta int) {
	if !s.composing() || len(s.cands) == 0 {
		return
	}
	next := s.sel + delta
	if next < 0 || next >= len(s.cands) {
		return
	}
	s.sel = next
	// Keep the highlight on screen.
	for s.sel < s.pageStart {
		s.pageStart -= s.pageSize()
	}
	for s.sel >= s.pageStart+s.pageSize() {
		s.pageStart += s.pageSize()
	}
	if s.pageStart < 0 {
		s.pageStart = 0
	}
	s.syncPreedit()
	s.updatePanel()
}

// syncPreedit re-marks the host's text to match the highlighted candidate,
// so the underlined reading follows the selection.
func (s *imeSession) syncPreedit() {
	if s.client != nil {
		s.setMarked(s.client, s.preedit())
	}
}

func (s *imeSession) updatePanel() {
	s.panel.update(panelState{
		English:    s.englishMode,
		Preedit:    s.preedit(),
		Candidates: s.visible(),
		Selected:   s.sel - s.pageStart,
		PageStart:  s.pageStart,
		Total:      len(s.cands),
		PageSize:   s.pageSize(),
	})
}

// pickVisible commits the candidate at index i on the CURRENT PAGE, which is
// what a digit key or a mouse click means.
func (s *imeSession) pickVisible(i int) {
	if i < 0 || i >= len(s.visible()) {
		return // out-of-range digit: swallowed rather than emitting a stray "7"
	}
	s.commit(s.pageStart+i, pickedByUser)
}

// commit sends candidate idx to the host app and ends — or shortens — the
// composition.
//
// A candidate need not account for the whole buffer. Committing one that
// covers part of it inserts that text and leaves the REST composing, which
// is how a long run gets typed once and committed a word at a time. That
// path is also how the engine's segmentation gets corrected: commit the
// piece that is right, and the rest is re-ranked without it.
func (s *imeSession) commit(idx int, kind commitKind) {
	client := s.client
	if client == nil || idx < 0 || idx >= len(s.cands) {
		s.reset(client)
		return
	}
	c := s.cands[idx]

	// Remember the pick before anything can fail, so learning does not
	// depend on the insertion path.
	s.learn(c, kind)

	// Clear the marked text before inserting: a host that still holds a
	// marked range can otherwise end up with the committed word inside it.
	s.setMarked(client, "")
	s.emit(client, c.Word)

	typed := s.typed()
	if c.Consumed >= len(typed) {
		// The composition is finished. If it took more than one pick to get
		// here, the pieces together are a word this user built — see coinPhrase.
		s.coinPhrase(c, kind)
		s.reset(client)
		return
	}
	// Partial commit: carry on with what is left. Pinyin only — an English
	// candidate always accounts for the whole buffer.
	//
	// Nothing is logged here: the log is a plain file, and what the user typed
	// does not belong in it.
	s.pending = append(s.pending, committedRun{
		word:    c.Word,
		reading: append([]string(nil), c.Reading...),
	})
	s.pendingPicked = s.pendingPicked && kind == pickedByUser
	s.buffer = typed[c.Consumed:]
	s.refresh(client)
}

// coinPhrase records the pieces of one composition as a single word.
//
// This is 造词, and it is the other half of LearnedPhrases: that lookup can
// produce a word the lexicon does not have, and this is what puts one there.
// Without it the pair is useless, because nothing else ever writes a
// multi-syllable entry the lexicon lacks — every other path records the pieces
// SEPARATELY (业务 under ye'wu, 侧 under ce), and no combination of those can
// answer "yewuce".
//
// Which is why 业务侧 was structurally unreachable: the stitched path offers
// exactly one candidate and 测 beats 侧 for `ce`, and typing it the long way
// taught nothing that could be read back. One long-way commit now teaches it.
//
// Only when every piece was picked, for the reason commitKind exists at all: a
// composition flushed out of the way by a comma is not a word anybody coined.
func (s *imeSession) coinPhrase(last engine.Candidate, kind commitKind) {
	if kind != pickedByUser || !s.pendingPicked || len(s.pending) == 0 {
		return
	}
	runs := append(append([]committedRun(nil), s.pending...), committedRun{
		word:    last.Word,
		reading: last.Reading,
	})
	word, reading := joinRuns(runs)
	// A cap, because the same mechanism that captures a three-character term
	// would otherwise turn a sentence committed in eight pieces into an
	// eight-syllable "word" and let it outrank everything for that reading.
	// Four is where Chinese words stop being words.
	if len(reading) < 2 || len(reading) > maxCoinedSyllables {
		return
	}
	logf("coin %q (%v) from %d pieces", word, reading, len(runs))
	// Both namespaces, and each does a job the other cannot. The coined entry is
	// what makes the word RETRIEVABLE when the lexicon has no such word; the
	// reading entry is the ordinary boost, which is what ranks it when the
	// lexicon does. Whichever applies, the other is inert.
	s.user.RecordCoined(reading, word)
	s.user.Record(reading, word)
}

// maxCoinedSyllables is how long a coined word may be. See coinPhrase.
const maxCoinedSyllables = 4

// commitAll flushes the entire buffer, not just the highlighted candidate.
//
// The highlighted candidate may account for only part of what was typed, in
// which case commit leaves the rest composing. When something else has to go
// in next — a punctuation mark, a passthrough character, focus leaving — that
// remainder must not be left dangling, so this keeps going until the buffer
// is empty.
//
// Bounded rather than a bare loop: every candidate consumes at least one byte,
// so this terminates, but a bug that produced a zero-width candidate would
// otherwise hang the input method inside a keystroke — and an input method
// that hangs takes the app it is typing into with it.
func (s *imeSession) commitAll() {
	for i := 0; s.composing() && i <= dict.MaxSyllables; i++ {
		before := len(s.buffer)
		s.commit(s.sel, flushed)
		if len(s.buffer) >= before {
			logf("commitAll made no progress on %q; abandoning", s.buffer)
			s.buffer = ""
			return
		}
	}
}

// reset ends the composition and hides the panel.
func (s *imeSession) reset(client unsafe.Pointer) {
	s.buffer = ""
	s.englishMode = false
	s.emojiMode = false
	s.cands = nil
	s.sel, s.pageStart = 0, 0
	// A composition is over, so its pieces are no longer a word in the making.
	// pendingPicked resets to true because "every piece so far was picked" is
	// vacuously true of no pieces.
	s.pending = s.pending[:0]
	s.pendingPicked = true
	if client != nil {
		s.setMarked(client, "")
	}
	s.setClient(nil)
	s.panel.hide()
}
