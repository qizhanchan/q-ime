package engine

import (
	"math"
	"sort"
	"strings"

	"github.com/qizhanchan/q-ime/internal/dict"
)

// singletonPenalty is charged for a chain word that explains a SINGLE byte of
// input — one bare onset standing in for a whole word.
//
// Such a word carries no evidence. "zhg" → 中国 is a two-character word
// confirmed by two onsets; "t" → 他 is one character confirmed by nothing,
// and a sentence assembled out of those is the engine spelling the input out
// letter by letter rather than reading it.
const singletonPenalty = 4.0

// maxComposeSyllables caps how long a single word in a composed sentence may
// be.
//
// Composition walks the trie from EVERY input position, so its cost scales
// with this; almost no dictionary entry runs past ten syllables, and the ones
// that do (idioms, place names spelled out in full) are found by the ordinary
// single-word path anyway, which is not capped.
const maxComposeSyllables = 10

// step is one backpointer in the Viterbi table: position i was reached by
// reading word over input[from:i].
type step struct {
	from    int
	word    string
	reading []string
	ends    []int
	// node is the trie state the word was read off, kept so the recovered
	// chain can offer the OTHER words at the same node — the homophone
	// alternates below.
	node dict.Node
	// fromLane is which typo lane this step was reached FROM, so the chain can
	// be walked back through a table indexed by both position and slips used.
	fromLane int
	typo     bool
}

// maxComposeAlternates caps how many homophone variants of the best sentence
// are offered beside it.
//
// Small, because each one takes a slot from a single-word candidate the user
// may want more; nonzero, because without them a sentence with ONE wrong
// homophone is all-or-nothing — the user's only recourse is to abandon the
// sentence and commit word by word, which is the failure a sentence candidate
// exists to avoid.
const maxComposeAlternates = 3

// altsPerStep is how many runner-up words one chain position may contribute.
// Postings are weight-descending, so the first couple past the winner are the
// only ones with a realistic claim.
const altsPerStep = 2

