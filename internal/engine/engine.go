// Package engine turns raw typed ASCII into ranked Chinese candidates.
//
// It is the half of the input method that has nothing to do with macOS or
// with qui: given a string like "nihao", "zhg" or "womenshi", produce the
// words a user meant, best first. Everything here is pure Go over an mmap'd
// lexicon, so it can be tested without a window, a text client, or a build
// of the app bundle.
//
// The pipeline is three stages:
//
//	raw text ──▶ lattice ──▶ trie walk ──▶ ranked candidates
//	            (how the      (which of
//	             input could    those readings
//	             be read)       exist as words)
//
// plus a Viterbi pass that stitches several words together when no single
// word covers what was typed — the difference between an IME that handles
// "nihao" and one that handles "womenmingtianqubeijing".
package engine

import (
	"math"
	"sort"
	"strings"

	"github.com/qizhanchan/q-ime/internal/dict"
	"github.com/qizhanchan/q-ime/internal/emoji"
	"github.com/qizhanchan/q-ime/internal/english"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// Source says where a candidate came from, which the UI uses to mark
// user-learned entries and which the tests use to assert on behaviour.
type Source uint8

const (
	SourceLexicon  Source = iota // a single word from the compiled dictionary
	SourceSentence               // several words stitched together
	SourceUser                   // remembered from what this user picked before
	SourceContext                // suffix of a dictionary phrase after committed text
	SourceEnglish                // the typed letters are an English word
	SourceEmoji                  // an emoji standing for a word the input reads as
)

// Candidate is one option offered to the user.
type Candidate struct {
	Word string
	// Consumed is how many bytes of the (separator-stripped) input this
	// candidate accounts for. Committing a candidate that consumes less than
	// the whole buffer leaves the rest composing, which is how a user types a
	// long run and commits it a word at a time.
	Consumed int
	// Reading is the syllables the consumed input was read as. Used to key
	// the user dictionary — NOT to draw the preedit, because a reading may
	// spell out letters the user never typed ("h" reads as "huo").
	Reading []string
	// Spans are the input offsets each syllable ended at, so a caller can
	// segment the TYPED text without inventing characters: typing "h" must
	// show "h", not the "huo" it was understood as.
	//
	// Parallel to Reading for a plain match. A candidate that predicts past
	// the input has fewer Spans than Reading, since the predicted syllables
	// correspond to nothing typed.
	Spans  []int
	Source Source
	// Corrected says this candidate was only reachable by assuming the user
	// transposed two letters. Worth surfacing: the preedit shows what was
	// TYPED ("pign"), so without a mark there is nothing on screen to explain
	// why the candidate reads 平 — and a correction the user cannot see is one
	// they cannot distrust.
	Corrected bool
	Score     float64
	// Words is the individual dictionary words a SourceSentence candidate was
	// assembled from; nil for a single-word match. Kept because how a
	// sentence was cut up is the only way to judge whether the cut is
	// plausible — see singletonPenalty.
	Words []string
}

// Engine is the ranked-candidate search. Safe for use from one goroutine —
// the input method's — which is the only place it runs.
type Engine struct {
	dict  *dict.Reader
	user  *UserDict
	fuzzy pinyin.Fuzzy
	// typo enables adjacent-letter transposition correction. Separate from
	// fuzzy because they are different claims about what the user did — fuzzy
	// is "you may not distinguish these sounds", typo is "your finger slipped"
	// — and every shipping IME lets them be turned on independently.
	typo bool
	// english is optional; nil simply means no English candidates.
	english *english.Dict
	// emoji is optional too, and is a DECORATION rather than a search: it is
	// consulted with words the rest of the engine has already produced.
	emoji *emoji.Dict
	// inlineEmoji gates the decoration only; the explicit "v" mode works
	// regardless. See SetEmoji.
	inlineEmoji bool

	// context is the last word the user committed, or "" for the start of a
	// sentence. The only state here that outlives a single query.
	//
	// A field rather than a Candidates parameter because it is a property of the
	// typing SESSION, not of the lookup: every caller would otherwise have to
	// thread it through, and the tools that have no session (qime-query,
	// qime-audit) would have to pass "" at every call site to say nothing.
	context string
	// succList is what the user has typed after context, best first, and succ
	// is the same thing keyed by word for the ranking path.
	//
	// Cached across keystrokes rather than looked up per query: the ranking
	// consults it once per CANDIDATE — hundreds of times for a wide
	// abbreviation — and taking the user dictionary's mutex that often on the
	// keystroke path is exactly the kind of cost that does not show up in a
	// benchmark of one query and does show up as a stutter while typing.
	succList  []Successor
	succ      map[string]float64
	succGen   uint64
	succReady bool

	// logRef is ln(reference weight), subtracted from every ln(weight) so
	// scores are log-PROBABILITIES (at most ~0, usually negative) rather
	// than log-counts. This is what makes multi-word candidates rankable:
	// summing log-counts rewards using more words, so the best "sentence"
	// would always be the one chopped into the most pieces.
	logRef float64

	// Scratch reused across queries so the hot path — a keystroke — does not
	// allocate a fresh path stack and state set every time.
	path     []uint16
	pathEnds []int
	visited  map[uint64]bool

	// readings caches a trie node's syllable spelling.
	//
	// Sound because a trie node has exactly ONE path from the root, so the
	// reading is a pure function of the node. Worth it because composing a
	// long sentence reaches the same nodes from many start positions, and
	// rebuilding those []string every time was the single largest source of
	// garbage on the keystroke path.
	readings map[uint32][]string

	// delIndex maps "a syllable with one letter removed" to the syllables it
	// could be: delIndex["zhog"] and delIndex["zhon"] both hold zhong. This is
	// the repair table for a DROPPED keystroke, precomputed over the alphabet
	// (~416 syllables × a handful of deletions each) so that asking "which
	// syllable is this chunk one letter short of" costs one map hit at lattice
	// build instead of 26 trial insertions at every position.
	delIndex map[string][]uint16
}

// maxReadingCache bounds the reading cache. The lexicon has millions of
// nodes and this process runs for weeks, so the cache needs a ceiling; it is
// dropped wholesale rather than evicted per-entry because it is pure derived
// data and rebuilding an entry costs a handful of slice reads.
const maxReadingCache = 1 << 16

// New builds an engine over an already-open lexicon. user may be nil.
func New(d *dict.Reader, user *UserDict, fuzzy pinyin.Fuzzy) *Engine {
	e := &Engine{
		dict:     d,
		user:     user,
		fuzzy:    fuzzy,
		logRef:   math.Log(float64(d.RefWeight()) + 1),
		path:     make([]uint16, 0, dict.MaxSyllables),
		pathEnds: make([]int, 0, dict.MaxSyllables),
		visited:  make(map[uint64]bool, 1024),
		readings: make(map[uint32][]string, 1024),
		delIndex: make(map[string][]uint16, 2048),
	}
	// Only syllables of three letters or more take part in dropped-letter
	// repair: recovering a two-letter syllable from a single leftover letter
	// is not reading the input, it is inventing it — and single letters
	// already have a meaning, the onset abbreviation.
	for id, s := range d.Syllables() {
		if len(s) < 3 {
			continue
		}
		for _, k := range pinyin.DropOne(s) {
			e.delIndex[k] = append(e.delIndex[k], uint16(id))
		}
	}
	// Drop the keys that point everywhere. "zhon" names zhong and nothing
	// else — strong evidence, one arc. "hi" could be the remains of hai, chi,
	// shi or zhi — which is to say it says almost nothing, and offering all
	// four turns every "zhi" keystroke into a fan-out over their subtrees:
	// measured on the shipped lexicon, keeping ambiguous keys took the query
	// for plain "zhi" from ~0.5ms to ~2.8ms. An over-ambiguous repair is weak
	// exactly in proportion to what it costs, so it is not offered at all.
	for k, ids := range e.delIndex {
		if len(ids) > maxDeletionRepairs {
			delete(e.delIndex, k)
		}
	}
	return e
}

// maxDeletionRepairs bounds how many syllables one dropped-letter chunk may
// suggest before the suggestion is judged too vague to make — see the index
// construction in New.
const maxDeletionRepairs = 3

// SetEnglish attaches an English word list, or removes it with nil. Separate
// from New because it is optional and because most of the ranking tests have
// no business loading a 23,000-word table.
func (e *Engine) SetEnglish(d *english.Dict) { e.english = d }

// SetContext tells the engine which word was committed last, so the next
// query can be ranked in its light. Pass "" at a sentence boundary — a word
// after a full stop is not a continuation of anything.
//
// Called on every keystroke, so it does nothing when neither the word nor the
// history has changed. The generation check is what makes committing the same
// word twice running (好, 好) still see the pair just recorded.
func (e *Engine) SetContext(prev string) {
	gen := e.user.Generation()
	if e.succReady && e.context == prev && e.succGen == gen {
		return
	}
	e.context, e.succGen, e.succReady = prev, gen, true
	e.succList = e.user.Successors(prev)
	e.succ = nil
	if len(e.succList) == 0 {
		return
	}
	e.succ = make(map[string]float64, len(e.succList))
	for _, s := range e.succList {
		// One word may be recorded under several readings (行 as háng and as
		// xíng). For the ranking path, which only has the word, the best of
		// them is the honest answer.
		if b, ok := e.succ[s.Word]; !ok || s.Boost > b {
			e.succ[s.Word] = s.Boost
		}
	}
}

// Context reports the word the next query will be ranked against.
func (e *Engine) Context() string { return e.context }

// SetEmoji attaches a word→emoji table, or removes it with nil.
//
// inline says whether emoji may appear in the ORDINARY candidate list beside
// the word they decorate. Off still leaves them reachable through EmojiWords —
// the explicit "v" mode — which costs a user who never types emoji nothing at
// all, so the two are separate switches rather than one.
//
// Both in one call because they are one decision, and splitting them into two
// setters would make "attached the table but forgot to turn it on" a bug the
// compiler cannot see.
func (e *Engine) SetEmoji(d *emoji.Dict, inline bool) {
	e.emoji, e.inlineEmoji = d, inline
}

// SetFuzzy swaps the fuzzy-matching rules.
func (e *Engine) SetFuzzy(f pinyin.Fuzzy) { e.fuzzy = f }

// Fuzzy reports the current rules.
func (e *Engine) Fuzzy() pinyin.Fuzzy { return e.fuzzy }

// SetTypoCorrection turns adjacent-letter transposition correction on or off.
func (e *Engine) SetTypoCorrection(on bool) { e.typo = on }

// TypoCorrection reports whether transposition correction is on.
func (e *Engine) TypoCorrection() bool { return e.typo }

// Ranking weights. These are the entire "feel" of the input method, so they
// are named and gathered rather than sprinkled through the search.
//
// The score is a log-probability: ln P(word) + ln P(what was typed | that
// word's reading). Every constant below is a term of that second factor —
// how likely a user is to have typed what they typed, given they meant this
// word. Keeping it in those terms is why the numbers can be reasoned about
// instead of only tuned.
//
// Units are natural logs, so 2.3 is worth "ten times more likely".
const (
	// coverBonus is per byte of input accounted for — really the cost of
	// input this candidate does NOT explain, moved to the other side.
	//
	// Large, because explaining everything the user typed is close to
	// decisive: 中国 reads all of "zhg" and must beat 这, which reads "zh"
	// and abandons the rest. It also sets the ceiling on the penalties
	// below. An onset abbreviation buys just ONE byte of coverage, so if
	// abbrevPenalty + syllablePenalty ever exceeded coverBonus, taking an
	// abbreviation would be a net loss at every position and "zhg" would
	// stop finding 中国 at all.
	//
	// # Why 6.5 and not 5.0
	//
	// 5.0 did not deliver what the paragraph above claims. Abbreviating the
	// last word of a phrase — "lihail" for 厉害了, "meiguor" for 美国人 — earns
	// coverBonus − abbrevPenalty − syllablePenalty = +1.8, which loses to any
	// frequency gap over 6×, and 厉害 is 17× commoner than 厉害了 in the corpus.
	// So the engine answered 厉害 and left the "l" hanging: it declined to
	// explain a letter the user had deliberately typed.
	//
	// This is the ONLY constant that can be moved to fix that. The score is
	// coverBonus×bytes − penalties + lnP, so between two candidates covering
	// the SAME input the coverage term cancels exactly — which is where all the
	// delicate balances live (English against Chinese, a corrected reading
	// against a typed one, 山 against 上). Raising it therefore cannot disturb
	// any of them; it moves only the comparison it is meant to move.
	//
	// The value is measured, not chosen. tools/qime-audit -mode coverage ranks
	// every case by how far the full-coverage reading trails, and the character
	// of the list changes with the distance:
	//
	//	trails by <1.5   选择权 默认值 上海人 中国式 生活上 侵犯了   — extensions, all wanted
	//	trails by 1.5–2   组织法 个人版 比较快 … but also 监察室 over 检查
	//	trails by >2      交友 over 教育, 几束花 over 技术, 我门派 over 我们
	//
	// The junk past 2.0 is not extension at all, it is RE-SEGMENTATION: a
	// different cut of the same letters that happens to reach the end. 6.5
	// (+1.5) takes the clean band and stops before the mixed one.
	//
	// Cost, measured on the shipped lexicon: of 35 everyday inputs the top
	// candidate changed for exactly two — lihail and meiguor, the cases this is
	// for. Typing two full sentences letter by letter, 48 intermediate states,
	// nothing changed at all. Typo correction is untouched (0/2880 harm, and
	// in-syllable recovery ticks up from 94.8% to 95.3%).
	coverBonus = 6.5

	// abbrevPenalty is charged when a syllable was given only as its onset —
	// "zhg" for 中国. A deliberate act by the user, and common, so the cost
	// is real but modest.
	abbrevPenalty = 1.6

	// partialPenalty is charged when the engine EXTENDS a syllable the user
	// already finished: reading the "me" of "wome" as men, or the "shan" of
	// "shan" as shang.
	//
	// Distinct from abbrevPenalty, and much steeper, because it is a
	// different claim. An onset is the user saying "you fill this in"; a
	// completed syllable is the user having said what they meant, and
	// overriding that is speculation. The an/ang, en/eng and in/ing pairs are
	// where this shows: 上 is far commoner than 山, and with the two costs
	// merged it takes the top slot for "shan" — which is not what was typed.
	partialPenalty = 3.2

	// syllablePenalty is charged per syllable beyond the first.
	//
	// Without it there is nothing to prefer the simpler reading of an
	// ambiguous string: "ban" splits as ba+n (把你) just as legally as it
	// reads as ban (半), both cover three bytes, and the two-character word
	// wins on raw frequency. Positing an extra syllable is positing extra
	// structure, and it has to cost something.
	//
	// It cannot be folded into abbrevPenalty, which is bounded above by
	// coverBonus: an abbreviation that cost more than the letter it saves
	// would never be worth taking, and "zhg" would stop finding 中国.
	syllablePenalty = 1.6

	fuzzyPenalty = 1.1 // per syllable that needed a fuzzy substitution

	// typoPenalty is charged when a syllable was only found after swapping two
	// adjacent letters.
	//
	// Nearly inert, and that is worth saying plainly rather than dressing the
	// number up. What protects correctly typed input is not this penalty but
	// the guard in buildLattice: a chunk that already spells a syllable gets no
	// transposed arc at all, so for correct input there is usually nothing here
	// to compete. Measured over the 2880 commonest readings in the shipped
	// lexicon, turning correction on changes the top candidate for ZERO of
	// them — at every value from 2.0 to 6.0. Recovery of the same 2880 with one
	// transposition each moves just as little: 98.8% at 2.0, 98.6% at 6.0.
	//
	// So the constant is chosen for the cases the audit does not cover. Since a
	// transposed arc spans exactly the same input as an ordinary one, the
	// coverage term cancels and this is weighed directly against a difference
	// in ln P(word) — the same cancellation that lets englishBase be a single
	// number. 6.0 puts it clear of the band where the lexicon's own weights are
	// meaningless (18% of common readings have their top two candidates within
	// 2.0 of each other, an artefact of the upstream corpus — see the README),
	// so a correction cannot win a coin toss against something actually typed.
	// 8.0 was measurably worse at the job: recovery drops to 97.4%.
	typoPenalty = 6.0

	// completionPenalty applies to words that run PAST the input ("ni" →
	// 你们). Deliberately steep: these are predictions, and must never
	// outrank something the user actually spelled.
	completionPenalty = 5.0

	// junctionPenalty is charged per word boundary when composing a
	// sentence, so a single dictionary word always beats the same text
	// assembled from pieces.
	junctionPenalty = 3.2

	// englishBase is what an exact English word scores, on top of the same
	// coverBonus every candidate gets for the input it explains.
	//
	// An English hit is the cleanest possible parse — it accounts for every
	// letter with no abbreviation, no extended syllable, no fuzzy
	// substitution and no word junction — so its score is coverBonus × length
	// and nothing else. Which means that when it is compared against a Chinese
	// reading of the SAME input, the coverage term cancels, and this one
	// constant is weighed against exactly the quantity that should decide it:
	//
	//	englishBase   vs   ln P(word) − (how much force-fitting it took)
	//
	// That is why a single constant is enough, and why it does not need to
	// know how long the input is. A clean Chinese reading lands around −2 to
	// −6 on the right-hand side: 很 for "hen" is one syllable, no penalties, a
	// common word. A force-fitted one lands near −13: 合理 for "hello" needs
	// two abbreviated syllables and is not a common word. Sitting between
	// them, this rule reads as "English wins exactly when Chinese had to be
	// forced", which is the distinction a user actually makes — and the reason
	// "hen", "ban", "song" and "men" still put the Chinese first even though
	// all four are English words.
	englishBase = -8.0

	// englishAltPenalty separates the words sharing one code, so "hell" stays
	// ahead of "he'll" and plain "iPhone" ahead of "iPhone 17 Pro". Small: the
	// list is already in the order rime-ice put them in, and this only has to
	// preserve it against the sort.
	englishAltPenalty = 0.4

	// User adaptation. A word picked even once for a given reading should
	// jump the list; picked repeatedly it should be immovable.
	userBase   = 3.2
	userScale  = 1.15
	recentSlot = 2.0

	// Context adaptation: what following the LAST COMMITTED WORD with this one
	// has earned. This is the transition probability the Viterbi never had — the
	// junction between two words has always been a flat junctionPenalty, which
	// says a boundary costs something but nothing about which words meet at it.
	//
	// The base is anchored, not tuned. A prediction that runs past the input is
	// docked completionPenalty (5.0), and the case this exists for is exactly a
	// prediction: type 真, then `b`, and 不错 is a word the input does not spell.
	// One recorded pair scores base + scale·ln2 + recentSlot ≈ 5.7 while it is
	// fresh, so a pair the user has actually typed BARELY clears that penalty and
	// nothing else does. That is the intended reading of the rule: a completion
	// stays speculation until this particular user has made it a habit.
	//
	// Below userBase on purpose. "You picked this word for these letters" is a
	// direct statement about what was typed; "you once typed this word after
	// that one" is circumstantial, and sparser — a vocabulary of N words has N²
	// possible pairs, so any single pair is thinner evidence than any single
	// pick.
	bigramBase  = 3.0
	bigramScale = 1.0

	// emojiPenalty puts an emoji just behind the word it decorates.
	//
	// Small on purpose, and it is NOT what keeps emoji out of the way — the
	// panel's reserved slot is, exactly as it is for English. This only has to
	// guarantee that 😄 never outranks 开心, because the word is what the user
	// typed and the picture is an offer. Anything larger would push emoji past
	// the homophones of common syllables, where the reserved slot would have to
	// drag them back from position forty.
	emojiPenalty = 0.5
)

// searchBudget caps how many trie states one query may visit.
//
// An abbreviation like "j" fans out to every syllable starting with j, and a
// string of them multiplies. Bounding the search is what keeps the worst case
// (a user holding down a key) from turning into a visible stall in whatever
// app they are typing into — a slow input method is a broken input method.
const searchBudget = 60000

// maxPostsPerNode caps how many words are taken from one trie node. Postings
// are stored weight-descending and every candidate from a single node shares
// the same coverage and penalties, so the best few by weight are exactly the
// best few by score: truncating here loses nothing that would have ranked.
const maxPostsPerNode = 24

// maxCompletionExpansions caps how many children a prediction pass expands
// per node — completions and the continuation search share it.
const maxCompletionExpansions = 12

// reach is a trie state the input can arrive at.
type reach struct {
	end     int       // input position after the consumed syllables
	node    dict.Node //
	syls    []uint16  // the reading that got here
	ends    []int     // input offset each of those syllables ended at
	abbrev  int       // syllables given as a bare onset
	partial int       // finished syllables the engine extended
	fuzzy   int
	typo    int // syllables found only after a letter transposition
}

// penalty is what this reading cost: how unlikely the typed text is, given
// the user meant a word with this reading.
func (r reach) penalty() float64 {
	return abbrevPenalty*float64(r.abbrev) +
		partialPenalty*float64(r.partial) +
		fuzzyPenalty*float64(r.fuzzy) +
		typoPenalty*float64(r.typo) +
		syllablePenalty*float64(len(r.syls)-1)
}

// maxTypos caps how many transpositions one reading may assume.
//
// One. A slip is a single event, and a reading positing two of them is not
// reading the input any more — it is searching for something the input could
// have been. The cost is also multiplicative: every extra allowed typo
// multiplies the arcs at every position, on the keystroke path.
const maxTypos = 1

// Candidates is the main entry point: rank what raw could mean.
//
// limit caps the returned slice; pass 0 for "everything found".
func (e *Engine) Candidates(raw string, limit int) []Candidate {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return nil
	}
	input, breaks := pinyin.StripSeparators(raw)
	if input == "" {
		return nil
	}
	lat := e.buildLattice(input, breaks)

	budget := searchBudget
	acc := newAccumulator()

	// One walk feeds both of the next two steps; the trie states reachable
	// from position 0 are the same set either way, and this search is the
	// expensive part of a keystroke.
	roots := e.collect(lat, 0, dict.MaxSyllables, &budget, e.dict.Root())

	// 1. Single words reading a prefix of the input.
	for _, r := range roots {
		if !lat.canContinue(r.end, len(input)) {
			continue
		}
		e.harvest(&acc, input, r, SourceLexicon)
	}

	// The best score any candidate that reads the WHOLE input has earned.
	// Predictions are capped just under it — see completions.
	bestSpelled := math.Inf(-1)
	for _, c := range acc.list {
		if c.Consumed >= len(input) && c.Score > bestSpelled {
			bestSpelled = c.Score
		}
	}

	// 2. Predictions that continue past what was typed.
	e.completions(roots, input, &acc, &budget, bestSpelled)

	// 3. A stitched-together reading of the whole input, when one exists —
	// plus a few homophone variants of it, so one wrong character in a long
	// sentence is a different pick rather than a word-by-word retype.
	for _, s := range e.compose(lat, input, &budget) {
		acc.add(s)
	}

	// 4. The input read as English, if it spells a word exactly.
	e.englishExact(input, &acc)

	// 5. Words this user has typed after the last one they committed.
	e.contextCandidates(input, &acc)

	// 6. Multi-syllable words this user BUILT, which the lexicon has no entry
	// for and so no earlier step can have produced.
	e.learnedPhrases(input, &acc)

	out := acc.sorted()
	// 7. Emoji for the words above. LAST, and over the sorted list, because an
	// emoji is a decoration on a candidate rather than a candidate in its own
	// right: which words are worth decorating is a question only the finished
	// ranking can answer.
	out = e.decorateWithEmoji(input, out)
	if limit > 0 && len(out) > limit {
		out = truncateKeepingEnglish(out, limit)
	}
	return out
}

