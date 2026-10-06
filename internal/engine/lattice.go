package engine

import (
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// sylArc is one way of reading input[start:end] as a single syllable.
//
// The reading is an ID RANGE rather than one ID because typing a bare onset
// ("n") means "any syllable that starts this way". Syllable IDs are assigned
// in ascending byte order, so that set is always contiguous — see
// dict.PrefixRange.
type sylArc struct {
	end     int
	lo, hi  uint16 // half-open ID range
	abbrev  bool   // an onset standing in for a whole syllable ("n" → n*)
	partial bool   // a finished syllable read as the start of a longer one
	fuzzy   bool   // matched only after a fuzzy substitution
	typo    bool   // matched after swapping two adjacent letters ("pign" → ping)
}

// lattice[i] is every syllable reading that can start at input position i.
type lattice [][]sylArc

// buildLattice segments raw input into the syllable readings the lexicon
// could possibly want, without consulting the lexicon.
//
// Four kinds of arc, and the distinction between them is the whole
// difference between a toy segmenter and one that feels like a real IME:
//
//   - a FULL syllable ("hao"), optionally after a fuzzy substitution;
//   - an ONSET standing in for a syllable ("h" in "nih"), allowed anywhere,
//     which is what makes abbreviations work;
//   - a PREFIX of some syllable ("ha" in "niha"), allowed ONLY at the end of
//     the input, because that is the half-typed syllable the user is still
//     working on. Allowing prefixes in the middle would read "hao" as
//     "h"+"ao" and flood every list with noise;
//   - a MISTYPED full syllable — TRANSPOSED ("pign" for ping), one letter
//     DROPPED ("zhog" for zhong) or one EXTRA ("guuo" for guo) — allowed only
//     where the chunk is not already a syllable; see the typo branches below.
//
// Arcs are added longest-chunk-first, and within a chunk exact before fuzzy
// before transposed. That order is load-bearing: collect memoizes on
// (position, node) and keeps the first reading to arrive, so the cheapest
// explanation of a span is the one that gets recorded.
//
// breaks marks positions where the user typed an apostrophe; no arc may span
// one, which is how "xi'an" stays two syllables instead of collapsing to
// "xian".
func (e *Engine) buildLattice(input string, breaks map[int]bool) lattice {
	n := len(input)
	lat := make(lattice, n)
	maxSyll := e.dict.MaxSyllableLen()
	if e.typo {
		// One more than the longest syllable, so a chunk carrying an EXTRA
		// letter ("zhuangg", "zzhong") can still shrink to a syllable. Only
		// the extra-letter branch can match at this length — every other
		// lookup on an over-long chunk simply fails.
		maxSyll++
	}

	spansBreak := func(i, j int) bool {
		if breaks == nil {
			return false
		}
		for k := i + 1; k < j; k++ {
			if breaks[k] {
				return true
			}
		}
		return false
	}

	for i := 0; i < n; i++ {
		limit := maxSyll
		if i+limit > n {
			limit = n - i
		}
		seen := make(map[sylArc]bool, 8)
		add := func(a sylArc) {
			if a.lo >= a.hi || seen[a] {
				return
			}
			seen[a] = true
			lat[i] = append(lat[i], a)
		}

		for l := limit; l >= 1; l-- {
			j := i + l
			if spansBreak(i, j) {
				continue
			}
			chunk := input[i:j]
			norm := pinyin.Normalize(chunk)

			// Full syllable, exact spelling or a lexicon spelling the user
			// would not naturally type ("nue" for "nve").
			whole := false
			if id, ok := e.dict.SyllableID(norm); ok {
				add(sylArc{end: j, lo: id, hi: id + 1})
				whole = true
			}
			// Fuzzy readings of the same chunk.
			if e.fuzzy.AnyEnabled() {
				for _, v := range e.fuzzy.Variants(norm) {
					if id, ok := e.dict.SyllableID(v); ok {
						add(sylArc{end: j, lo: id, hi: id + 1, fuzzy: true})
					}
				}
			}
			// Transposed spelling of a full syllable: "pign" for ping, "hzi"
			// for zhi. Offered ONLY when the chunk is not a syllable in its own
			// right, which is the entire safety argument.
			//
			// A completed syllable is the user having said what they meant, and
			// re-reading it as a different one is speculation — the same
			// principle partialPenalty rests on, except here there is no honest
			// price to charge, because the evidence points the other way. So
			// this does not merely cost more, it does not happen at all: typing
			// "diu" offers 丢 and never 对, "she" never 学, "gui" never 归 read
			// as 鬼's neighbours.
			//
			// rime-ice reaches the same exclusions by hand, which is the
			// strongest evidence this guard is the right shape: its ui→iu rule
			// is spelled derive/([rtsghkzc])ui$/$1iu/ — d is missing from that
			// onset list, precisely because "diu" is a real syllable. Every such
			// omission falls out of the guard here instead of being maintained.
			if e.typo && !whole && l >= 2 {
				for _, t := range pinyin.Transpositions(chunk) {
					if id, ok := e.dict.SyllableID(pinyin.Normalize(t)); ok {
						add(sylArc{end: j, lo: id, hi: id + 1, typo: true})
					}
				}
			}
			// Onset abbreviation, legal at any position.
			if pinyin.IsInitial(chunk) {
				lo, hi := e.dict.PrefixRange(chunk)
				add(sylArc{end: j, lo: lo, hi: hi, abbrev: true})
			}
			// Half-typed trailing syllable. Charged as a partial rather than
			// an abbreviation when the user already typed a complete
			// syllable, since reading "me" as men overrides something they
			// finished saying — see partialPenalty.
			if j == n {
				if lo, hi := e.dict.PrefixRange(chunk); hi > lo {
					add(sylArc{end: j, lo: lo, hi: hi, abbrev: !whole, partial: whole})
				}
			}
			// A dropped letter ("zhog", "zhon" mid-input for zhong) or a
			// doubled one ("guuo" for guo — key repeat, the only extra-letter
			// shape repaired; see pinyin.Undouble), under the same guard as
			// transposition: never where the chunk already IS a syllable.
			//
			// Added AFTER the abbreviation and trailing-prefix arcs, and that
			// order is load-bearing exactly as the header comment says.
			// delIndex["zh"] would hold zha, zhe, zhi… — every syllable the
			// abbreviation arc above already reaches at a quarter of the
			// price — so onset chunks are excluded outright; and a trailing
			// "zhon" is reached first by the prefix arc as the half-typed
			// syllable it almost certainly is, leaving the deletion arc to
			// contribute only what nothing cheaper explains ("zhog", which no
			// prefix range covers).
			if e.typo && !whole {
				if l >= 2 && !pinyin.IsInitial(chunk) {
					for _, id := range e.delIndex[norm] {
						add(sylArc{end: j, lo: id, hi: id + 1, typo: true})
					}
				}
				// l >= 3 so the repaired syllable has two letters left —
				// mirroring the delIndex bound from the other side.
				if l >= 3 {
					for _, t := range pinyin.Undouble(chunk) {
						if id, ok := e.dict.SyllableID(pinyin.Normalize(t)); ok {
							add(sylArc{end: j, lo: id, hi: id + 1, typo: true})
						}
					}
				}
			}
		}
	}
	return lat
}

// hasTypoArc reports whether any position offers a transposed reading.
//
// Worth asking before composing: the Viterbi table carries a second axis for
// the transposition allowance, and this says whether that axis can ever be
// entered. Input typed correctly — which is nearly all of it — answers no, and
// then the sentence search costs exactly what it did before the feature
// existed. Scanning the arcs is free next to the trie walk.
func (l lattice) hasTypoArc() bool {
	for _, arcs := range l {
		for _, a := range arcs {
			if a.typo {
				return true
			}
		}
	}
	return false
}

// canContinue reports whether a reading that stopped at position end is
// still viable: either it consumed everything, or the input it left behind
// can begin a syllable WITHOUT assuming a typo.
//
// This is what keeps dead-end readings out of the list. Typing "ni" can be
// read as the onset "n" alone, leaving "i" — but no syllable starts with i,
// so that reading can never become a word no matter what is typed next. It
// is not a candidate the user is part-way through; it is a candidate that
// does not exist, and offering 那 for "ni" is exactly how it looked.
//
// Typo arcs do not count as viability. A leftover that can only be read by
// assuming it was mistyped is speculation squared: it would defeat the
// real-syllable guard, because dropped-letter repair turns the leftover of an
// abbreviated reading into a legal syllable again. A reading for correctly
// spelled input must not be kept alive that way.
func (l lattice) canContinue(end, n int) bool {
	if end >= n {
		return true
	}
	for _, a := range l[end] {
		if !a.typo {
			return true
		}
	}
	return false
}