// compose finds the best way to read the WHOLE input as a run of dictionary
// words, and returns it as a candidate — first in the returned slice —
// followed by up to maxComposeAlternates homophone variants of it.
//
// This is the difference between an input method that handles "nihao" and one
// that handles "womenmingtianqubeijing". No lexicon contains the second as an
// entry — it is 我们 + 明天 + 去 + 北京 — so covering it means searching over
// where the word boundaries go.
//
// Viterbi over input positions: best[i] is the score of the best reading of
// input[:i], and the answer is best[len(input)]. Each transition is one
// dictionary word scored as it would be on its own, minus a junction
// penalty, so the same text as a single lexicon entry always wins over the
// assembled version.
//
// The results are offered as candidates among the rest rather than forced to
// the top: a stitched sentence is a guess about segmentation, and the ranking
// in engine.go decides how it fares against genuine single-word matches.
//
// # The alternates vary ONE word, never the segmentation
//
// Each variant swaps a single chain position to a runner-up posting at the
// same trie node, so it has the same reading, the same spans, and a score
// lower by exactly the two words' weight gap. That is deliberately narrower
// than a k-best Viterbi: a second-best CUT of the letters is another guess
// about structure, while a second-best homophone at the same cut is the error
// users actually hit — 他 for 她, 在 for 再 — and the one a variant slot can
// fix. It is also why the scores need no new constants: the coverage,
// junction and singleton terms are identical between a variant and the chain
// it came from, so the swap is weighed purely by ln P(word), the same
// comparison the postings' own order encodes.
//
// # Why no user-dictionary boost here
//
// Learned words are deliberately NOT consulted. Two reasons, and the first
// one alone settles it:
//
//  1. It cannot help. This loop only ever looks at Post(node, 0) — the
//     top-weighted word for a span — so a boost cannot change WHICH word is
//     chosen, only how attractive the SEGMENTATION looks.
//
//  2. It actively hurts, because the boosts compound. A boost is calibrated
//     to lift one word above its same-reading rivals; adding one per word in
//     a chain lets a five-character sentence collect five of them, summing
//     into a sentence nobody would want.
func (e *Engine) compose(lat lattice, input string, budget *int) []Candidate {
	n := len(input)
	if n < 2 {
		return nil
	}

	negInf := math.Inf(-1)

	// The table is indexed by position AND by how many transpositions the path
	// has already spent, because maxTypos bounds a whole READING and each word
	// of a sentence is searched separately. Without the second axis every word
	// gets its own allowance: "inhoa" came out as 你好 by assuming a slip in
	// 你 and another in 好 — two guesses, which is no longer reading the input.
	//
	// Collapsed to a single lane when the lattice offers no transposed reading
	// at all, which is the case for correctly typed input — so the common
	// keystroke costs exactly what it did before this feature existed.
	lanes := 1
	if lat.hasTypoArc() {
		lanes = maxTypos + 1
	}
	at := func(pos, lane int) int { return pos*lanes + lane }
	best := make([]float64, (n+1)*lanes)
	for i := range best {
		best[i] = negInf
	}
	best[at(0, 0)] = 0
	back := make([]step, (n+1)*lanes)

	for i := 0; i < n; i++ {
		if *budget <= 0 {
			continue // out of search budget
		}
		reachable := false
		for lane := 0; lane < lanes; lane++ {
			if best[at(i, lane)] != negInf {
				reachable = true
			}
		}
		if !reachable {
			continue
		}
		for _, r := range e.collect(lat, i, maxComposeSyllables, budget, e.dict.Root()) {
			if e.dict.PostCount(r.node) == 0 {
				continue
			}
			// Only the best word at this span matters: alternatives differ
			// solely by weight, so the top one dominates every path through
			// this state.
			c := e.dict.Post(r.node, 0)
			reading := e.readingOf(r.node, r.syls)
			score := e.logProb(c.Weight) +
				coverBonus*float64(r.end-i) -
				r.penalty() -
				junctionPenalty
			if r.end-i <= 1 {
				score -= singletonPenalty
			}
			for lane := 0; lane < lanes; lane++ {
				from := best[at(i, lane)]
				if from == negInf {
					continue
				}
				next := lane + r.typo
				if next >= lanes {
					continue // this path has already used its one slip
				}
				if v := from + score; v > best[at(r.end, next)] {
					best[at(r.end, next)] = v
					back[at(r.end, next)] = step{
						from: i, fromLane: lane, word: c.Word, node: r.node,
						reading: reading, ends: r.ends, typo: r.typo > 0,
					}
				}
			}
		}
	}

	// The answer is the best lane at the end — spending the allowance is not
	// itself a cost, since typoPenalty was already charged on the transition.
	endLane, total := -1, negInf
	for lane := 0; lane < lanes; lane++ {
		if v := best[at(n, lane)]; v > total {
			endLane, total = lane, v
		}
	}
	if endLane < 0 {
		return nil
	}

	// Recover the chain front-to-back so words and syllables come out in
	// reading order. The entries are flat (position, lane) indices, since a
	// position alone no longer identifies a step.
	var chain []int
	for i, lane := n, endLane; i > 0; {
		chain = append(chain, at(i, lane))
		i, lane = back[at(i, lane)].from, back[at(i, lane)].fromLane
	}
	if len(chain) < 2 {
		// A single word covering everything: the ordinary path already
		// found it, and here it would be needlessly docked a junction
		// penalty.
		return nil
	}
	reverse(chain)

	var text strings.Builder
	reading := make([]string, 0, len(chain)*2)
	spans := make([]int, 0, len(chain)*2)
	words := make([]string, 0, len(chain))
	corrected := false
	for _, i := range chain {
		text.WriteString(back[i].word)
		reading = append(reading, back[i].reading...)
		spans = append(spans, back[i].ends...)
		words = append(words, back[i].word)
		corrected = corrected || back[i].typo
	}

	main := Candidate{
		Word:      text.String(),
		Consumed:  n,
		Reading:   reading,
		Spans:     spans,
		Source:    SourceSentence,
		Words:     words,
		Corrected: corrected,
		Score:     total,
	}
	return append([]Candidate{main}, e.composeAlternates(main, back, chain)...)
}

// composeAlternates builds the homophone variants of a recovered chain: each
// swaps ONE position to a runner-up posting at the same node, keeping the
// best few by score. See the compose comment for why the segmentation is
// never varied and why no new constant is involved.
func (e *Engine) composeAlternates(main Candidate, back []step, chain []int) []Candidate {
	var alts []Candidate
	for pos, i := range chain {
		s := back[i]
		topW := e.dict.PostWeight(s.node, 0)
		cnt := e.dict.PostCount(s.node)
		if cnt > altsPerStep+1 {
			cnt = altsPerStep + 1
		}
		for j := 1; j < cnt; j++ {
			alt := e.dict.Post(s.node, j)
			var text strings.Builder
			words := make([]string, len(main.Words))
			copy(words, main.Words)
			words[pos] = alt.Word
			for _, w := range words {
				text.WriteString(w)
			}
			c := main
			c.Word = text.String()
			c.Words = words
			// The swap is weighed purely by the two words' weight gap; every
			// other term of the sentence score is shared with the chain.
			c.Score = main.Score - e.logProb(topW) + e.logProb(alt.Weight)
			alts = append(alts, c)
		}
	}
	sort.SliceStable(alts, func(i, j int) bool {
		if alts[i].Score != alts[j].Score {
			return alts[i].Score > alts[j].Score
		}
		return alts[i].Word < alts[j].Word
	})
	if len(alts) > maxComposeAlternates {
		alts = alts[:maxComposeAlternates]
	}
	return alts
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
