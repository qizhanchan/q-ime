// Package golden is the pinned ranking regressions: a list of inputs whose
// top candidate (or whose presence on the panel) is a decision somebody made
// on purpose, kept as data so every change to the constants, the search or
// the lexicon build can be checked against ALL of them in one run.
//
// A case that catches a regression once earns a permanent place here: the file
// is append-mostly, and shrinking it needs the same kind of justification as
// weakening a test.
//
// The runner is a library because it has two callers with different jobs:
// qime-audit -mode golden is the tool a human runs before shipping a ranking
// change, and the go test in the app package is the machine running the same
// file on every `go test .` where the lexicon exists.
package golden

import (
	"fmt"
	"strings"

	"github.com/qizhanchan/q-ime/internal/dict"
	"github.com/qizhanchan/q-ime/internal/engine"
	"github.com/qizhanchan/q-ime/internal/english"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// Case is one pinned expectation.
type Case struct {
	Input string
	Want  string
	// Top says the word must be the FIRST candidate — the one Space commits.
	// False means "has": the word must appear in the list the panel is given
	// (the engine is asked for PanelLimit, matching the session's request),
	// which is the promise behind reserved-slot machinery like
	// truncateKeepingEnglish.
	Top bool
	// Lite marks an expectation that only holds when the lexicon was built
	// with the optional tencent_lite source. Such cases are skipped when the
	// lexicon was built without it — see HasTencentLite.
	Lite bool
	Line int // 1-based line in the source file, for error messages
}

// PanelLimit is how many candidates a "has" case searches — the same number
// the session asks the engine for, so "has" means "reachable from the panel".
const PanelLimit = 60

// Parse reads the golden file format: one case per line,
//
//	input<TAB>want[<TAB>has][<TAB>lite]   [# comment]
//
// Blank lines and lines starting with # are skipped. After the first two
// columns, the literals "has" and "lite" are recognized in any order;
// anything else is an error rather than being silently ignored, so a typo
// cannot quietly weaken or drop a case.
func Parse(data []byte) ([]Case, error) {
	var out []Case
	for i, line := range strings.Split(string(data), "\n") {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		// Collapse empty columns so aligning the file with extra tabs is legal.
		fields := cols[:0]
		for _, c := range cols {
			if c = strings.TrimSpace(c); c != "" {
				fields = append(fields, c)
			}
		}
		if len(fields) < 2 {
			return nil, fmt.Errorf("line %d: %d columns, want at least 2", i+1, len(fields))
		}
		c := Case{Line: i + 1, Top: true, Input: fields[0], Want: fields[1]}
		for _, f := range fields[2:] {
			switch f {
			case "has":
				c.Top = false
			case "lite":
				c.Lite = true
			default:
				return nil, fmt.Errorf("line %d: unexpected column %q, want \"has\" or \"lite\"", i+1, f)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// liteProbe is a word that only the optional tencent_lite source promotes
// high enough to survive TrimPostings: without lite it is absent from the
// lexicon entirely. It is the marker for whether a lexicon was built with
// lite, and therefore whether the cases marked "lite" can be checked.
var liteProbe = struct {
	reading []string
	word    string
}{
	reading: []string{"tu", "ba", "tu", "zhuang", "xiu"},
	word:    "土巴兔装修",
}

// HasTencentLite reports whether d was built with the optional tencent_lite
// source, by looking for a word only that source lifts past the per-reading
// posting cap. This is what lets a no-lite build skip the lite expectations
// instead of failing them.
func HasTencentLite(d *dict.Reader) bool {
	ids := make([]uint16, 0, len(liteProbe.reading))
	for _, s := range liteProbe.reading {
		id, ok := d.SyllableID(s)
		if !ok {
			return false
		}
		ids = append(ids, id)
	}
	for _, c := range d.Lookup(ids, 0) {
		if c.Word == liteProbe.word {
			return true
		}
	}
	return false
}

// Failure is one case the engine no longer satisfies, with enough of the
// actual list to see what happened without re-running anything.
type Failure struct {
	Case Case
	Got  []string // the head of the actual candidate list
}

func (f Failure) String() string {
	kind := "top"
	if !f.Case.Top {
		kind = "has"
	}
	return fmt.Sprintf("line %d: %-24s want %s %-12s got %v",
		f.Case.Line, f.Case.Input, kind, f.Case.Want, f.Got)
}

// Run checks every case and returns the ones that fail, in file order, along
// with how many lite-only cases were skipped because the lexicon has no
// tencent_lite data.
func Run(e *engine.Engine, cases []Case, hasLite bool) (failures []Failure, skipped int) {
	var out []Failure
	for _, c := range cases {
		if c.Lite && !hasLite {
			skipped++
			continue
		}
		if c.Top {
			got := e.Candidates(c.Input, 5)
			if len(got) > 0 && got[0].Word == c.Want {
				continue
			}
			out = append(out, Failure{Case: c, Got: words(got)})
			continue
		}
		got := e.Candidates(c.Input, PanelLimit)
		found := false
		for _, cand := range got {
			if cand.Word == c.Want {
				found = true
				break
			}
		}
		if !found {
			out = append(out, Failure{Case: c, Got: words(got[:min(8, len(got))])})
		}
	}
	return out, skipped
}

func words(cs []engine.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Word
	}
	return out
}

// NewEngine builds the engine the golden file is measured against: the
// shipped defaults — typo correction on, fuzzy off, English list in, no
// emoji, and NO user dictionary. Learned words are per-person and would make
// every expectation unreproducible; what this file pins is the engine every
// new user gets.
func NewEngine(d *dict.Reader) (*engine.Engine, error) {
	e := engine.New(d, nil, pinyin.Fuzzy{})
	e.SetTypoCorrection(true)
	en, err := english.Builtin()
	if err != nil {
		return nil, fmt.Errorf("english list: %w", err)
	}
	e.SetEnglish(en)
	return e, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