// Continuations returns suffixes of dictionary phrases that begin with the
// previously committed word. It is the cold-start counterpart to the learned
// bigram path: when the user has committed 吃饭 and types `l`, the lexicon can
// reach entries such as 吃饭了 and 吃饭了吗 even though neither phrase is a
// top-level candidate for `l` on its own.
//
// The returned candidates consume only the newly typed input and contain only
// the suffix text/reading. Selecting one therefore appends the suffix to the
// already committed word, while the original full-phrase candidate remains
// untouched for ordinary composition.
//
// # The search walks the trie from the committed reading's node
//
// The first implementation re-ranked the ENTIRE combined reading: it built
// "chi'fan'" + input as a string, ran the full Candidates query on it, and
// filtered the 96 results down to the ones that were actually continuations —
// re-parsing on every keystroke syllables that were already committed and could
// not be read any other way. That cost 600µs–1ms per call against 66µs for an
// ordinary query, and the session pays it up to twice per keystroke; the README
// carried it as a known cost for exactly as long as it took to remove.
//
// Descending to the committed reading's node FIRST and searching only the new
// input from there makes the committed boundaries binding for free (there is no
// other path through the trie), spends the whole budget on real continuations
// instead of on 先 现 显 线 readings the filter would discard, and drops the
// call under 40µs. What it deliberately no longer offers is a STITCHED suffix —
// a continuation assembled by the sentence composer rather than found as a
// lexicon phrase. A stitched sentence is a guess about segmentation; the whole
// point of this path is to surface phrases the lexicon actually contains, and
// the ordinary composition of the remaining buffer still handles the rest.
//
// # Scoring
//
// A continuation scores the phrase's score MINUS what the already-committed word
// scored on its own — the marginal, which is what the suffix actually
// contributed. That subtraction is the whole reason these can be ranked at all,
// and it replaces a constant that only ever looked right for one input length:
// on the old fixed 3.0 scale, 博物 + `guan` offered 馆 at 3.0 against homophones
// of `guan` scoring 21 to 24, so the continuation carried no ranking weight
// whatsoever and 关 stayed first. The marginal puts 馆 at 26.9 and first, which
// is what typing `bowuguan` in one breath has always given.
//
// It also needs no calibration, because the coverage term cancels exactly as it
// does for englishBase:
//
//	marginal = coverBonus·len(input) − (penalties the suffix added)
//	                                 + ln P(phrase) − ln P(committed word)
//
// The last line is the transition probability, straight out of the lexicon. And
// the cancellation is why the single-letter case did not need protecting after
// all: 八达 + `l` reaches 岭 through an abbreviated syllable, so the phrase is
// already docked abbrevPenalty and the marginal lands at 3.9 — behind 来 (4.7),
// exactly where a one-letter guess belongs. The same formula that promotes a
// fully spelled continuation to first keeps a guessed one modest.
//
// ln P(committed word) falls back to 0 when the lexicon has no entry at this
// reading — the ceiling of a log-probability, so the baseline over-estimates
// and the marginal under-estimates: an unknown prefix makes the engine more
// cautious about continuing it, which is the right direction to be wrong in.
func (e *Engine) Continuations(prevWord string, prevReading []string, input string, limit int) []Candidate {
	if prevWord == "" || isASCII(prevWord) || len(prevReading) == 0 || input == "" {
		return nil
	}
	// Byte offsets in a Candidate are measured against the SEPARATOR-STRIPPED
	// input — the user may have typed their own apostrophes into the new input.
	cleanInput, breaks := pinyin.StripSeparators(input)
	if cleanInput == "" {
		return nil
	}
	// The committed text was typed as these syllables, so no phrase continuing
	// it can read them any other way: start the walk at their node.
	n := e.dict.Root()
	prevIDs := make([]uint16, len(prevReading))
	for i, syl := range prevReading {
		id, ok := e.dict.SyllableID(syl)
		if !ok {
			return nil
		}
		prevIDs[i] = id
		if n, ok = e.dict.Child(n, id); !ok {
			return nil
		}
	}
	maxSyls := dict.MaxSyllables - len(prevReading)
	if maxSyls <= 0 {
		return nil
	}
	baseline := 0.0
	if w, ok := e.lexiconWeight(prevWord, prevReading); ok {
		baseline = e.logProb(w)
	}

	lat := e.buildLattice(cleanInput, breaks)
	budget := searchBudget
	roots := e.collect(lat, 0, maxSyls, &budget, n)

	var out []Candidate
	add := func(node dict.Node, r reach, extra int, fullSyls []uint16) {
		cnt := e.dict.PostCount(node)
		if cnt > maxPostsPerNode {
			cnt = maxPostsPerNode
		}
		if extra > 0 && cnt > 4 {
			cnt = 4 // predictions are speculation; same cap as completions
		}
		reading := e.readingOf(node, fullSyls)
		base := coverBonus*float64(r.end) - r.penalty() -
			// One more than r.penalty() charges: the marginal pays
			// syllablePenalty per TYPED suffix syllable, and the first one is
			// already "one beyond" the committed reading. A PREDICTED syllable
			// pays completionPenalty instead, never both — the same convention
			// the completions pass uses.
			syllablePenalty -
			completionPenalty*float64(extra)
		for i := 0; i < cnt; i++ {
			p := e.dict.Post(node, i)
			if len(p.Word) <= len(prevWord) || !strings.HasPrefix(p.Word, prevWord) {
				continue
			}
			score := base + e.logProb(p.Weight) - baseline
			// The phrase keeps its own boosts — picking 博物馆 still helps —
			// while the baseline stays lexicon-only, so picking 博物 alone
			// never makes the engine less willing to continue it.
			if r.end >= len(cleanInput) {
				if b, ok := e.user.Boost(reading, p.Word); ok {
					score += b
				}
				if b, ok := e.succ[p.Word]; ok {
					score += b
				}
			}
			out = append(out, Candidate{
				Word:     p.Word[len(prevWord):],
				Consumed: r.end,
				Reading:  append([]string(nil), reading[len(prevReading):]...),
				// r.ends is already relative to the new input, and predicted
				// syllables span none of it.
				Spans:  r.ends,
				Source: SourceContext,
				Score:  score,
			})
		}
	}

	scratch := make([]uint16, 0, dict.MaxSyllables)
	type childRef struct {
		syl    uint16
		node   dict.Node
		weight uint32
	}
	best := make([]childRef, 0, maxCompletionExpansions)
	for _, r := range roots {
		if !lat.canContinue(r.end, len(cleanInput)) {
			continue
		}
		fullSyls := append(append(scratch[:0], prevIDs...), r.syls...)
		add(r.node, r, 0, fullSyls)
		// Phrases that run one syllable past the input — 吃饭 + `l` reaching
		// 了吗 — using the same weight-ranked child pick as completions, and
		// the same bound: predictions that cost latency are not worth having.
		if r.end != len(cleanInput) || !e.dict.HasChildren(r.node) {
			continue
		}
		best = best[:0]
		e.dict.ChildrenInRange(r.node, 0, ^uint16(0), func(syl uint16, child dict.Node) bool {
			if budget <= 0 {
				return false
			}
			budget--
			if e.dict.PostCount(child) == 0 {
				return true
			}
			w := e.dict.PostWeight(child, 0)
			i := sort.Search(len(best), func(i int) bool {
				if best[i].weight != w {
					return best[i].weight < w
				}
				return best[i].syl > syl
			})
			if i >= maxCompletionExpansions {
				return true
			}
			if len(best) < maxCompletionExpansions {
				best = append(best, childRef{})
			}
			copy(best[i+1:], best[i:])
			best[i] = childRef{syl: syl, node: child, weight: w}
			return true
		})
		for _, ch := range best {
			add(ch.node, r, 1, append(fullSyls, ch.syl))
		}
	}

	// Rank by the marginal, dedupe by suffix keeping the best reading of it,
	// exactly as the accumulator does for ordinary candidates.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Word < out[j].Word
	})
	seen := make(map[string]bool, len(out))
	kept := out[:0]
	for _, c := range out {
		if seen[c.Word] {
			continue
		}
		seen[c.Word] = true
		kept = append(kept, c)
		if limit > 0 && len(kept) >= limit {
			break
		}
	}
	return kept
}

