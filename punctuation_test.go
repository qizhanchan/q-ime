package main

import "testing"

func TestPunctuationConversion(t *testing.T) {
	p := newPunctuator()
	cases := []struct {
		in   rune
		want string
	}{
		{',', "，"},
		{'.', "。"},
		{'?', "？"},
		{'!', "！"},
		{'\\', "、"},
		{'<', "《"},
		{')', "）"},
	}
	for _, tc := range cases {
		if got := p.convert(tc.in); got != tc.want {
			t.Errorf("convert(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPunctuationLeavesTechnicalCharactersAlone is the rule that keeps the
// input method usable for addresses, code and maths typed mid-sentence.
func TestPunctuationLeavesTechnicalCharactersAlone(t *testing.T) {
	p := newPunctuator()
	for _, r := range []rune{'@', '#', '$', '%', '^', '&', '*', '_', '+', '=', '/', '|', '~', '-'} {
		if got := p.convert(r); got != "" {
			t.Errorf("convert(%q) = %q, want it left alone", r, got)
		}
	}
}

func TestPairedPunctuationAlternates(t *testing.T) {
	p := newPunctuator()
	want := []string{"“", "”", "“", "”"}
	for i, w := range want {
		if got := p.convert('"'); got != w {
			t.Errorf("quote %d = %q, want %q", i, got, w)
		}
	}
}

// Brackets have distinct keys, so they must NOT consult the alternating
// state — typing [[ means two opening brackets.
func TestBracketsAreNotAlternating(t *testing.T) {
	p := newPunctuator()
	for i := 0; i < 3; i++ {
		if got := p.convert('['); got != "【" {
			t.Errorf("[ #%d = %q, want 【", i, got)
		}
	}
	if got := p.convert(']'); got != "】" {
		t.Errorf("] = %q, want 】", got)
	}
}

func TestPunctuationResetClearsQuoteState(t *testing.T) {
	p := newPunctuator()
	if got := p.convert('"'); got != "“" {
		t.Fatalf("first quote = %q", got)
	}
	p.reset()
	if got := p.convert('"'); got != "“" {
		t.Errorf("after reset, quote = %q, want an opening “", got)
	}
}

func TestPageLabel(t *testing.T) {
	cases := []struct {
		name string
		st   panelState
		want string
	}{
		{
			name: "single page shows nothing",
			st:   panelState{Total: 4, PageSize: 5, PageStart: 0},
			want: "",
		},
		{
			name: "exactly one full page shows nothing",
			st:   panelState{Total: 5, PageSize: 5, PageStart: 0},
			want: "",
		},
		{
			name: "first of several has only a forward arrow",
			st:   panelState{Total: 12, PageSize: 5, PageStart: 0},
			want: "1/3 ▸",
		},
		{
			name: "middle page has both arrows",
			st:   panelState{Total: 12, PageSize: 5, PageStart: 5},
			want: "◂ 2/3 ▸",
		},
		{
			name: "last page has only a back arrow",
			st:   panelState{Total: 12, PageSize: 5, PageStart: 10},
			want: "◂ 3/3",
		},
	}
	for _, tc := range cases {
		if got := pageLabel(tc.st); got != tc.want {
			t.Errorf("%s: pageLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
}
