// Package emoji maps a Chinese word to the emoji that stand for it.
//
// Keyed by WORD, not by pinyin, and that is the whole design. An emoji is not
// a reading of anything — nobody sounds out 😄 — it is a decoration on a word
// the engine has already found. Keying it that way means the emoji arrives
// through every path the engine already has: abbreviations reach it ("kx" →
// 开心 → 😄), so do fuzzy readings, transposition correction, the user
// dictionary and the context memory. Not one line of matching code here.
//
// The alternative — a second table keyed by pinyin — would duplicate all of
// that and then drift out of step with it.
//
// # Shape of the data
//
// One line per WORD, tab-separated, sorted by word, best emoji first:
//
//	开心	😄
//	笑	😄	😊
//	好	👌	🙆‍♂️	🙆‍♀️
//
// Order within a line is the map author's, since there is no frequency data to
// do better with. See tools/emojic.
//
// # Why a blob and an index instead of a map
//
// The same reason package english gives: a map would put ~10,000 string headers
// on the heap of a process that runs for weeks, all of them scanned on every GC
// cycle. Here the table is one embedded string in the binary's read-only data
// plus a []int32 of line offsets, neither of which contains a pointer. Lookup
// is a binary search. The whole structure is ~20KB of scratch over data that
// was already in the executable.
package emoji

import (
	"embed"
	"sort"
	"strings"
)

// The table is embedded rather than shipped beside the executable: 85KB is
// small, it is never edited at runtime, and a data file that can go missing is
// a startup failure mode not worth having. (The Chinese lexicon is external
// because it is 77MB.)
//
//go:embed data/emoji.txt
var files embed.FS

// Dict is a compiled word→emoji table. Read-only after Load, so it is safe to
// share.
type Dict struct {
	blob string
	// lines holds the offset of each entry's first byte, in word order.
	lines []int32
}

// Builtin returns the embedded table.
func Builtin() (*Dict, error) {
	b, err := files.ReadFile("data/emoji.txt")
	if err != nil {
		return nil, err
	}
	return Load(string(b)), nil
}

// Load indexes an already-read table.
func Load(blob string) *Dict {
	d := &Dict{blob: blob, lines: make([]int32, 0, 1<<13)}
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
			if word := wordOf(line); word < prev {
				sorted = false
			} else {
				prev = word
			}
		}
		off = end + 1
	}
	// The generator emits sorted words and the binary search below depends on
	// it. Sorting here anyway costs a millisecond once and makes a hand-edited
	// file work instead of silently losing lookups.
	if !sorted {
		sort.Slice(d.lines, func(i, j int) bool {
			return d.wordAt(i) < d.wordAt(j)
		})
	}
	return d
}

// Len is how many words the table holds.
func (d *Dict) Len() int {
	if d == nil {
		return 0
	}
	return len(d.lines)
}

// Emoji calls fn with each emoji standing for word, best first, and stops early
// if fn returns false. Nothing is allocated: the strings handed to fn are
// slices of the embedded table.
func (d *Dict) Emoji(word string, fn func(e string) bool) {
	if d == nil || word == "" {
		return
	}
	i := sort.Search(len(d.lines), func(i int) bool { return d.wordAt(i) >= word })
	if i >= len(d.lines) || d.wordAt(i) != word {
		return
	}
	rest := d.lineAt(i)
	rest = rest[len(word):] // leaves "\temoji\temoji"
	for len(rest) > 1 {
		rest = rest[1:] // the tab
		e := rest
		if tab := strings.IndexByte(rest, '\t'); tab >= 0 {
			e, rest = rest[:tab], rest[tab:]
		} else {
			rest = ""
		}
		if e == "" {
			continue
		}
		if !fn(e) {
			return
		}
	}
}

// First returns the emoji to offer for word, or "" if there is none.
//
// Separate from Emoji because the main candidate list takes exactly one — a
// word with three of them (好 → 👌🙆‍♂️🙆‍♀️) must not spend three slots on a page
// that holds five.
func (d *Dict) First(word string) string {
	out := ""
	d.Emoji(word, func(e string) bool { out = e; return false })
	return out
}

// Has reports whether the word has any emoji at all.
func (d *Dict) Has(word string) bool { return d.First(word) != "" }

func (d *Dict) lineAt(i int) string {
	off := int(d.lines[i])
	if end := strings.IndexByte(d.blob[off:], '\n'); end >= 0 {
		return d.blob[off : off+end]
	}
	return d.blob[off:]
}

func (d *Dict) wordAt(i int) string { return wordOf(d.lineAt(i)) }

func wordOf(line string) string {
	if tab := strings.IndexByte(line, '\t'); tab >= 0 {
		return line[:tab]
	}
	return line
}
