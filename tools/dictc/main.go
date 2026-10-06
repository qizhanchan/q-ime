// Command dictc compiles rime-ice dictionaries into q-ime's binary lexicon.
//
//	go run ./tools/dictc -out build/lexicon.bin
//
// Runs offline, once, at build time. Nothing here ships in the input method:
// the app only ever mmaps the output.
//
// # What it reads
//
// rime-ice ships several dictionaries in two shapes, both tab-separated:
//
//	word \t pinyin with spaces \t weight     (base, ext, 8105, others)
//	word \t pinyin                           (41448 — no weight)
//	word \t weight                           (tencent — no pinyin)
//
// The last shape is the interesting one. tencent.dict.yaml is ~980k entries
// of names, brands and places that no hand-curated list has, but it is
// unannotated, so its readings must be derived from the character table —
// which is what Rime itself does at build time. See annotate.
//
// One source lives outside the checkout: -lite points at
// third_party/tencent_lite.dict.yaml, a refined subset of the tencent list
// committed to this repository. See tencentLite for why it is a re-ranking
// rather than added coverage.
//
// # Why the sources are weighted against each other
//
// The files use incompatible weight scales, and two of them have no scale at
// all: base spans 1 to 19 million, while ext and tencent are uniformly 100.
// Merged raw, a million weightless tencent entries would rank alongside the
// most common words in the language.
//
// So each source is rescaled onto a shared range before merging. The scaling
// is LINEAR — a constant offset in the log domain the engine scores in — so
// that a source's own frequency ratios survive intact; those ratios are the
// most valuable thing these files contain. A source with no spread at all is
// detected and parked at one modest level rather than being given an order it
// does not have. A per-source trust multiplier then says how loudly each
// source is allowed to speak.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/qizhanchan/q-ime/internal/dict"
)

// source is one input dictionary and how much it is trusted.
//
// Order matters only for reporting; merging keeps the highest weight for a
// duplicate, and the trust multiplier is what actually decides which source
// wins a collision.
type source struct {
	// file is joined with -src unless it is absolute, which is how a
	// dictionary living outside the rime-ice checkout (tencent_lite) joins the
	// same pipeline.
	file string
	// trust scales this source's normalized weights. base and 8105 are
	// curated and get full trust; tencent is bulk web data — great coverage,
	// unreliable frequencies — so it is deliberately quieter.
	trust float64
	// annotate says the file has no pinyin column and readings must be
	// derived from the character table.
	annotate bool
	// flatLevel overrides flatSourceLevel for a source whose weights carry no
	// information. Zero means the default. This is the only knob that can
	// separate two weightless sources from each other — see tencentLite.
	flatLevel float64
	// optional says a missing file is a note, not an error. Only the
	// out-of-checkout sources are optional; a missing rime-ice dictionary
	// means the checkout is wrong and should fail loudly.
	optional bool
}

// maxScaled is the top of the normalized weight range — what the single most
// common word in a fully-trusted source scores. Recorded in the compiled file
// so the engine can turn weights back into log-probabilities against the same
// scale; see dict.Builder.SetRefWeight.
const maxScaled = 4e6

var sources = []source{
	{file: "cn_dicts/8105.dict.yaml", trust: 1.00},
	{file: "cn_dicts/41448.dict.yaml", trust: 0.55},
	{file: "cn_dicts/base.dict.yaml", trust: 1.00},
	{file: "cn_dicts/ext.dict.yaml", trust: 0.70},
	{file: "cn_dicts/others.dict.yaml", trust: 0.80},
	{file: "cn_dicts/tencent.dict.yaml", trust: 0.42, annotate: true},
}

