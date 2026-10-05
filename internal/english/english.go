// Package english is an exact-match English word list, keyed by what the user
// types.
//
// It exists so that typing an English word while the input method is in
// Chinese mode offers that word as a candidate — "hello" should not have to be
// force-fitted into 合理 or, worse, into a chain of single characters that
// happen to share its letters. There is no spell-checking, no prefix
// completion and no stemming here: an exact hit or nothing. That narrowness is
// the point, because the alternative is polluting every two-letter
// composition with English guesses.
//
// # Shape of the data
//
// One line per typing CODE, tab-separated, sorted by code:
//
//	hell	hell	he'll
//	iphone	iPhone	iPhone 17	iPhone 17 Pro
//
// The code is the keystrokes; the words after it are what may be inserted,
// best first. Code and text differ whenever spelling and typing differ —
// apostrophes, spaces and capitals are in the text and not in the code — which
// is what lets "buenosaires" produce "Buenos Aires".
//
// # Why a blob and an index instead of a map
//
// The obvious implementation is map[string][]string, and it costs ~45,000
// string headers that Go's collector walks on every cycle, for the lifetime of
// a process that runs for weeks. Here the table is one embedded string in the
// binary's read-only data — never copied to the heap, never scanned — plus a
// []int32 of line offsets, which contains no pointers either. Lookup is a
// binary search over that index. The whole structure is 90KB of scratch over
// data that was already in the executable.
package english

import (
	"embed"
	"sort"
	"strings"
)

// The table is embedded rather than shipped beside the executable: it is
// small, it is never edited at runtime, and a data file that can go missing is
// a startup failure mode not worth having for 379KB. (The Chinese lexicon is
// external because it is 76MB.)
//
//go:embed data/english.txt
var files embed.FS

// Dict is a compiled word list. Read-only after Load, so it is safe to share.
type Dict struct {
	blob string
	// lines holds the offset of each entry's first byte, in code order.
	lines []int32
}

// Builtin returns the embedded list. Cheap enough to call at startup and not
// cached, since the input method builds exactly one.
func Builtin() (*Dict, error) {
	b, err := files.ReadFile("data/english.txt")
	if err != nil {
		return nil, err
	}
	return Load(string(b)), nil
}

// Load indexes an already-read table.
func Load(blob string) *Dict {
	d := &Dict{blob: blob, lines: make([]int32, 0, 1<<15)}
	sorted := true
	prev := ""
	for off := 0; off < len(blob); {
		end := off + strings.IndexByte(blob[off:], '\n')
		if end < off {
			end = len(blob)
		}
		line := blob[off:end]
		// Comments carry the provenance header. Skipping them here rather than
		// stripping them in the generator keeps the file readable.
		if line != "" && line[0] != '#' {
			d.lines = append(d.lines, int32(off))
			if code := codeOf(line); code < prev {
				sorted = false
			} else {
				prev = code
			}
		}
		off = end + 1
	}
	// The generator emits sorted codes and the binary search below depends on
	// it. Sorting here anyway costs a few milliseconds once and makes a
	// hand-edited file work instead of silently losing lookups.
	if !sorted {
		sort.Slice(d.lines, func(i, j int) bool {
			return d.codeAt(i) < d.codeAt(j)
		})
	}
	return d
}

// Len is how many codes the list holds.
func (d *Dict) Len() int {
	if d == nil {
		return 0
	}
	return len(d.lines)
}

// Words calls fn with each word the code can produce, best first, and stops
// early if fn returns false. Nothing is allocated: the strings handed to fn
// are slices of the embedded table.
//
// code must already be lowercase — the caller has the typed buffer, which this
// input method only ever fills with a-z.
func (d *Dict) Words(code string, fn func(word string) bool) {
	if d == nil || code == "" {
		return
	}
	i := sort.Search(len(d.lines), func(i int) bool { return d.codeAt(i) >= code })
	if i >= len(d.lines) || d.codeAt(i) != code {
		return
	}
	rest := d.lineAt(i)
	rest = rest[len(code):] // leaves "\ttext\ttext"
	for len(rest) > 1 {
		rest = rest[1:] // the tab
		word := rest
		if tab := strings.IndexByte(rest, '\t'); tab >= 0 {
			word, rest = rest[:tab], rest[tab:]
		} else {
			rest = ""
		}
		if word == "" {
			continue
		}
		if !fn(word) {
			return
		}
	}
}

// PrefixWords calls fn for each word whose code STARTS WITH prefix, code by
// code in alphabetical order, stopping early if fn returns false.
//
// Separate from Words, and used only once the user has committed to typing an
// English word, because a prefix match is a guess where an exact one is a
// fact. Offering guesses to somebody typing pinyin would put "nice" and
// "night" in front of them at "ni"; offering them to somebody who has already
// pressed Shift and typed "Sh" is the entire point of being in that mode.
//
// Alphabetical is the only order available — the table has no frequency data
// — so callers that want the plausible completions first should sort what they
// collect. See engine.EnglishWords.
func (d *Dict) PrefixWords(prefix string, fn func(word string) bool) {
	if d == nil || prefix == "" {
		return
	}
	i := sort.Search(len(d.lines), func(i int) bool { return d.codeAt(i) >= prefix })
	for ; i < len(d.lines); i++ {
		line := d.lineAt(i)
		code := codeOf(line)
		if !strings.HasPrefix(code, prefix) {
			return // codes are sorted, so the run of matches is over
		}
		rest := line[len(code):]
		for len(rest) > 1 {
			rest = rest[1:]
			word := rest
			if tab := strings.IndexByte(rest, '\t'); tab >= 0 {
				word, rest = rest[:tab], rest[tab:]
			} else {
				rest = ""
			}
			if word != "" && !fn(word) {
				return
			}
		}
	}
}

// Has reports whether the code is a word at all, without walking its words.
func (d *Dict) Has(code string) bool {
	found := false
	d.Words(code, func(string) bool { found = true; return false })
	return found
}

func (d *Dict) lineAt(i int) string {
	off := int(d.lines[i])
	if end := strings.IndexByte(d.blob[off:], '\n'); end >= 0 {
		return d.blob[off : off+end]
	}
	return d.blob[off:]
}

func (d *Dict) codeAt(i int) string { return codeOf(d.lineAt(i)) }

func codeOf(line string) string {
	if tab := strings.IndexByte(line, '\t'); tab >= 0 {
		return line[:tab]
	}
	return line
}