// MayContinue reports whether the lexicon holds any phrase whose reading starts
// with this one and is longer.
//
// A cheap gate in front of Continuations, which costs a full ranked query. The
// caller tries several lengths of committed text per keystroke and most of them
// are dead ends — 我 then 说, and nothing in the lexicon reads 说… after 我说 —
// so answering "no" in a few trie steps is the difference between one lookup per
// keystroke and three.
//
// Only the READING is checked, not the words at the node: a false positive costs
// one query that returns nothing, while walking the postings to be sure would
// cost more than the query it is trying to avoid.
func (e *Engine) MayContinue(reading []string) bool {
	if len(reading) == 0 {
		return false
	}
	n := e.dict.Root()
	for _, syl := range reading {
		id, ok := e.dict.SyllableID(syl)
		if !ok {
			return false
		}
		if n, ok = e.dict.Child(n, id); !ok {
			return false
		}
	}
	return e.dict.HasChildren(n)
}

// lexiconWeight is the compiled lexicon's weight for exactly this word at
// exactly this reading.
//
// Walks the trie rather than calling dict.Lookup, which materializes every
// posting at the node as a Go string: this runs on the keystroke path, and the
// node for a common two-syllable reading holds dozens of homophones when only
// one of them is being asked about.
func (e *Engine) lexiconWeight(word string, reading []string) (uint32, bool) {
	n := e.dict.Root()
	for _, syl := range reading {
		id, ok := e.dict.SyllableID(syl)
		if !ok {
			return 0, false
		}
		if n, ok = e.dict.Child(n, id); !ok {
			return 0, false
		}
	}
	for i, cnt := 0, e.dict.PostCount(n); i < cnt; i++ {
		if p := e.dict.Post(n, i); p.Word == word {
			return p.Weight, true
		}
	}
	return 0, false
}