// tencentLite is the second half of the tencent story.
//
// tencent.dict.yaml is ~980k entries at a uniform weight of 100, so it carries
// no order of its own and lands wholesale on flatSourceLevel. That is the
// right call for a source that mixes 中国国家足球队 with 味道也很不错 — roughly
// a sixth of it is real lexical items and the rest is sentence fragments
// harvested from web text, and nothing in the file says which is which.
//
// third_party/tencent_lite.dict.yaml is exactly that verdict: ~167k entries, a
// semantically-refined subset of the same list. Against the bulk file it is NOT
// a coverage drop-in (it contributes few genuinely new words), it is a
// QUALITY LABEL on a sixth of a source we already ship.
//
// So it is merged as its own source at a higher flat level, fed after the bulk
// file so Builder.Add's keep-the-higher-weight rule promotes the overlap.
// Two things change as a result:
//
//   - the labelled entries outrank their unlabelled neighbours in the lattice
//     instead of tying with them at a single level, and
//   - they survive TrimPostings. On a crowded reading the 96-candidate cap
//     used to fall on whichever tencent entries happened to be inserted last,
//     since they all had the same weight; now the fragments go first.
//
// The level is ext's — the same shelf as rime-ice's own extended word list,
// which is likewise real words with no usable frequency data. Deliberately not
// higher: an LLM's opinion that a string is a word is not evidence about how
// often it is typed, and base/8105 carry that evidence.
var tencentLite = source{
	trust:    0.70,
	annotate: true,
	optional: true,
}

func main() {
	var (
		src     = flag.String("src", "third_party/rime-ice", "rime-ice checkout")
		lite    = flag.String("lite", "third_party/tencent_lite.dict.yaml", "tencent_lite.dict.yaml (optional, outside -src)")
		out     = flag.String("out", "lexicon.bin", "output file")
		maxPost = flag.Int("max-per-key", 96, "cap candidates kept per reading")
		verbose = flag.Bool("v", false, "log per-source detail")
	)
	flag.Parse()

	srcs := sources
	if *lite != "" {
		// The lite file lives outside -src, so resolve it against the working
		// directory now: readSource joins a relative path with -src, which
		// would look for it inside the rime-ice checkout.
		abs, err := filepath.Abs(*lite)
		if err != nil {
			log.Fatalf("dictc: -lite %q: %v", *lite, err)
		}
		s := tencentLite
		s.file = abs
		srcs = append(append([]source{}, srcs...), s)
	}

	c := &compiler{root: *src, sources: srcs, verbose: *verbose}
	if err := c.run(*out, *maxPost); err != nil {
		log.Fatalf("dictc: %v", err)
	}
}

type compiler struct {
	root    string
	sources []source
	verbose bool

	// charReadings maps a single character to its readings, best first.
	// Built from the character tables and used to annotate the unannotated
	// sources.
	charReadings map[rune][]reading

	syllables map[string]bool
}

type reading struct {
	pinyin string
	weight float64
}

// rawEntry is one parsed line before weights are normalized.
type rawEntry struct {
	word   string
	syls   []string
	weight float64
}

