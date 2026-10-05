// Package pinyin holds the phonetic knowledge the search needs: what counts
// as an initial, which syllables users routinely confuse, and how to spell a
// syllable the way the lexicon spells it.
//
// It knows nothing about the lexicon or the trie — it maps text to text, so
// it can be unit-tested without a compiled dictionary.
package pinyin

import "strings"

// Initials are the syllable onsets a user may type on their own to stand for
// a whole syllable ("zhg" → 中国). Two-character onsets come first in the
// list because matching is greedy and "zh" must win over "z".
//
// The bare vowels a/o/e are included: they are legal syllables by themselves,
// so a user typing "a" means either the syllable "a" or an abbreviation of
// "ai"/"an"/"ao". Both readings are wanted.
var Initials = []string{
	"zh", "ch", "sh",
	"b", "p", "m", "f", "d", "t", "n", "l", "g", "k", "h",
	"j", "q", "x", "r", "z", "c", "s", "y", "w",
	"a", "o", "e",
}

var initialSet = func() map[string]bool {
	m := make(map[string]bool, len(Initials))
	for _, s := range Initials {
		m[s] = true
	}
	return m
}()

// IsInitial reports whether s may stand alone as an abbreviated syllable.
func IsInitial(s string) bool { return initialSet[s] }

// Fuzzy is the set of confusions to tolerate. Every field defaults to off:
// fuzziness is not free — each enabled rule multiplies the candidate set and
// pushes exact matches down the list — so it is the user's call, not ours.
type Fuzzy struct {
	// Initial confusions.
	ZhZ bool // zh ↔ z
	ChC bool // ch ↔ c
	ShS bool // sh ↔ s
	NL  bool // n  ↔ l
	FH  bool // f  ↔ h
	RL  bool // r  ↔ l
	// Final confusions.
	AngAn   bool // ang ↔ an
	EngEn   bool // eng ↔ en
	IngIn   bool // ing ↔ in
	IangIan bool // iang ↔ ian
	UangUan bool // uang ↔ uan
}

// AnyEnabled reports whether at least one rule is on, so callers can skip
// the expansion work entirely in the common case.
func (f Fuzzy) AnyEnabled() bool {
	return f.ZhZ || f.ChC || f.ShS || f.NL || f.FH || f.RL ||
		f.AngAn || f.EngEn || f.IngIn || f.IangIan || f.UangUan
}

type rule struct{ a, b string }

func (f Fuzzy) initialRules() []rule {
	var rs []rule
	add := func(on bool, a, b string) {
		if on {
			rs = append(rs, rule{a, b})
		}
	}
	add(f.ZhZ, "zh", "z")
	add(f.ChC, "ch", "c")
	add(f.ShS, "sh", "s")
	add(f.NL, "n", "l")
	add(f.FH, "f", "h")
	add(f.RL, "r", "l")
	return rs
}

func (f Fuzzy) finalRules() []rule {
	var rs []rule
	add := func(on bool, a, b string) {
		if on {
			rs = append(rs, rule{a, b})
		}
	}
	// Longer finals first: "iang" must be tried before "ang" so that
	// "xiang" fuzzes to "xian" rather than to the nonexistent "xin" + "g".
	add(f.IangIan, "iang", "ian")
	add(f.UangUan, "uang", "uan")
	add(f.AngAn, "ang", "an")
	add(f.EngEn, "eng", "en")
	add(f.IngIn, "ing", "in")
	return rs
}

// Variants returns every spelling that syllable s could stand for under f,
// including s itself, with s first. The result is small (at most a handful)
// and freshly allocated; callers may keep it.
//
// One substitution per axis: a single rule for the onset and a single rule
// for the rime. Chaining more than that produces pairs no user would confuse
// and floods the candidate list.
func (f Fuzzy) Variants(s string) []string {
	if s == "" || !f.AnyEnabled() {
		return []string{s}
	}
	out := []string{s}
	seen := map[string]bool{s: true}
	push := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}

	// Onset substitutions, then rime substitutions on each result so far, so
	// "zhang" can reach "zang", "zhan" and "zan" with one rule from each axis.
	onsets := []string{s}
	for _, r := range f.initialRules() {
		if v, ok := swapPrefix(s, r.a, r.b); ok {
			onsets = append(onsets, v)
		} else if v, ok := swapPrefix(s, r.b, r.a); ok {
			onsets = append(onsets, v)
		}
	}
	for _, o := range onsets {
		push(o)
		for _, r := range f.finalRules() {
			if v, ok := swapSuffix(o, r.a, r.b); ok {
				push(v)
			} else if v, ok := swapSuffix(o, r.b, r.a); ok {
				push(v)
			}
		}
	}
	return out
}