// truncateKeepingEnglish cuts the list to limit without throwing away an exact
// English match.
//
// Short inputs have far more than `limit` Chinese candidates ranked above the
// English one — "ssh" scores 7.0, and every plausible reading of s+sh beats
// that — so a plain cut loses precisely the words a user is most likely to
// want in English. Measured against the shipped list: of the 927 English codes
// of three letters or fewer, 15 fell off the end of a 60-candidate list, among
// them js, ssh, zsh, cd, my, she and bus.
//
// This is a lower-level version of the same promise reserveEnglishSlot makes in
// the UI, and it has to exist here too: that function can only move a
// candidate the engine actually returned.
//
// The slot given up is the LAST one, which is the least valuable place in the
// list — nothing there is reachable without paging to the very end — and never
// the first, which is what Space commits. A caller asking for a single
// candidate is asking which one ranked highest, so it is left alone entirely.
func truncateKeepingEnglish(out []Candidate, limit int) []Candidate {
	if limit < 2 {
		return out[:limit]
	}
	for i, c := range out {
		if c.Source != SourceEnglish {
			continue
		}
		if i < limit {
			break // it made the cut on its own
		}
		moved := out[i]
		out = out[:limit]
		out[limit-1] = moved
		return out
	}
	return out[:limit]
}

