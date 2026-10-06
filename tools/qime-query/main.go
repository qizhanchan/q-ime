// Command qime-query runs the input method's engine from a shell.
//
//	go run ./tools/qime-query -dict build/lexicon.bin nihao zhg womenmingtianqubeijing
//
// The engine is the half of q-ime that decides whether it is pleasant to
// type with, and it is also the half that cannot be judged from a unit test
// alone — ranking is a matter of taste as much as correctness. This makes
// that half inspectable without installing an input method, logging out, and
// typing into a text field.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qizhanchan/q-ime/internal/dict"
	"github.com/qizhanchan/q-ime/internal/emoji"
	"github.com/qizhanchan/q-ime/internal/engine"
	"github.com/qizhanchan/q-ime/internal/english"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// segment renders what the preedit would show: the TYPED text with a
// separator at each syllable boundary, never a character the user did not
// type. Mirrors imeSession.preedit so the engine's spans can be checked
// without installing an input method.
func segment(typed string, spans []int) string {
	if len(spans) == 0 {
		return typed
	}
	var b strings.Builder
	prev := 0
	for _, end := range spans {
		if end <= prev || end > len(typed) {
			continue
		}
		if prev > 0 {
			b.WriteByte('\'')
		}
		b.WriteString(typed[prev:end])
		prev = end
	}
	if prev < len(typed) {
		if prev > 0 {
			b.WriteByte('\'')
		}
		b.WriteString(typed[prev:])
	}
	return b.String()
}

func main() {
	var (
		path    = flag.String("dict", "lexicon.bin", "compiled lexicon")
		n       = flag.Int("n", 12, "candidates to show")
		fuzzy   = flag.Bool("fuzzy", false, "enable the common fuzzy rules")
		bench   = flag.Int("bench", 0, "repeat each query N times and report timing")
		cpu     = flag.String("cpuprofile", "", "write a CPU profile here")
		user    = flag.String("user", "", `learned-words file ("real" for the installed one)`)
		context = flag.String("context", "", "the word committed just before, for the bigram ranking")
		noEn    = flag.Bool("noen", false, "leave the English word list out")
		noEmoji = flag.Bool("noemoji", false, "keep emoji out of the ordinary candidate list")
		vmode   = flag.Bool("v", false, "explicit emoji mode: query as if typed after a leading v")
		typo    = flag.Bool("typo", true, "correct one adjacent-letter transposition per syllable")
	)
	flag.Parse()
	defer startProfile(*cpu)()

	d, err := dict.Open(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qime-query: %v\n", err)
		os.Exit(1)
	}
	defer d.Close()
	nodes, entries, bytes := d.Stats()
	fmt.Fprintf(os.Stderr, "lexicon: %d nodes, %d entries, %.1f MB, %d syllables\n",
		nodes, entries, float64(bytes)/(1<<20), len(d.Syllables()))

	var f pinyin.Fuzzy
	if *fuzzy {
		f = pinyin.Fuzzy{ZhZ: true, ChC: true, ShS: true, AngAn: true, EngEn: true, IngIn: true}
	}
	var u *engine.UserDict
	if *user != "" {
		path := *user
		if path == "real" {
			path = engine.DefaultUserDictPath()
		}
		u = engine.NewUserDict(path)
		fmt.Fprintf(os.Stderr, "user dictionary: %s (%d entries)\n", path, u.Len())
	}
	e := engine.New(d, u, f)
	e.SetTypoCorrection(*typo)
	if *context != "" {
		// Stands in for "the user just committed this word", which is the only
		// way to see the context ranking from outside the running input method.
		e.SetContext(*context)
		fmt.Fprintf(os.Stderr, "context: %q\n", *context)
	}
	if !*noEn {
		en, err := english.Builtin()
		if err != nil {
			fmt.Fprintf(os.Stderr, "qime-query: english: %v\n", err)
			os.Exit(1)
		}
		e.SetEnglish(en)
		fmt.Fprintf(os.Stderr, "english: %d codes\n", en.Len())
	}
	if em, err := emoji.Builtin(); err == nil {
		e.SetEmoji(em, !*noEmoji)
		fmt.Fprintf(os.Stderr, "emoji: %d words (inline=%v)\n", em.Len(), !*noEmoji)
	}

	run := func(q string) {
		start := time.Now()
		var cands []engine.Candidate
		if *vmode {
			cands = e.EmojiWords(q, *n)
		} else {
			cands = e.Candidates(q, *n)
		}
		el := time.Since(start)
		if *bench > 0 {
			for i := 0; i < *bench; i++ {
				e.Candidates(q, *n)
			}
			el = time.Since(start) / time.Duration(*bench+1)
		}
		// Coverage is reported against the SEPARATOR-STRIPPED input, which is
		// what the engine measures against; using the raw length would make
		// "xi'an" look like it only covered 4 of 5.
		clean := strings.ReplaceAll(q, "'", "")
		fmt.Printf("\n%-24s  %v\n", q, el.Round(time.Microsecond))
		for i, c := range cands {
			mark := ""
			switch c.Source {
			case engine.SourceSentence:
				mark = " [sentence: " + strings.Join(c.Words, "+") + "]"
			case engine.SourceUser:
				mark = " [user]"
			case engine.SourceContext:
				mark = " [context]"
			case engine.SourceEnglish:
				mark = " [en]"
			case engine.SourceEmoji:
				mark = " [emoji]"
			}
			fmt.Printf("  %2d. %-16s %-24s preedit=%-16s cover=%d/%d score=%.2f%s\n",
				i+1, c.Word, strings.Join(c.Reading, "'"),
				segment(clean, c.Spans), c.Consumed, len(clean), c.Score, mark)
		}
	}

	if flag.NArg() > 0 {
		for _, q := range flag.Args() {
			run(q)
		}
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	fmt.Fprintln(os.Stderr, "reading queries from stdin, one per line")
	for sc.Scan() {
		if q := strings.TrimSpace(sc.Text()); q != "" {
			run(q)
		}
	}
}