func (c *compiler) run(out string, maxPost int) error {
	c.syllables = make(map[string]bool)
	c.charReadings = make(map[rune][]reading)

	// Pass 1: read every source, collect the syllable alphabet and the
	// character readings. Annotation needs the character tables complete
	// before any unannotated source can be processed, so this cannot be
	// folded into the build pass.
	perSource := make([][]rawEntry, len(c.sources))
	for i, s := range c.sources {
		entries, err := c.readSource(s)
		if err != nil {
			if s.optional && os.IsNotExist(err) {
				log.Printf("note: %s not found; skipping", s.file)
				continue
			}
			return fmt.Errorf("%s: %w", s.file, err)
		}
		perSource[i] = entries
		if c.verbose {
			log.Printf("%-28s %7d entries", filepath.Base(s.file), len(entries))
		}
	}

	// Pass 2: annotate the sources that had no pinyin, now that the
	// character tables are known.
	var annotated, unannotatable int
	for i, s := range c.sources {
		if !s.annotate {
			continue
		}
		kept := perSource[i][:0]
		for _, e := range perSource[i] {
			if len(e.syls) > 0 {
				kept = append(kept, e)
				continue
			}
			syls, ok := c.annotate(e.word)
			if !ok {
				unannotatable++
				continue
			}
			e.syls = syls
			kept = append(kept, e)
			annotated++
		}
		perSource[i] = kept
	}
	log.Printf("auto-annotated %d entries; dropped %d with no derivable reading",
		annotated, unannotatable)

	// Pass 3: normalize weights within each source, then merge.
	alphabet := make([]string, 0, len(c.syllables))
	for s := range c.syllables {
		alphabet = append(alphabet, s)
	}
	sort.Strings(alphabet)
	log.Printf("syllable alphabet: %d", len(alphabet))

	b := dict.NewBuilder(alphabet)
	b.SetRefWeight(maxScaled)
	ids := make([]uint16, 0, dict.MaxSyllables)
	var added, skipped int
	for i, s := range c.sources {
		entries := perSource[i]
		note := normalizeWeights(entries, s.trust, s.flatLevel)
		if c.verbose {
			log.Printf("  %-24s %s", filepath.Base(s.file), note)
		}
		for _, e := range entries {
			ids = ids[:0]
			ok := true
			for _, sy := range e.syls {
				id, found := b.SyllableID(sy)
				if !found {
					ok = false
					break
				}
				ids = append(ids, id)
			}
			if !ok || len(ids) == 0 || len(ids) > dict.MaxSyllables {
				skipped++
				continue
			}
			b.Add(ids, e.word, uint32(e.weight))
			added++
		}
		if c.verbose {
			nodes, ents := b.Stats()
			log.Printf("  after %-22s nodes=%-9d entries=%d",
				filepath.Base(s.file), nodes, ents)
		}
	}
	log.Printf("added %d entries, skipped %d", added, skipped)

	if dropped := b.TrimPostings(maxPost); dropped > 0 {
		log.Printf("trimmed %d entries beyond %d per reading", dropped, maxPost)
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := b.WriteFile(out); err != nil {
		return err
	}
	fi, err := os.Stat(out)
	if err != nil {
		return err
	}
	nodes, ents := b.Stats()
	log.Printf("wrote %s: %d nodes, %d entries, %.1f MB",
		out, nodes, ents, float64(fi.Size())/(1<<20))
	return nil
}

// readSource parses one dictionary file.
//
// Rime dictionaries are YAML only in their header; everything after the "..."
// terminator is tab-separated text. Parsing that by hand rather than pulling
// in a YAML library is both faster over ~50MB and immune to the header's
// occasional oddities, since the header is not what we want anyway.
func (c *compiler) readSource(s source) ([]rawEntry, error) {
	path := s.file
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.root, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)

	var entries []rawEntry
	inBody := false
	for sc.Scan() {
		line := sc.Text()
		if !inBody {
			// The header ends at "..."; until then nothing is an entry.
			if strings.TrimSpace(line) == "..." {
				inBody = true
			}
			continue
		}
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		word := strings.TrimSpace(fields[0])
		if word == "" {
			continue
		}

		var syls []string
		var weight float64
		if len(fields) >= 3 {
			syls = strings.Fields(fields[1])
			weight = parseWeight(fields[2])
		} else {
			// Two fields: either "word \t weight" (unannotated) or
			// "word \t pinyin" (weight omitted).
			//
			// Tell them apart by SHAPE, not by whether ParseFloat succeeds.
			// ParseFloat accepts "nan" and "inf" — and "nan" is a pinyin
			// syllable (南, 难, 男). Trusting it here silently turned every
			// nan-reading character into a NaN weight, which then made the
			// percentile comparison in normalizeWeights false for every
			// pair (NaN compares false to everything) and disabled the
			// flat-source branch entirely.
			if isNumeric(fields[1]) {
				weight = parseWeight(fields[1])
			} else {
				syls = strings.Fields(fields[1])
				weight = 1
			}
		}
		if weight <= 0 {
			weight = 1
		}

		for _, sy := range syls {
			c.syllables[sy] = true
		}
		// Single characters feed the reading table used for annotation.
		if r := []rune(word); len(r) == 1 && len(syls) == 1 {
			c.charReadings[r[0]] = append(c.charReadings[r[0]], reading{syls[0], weight})
		}
		entries = append(entries, rawEntry{word: word, syls: syls, weight: weight})
	}
	return entries, sc.Err()
}

