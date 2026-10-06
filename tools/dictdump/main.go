// Command dictdump prints every entry in a compiled lexicon, one per line:
//
//	读音'带'撇	词	权重
//
//	go run ./tools/dictdump build/lexicon.bin | sort > /tmp/a.tsv
//
// # Why this exists
//
// Every other tool here answers "what does the engine do with this input".
// This one answers "what is actually IN the file", which is a different
// question and the only way to check a claim about a dictionary CHANGE — for
// example that adding tencent_lite only promotes entries (weights up, none
// down, none evicted) rather than displacing neighbours past TrimPostings.
//
// Output is deliberately dumb and line-oriented: the interesting operations
// are `comm` and `join` against another dump, not anything this program
// should be doing itself.
package main

import (
	"bufio"
	"fmt"
	"os"

	"github.com/qizhanchan/q-ime/internal/dict"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: dictdump <lexicon.bin>")
		os.Exit(2)
	}
	d, err := dict.Open(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "dictdump: %v\n", err)
		os.Exit(1)
	}
	defer d.Close()

	w := bufio.NewWriterSize(os.Stdout, 1<<20)
	defer w.Flush()

	// Depth-first over the trie, carrying the syllable path that reaches the
	// current node — that path IS the key, and it is not stored anywhere in
	// the file, so it has to be reconstructed on the way down.
	key := make([]uint16, 0, dict.MaxSyllables)
	var walk func(n dict.Node)
	walk = func(n dict.Node) {
		for i, c := 0, d.PostCount(n); i < c; i++ {
			p := d.Post(n, i)
			for j, s := range key {
				if j > 0 {
					w.WriteByte('\'')
				}
				w.WriteString(d.Syllable(s))
			}
			fmt.Fprintf(w, "\t%s\t%d\n", p.Word, p.Weight)
		}
		d.ChildrenInRange(n, 0, 65535, func(syl uint16, child dict.Node) bool {
			key = append(key, syl)
			walk(child)
			key = key[:len(key)-1]
			return true
		})
	}
	walk(d.Root())
}
