package main

// The engine's attachment to the app: finding the compiled lexicon, opening
// it, and keeping the user's learned words saved.
//
// The search itself lives in internal/engine and knows nothing about macOS,
// bundles or qui — see that package's doc comment. This file is the seam,
// and it is the only place that knows where the data lives.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/qizhanchan/q-ime/internal/dict"
	"github.com/qizhanchan/q-ime/internal/emoji"
	"github.com/qizhanchan/q-ime/internal/engine"
	"github.com/qizhanchan/q-ime/internal/english"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// lexiconName is the compiled dictionary, built by tools/dictc and copied
// into the bundle by build.sh.
const lexiconName = "lexicon.bin"

// userSaveInterval is how often learned words are flushed to disk.
//
// Not on every pick: an input method writes to this on a keystroke path, and
// a file write there would be felt. Not only at exit either — an input method
// is killed, not quit, so anything held until shutdown is anything lost.
// Cheap at this rate because a save appends only the entries that changed —
// see userdict_store.go.
const userSaveInterval = 20 * time.Second

// openLexicon mmaps the compiled dictionary.
//
// Looks beside the executable inside the .app first, which is where a real
// install has it, then falls back to paths that make `go run` work during
// development. The error names every place it looked, because "dictionary not
// found" in a process with no console is otherwise unactionable.
func openLexicon() (*dict.Reader, error) {
	var tried []string
	for _, p := range lexiconSearchPath() {
		r, err := dict.Open(p)
		if err == nil {
			return r, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			// A file that exists but will not open is a real problem — a
			// stale build, a truncated copy — and must not be silently
			// stepped over in favour of some other copy.
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		tried = append(tried, p)
	}
	return nil, fmt.Errorf("no %s found; looked in %v (run tools/dictc to build it)",
		lexiconName, tried)
}

func lexiconSearchPath() []string {
	var paths []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		// Inside the bundle: Contents/MacOS/QuiIME → Contents/Resources.
		paths = append(paths,
			filepath.Join(dir, "..", "Resources", lexiconName),
			filepath.Join(dir, lexiconName),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths,
			filepath.Join(home, "Library", "Application Support", "QuiIME", lexiconName))
	}
	// Development fallbacks, so the engine can be exercised without
	// installing an input method.
	paths = append(paths, lexiconName, filepath.Join("/tmp", lexiconName))
	return paths
}

// newEngine opens the lexicon and the user's history and wires them together.
func newEngine() (*engine.Engine, *engine.UserDict, *dict.Reader, error) {
	d, err := openLexicon()
	if err != nil {
		return nil, nil, nil, err
	}
	user := engine.NewUserDict(engine.DefaultUserDictPath())
	p := loadPrefs()
	// Fuzzy matching stays off by default: every rule enabled is a wider
	// candidate list and exact matches pushed down it, which is a trade only
	// the person typing can make. Transposition correction is on — it only
	// applies where the letters spell no syllable at all. See prefs.go.
	e := engine.New(d, user, p.Fuzzy)
	e.SetTypoCorrection(p.TypoCorrection)

	// English is embedded in the binary, so the only way this fails is a
	// corrupt executable. Log and carry on rather than refusing to start: an
	// input method that cannot type Chinese is broken, one that cannot offer
	// "hello" is merely worse.
	if en, err := english.Builtin(); err != nil {
		logf("english word list unavailable: %v", err)
	} else {
		e.SetEnglish(en)
		englishCodes = en.Len()
	}
	// Emoji, same story and the same failure mode: embedded, so losing it means
	// a corrupt binary, and losing it must not stop anyone typing.
	if em, err := emoji.Builtin(); err != nil {
		logf("emoji table unavailable: %v", err)
	} else {
		e.SetEmoji(em, p.EmojiInCandidates)
		emojiWords = em.Len()
	}
	return e, user, d, nil
}

// englishCodes is how many English codes loaded, for the startup log line.
// Package-level because it is the only thing describeEngine would otherwise
// need the engine's internals for, and it is written exactly once.
var englishCodes int

// emojiWords is how many word→emoji entries loaded, for the startup log line.
var emojiWords int

// startUserDictSaver flushes learned words on a timer.
//
// Runs on its own goroutine and touches only UserDict, which is
// mutex-guarded for exactly this reason; it must not reach into the session
// or the panel, which belong to the UI thread.
func startUserDictSaver(u *engine.UserDict) {
	go func() {
		for range time.Tick(userSaveInterval) {
			if err := u.Save(); err != nil {
				logf("saving learned words: %v", err)
			}
		}
	}()
}

// describeEngine is the startup log line, so a running input method can be
// asked what it loaded without attaching a debugger.
func describeEngine(d *dict.Reader, u *engine.UserDict, f pinyin.Fuzzy) string {
	nodes, entries, bytes := d.Stats()
	return fmt.Sprintf("lexicon %.1fMB (%d entries, %d nodes, %d syllables); "+
		"%d english codes; %d emoji words; %d learned words; fuzzy=%v",
		float64(bytes)/(1<<20), entries, nodes, len(d.Syllables()),
		englishCodes, emojiWords, u.Len(), f.AnyEnabled())
}
