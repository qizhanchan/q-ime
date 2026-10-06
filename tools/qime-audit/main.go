// Command qime-audit measures what typo correction costs and what it buys,
// against the real lexicon.
//
//	go build -o /tmp/qime-audit ./tools/qime-audit
//	/tmp/qime-audit -dict build/lexicon.bin
//
// Three measurements, and they pull in different directions:
//
//   - HARM — of the N commonest readings, how many get a different first
//     candidate once correction is turned on. This must be ZERO. Correction
//     that quietly re-reads input the user typed correctly is not a feature,
//     it is the input method disagreeing with the keyboard.
//   - RECOVERY — of the same readings, perturbed by one slip each, how many
//     put the original word back on top.
//   - COVERAGE (-mode coverage) — cases where a reading that explains only
//     part of the input outranks one that explains all of it.
//
// It is not a `go test` because it needs the GPL lexicon, which is not in the
// repo: a test that skips on every machine but one is not a test. It is what
// lets a number in a comment be re-derived after a rebuild.
package main

import (
	"container/heap"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/qizhanchan/q-ime/internal/dict"
	"github.com/qizhanchan/q-ime/internal/engine"
	"github.com/qizhanchan/q-ime/internal/english"
	"github.com/qizhanchan/q-ime/internal/golden"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

func main() {
	var (
		path    = flag.String("dict", "build/lexicon.bin", "compiled lexicon")
		top     = flag.Int("n", 2880, "how many of the commonest readings to audit")
		mode    = flag.String("mode", "typo", "typo | coverage | golden")
		edit    = flag.String("edit", "transpose", "perturbation: transpose")
		goldens = flag.String("golden", "testdata/golden.tsv", "pinned-rankings file for -mode golden")
		noEn    = flag.Bool("noen", false, "leave the English word list out")
		fuzzy   = flag.Bool("fuzzy", false, "enable the common fuzzy rules")
		verbose = flag.Bool("v", false, "list every harmed reading and every miss")
		misses  = flag.Int("misses", 20, "how many misses to show without -v")
	)
	flag.Parse()

	d, err := dict.Open(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer d.Close()

	fmt.Printf("lexicon: %s\n", *path)

	// The golden mode is the one to run before shipping ANY ranking change: it
	// replays every pinned expectation in the file. It exits non-zero on a
	// failure so it can gate a script, and the same file runs as a go test on
	// machines that have the lexicon (see the app package's golden test).
	if *mode == "golden" {
		os.Exit(goldenAudit(d, *goldens))
	}

	perturb, ok := perturbations[*edit]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown -edit %q; have: %s\n", *edit, editNames())
		os.Exit(2)
	}

	readings := topReadings(d, *top)

	if *mode == "coverage" {
		coverageAudit(newEngine(d, *fuzzy, true, !*noEn), readings, *verbose, *misses)
		return
	}
	fmt.Printf("auditing %d readings, perturbation=%s\n\n", len(readings), *edit)

	// Two engines rather than one with the flag flipped between queries: the
	// reading cache and the scratch buffers are per-engine, and a shared engine
	// would have the off-run warming the on-run's caches. Cheap enough — the
	// mmap'd lexicon is shared, which is the only large thing here.
	off := newEngine(d, *fuzzy, false, !*noEn)
	on := newEngine(d, *fuzzy, true, !*noEn)

	var harmed []harm
	for _, r := range readings {
		a, aok := topWord(off, r.typed)
		b, bok := topWord(on, r.typed)
		if aok != bok || a != b {
			harmed = append(harmed, harm{typed: r.typed, was: a, now: b})
		}
	}

	// Split by whether the slip lands INSIDE one syllable or straddles two.
	//
	// Without the split the headline number is meaningless. A correction that
	// works on syllable-internal arcs cannot possibly repair a slip that swapped
	// the last letter of one syllable with the first of the next — no single
	// chunk contains the fix. Averaging the two together reports a low
	// percentage that no change to the engine would move, which is the shape of
	// a metric people learn to ignore.
	var in, out counter
	var readingsHit int
	var missed []miss
	for _, r := range readings {
		variants := perturb(r.typed)
		if len(variants) == 0 {
			continue
		}
		hit := 0
		for _, v := range variants {
			c := &in
			if !r.within(v.at, v.width) {
				c = &out
			}
			c.total++
			w, _ := topWord(on, v.text)
			if w == r.word {
				c.ok++
				hit++
				continue
			}
			if c == &in && len(missed) < 1<<16 {
				// Only in-syllable misses are worth listing: the others are
				// known-unreachable, and burying the real ones under them is
				// how a list stops being read.
				missed = append(missed, miss{typed: r.typed, as: v.text, want: r.word, got: w})
			}
		}
		if hit > 0 {
			readingsHit++
		}
	}

	fmt.Printf("HARM      first candidate changed by correction: %d / %d\n",
		len(harmed), len(readings))
	fmt.Printf("RECOVERY  slip inside one syllable:   %5d / %5d (%.1f%%)   ← what the arcs can reach\n",
		in.ok, in.total, pct(in.ok, in.total))
	fmt.Printf("          slip across a boundary:     %5d / %5d (%.1f%%)   ← out of scope by construction\n",
		out.ok, out.total, pct(out.ok, out.total))
	fmt.Printf("          readings recoverable from at least one slip: %d / %d (%.1f%%)\n",
		readingsHit, len(readings), pct(readingsHit, len(readings)))

	if len(harmed) > 0 {
		fmt.Printf("\nharmed (this number is supposed to be zero):\n")
		for i, h := range harmed {
			if !*verbose && i >= *misses {
				fmt.Printf("  … and %d more (-v for all)\n", len(harmed)-i)
				break
			}
			fmt.Printf("  %-16s %s → %s\n", h.typed, h.was, h.now)
		}
	}

	// The misses are the interesting half of the recovery number: many turn
	// out not to be failures at all — the mistyped string is itself another
	// legal, common reading, and the engine preferring what was actually typed
	// is the guard working. Anyone widening the edit set needs to read this
	// list, not just the percentage.
	if len(missed) > 0 {
		fmt.Printf("\nmisses (%d), the ones worth reading by hand:\n", len(missed))
		for i, m := range missed {
			if !*verbose && i >= *misses {
				fmt.Printf("  … and %d more (-v for all)\n", len(missed)-i)
				break
			}
			fmt.Printf("  %-16s typed as %-16s want %-8s got %s\n", m.typed, m.as, m.want, m.got)
		}
	}
}

// coverageAudit measures the case a user hits every time they abbreviate the
// last word of a phrase: a finished reading plus one more onset letter.
//
// "lihai" is 厉害; "lihail" is 厉害了 — or it is 厉害 with a letter the engine
// declined to explain. Both are legal readings, and which one leads is decided
// by coverBonus against the corpus frequency gap, so this reports the gap
// itself rather than a verdict.
//
// Why the DEFICIT and not just a pass rate: coverBonus multiplies coverage, so
// raising it by δ moves every one of these comparisons by exactly δ — the two
// candidates differ by exactly one byte of input by construction. One run
// therefore predicts the effect of any δ, which beats rebuilding the binary
// once per guess.
func coverageAudit(e *engine.Engine, readings []reading, verbose bool, show int) {
	initials := pinyin.Initials
	fmt.Printf("auditing %d readings × %d trailing onsets\n\n", len(readings), len(initials))

	var pairs, explained int
	var deficits []float64
	var losers []coverLoss
	for _, r := range readings {
		for _, c := range initials {
			input := r.typed + c
			cs := e.Candidates(input, 30)
			full, partial := -1, -1
			for i, cand := range cs {
				if cand.Consumed >= len(input) {
					if full < 0 {
						full = i
					}
				} else if partial < 0 {
					partial = i
				}
				if full >= 0 && partial >= 0 {
					break
				}
			}
			if full < 0 || partial < 0 {
				continue // nothing to compare: one of the two readings does not exist
			}
			pairs++
			if full < partial {
				explained++
				continue
			}
			d := cs[partial].Score - cs[full].Score
			deficits = append(deficits, d)
			losers = append(losers, coverLoss{
				typed: input, won: cs[partial].Word, lost: cs[full].Word, deficit: d,
			})
		}
	}

	fmt.Printf("COVERAGE  the candidate explaining every letter leads: %d / %d (%.1f%%)\n",
		explained, pairs, pct(explained, pairs))
	if len(deficits) == 0 {
		return
	}
	sort.Float64s(deficits)
	fmt.Printf("          when it does not, it trails by: median %.2f, p90 %.2f, max %.2f\n",
		deficits[len(deficits)/2], deficits[len(deficits)*9/10], deficits[len(deficits)-1])
	fmt.Println("\nraising coverBonus by δ would flip:")
	for _, delta := range []float64{0.5, 1.0, 1.5, 2.0, 3.0} {
		flipped := sort.SearchFloat64s(deficits, delta)
		fmt.Printf("  +%.1f → %d more (%.1f%% total)\n",
			delta, flipped, pct(explained+flipped, pairs))
	}

	sort.Slice(losers, func(i, j int) bool { return losers[i].deficit < losers[j].deficit })
	fmt.Printf("\nnearest misses (%d total), the ones a small δ would move:\n", len(losers))
	for i, l := range losers {
		if !verbose && i >= show {
			fmt.Printf("  … and %d more (-v for all)\n", len(losers)-i)
			break
		}
		fmt.Printf("  %-14s %-8s beat %-10s by %.2f\n", l.typed, l.won, l.lost, l.deficit)
	}
}

// goldenAudit replays the pinned-rankings file and reports what broke.
// Returns the process exit code: 0 all pinned, 1 regressions, 2 setup error.
func goldenAudit(d *dict.Reader, path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cases, err := golden.Parse(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		return 2
	}
	e, err := golden.NewEngine(d)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	hasLite := golden.HasTencentLite(d)
	failures, skipped := golden.Run(e, cases, hasLite)
	total := len(cases) - skipped
	fmt.Printf("GOLDEN    pinned rankings hold: %d / %d", total-len(failures), total)
	if skipped > 0 {
		fmt.Printf(" (%d lite-only cases skipped: lexicon built without tencent_lite)", skipped)
	}
	fmt.Println()
	if len(failures) == 0 {
		return 0
	}
	fmt.Printf("\nregressions (each of these lines was pinned on purpose — see %s):\n", path)
	for _, f := range failures {
		fmt.Printf("  %s\n", f)
	}
	return 1
}

type coverLoss struct {
	typed, won, lost string
	deficit          float64
}

type harm struct{ typed, was, now string }
type miss struct{ typed, as, want, got string }
type counter struct{ ok, total int }

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func newEngine(d *dict.Reader, fuzzy, typo, withEnglish bool) *engine.Engine {
	f := pinyin.Fuzzy{}
	if fuzzy {
		// The same six rules qime-query calls "common", so the two tools agree
		// about what -fuzzy means.
		f = pinyin.Fuzzy{ZhZ: true, ChC: true, ShS: true, AngAn: true, EngEn: true, IngIn: true}
	}
	// No user dictionary, deliberately: this measures the ENGINE. Learned words
	// are per-person and would make the numbers unreproducible.
	e := engine.New(d, nil, f)
	e.SetTypoCorrection(typo)
	if withEnglish {
		if en, err := english.Builtin(); err == nil {
			e.SetEnglish(en)
		}
	}
	return e
}

func topWord(e *engine.Engine, typed string) (string, bool) {
	cs := e.Candidates(typed, 1)
	if len(cs) == 0 {
		return "", false
	}
	return cs[0].Word, true
}

// variant is one mistyping, and WHERE the slip landed.
//
// The position is not decoration: whether a slip falls inside a syllable or
// across a boundary decides whether any syllable-local arc could ever repair
// it, and that is the difference between a number that measures the engine and
// one that measures the alphabet.
type variant struct {
	text  string
	at    int // input offset the edit starts at
	width int // how many original bytes it covers (2 for a swap, 1 for a substitution)
}

// perturbations is the seam this tool exists for. Adding an edit kind means
// adding a function here and a lattice arc in the engine — and then this
// reports what the pair costs before either ships.
var perturbations = map[string]func(string) []variant{
	"transpose": transposeAll,
	"delete":    deleteAll,
	"double":    doubleAll,
}

func editNames() string {
	names := make([]string, 0, len(perturbations))
	for k := range perturbations {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// transposeAll returns the input with each adjacent pair swapped in turn.
//
// Every position rather than one sampled one: reporting per-SWAP recovery is
// deterministic and stricter — it asks whether the correction works wherever
// the slip lands, not whether it works somewhere.
func transposeAll(s string) []variant {
	var out []variant
	seen := map[string]bool{s: true}
	b := []byte(s)
	for i := 0; i+1 < len(b); i++ {
		if b[i] == b[i+1] {
			continue // swapping a doubled letter changes nothing
		}
		b[i], b[i+1] = b[i+1], b[i]
		v := string(b)
		b[i], b[i+1] = b[i+1], b[i]
		if !seen[v] {
			seen[v] = true
			out = append(out, variant{text: v, at: i, width: 2})
		}
	}
	return out
}

// deleteAll returns the input with each letter dropped in turn — the missing
// keystroke the engine's deletion index exists to repair.
func deleteAll(s string) []variant {
	var out []variant
	seen := map[string]bool{s: true}
	for i := 0; i < len(s); i++ {
		v := s[:i] + s[i+1:]
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, variant{text: v, at: i, width: 1})
		}
	}
	return out
}

// doubleAll returns the input with each letter doubled in turn — the key
// repeat that the extra-letter repair exists for. Doubling rather than
// inserting arbitrary letters because a repeat is the mistyping people
// actually produce; 26 random insertions per position would mostly measure
// strings no finger ever types.
func doubleAll(s string) []variant {
	var out []variant
	seen := map[string]bool{s: true}
	for i := 0; i < len(s); i++ {
		v := s[:i+1] + s[i:]
		if !seen[v] {
			seen[v] = true
			out = append(out, variant{text: v, at: i, width: 1})
		}
	}
	return out
}

// reading is one lexicon entry: the letters a user would type, the word that
// is currently top for them, and where its syllables end.
type reading struct {
	typed  string
	word   string
	weight uint32
	// ends is the offset each syllable finishes at, so a slip can be placed
	// relative to the segmentation the user meant.
	ends []int
}

// within reports whether an edit covering [at, at+width) lies inside a single
// syllable of this reading.
func (r reading) within(at, width int) bool {
	start := 0
	for _, end := range r.ends {
		if at >= start && at+width <= end {
			return true
		}
		start = end
	}
	return false
}

// topReadings walks the whole trie and keeps the n entries with the heaviest
// top posting.
//
// A bounded min-heap rather than sorting 1.9M entries: the walk visits every
// node in a 77MB file, and materializing a string per entry to throw almost
// all of them away is the difference between a tool that runs in seconds and
// one nobody runs.
func topReadings(d *dict.Reader, n int) []reading {
	h := &readingHeap{}
	var syls []string

	var walk func(node dict.Node, depth int)
	walk = func(node dict.Node, depth int) {
		if cnt := d.PostCount(node); cnt > 0 && len(syls) > 0 {
			w := d.PostWeight(node, 0)
			if h.Len() < n || w > (*h)[0].weight {
				top := d.Post(node, 0)
				ends := make([]int, 0, len(syls))
				at := 0
				for _, s := range syls {
					at += len(s)
					ends = append(ends, at)
				}
				heap.Push(h, reading{
					typed:  strings.Join(syls, ""),
					word:   top.Word,
					weight: top.Weight,
					ends:   ends,
				})
				for h.Len() > n {
					heap.Pop(h)
				}
			}
		}
		if depth >= dict.MaxSyllables {
			return
		}
		d.ChildrenInRange(node, 0, ^uint16(0), func(syl uint16, child dict.Node) bool {
			syls = append(syls, d.Syllable(syl))
			walk(child, depth+1)
			syls = syls[:len(syls)-1]
			return true
		})
	}
	walk(d.Root(), 0)

	out := make([]reading, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(h).(reading)
	}
	return out
}

// readingHeap is a MIN-heap on weight, so the cheapest kept entry is the one
// evicted when a better one arrives.
type readingHeap []reading

func (h readingHeap) Len() int           { return len(h) }
func (h readingHeap) Less(i, j int) bool { return h[i].weight < h[j].weight }
func (h readingHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *readingHeap) Push(x any)        { *h = append(*h, x.(reading)) }
func (h *readingHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }
