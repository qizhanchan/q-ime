package main

import (
	"testing"

	"github.com/qizhanchan/q-ime/internal/engine"
	"github.com/qizhanchan/q-ime/internal/pinyin"
)

// BenchmarkContinuations times the cold-start continuation lookup — the
// engine work behind every keystroke typed right after a Chinese commit,
// which the session runs up to twice (last word + longest run).
//
// This is the number the trie-suffix rewrite exists to move: the original
// implementation re-ranked the ENTIRE combined reading from scratch
// (600µs–1ms per call, ~20x an ordinary query), because it was implemented
// as a full Candidates() call on prevReading + input.
func BenchmarkContinuations(b *testing.B) {
	d, err := openLexicon()
	if err != nil {
		b.Skipf("no compiled lexicon: %v", err)
	}
	defer d.Close()
	e := engine.New(d, nil, pinyin.Fuzzy{})

	cases := []struct {
		word    string
		reading []string
		input   string
	}{
		{"博物", []string{"bo", "wu"}, "guan"},
		{"吃饭", []string{"chi", "fan"}, "l"},
		{"八达", []string{"ba", "da"}, "l"},
		{"西", []string{"xi"}, "an"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := cases[i%len(cases)]
		e.Continuations(c.word, c.reading, c.input, 8)
	}
}
