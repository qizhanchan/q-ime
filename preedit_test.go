package main

import (
	"testing"

	"github.com/qizhanchan/q-ime/internal/engine"
)

// TestPreeditShowsOnlyTypedCharacters is the rule the preedit exists to obey.
//
// The underlined text in the host app is a record of what the user pressed,
// segmented into syllables. It must never spell out the engine's guess:
// typing "h" once showed "huo", because the preedit was built from the
// candidate's READING rather than from the buffer.
func TestPreeditShowsOnlyTypedCharacters(t *testing.T) {
	cases := []struct {
		name   string
		buffer string
		cand   engine.Candidate
		want   string
	}{
		{
			name:   "a bare onset stays a bare onset",
			buffer: "h",
			// The engine read "h" as the syllable huo — the candidate list may
			// say so, the preedit may not.
			cand: engine.Candidate{Word: "或", Consumed: 1, Reading: []string{"huo"}, Spans: []int{1}},
			want: "h",
		},
		{
			name:   "two full syllables get a separator",
			buffer: "nihao",
			cand:   engine.Candidate{Word: "你好", Consumed: 5, Reading: []string{"ni", "hao"}, Spans: []int{2, 5}},
			want:   "ni'hao",
		},
		{
			name:   "a trailing onset is not completed",
			buffer: "nih",
			cand:   engine.Candidate{Word: "你好", Consumed: 3, Reading: []string{"ni", "hao"}, Spans: []int{2, 3}},
			want:   "ni'h",
		},
		{
			name:   "a half-typed syllable is not completed",
			buffer: "niha",
			cand:   engine.Candidate{Word: "你好", Consumed: 4, Reading: []string{"ni", "hao"}, Spans: []int{2, 4}},
			want:   "ni'ha",
		},
		{
			name:   "an explicit separator is preserved",
			buffer: "xi'an",
			cand:   engine.Candidate{Word: "西安", Consumed: 4, Reading: []string{"xi", "an"}, Spans: []int{2, 4}},
			want:   "xi'an",
		},
		{
			name:   "input the candidate does not cover stays visible",
			buffer: "nihaoshijie",
			cand:   engine.Candidate{Word: "你好", Consumed: 5, Reading: []string{"ni", "hao"}, Spans: []int{2, 5}},
			want:   "ni'hao'shijie",
		},
		{
			name:   "a predicted syllable adds nothing",
			buffer: "ni",
			// 你们 predicts "men"; only "ni" was typed, so only "ni" shows.
			cand: engine.Candidate{Word: "你们", Consumed: 2, Reading: []string{"ni", "men"}, Spans: []int{2}},
			want: "ni",
		},
		{
			name:   "no candidate at all falls back to the raw buffer",
			buffer: "iii",
			cand:   engine.Candidate{},
			want:   "iii",
		},
	}
	for _, tc := range cases {
		s := &imeSession{buffer: tc.buffer}
		if tc.cand.Word != "" {
			s.cands = []engine.Candidate{tc.cand}
		}
		if got := s.preedit(); got != tc.want {
			t.Errorf("%s: buffer %q → preedit %q, want %q", tc.name, tc.buffer, got, tc.want)
		}
	}
}

// TestPreeditNeverAddsCharacters is the same rule stated as an invariant, so a
// future change to segmentation cannot reintroduce the bug in a shape the
// table above does not happen to cover.
func TestPreeditNeverAddsCharacters(t *testing.T) {
	buffers := []string{"h", "n", "ni", "nih", "niha", "nihao", "zhg", "wome", "iii", "xi'an"}
	for _, buf := range buffers {
		for _, spans := range [][]int{nil, {1}, {2}, {1, 2}, {2, 3}, {2, 5}, {1, 2, 3}} {
			s := &imeSession{
				buffer: buf,
				cands: []engine.Candidate{{
					Word:    "x",
					Reading: []string{"huo", "en", "ling"},
					Spans:   spans,
				}},
			}
			got := s.preedit()
			// Stripping separators must give back exactly what was typed.
			if stripped := removeAll(got, '\''); stripped != removeAll(buf, '\'') {
				t.Errorf("buffer %q spans %v → preedit %q, whose letters are %q",
					buf, spans, got, stripped)
			}
		}
	}
}

func removeAll(s string, drop byte) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != drop {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// TestPreeditLineShowsTheCorrection covers the panel's half of transposition
// correction: the candidate says 平 while the typed letters say "pign", and
// the panel is the only place allowed to bridge the two.
func TestPreeditLineShowsTheCorrection(t *testing.T) {
	cases := []struct {
		name string
		st   panelState
		want string
	}{
		{
			name: "corrected candidate highlighted",
			st: panelState{Preedit: "pign", Selected: 0, Candidates: []engine.Candidate{
				{Word: "平", Reading: []string{"ping"}, Corrected: true},
			}},
			want: "pign → ping",
		},
		{
			name: "nothing was corrected",
			st: panelState{Preedit: "ping", Selected: 0, Candidates: []engine.Candidate{
				{Word: "平", Reading: []string{"ping"}},
			}},
			want: "ping",
		},
		{
			name: "the correction is not the highlighted one",
			st: panelState{Preedit: "pign", Selected: 1, Candidates: []engine.Candidate{
				{Word: "平", Reading: []string{"ping"}, Corrected: true},
				{Word: "皮革", Reading: []string{"pi", "ge"}},
			}},
			want: "pign",
		},
		{
			name: "no selection",
			st:   panelState{Preedit: "pign", Selected: -1},
			want: "pign",
		},
	}
	for _, c := range cases {
		if got := preeditLine(c.st); got != c.want {
			t.Errorf("%s: preeditLine = %q, want %q", c.name, got, c.want)
		}
	}
}