// collect walks the trie from the given node at input position start,
// following every syllable reading the lattice allows, and returns each state
// that has words on it. Ordinary queries walk from the root; the continuation
// search walks from the already-committed reading's node, which is what makes
// the committed syllable boundaries binding without re-parsing them.
//
// Depth-first with a visited set keyed on (position, node): the same trie
// node is reachable by several readings of the same prefix, and without the
// memo an input of a dozen abbreviated syllables re-explores the same
// subtrees exponentially.
func (e *Engine) collect(lat lattice, start, maxSyls int, budget *int, from dict.Node) []reach {
	var out []reach
	clear(e.visited) // reused rather than reallocated; see Engine.visited
	visited := e.visited
	e.path = e.path[:0]
	e.pathEnds = e.pathEnds[:0]

	var walk func(pos int, n dict.Node, abbrev, part, fuzz, typo int)
	walk = func(pos int, n dict.Node, abbrev, part, fuzz, typo int) {
		if *budget <= 0 || len(e.path) >= maxSyls {
			return
		}
		if e.dict.PostCount(n) > 0 && pos > start {
			out = append(out, reach{
				end:     pos,
				node:    n,
				syls:    append([]uint16(nil), e.path...),
				ends:    append([]int(nil), e.pathEnds...),
				abbrev:  abbrev,
				partial: part,
				fuzzy:   fuzz,
				typo:    typo,
			})
		}
		if pos >= len(lat) {
			return
		}
		for _, arc := range lat[pos] {
			if arc.typo && typo >= maxTypos {
				continue // one slip per reading — see maxTypos
			}
			e.dict.ChildrenInRange(n, arc.lo, arc.hi, func(syl uint16, child dict.Node) bool {
				*budget--
				if *budget <= 0 {
					return false
				}
				key := uint64(arc.end)<<40 | uint64(child.ID())
				if visited[key] {
					return true
				}
				visited[key] = true

				na, np, nf, nt := abbrev, part, fuzz, typo
				if arc.abbrev {
					na++
				}
				if arc.partial {
					np++
				}
				if arc.fuzzy {
					nf++
				}
				if arc.typo {
					nt++
				}
				e.path = append(e.path, syl)
				e.pathEnds = append(e.pathEnds, arc.end)
				walk(arc.end, child, na, np, nf, nt)
				e.path = e.path[:len(e.path)-1]
				e.pathEnds = e.pathEnds[:len(e.pathEnds)-1]
				return true
			})
		}
	}
	walk(start, from, 0, 0, 0, 0)
	return out
}

// harvest turns one reached trie state into scored candidates.
//
// Only called for readings that are still alive — see lattice.canContinue.
func (e *Engine) harvest(acc *accumulator, input string, r reach, src Source) {
	cnt := e.dict.PostCount(r.node)
	if cnt > maxPostsPerNode {
		cnt = maxPostsPerNode
	}
	base := coverBonus*float64(r.end) - r.penalty()

	reading := e.readingOf(r.node, r.syls)
	for i := 0; i < cnt; i++ {
		c := e.dict.Post(r.node, i)
		score := base + e.logProb(c.Weight)
		source := src
		// Learned-word boosts apply only to candidates that explain the WHOLE
		// input — see explainsEverything. The context boost is held to the same
		// rule and for the same reason: a short learned word that happens to
		// follow the last one would otherwise hijack the front of a longer
		// composition the user is still typing.
		if e.user != nil && explainsEverything(r.end, len(input)) {
			if boost, ok := e.user.Boost(reading, c.Word); ok {
				score += boost
				source = SourceUser
			}
			if boost, ok := e.succ[c.Word]; ok {
				score += boost
				source = SourceUser
			}
		}
		acc.add(Candidate{
			Word:      c.Word,
			Consumed:  r.end,
			Reading:   reading,
			Spans:     r.ends,
			Source:    source,
			Corrected: r.typo > 0,
			Score:     score,
		})
	}
}

// completions offers words that continue past the input: typing "ni" should
// eventually be able to reach 你们 without typing "men".
//
// Bounded hard — one extra syllable, a dozen expanded children — because this
// is speculation, and speculation that costs latency is not worth having.
//
// WHICH dozen is decided by each child's best posting weight, not by syllable
// order. Children are stored sorted by syllable ID — alphabetically — and the
// first version expanded the first twelve of those, so whether a continuation
// could be offered at all depended on where its next syllable happened to
// sort: the predictions for "ni" were 你啊 and 你把 while 你们 and 你好 never
// appeared, and 不错 was unreachable from `b` (measured at sixty results —
// the gap the user-bigram namespace was built to route around). Ranking the
// scan costs two mmap reads per child and no allocation, and it spends the
// twelve slots on the continuations a user might actually mean.
//
// bestSpelled is the ceiling: no unboosted prediction may score at or above
// the best candidate that reads the whole input. completionPenalty makes the
// same promise — "these must never outrank something the user actually
// spelled" — but a constant cannot keep it against the corpus's worst weight
// skew: 版权 rides "版权所有" boilerplate to a weight ~150x that of 半, which
// beats the 5.0 penalty, so typing "ban" answered 版权 the moment the ranked
// scan started surfacing the heaviest child instead of the alphabetically
// lucky ones. The cap enforces the promise structurally; a USER-boosted
// prediction is exempt because the boost is the documented exception (真 +
// `b` → 不错 first, once the user has typed that pair — see the succ lookup
// below and bigramBase).
func (e *Engine) completions(roots []reach, input string, acc *accumulator, budget *int, bestSpelled float64) {
	// A child worth expanding, remembered by its best posting's weight. The
	// slice is insertion-sorted weight-descending and capped, so the scan
	// keeps exactly the top few without collecting everything first.
	type childRef struct {
		syl    uint16
		node   dict.Node
		weight uint32
	}
	best := make([]childRef, 0, maxCompletionExpansions)
	// Successive clamped predictions step down a hair each, so the ones that
	// hit the ceiling keep their weight order instead of tying and falling
	// back to the alphabetical tiebreak.
	clamped := 0
	for _, r := range roots {
		if r.end != len(input) || !e.dict.HasChildren(r.node) {
			continue
		}
		best = best[:0]
		e.dict.ChildrenInRange(r.node, 0, ^uint16(0), func(syl uint16, child dict.Node) bool {
			if *budget <= 0 {
				return false
			}
			*budget--
			if e.dict.PostCount(child) == 0 {
				return true // a pure prefix node: no word one syllable deep
			}
			w := e.dict.PostWeight(child, 0)
			// Weight-descending, syllable ID as the tiebreak so equal weights
			// keep a deterministic order between keystrokes.
			i := sort.Search(len(best), func(i int) bool {
				if best[i].weight != w {
					return best[i].weight < w
				}
				return best[i].syl > syl
			})
			if i >= maxCompletionExpansions {
				return true
			}
			if len(best) < maxCompletionExpansions {
				best = append(best, childRef{})
			}
			copy(best[i+1:], best[i:])
			best[i] = childRef{syl: syl, node: child, weight: w}
			return true
		})
		for _, ch := range best {
			cnt := e.dict.PostCount(ch.node)
			if cnt > 4 {
				cnt = 4
			}
			reading := e.readingOf(ch.node, append(append([]uint16(nil), r.syls...), ch.syl))
			for i := 0; i < cnt; i++ {
				c := e.dict.Post(ch.node, i)
				score := e.logProb(c.Weight) + coverBonus*float64(r.end) -
					r.penalty() - completionPenalty
				source := SourceLexicon
				// The context boost belongs here more than anywhere else. This
				// is the 真 + `b` → 不错 case: the input spells one letter and
				// the word is two syllables, so it can only ever arrive as a
				// prediction — and completionPenalty is calibrated to keep
				// predictions down. Having typed this pair before is the one
				// piece of evidence that should be able to lift it.
				if boost, ok := e.succ[c.Word]; ok {
					score += boost
					source = SourceUser
				} else if !math.IsInf(bestSpelled, -1) && score >= bestSpelled {
					// The prediction ceiling — see the function comment. Just
					// under, not dropped: the offer is legitimate, it merely
					// has no claim on the slot Space commits. (When NOTHING
					// spells the whole input the ceiling is -Inf and does not
					// apply — the predictions are then the only full-input
					// explanations there are.)
					clamped++
					score = bestSpelled - 0.001*float64(clamped)
				}
				acc.add(Candidate{
					Word:     c.Word,
					Consumed: r.end,
					Reading:  reading,
					// r.ends only, deliberately: the extra syllable was
					// predicted, not typed, so it spans no input.
					Spans:     r.ends,
					Source:    source,
					Corrected: r.typo > 0,
					Score:     score,
				})
			}
		}
	}
}

