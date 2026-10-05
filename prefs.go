package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// prefs is what the user can change about how the input method behaves.
//
// A JSON file rather than a settings window: an input method has nowhere
// natural to put one (it has no Dock icon and no main window), and every
// setting here is one people set once. The file is created on first launch
// with the defaults spelled out, so it documents itself.
type prefs struct {
	// PageSize is how many candidates are offered at a time, minPageSize to
	// maxPageSize. Five is the default because it is the convention every
	// Chinese IME follows, and it is what makes the digit keys 1-5 reachable
	// without looking; the ceiling is where reaching for 9 stops being faster
	// than pressing Space twice.
	//
	// Changeable from the input-source menu ("候选词数量"), which writes this
	// file — see qimeBridgeGoSetPageSize.
	PageSize int `json:"pageSize"`

	// Fuzzy holds the confusions to tolerate. Off by default — see
	// pinyin.Fuzzy.
	Fuzzy pinyin.Fuzzy `json:"fuzzy"`

	// TypoCorrection accepts a syllable whose letters were typed with one
	// adjacent pair swapped: "pign" for ping, "hzi" for zhi.
	//
	// ON by default, unlike Fuzzy, and the difference is not a matter of taste.
	// A fuzzy rule widens the reading of input that was typed CORRECTLY —
	// everyone who types "zi" starts competing with 只 — so it costs something
	// on every keystroke and only some users get anything back. A transposition
	// arc exists only where the letters do not spell a syllable at all, so for
	// correctly typed input it adds nothing to compete with.
	TypoCorrection bool `json:"typoCorrection"`

	// ChinesePunctuation maps ASCII punctuation onto its full-width
	// equivalent while in Chinese mode.
	ChinesePunctuation bool `json:"chinesePunctuation"`

	// EmojiInCandidates offers an emoji beside the word it stands for in the
	// ordinary candidate list — type "kaixin" and 😄 sits next to 开心.
	//
	// ON by default, which is affordable because the map is 4854 hand-picked
	// words rather than a layer over the language: measured on the shipped
	// lexicon, only 1.7% of the 3000 commonest readings produce an emoji at
	// all. Turning it off does NOT take emoji away — the explicit "v" mode
	// ("vkaixin" → 😄) is always there and costs nothing to anyone who never
	// uses it.
	EmojiInCandidates bool `json:"emojiInCandidates"`
}

// The range the candidate count may be set to. The floor is the convention
// every Chinese IME follows and the shape the reserved English/context slots
// are positioned against (see reserveEnglishSlot); the ceiling keeps every
// candidate on a digit key the hand can find without looking.
const (
	minPageSize = 5
	maxPageSize = 8
)

// clampPageSize brings a page size into range. Clamping rather than rejecting:
// every caller — a hand-edited preferences file, a menu pick, a file written by
// a build with different bounds — wants the nearest usable answer, not a
// refusal it has nowhere to report.
func clampPageSize(n int) int {
	if n < minPageSize {
		return minPageSize
	}
	if n > maxPageSize {
		return maxPageSize
	}
	return n
}

func defaultPrefs() prefs {
	return prefs{
		PageSize:           5,
		ChinesePunctuation: true,
		TypoCorrection:     true,
		EmojiInCandidates:  true,
	}
}

func prefsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "quiime-prefs.json")
	}
	return filepath.Join(home, "Library", "Application Support", "QuiIME", "prefs.json")
}

// loadPrefs reads the preferences, falling back to defaults for anything
// missing or unreadable.
//
// Never fails. A malformed preferences file must not stop the user from
// typing; it gets the defaults and a log line.
func loadPrefs() prefs {
	p := defaultPrefs()
	path := prefsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			writePrefs(p) // leave a documented file behind on first launch
		}
		return p
	}
	if err := json.Unmarshal(data, &p); err != nil {
		logf("prefs: %v (using defaults)", err)
		return defaultPrefs()
	}
	p.PageSize = clampPageSize(p.PageSize)
	// Write it back so a file from an older build gains the settings added
	// since. The whole point of a JSON file with no settings window is that it
	// documents itself; a file that silently lacks `emojiInCandidates` documents
	// the version it was written by, and the option may as well not exist.
	//
	// Safe to do unconditionally: this is the file we just parsed, re-emitted
	// with defaults filled in, and it is only ever written at startup.
	writePrefs(p)
	return p
}

func writePrefs(p prefs) {
	path := prefsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(data, '\n'), 0o644)
}