// isNumeric reports whether s is a plain decimal number — digits with at
// most one dot. Deliberately stricter than strconv: it must reject the
// alphabetic spellings ParseFloat honours ("nan", "inf"), which collide with
// real pinyin.
func isNumeric(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	dots := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
		case c == '.':
			dots++
			if dots > 1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func parseWeight(s string) float64 {
	w, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(w) || math.IsInf(w, 0) {
		return 1
	}
	return w
}

// annotate derives a reading for a word that arrived without one.
//
// One reading per character, taking each character's most common. Words
// containing a character with no known reading, or with a genuinely
// ambiguous one, are dropped rather than guessed: a wrong reading is worse
// than a missing entry, because it makes the word appear under something the
// user did not type while still not appearing under what they did.
func (c *compiler) annotate(word string) ([]string, bool) {
	runes := []rune(word)
	if len(runes) == 0 || len(runes) > dict.MaxSyllables {
		return nil, false
	}
	syls := make([]string, 0, len(runes))
	for _, r := range runes {
		rs := c.charReadings[r]
		if len(rs) == 0 {
			return nil, false
		}
		best, total := rs[0], 0.0
		for _, x := range rs {
			total += x.weight
			if x.weight > best.weight {
				best = x
			}
		}
		// Rime's own rule of thumb: a secondary reading worth more than a
		// few percent of the total means the character is genuinely
		// ambiguous in context, and picking one is a coin flip.
		if total > 0 && best.weight/total < 0.80 {
			return nil, false
		}
		syls = append(syls, best.pinyin)
	}
	return syls, true
}

// flatSourceLevel is where a source whose weights carry no information is
// placed on the shared scale — low enough that its entries surface only when
// nothing better matches, high enough that they do surface.
//
// tencent.dict.yaml is the case: ~980k entries, every one of them weight
// 100. It is a coverage backstop (names, brands, places nothing else has),
// not a frequency table, and pretending otherwise would let a million
// uniformly-weighted entries elbow into lists where they do not belong.
const flatSourceLevel = 0.002

// normalizeWeights rewrites a source's weights onto the shared scale.
//
// LINEAR scaling, which is a constant offset in the log domain the engine
// scores in — so a source's own frequency distribution survives intact. That
// distribution is the most valuable thing these files contain: 我们 outweighs
// 我么 by a large ratio, and it is exactly that ratio which decides the two
// candidates. Rank-based normalization would throw the ratios away.
//
// The scale factor is set from a high percentile rather than the maximum, so
// one outlier entry cannot compress the rest of the source into the floor.
func normalizeWeights(entries []rawEntry, trust, flatLevel float64) (note string) {
	if len(entries) == 0 {
		return "empty"
	}
	if flatLevel <= 0 {
		flatLevel = flatSourceLevel
	}
	ws := make([]float64, len(entries))
	for i, e := range entries {
		ws[i] = e.weight
	}
	sort.Float64s(ws)
	hi := ws[int(float64(len(ws))*0.999)]
	lo := ws[0]

	if hi <= lo {
		// No spread: the weights say nothing. Put the whole source at one
		// modest level rather than inventing an order it does not have.
		level := math.Max(1, trust*maxScaled*flatLevel)
		for i := range entries {
			entries[i].weight = level
		}
		return fmt.Sprintf("flat (no weight spread), all at %.0f", level)
	}

	scale := trust * maxScaled / hi
	for i := range entries {
		entries[i].weight = math.Max(1, math.Min(entries[i].weight*scale, math.MaxUint32))
	}
	return fmt.Sprintf("scaled x%.4g (p99.9 was %.0f)", scale, hi)
}