// englishExact offers the typed letters as an English word when they spell one
// exactly — the whole buffer, not a prefix of it.
//
// Exactness is what makes this safe to run on every keystroke. A prefix match
// would put "nice" and "night" in front of somebody typing "ni", and no amount
// of penalty tuning fixes that: the user has not finished typing, so there is
// nothing to be right about yet. An exact hit, by contrast, is a complete
// claim — every letter accounted for — and can be scored against the Chinese
// readings on equal terms. See englishBase.
func (e *Engine) englishExact(input string, acc *accumulator) {
	// Reading is the typed letters, not a syllable list. It is only ever used
	// as a user-dictionary key and to draw the preedit, and no pinyin syllable
	// spells "hello", so this cannot collide with a Chinese entry.
	reading := []string{input}
	base := coverBonus * float64(len(input))
	// Spans stays nil on every candidate below, deliberately: nothing here was
	// read as a syllable, so the preedit shows the letters as typed with no
	// separators — "hello", not "he'llo".
	rank := 0
	if e.english != nil {
		e.english.Words(input, func(word string) bool {
			score := base + englishBase - englishAltPenalty*float64(rank)
			// The literal namespace, not the reading one: an English pick is
			// keyed by the letters typed. Both this and the learned words below
			// are promoted by the same boost, so which list a word came from
			// stops mattering once the user has chosen it.
			if boost, ok := e.user.LiteralBoost(input, word); ok {
				score += boost
			}
			// Source stays SourceEnglish even when the user has picked this
			// word before — being an English word is the durable fact about
			// it, and the UI reserves a slot for exactly that. A boost only
			// moves it up.
			acc.add(Candidate{
				Word:     word,
				Consumed: len(input),
				Reading:  reading,
				Source:   SourceEnglish,
				Score:    score,
			})
			rank++
			return true
		})
	}

	// Words this user has typed literally before.
	//
	// The table is a fixed list, so the words it cannot contain are exactly the
	// ones worth learning: a company name, a product, a piece of in-house
	// jargon. Without this, typing "AfterShip" a hundred times leaves the
	// hundred-and-first exactly as much work as the first — the pick is
	// recorded and then never consulted, which reads to the user as an input
	// method that does not learn.
	//
	// Entries whose spelling EQUALS the letters typed are offered too, and that
	// took a measurement to settle.
	//
	// They used to be skipped, on the reasoning that "the letters themselves are
	// already reachable — Enter commits them, and they are the fallback when
	// nothing matches". Both halves are false whenever a Chinese reading covers
	// the whole input, which for a long run of letters is almost always: Enter
	// confirms that reading instead, and the empty-list fallback never fires
	// because the list is not empty. `bigquery` reads as bi'g'qu'er'y and offers
	// 比过去而言 — so a user who had committed the word FOUR times was still
	// offered nothing, which reads as an input method that does not learn.
	//
	// The worry was the pinyin collisions, and the scoring already handles them,
	// because coverBonus·length against englishBase is exactly the comparison
	// englishBase was calibrated for. Measured over the 158 identical literals in
	// a real history — 110 take first place, 48 stay behind Chinese, none return
	// an empty list — and the split falls where it should:
	//
	//	first:  git, url, msg, dev, org, raw, mock, log, api, bigquery (44.0
	//	        against 比过去而言's 34.2) — none of them plausible pinyin
	//	behind: hao (好 25.6, hao 11.5), guan (馆 28.0, guan 18.0), bada, go,
	//	        the, pr, l (-1.5, off the list), b, c, dd, gt, kt — every one
	//	        of them pinyin-shaped, plus gst, which the user gave two
	//	        meanings themselves and which now resolves to 告诉他
	for _, l := range e.user.LiteralsFor(input) {
		score := base + englishBase
		// The boost is a claim about SPELLING — "when you type these letters you
		// mean it written this way" — so it applies only where the spelling
		// differs from the letters. An identical literal makes no such claim; it
		// records that the user once bailed out on these letters, which says
		// nothing about what they mean next time.
		//
		// Withholding it is what keeps this safe, and the margin is thin enough
		// to be worth writing down. `nihao` recorded as a literal scores 24.5
		// against 你好's 30.49 with the boost withheld, and 30.5 — a coin toss —
		// with it. Meanwhile `bigquery` wins at 44 against 34.2 either way, so
		// the case this exists for never needed the boost at all.
		if l.Word != input {
			score += l.Boost
		}
		acc.add(Candidate{
			Word:     l.Word,
			Consumed: len(input),
			Reading:  reading,
			Source:   SourceEnglish,
			Score:    score,
		})
	}
}

// maxEmojiDecorations bounds how many emoji one query may add.
//
// Two, because the panel reserves ONE slot for emoji and the second exists only
// so that a user who pages past the first is not looking at a list that quietly
// stopped having any. Scanning further would cost lookups on a keystroke path
// for candidates nobody reads.
const maxEmojiDecorations = 2

// emojiSourceDepth is how far down the ranked list a word may be and still lend
// its emoji.
//
// The emoji for 开心 is worth offering when 开心 is what the input means. It is
// not worth offering because 开心 happens to be the fortieth reading of a
// three-letter abbreviation — that is how a candidate list fills up with
// pictures of words the user never asked for.
const emojiSourceDepth = 8

// decorateWithEmoji appends an emoji candidate for the best few words that have
// one.
//
// # Why this runs on the finished list instead of inside the search
//
// Because an emoji is not a reading. Every other candidate here answers "what
// could these letters mean"; an emoji answers "what does that word stand for",
// which is a question about the ANSWER, not about the input. Running it last
// means it inherits every way the engine has of finding a word — abbreviations,
// fuzzy readings, a corrected transposition, a learned pick, a remembered
// continuation — without knowing that any of them exist.
//
// # Why only full coverage
//
// The same rule the user dictionary and the context memory obey. An emoji whose
// word explains half the input would be offering a picture for something the
// user is still in the middle of typing.
// # Why the emoji goes NEXT TO its word rather than in a reserved slot
//
// English gets a reserved slot at the back of the first page because an English
// candidate stands on its own: the letters ARE the word, and the corpus can
// legitimately rank it fortieth. An emoji stands on a word. If that word is not
// on the first page, its picture has no business being there ahead of it — and
// if it is, the place a user will look for the picture is beside it.
//
// This is affordable because it is rare. Measured over the 3000 commonest
// readings in the shipped lexicon, only 1.7% produce an emoji at all — the map
// is 4854 hand-picked words, not a layer over the language — so "a slot per
// page" is a cost paid by one query in sixty, not by every one.
func (e *Engine) decorateWithEmoji(input string, out []Candidate) []Candidate {
	if e.emoji == nil || !e.inlineEmoji || len(out) == 0 {
		return out
	}
	depth := emojiSourceDepth
	if depth > len(out) {
		depth = len(out)
	}
	// Collect first, splice after: inserting while scanning would shift the
	// positions being scanned.
	type insert struct {
		src    int
		before bool
		cand   Candidate
	}
	var adds []insert
	seen := map[string]bool{}
	for i := 0; i < depth && len(adds) < maxEmojiDecorations; i++ {
		c := out[i]
		if c.Source == SourceEnglish || c.Source == SourceEmoji {
			continue // an English word is not a Chinese one; emoji do not nest
		}
		if !explainsEverything(c.Consumed, len(input)) {
			continue
		}
		glyph := e.emoji.First(c.Word)
		if glyph == "" || seen[glyph] {
			continue
		}
		seen[glyph] = true
		cand := e.emojiCandidate(c, glyph)
		// Ahead of its word only once the user has put it there. By default a
		// picture never outranks the word it stands for — but "picked even once
		// for this reading jumps the list" is what the user dictionary promises
		// every other candidate, and an emoji somebody keeps choosing has as
		// much claim on it. The boost is what carries it past emojiPenalty.
		adds = append(adds, insert{src: i, before: cand.Score > c.Score, cand: cand})
	}
	if len(adds) == 0 {
		return out
	}
	res := make([]Candidate, 0, len(out)+len(adds))
	next := 0
	for i, c := range out {
		for next < len(adds) && adds[next].src == i && adds[next].before {
			res = append(res, adds[next].cand)
			next++
		}
		res = append(res, c)
		for next < len(adds) && adds[next].src == i {
			res = append(res, adds[next].cand)
			next++
		}
	}
	return res
}

// emojiCandidate builds the emoji's candidate from the word's.
//
// It inherits Reading and Spans wholesale, which is what makes everything else
// work without a special case: the preedit segments the same way, the user
// dictionary keys the pick under the same reading — so choosing 😄 for "kaixin"
// is learned exactly as choosing a word would be — and the context memory
// records it with a reading it can offer again.
//
// The score sits one place below the word it came from, close enough that the
// panel's reserved slot has something to promote and far enough that an emoji
// never takes a slot from the word it decorates.
func (e *Engine) emojiCandidate(from Candidate, glyph string) Candidate {
	c := from
	c.Word = glyph
	c.Source = SourceEmoji
	c.Score = from.Score - emojiPenalty
	// The emoji's OWN history, keyed under the word's reading — which is the
	// key the pick was recorded under, since the candidate carries that reading
	// wholesale. Without this lookup an emoji chosen a hundred times would
	// score exactly as one chosen never: its score is derived from the word's,
	// and the word's boost says nothing about the picture.
	//
	// Source stays SourceEmoji even when boosted, for the same reason an
	// English word stays English: what it IS does not change because it was
	// picked, and the panel's handling keys off that.
	if boost, ok := e.user.Boost(from.Reading, glyph); ok {
		c.Score += boost
	}
	return c
}

// EmojiFor returns the emoji offered for a word, best first, for the explicit
// emoji mode. Empty when there is no table or no entry.
func (e *Engine) EmojiFor(word string, limit int) []string {
	if e.emoji == nil {
		return nil
	}
	var out []string
	e.emoji.Emoji(word, func(g string) bool {
		out = append(out, g)
		return limit <= 0 || len(out) < limit
	})
	return out
}