// swapPrefix replaces prefix from with to, requiring something to remain —
// an onset rule must not consume the whole syllable, or "n" would "fuzz"
// into the empty string.
func swapPrefix(s, from, to string) (string, bool) {
	if len(s) > len(from) && strings.HasPrefix(s, from) {
		return to + s[len(from):], true
	}
	return "", false
}

// swapSuffix replaces suffix from with to, requiring an onset to remain, so
// "ang" itself is not rewritten to "an" — those are distinct syllables a
// user types deliberately, not a confusion.
func swapSuffix(s, from, to string) (string, bool) {
	if len(s) > len(from) && strings.HasSuffix(s, from) {
		return s[:len(s)-len(from)] + to, true
	}
	return "", false
}

// Transpositions returns every spelling reachable from s by swapping ONE pair
// of adjacent letters — the spellings s could be a mistyping of.
//
// Swapping is its own inverse, so "what s might be a transposition of" and
// "what s transposes into" are the same set, and one function answers both.
//
// One swap, not any number. Two swaps ("diao" typed as "doia") is a second
// independent slip in the same syllable, and allowing it multiplies the
// readings of every chunk for a case that is rare enough not to pay for
// itself. rime-ice does list a few double-swap forms; this deliberately
// does not reach them.
//
// Pairs of identical letters are skipped: swapping them yields s back, and an
// arc identical to the exact one would only compete with it.
func Transpositions(s string) []string {
	if len(s) < 2 {
		return nil
	}
	buf := []byte(s)
	out := make([]string, 0, len(buf)-1)
	for i := 0; i+1 < len(buf); i++ {
		if buf[i] == buf[i+1] {
			continue
		}
		buf[i], buf[i+1] = buf[i+1], buf[i]
		out = append(out, string(buf))
		buf[i], buf[i+1] = buf[i+1], buf[i]
	}
	if len(out) == 0 {
		return nil // one contract for "nothing to try", however it arose
	}
	return out
}

// DropOne returns every spelling reachable from s by removing ONE letter.
//
// This is the MISSING-letter repair, from the syllable's side: DropOne over
// each syllable of the alphabet keys the deletion index the engine builds, so
// asking "which syllable is this chunk one letter short of" ("zhog" → zhong)
// is one map hit instead of 26 trial insertions at every position.
//
// Duplicates are skipped — removing either of two identical adjacent letters
// gives the same string, and two identical arcs would only compete.
func DropOne(s string) []string {
	if len(s) < 2 {
		return nil
	}
	out := make([]string, 0, len(s))
	seen := map[string]bool{}
	for i := 0; i < len(s); i++ {
		v := s[:i] + s[i+1:]
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Undouble returns every spelling reachable from s by collapsing ONE doubled
// letter — the EXTRA-letter repair ("guuo" → guo, "zzhong" → zhong).
//
// Deliberately narrower than dropping an arbitrary letter, and the restriction
// is load-bearing. A key REPEAT is the insertion error fingers actually make;
// an arbitrary-drop rule additionally claims "the letter you just typed was
// never meant", which collides head-on with the trailing-onset abbreviation:
// it read "lihail" as 厉害 + a slipped l — explaining away the exact letter
// the user typed ON PURPOSE to reach 厉害了, and re-opening the abandonment
// bug that raising coverBonus to 6.5 was measured to close.
func Undouble(s string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i+1 < len(s); i++ {
		if s[i] != s[i+1] {
			continue
		}
		v := s[:i] + s[i+1:]
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Normalize rewrites a raw typed syllable into the spelling the lexicon
// uses. rime-ice spells ü as "v" ("nve", "lve") and drops the umlaut after
// j/q/x/y ("ju", "qu", "xu", "yu"), so a user typing "nue" or "lue" — which
// is what a keyboard makes natural — must still find 虐 and 略.
func Normalize(s string) string {
	switch {
	case strings.HasPrefix(s, "nu") || strings.HasPrefix(s, "lu"):
		// nue → nve, lue → lve. Only the "ue" rime; "nuan"/"luan" are real.
		if len(s) == 3 && s[2] == 'e' {
			return s[:1] + "ve"
		}
	}
	// ü typed as a literal character, in case a layout produces it.
	if strings.ContainsRune(s, 'ü') {
		return strings.ReplaceAll(s, "ü", "v")
	}
	return s
}

// StripSeparators removes the apostrophes a user types to force a syllable
// break, reporting the positions (in the cleaned string) where a break was
// requested. "xi'an" must not be read as "xian".
func StripSeparators(raw string) (clean string, breaks map[int]bool) {
	if !strings.ContainsAny(raw, "'") {
		return raw, nil
	}
	var b strings.Builder
	b.Grow(len(raw))
	breaks = make(map[int]bool)
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\'' {
			if b.Len() > 0 {
				breaks[b.Len()] = true
			}
			continue
		}
		b.WriteByte(raw[i])
	}
	return b.String(), breaks
}
