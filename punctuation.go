package main

// Chinese punctuation.
//
// Typing Chinese means typing Chinese punctuation, and no keyboard has keys
// for it. Every Chinese IME therefore rewrites ASCII punctuation to its
// full-width form while in Chinese mode — this is not a convenience feature,
// it is the only way to produce 。and 、at all.

// punctuationMap is the ASCII → full-width table.
//
// Deliberately not exhaustive. Characters whose ASCII form is wanted at least
// as often as the Chinese one are left alone: @ # $ % ^ & * _ + = / | ~ - all
// appear in addresses, code and maths that people type mid-sentence, and
// silently swapping them would be worse than not having them.
//
// The hyphen is in that list for a second reason as well: unlike （） or 《》,
// U+FF0D is not the Chinese form of anything anyone means. Chinese writing
// uses 破折号 —— for a dash and 、for a list, never a full-width hyphen, so
// rewriting - broke "utf-8" and "2024-01" while producing nothing in return.
// No mainstream pinyin IME converts it either.
var punctuationMap = map[rune]string{
	',':  "，",
	'.':  "。",
	';':  "；",
	':':  "：",
	'?':  "？",
	'!':  "！",
	'\\': "、",
	'<':  "《",
	'>':  "》",
	'(':  "（",
	')':  "）",
}

// pairedPunctuation are the marks that alternate between an opening and a
// closing form. A keyboard has one key for the quote; Chinese has two
// characters, and which one is meant depends on how many have been typed.
var pairedPunctuation = map[rune][2]string{
	'"':  {"“", "”"},
	'\'': {"‘", "’"},
	'[':  {"【", "】"},
	']':  {"【", "】"},
}

// punctuator rewrites ASCII punctuation, remembering which half of each
// paired mark comes next.
//
// The open/closed state is per process rather than per text field. That is
// wrong in principle — two documents have two independent quote states — but
// IMK gives no reliable "this is a different field" signal, and the
// alternative (guessing from the client pointer) would reset the state on
// every focus change and get it wrong far more often.
type punctuator struct {
	open map[rune]bool
}

func newPunctuator() *punctuator { return &punctuator{open: make(map[rune]bool)} }

// convert returns the Chinese form of r, or "" if r is not punctuation this
// table rewrites.
func (p *punctuator) convert(r rune) string {
	if s, ok := punctuationMap[r]; ok {
		return s
	}
	if pair, ok := pairedPunctuation[r]; ok {
		// Bracket keys are unambiguous — [ opens, ] closes — so they do not
		// consult the alternating state, only the quotes do.
		switch r {
		case '[':
			return pair[0]
		case ']':
			return pair[1]
		}
		closing := p.open[r]
		p.open[r] = !closing
		if closing {
			return pair[1]
		}
		return pair[0]
	}
	return ""
}

// reset forgets the quote state, for when a composition is abandoned or
// focus moves somewhere unrelated.
func (p *punctuator) reset() {
	for k := range p.open {
		delete(p.open, k)
	}
}