// EmojiWords is the candidate list for the explicit emoji mode, entered with a
// leading "v".
//
// It runs the ORDINARY query on the letters after the v and keeps only what has
// an emoji. That is the whole implementation: no second index, no matching
// rules of its own, and "vkx" reaches 😄 through 开心 because abbreviations
// already work. A mode that re-implemented matching would be a second thing to
// keep in step with the first.
//
// Unlike the decoration above this offers EVERY emoji a word has, because the
// user asking for emoji is not spending a slot they wanted for something else.
func (e *Engine) EmojiWords(typed string, limit int) []Candidate {
	if e.emoji == nil || typed == "" {
		return nil
	}
	var out []Candidate
	seen := map[string]bool{}
	for _, c := range e.Candidates(typed, 0) {
		if c.Source == SourceEnglish || c.Source == SourceEmoji {
			continue
		}
		if !explainsEverything(c.Consumed, len(typed)) {
			continue
		}
		e.emoji.Emoji(c.Word, func(glyph string) bool {
			if !seen[glyph] {
				seen[glyph] = true
				out = append(out, e.emojiCandidate(c, glyph))
			}
			return limit <= 0 || len(out) < limit
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// maxContextCandidates bounds how many remembered continuations one query may
// offer. A word somebody uses constantly accumulates successors, and the front
// of the list belongs to what was typed, not to a history dump.
const maxContextCandidates = 8

// contextCandidates offers words this user has typed after the last committed
// one, when the input is consistent with how they are spelled.
//
// This is the whole reason the context memory keeps readings. Ranking cannot
// deliver 真 + `b` → 不错, because 不错 is never generated for `b` in the first
// place: the completion path walks one extra syllable and takes the first few
// children of each node, and 不错 is not among them. Verified against the real
// lexicon — it appears nowhere in sixty results for `b`, at any score.
//
// # Why this cannot run away
//
// The match is exact about what was typed: every letter must be accounted for
// by the word's reading, either as a whole syllable or as the start of one. So
// the offer disappears the instant the input contradicts it — after 真, typing
// `b` offers 不错, and typing `ba` does not, because "ba" is not how 不错 begins.
// That is the escape hatch for the case where the guess is wrong: one more
// letter, and it is gone.
func (e *Engine) contextCandidates(input string, acc *accumulator) {
	if len(e.succList) == 0 {
		return
	}
	offered := 0
	for _, s := range e.succList {
		if offered >= maxContextCandidates {
			return
		}
		spans, abbrev, partial, ok := matchReading(input, s.Reading)
		if !ok {
			continue
		}
		// Scored with the same constants as everything else, so a remembered
		// continuation competes on the same terms rather than on a scale of its
		// own: what it explains, minus what it had to assume, plus what the pair
		// has earned.
		score := coverBonus*float64(len(input)) -
			abbrevPenalty*float64(abbrev) -
			partialPenalty*float64(partial) -
			syllablePenalty*float64(len(s.Reading)-1) +
			s.Boost
		if len(spans) < len(s.Reading) {
			// The word runs past what was typed: still a prediction, and still
			// charged for being one.
			score -= completionPenalty
		}
		acc.add(Candidate{
			Word:     s.Word,
			Consumed: len(input),
			Reading:  s.Reading,
			Spans:    spans,
			Source:   SourceUser,
			Score:    score,
		})
		offered++
	}
}

// maxLearnedPhrases bounds how many user-built words one query may offer, and
// phraseScanLimit how many the lookup will even look at. The index is sorted by
// what each has earned, so the scan cap costs only the least-earned tail.
const (
	maxLearnedPhrases = 6
	phraseScanLimit   = 64
)

// learnedPhrases offers multi-syllable words this user built out of several
// commits — the words the lexicon does not contain.
//
// Ranking cannot reach these, and that is the whole point: 业务侧 is not a
// dictionary word, so before this it could only ever arrive by stitching (which
// yields exactly ONE candidate, and 测 beats 侧 for `ce` by 0.37), and picking it
// taught the history something the history could never read back. This is the
// same wall the literal namespace was built to get past for English brands, and
// the same fix: an index that PRODUCES rather than only ranks.
//
// Scored with the same constants as everything else, exactly as
// contextCandidates is, so a built word competes on the same terms: what it
// explains, minus what it had to assume, plus what it has earned. There is no
// lnP term because a word outside the lexicon has no corpus frequency — the
// boost stands in for it, which is the same substitution the learned-successor
// path makes.
func (e *Engine) learnedPhrases(input string, acc *accumulator) {
	if e.user == nil || input == "" {
		return
	}
	offered := 0
	for _, p := range e.user.LearnedPhrases(input[0], phraseScanLimit) {
		if offered >= maxLearnedPhrases {
			return
		}
		spans, abbrev, partial, ok := matchReading(input, p.Reading)
		if !ok {
			continue
		}
		// A word the lexicon already has is already a candidate, already carrying
		// this same boost, and already scored with its corpus frequency. Offering
		// it again here would DROP that frequency term — lnP is negative, so the
		// duplicate would score HIGHER and the accumulator would keep it, quietly
		// inflating every learned word in the history rather than only producing
		// the ones that were missing. Skipping is both cheaper and the honest
		// statement of what this path is for.
		if _, inLexicon := e.lexiconWeight(p.Word, p.Reading); inLexicon {
			continue
		}
		score := coverBonus*float64(len(input)) -
			abbrevPenalty*float64(abbrev) -
			partialPenalty*float64(partial) -
			syllablePenalty*float64(len(p.Reading)-1) +
			p.Boost
		if len(spans) < len(p.Reading) {
			// Runs past what was typed: still a prediction, still charged.
			score -= completionPenalty
		}
		acc.add(Candidate{
			Word:     p.Word,
			Consumed: len(input),
			Reading:  p.Reading,
			Spans:    spans,
			Source:   SourceUser,
			Score:    score,
		})
		offered++
	}
}

// matchReading asks whether input could be the start of a word with this
// reading, and reports what had to be assumed to say yes.
//
// Each syllable is consumed the ways buildLattice would allow it: whole ("bu"
// for bu), as its bare onset at ANY position ("b" for bu, "zh" or "z" for
// zhong — which is what lets `ywc` reach a word read ye'wu'ce, exactly the
// abbreviation the main search grants every lexicon word), or as a longer
// leading fragment ONLY at the end of the input ("cu" for cuo), because that
// is the syllable the user is still typing. Anything else is a mismatch, and
// a mismatch is total: a single letter the word cannot explain rules it out,
// which is what keeps a remembered continuation from lingering once the user
// types past it.
//
// The walk backtracks — whole syllable first, then onsets longest-first — so
// "zg" still reaches zhong'guo after the whole-syllable read of "z…" fails.
// First success wins; the branch order tries cheaper assumptions before
// dearer ones, and readings are short enough (a handful of syllables, at most
// three moves each) that the worst case is a few dozen steps.
//
// spans holds the input offset each syllable ended at, so the preedit segments
// what was TYPED. Syllables past the end of the input contribute none — they
// were predicted, and predictions span no keystrokes.
func matchReading(input string, reading []string) ([]int, int, int, bool) {
	spans := make([]int, 0, len(reading))
	abbrev, partial := 0, 0
	var walk func(pos, i int) bool
	walk = func(pos, i int) bool {
		if pos >= len(input) {
			return true // everything from here on is prediction
		}
		if i >= len(reading) {
			return false // letters this word does not explain
		}
		rest, syl := input[pos:], reading[i]
		// Whole syllable: the reading the user spelled out, no assumption.
		if strings.HasPrefix(rest, syl) {
			spans = append(spans, pos+len(syl))
			if walk(pos+len(syl), i+1) {
				return true
			}
			spans = spans[:len(spans)-1]
		}
		// Bare onset standing in for the syllable, longest onset first so "zh"
		// is preferred over "z" where both letters were typed.
		for k := 2; k >= 1; k-- {
			if k >= len(syl) || k > len(rest) {
				continue
			}
			onset := syl[:k]
			if !pinyin.IsInitial(onset) || !strings.HasPrefix(rest, onset) {
				continue
			}
			spans = append(spans, pos+k)
			abbrev++
			if walk(pos+k, i+1) {
				return true
			}
			abbrev--
			spans = spans[:len(spans)-1]
		}
		// The tail of the input is a longer fragment of this syllable — a word
		// still being typed. Charged as partial rather than abbrev for the same
		// reason the lattice charges them differently: one or two onset letters
		// are a deliberate abbreviation, three are a half-finished syllable.
		if len(rest) < len(syl) && strings.HasPrefix(syl, rest) {
			spans = append(spans, len(input))
			partial++
			return true // consumed to the end; nothing left to contradict
		}
		return false
	}
	if !walk(0, 0) {
		return nil, 0, 0, false
	}
	return spans, abbrev, partial, true
}

// maxEnglishCompletions bounds the guesses offered while typing an English
// word. Small: the table has no frequency data, so past the first few these
// are alphabetical neighbours rather than likely words.
const maxEnglishCompletions = 6

// maxLearnedCompletions bounds the LEARNED guesses offered alongside them.
//
// Separate from the table's budget, and spent first, because these are a
// different kind of guess: every one is a word this user committed by hand.
// Small all the same — a page holds five candidates, and a prefix the user has
// typed many things under should not push the table's answers off it entirely.
const maxLearnedCompletions = 4

// EnglishWords is the candidate list for a literal English fragment being
// typed — the other mode, entered by a capital letter, where the buffer holds
// text rather than pinyin.
//
// Unlike Candidates this does no pinyin work at all. It also does not rank:
// the text AS TYPED is always first, so Space commits exactly what the user
// pressed and the mode is predictable no matter what the dictionary contains.
// Everything after it is a suggestion.
//
// The order past the literal is:
//
//  1. words this user has committed under exactly these letters, best first
//     ("Aftership" offers their own "AfterShip");
//  2. exact table matches, which is where canonical capitalisation comes from
//     ("iphone" typed as Iphone offers iPhone, "github" offers GitHub);
//  3. learned completions, best first ("Af" offers "AfterShip");
//  4. table completions, shortest first.
//
// Learned before table at both tiers: the table says these letters spell a word
// somewhere, the history says this user typed this word, and the second is the
// better evidence about what they are typing now.
//
// Shortest-first is a stand-in for commonest-first, which the table cannot
// support. Among words sharing a prefix the short ones are usually the common
// ones — "shed" before "sheathing" — and it beats the alphabetical order the
// table is stored in, which would offer "shea" and "sheaf" ahead of "she".
func (e *Engine) EnglishWords(typed string, limit int) []Candidate {
	if typed == "" {
		return nil
	}
	code := strings.ToLower(typed)
	out := []Candidate{{
		Word:     typed,
		Consumed: len(typed),
		Reading:  []string{code},
		Source:   SourceEnglish,
	}}
	seen := map[string]bool{typed: true}

	add := func(word string) {
		if seen[word] {
			return
		}
		seen[word] = true
		out = append(out, Candidate{
			Word:     word,
			Consumed: len(typed),
			Reading:  []string{code},
			Source:   SourceEnglish,
		})
	}

	// What this user has typed under exactly these letters comes first. It is a
	// stronger claim than anything a fixed table can make about them: the table
	// says "these letters spell a word somewhere", the history says "you typed
	// this, here". So "Aftership" offers the user's own "AfterShip" ahead of
	// the table's suggestions, the same way "Iphone" offers "iPhone".
	for _, l := range e.user.LiteralsFor(code) {
		add(l.Word)
	}

	if e.english != nil {
		e.english.Words(code, func(word string) bool { add(word); return true })
	}

	// Completions only once there is enough typed to narrow anything: a
	// single letter matches thousands of words in no useful order.
	if len(code) >= 2 {
		// Learned completions before table ones, and for the reason that makes
		// this whole path worth having: after "Af", "AfterShip" is a word this
		// user has committed and "affair" is a word somebody once put in a
		// list. Bounded like the table's, since a long history of literals
		// sharing a prefix would otherwise fill the page on its own.
		for _, l := range e.user.LiteralsWithPrefix(code, maxLearnedCompletions) {
			add(l.Word)
		}

		// Bounded insertion over the WHOLE prefix range, rather than
		// collecting a capped slice and sorting it. Capping the collection
		// was the obvious version and it was wrong: the table is
		// alphabetical, so "sh" filled its budget on "sha…" and never
		// reached "she" — the one completion that mattered. Scanning it all
		// is affordable because the largest two-letter prefix in the shipped
		// table holds 966 codes.
		if e.english != nil {
			var best []string
			e.english.PrefixWords(code, func(word string) bool {
				if !seen[word] && strings.ToLower(word) != code {
					best = keepShortest(best, word, maxEnglishCompletions)
				}
				return true
			})
			for _, w := range best {
				add(w)
			}
		}
	}

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// keepShortest inserts word into an already-ordered best-of list, holding at
// most n, ordered shortest first and alphabetically within a length.
func keepShortest(best []string, word string, n int) []string {
	i := sort.Search(len(best), func(i int) bool { return shorterFirst(word, best[i]) })
	if i >= n {
		return best
	}
	if len(best) < n {
		best = append(best, "")
	}
	copy(best[i+1:], best[i:])
	best[i] = word
	return best
}

func shorterFirst(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// explainsEverything reports whether a candidate accounts for all of the
// input, which is the condition for it to receive a user-dictionary boost.
//
// A boost is evidence of the form "for the reading R, this user wants the word
// W". It is not evidence that they want W right now, and the difference showed
// up as 是 beating 深圳 for "sz":
//
//	是    shi        cover=1/2  score=12.03   boost 8.05, picked 11 times
//	深圳  shen'zhen  cover=2/2  score=11.66   boost 6.46, picked 2 times
//
// The boost on 是 was earned by typing the whole syllable "shi". Here the
// engine reached that reading from a bare "s", then abandoned the "z" — so it
// explained half the input using history gathered when it explained all of it.
//
// # Why this is all-or-nothing rather than a discount
//
// Scaling the boost by coverage is the obvious fix and it does not work, for a
// reason that is structural rather than a matter of tuning. On
// all-abbreviation input each additional explained byte is worth
// coverBonus − abbrevPenalty − syllablePenalty = 1.8, less whatever frequency
// advantage the shorter word has — about 1.2 in the case above. A boost is
// userBase + userScale·ln(picks+1) + recency, which GROWS WITHOUT BOUND in how
// often a word was picked. So for any fixed discount factor there is a pick
// count that overcomes the margin, and the bug returns for whoever types that
// word most.
//
// Measured against this user's own 135 learned words, that is not theoretical:
//
//	                sz      ban    nin
//	no discount     是 ✗    吧 ✗   你 ✗
//	× coverage      深圳 ✓   吧 ✗   你 ✗
//	× coverage²     深圳 ✓   半 ✓   您 ✓      ... but only because they had
//	                                          also picked 深圳 twice; with 是
//	                                          alone learned it is 是 again
//	full coverage   深圳 ✓   半 ✓   您 ✓
//	only
//
// # What this costs
//
// A learned word stops being boosted the moment one stray letter is typed
// past it: with "nihaoo", 拟好 covers 5 of 6 bytes and loses its boost. That
// is a real cost, and it is bounded — the learned word only loses its lead if
// it was the underdog on frequency, which is exactly the case the boost exists
// for. The alternative is a rule that quietly stops working for the user's
// most-typed words, which is worse: it fails for the people who use the input
// method most, and it fails silently.
func explainsEverything(consumed, total int) bool { return consumed >= total }

// readingOf spells a reach's syllables, memoized on the trie node it ended
// at — see Engine.readings for why the node alone is a valid key.
//
// The returned slice is shared with the cache and with every candidate built
// from the same node, so callers must treat it as read-only. Candidate.Reading
// is only ever read (to draw the preedit and to key the user dictionary), and
// keeping it shared is the point: it is what makes composing a long sentence
// stop allocating a fresh []string per position.
func (e *Engine) readingOf(node dict.Node, syls []uint16) []string {
	id := node.ID()
	if got, ok := e.readings[id]; ok {
		return got
	}
	out := make([]string, len(syls))
	for i, s := range syls {
		out[i] = e.dict.Syllable(s)
	}
	if len(e.readings) >= maxReadingCache {
		clear(e.readings)
	}
	e.readings[id] = out
	return out
}

// logProb maps a corpus weight onto the score scale: ln(weight / reference),
// so the most common words sit near 0 and everything else is negative.
//
// The sign is the point. Scoring a multi-word candidate means adding its
// words' scores together, and that is only meaningful if each word COSTS
// something — with log-counts, chopping a sentence into more pieces would
// always score higher, which is exactly the failure this replaced.
//
// The +1 keeps weight 0 finite rather than -Inf.
func (e *Engine) logProb(w uint32) float64 {
	return math.Log(float64(w)+1) - e.logRef
}

// accumulator dedupes candidates by word, keeping the best-scoring reading.
//
// The same word is reachable several ways — as an exact parse and again as a
// fuzzy or abbreviated one — and showing it twice in the list is the kind of
// thing users read as a bug.
type accumulator struct {
	idx  map[string]int
	list []Candidate
}

func newAccumulator() accumulator {
	return accumulator{idx: make(map[string]int, 128), list: make([]Candidate, 0, 128)}
}

func (a *accumulator) add(c Candidate) {
	if i, ok := a.idx[c.Word]; ok {
		if c.Score > a.list[i].Score {
			a.list[i] = c
		}
		return
	}
	a.idx[c.Word] = len(a.list)
	a.list = append(a.list, c)
}

func (a *accumulator) sorted() []Candidate {
	sort.SliceStable(a.list, func(i, j int) bool {
		if a.list[i].Score != a.list[j].Score {
			return a.list[i].Score > a.list[j].Score
		}
		// Stable tiebreak so the list does not shuffle between keystrokes
		// that produce equal scores.
		return a.list[i].Word < a.list[j].Word
	})
	return a.list
}
