package main

import (
	"strings"
	"testing"

	"github.com/qizhanchan/q-ime/internal/engine"
)

// cands builds a candidate list from a compact spelling: a word ending in "!"
// is the English one.
func cands(spec ...string) []engine.Candidate {
	out := make([]engine.Candidate, len(spec))
	for i, s := range spec {
		c := engine.Candidate{Word: strings.TrimSuffix(s, "!")}
		if strings.HasSuffix(s, "!") {
			c.Source = engine.SourceEnglish
		}
		out[i] = c
	}
	return out
}

func joined(cs []engine.Candidate) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.Word
		if c.Source == engine.SourceEnglish {
			parts[i] += "!"
		}
	}
	return strings.Join(parts, " ")
}

// TestReserveEnglishSlot is the visibility guarantee: an exact English match is
// on the first page even when it did not score its way there.
//
// The ranking is right to bury "song" under 送 and 松 — that is what somebody
// typing "song" in Chinese mode means — but it buried it at rank 26, and a
// candidate six pages deep is not offered in any sense a user would
// recognise. So the score decides whether English goes FIRST and this decides
// that it is at least visible.
func TestReserveEnglishSlot(t *testing.T) {
	cases := []struct {
		name     string
		in       []engine.Candidate
		pageSize int
		want     string
		why      string
	}{
		{
			name:     "buried English moves to the last slot of page one",
			in:       cands("送", "松", "宋", "耸", "颂", "讼", "song!"),
			pageSize: 5,
			want:     "送 松 宋 耸 song! 颂 讼",
			why:      "this is the case the whole function exists for",
		},
		{
			name:     "the top pick is never displaced",
			in:       cands("的", "地", "得", "德", "底", "de!"),
			pageSize: 5,
			want:     "的 地 得 德 de! 底",
			why:      "Space commits candidate 0; it must stay whatever ranking chose",
		},
		{
			name:     "English that earned a place is left alone",
			in:       cands("我们", "我门", "women!", "我闷", "我们的", "我们是"),
			pageSize: 5,
			want:     "我们 我门 women! 我闷 我们的 我们是",
			why:      "reordering a candidate that already ranked would be a demotion",
		},
		{
			name:     "English already first stays first",
			in:       cands("hello!", "合理", "荷兰", "河流", "合力", "何"),
			pageSize: 5,
			want:     "hello! 合理 荷兰 河流 合力 何",
			why:      "the force-fitted case, where ranking got it right on its own",
		},
		{
			name:     "a list that fits on one page needs no surgery",
			in:       cands("和", "何", "he!"),
			pageSize: 5,
			want:     "和 何 he!",
			why:      "everything is visible already",
		},
		{
			name:     "only the best English word gets the slot",
			in:       cands("河", "喝", "贺", "核", "禾", "和", "hell!", "he'll!"),
			pageSize: 5,
			want:     "河 喝 贺 核 hell! 禾 和 he'll!",
			why:      "one guaranteed slot must not become four for codes like hell/he'll",
		},
		{
			name:     "no English candidate leaves the list untouched",
			in:       cands("你好", "拟好", "你", "尼", "泥", "妮", "腻"),
			pageSize: 5,
			want:     "你好 拟好 你 尼 泥 妮 腻",
			why:      "the overwhelmingly common case; it must cost nothing",
		},
		{
			name:     "a one-candidate page cannot reserve anything",
			in:       cands("和", "何", "合", "he!"),
			pageSize: 1,
			want:     "和 何 合 he!",
			why:      "slot 0 is the committed candidate, so there is no slot to give",
		},
	}
	for _, tc := range cases {
		got := reserveEnglishSlot(tc.in, tc.pageSize)
		if joined(got) != tc.want {
			t.Errorf("%s:\n  got  %s\n  want %s\n  (%s)",
				tc.name, joined(got), tc.want, tc.why)
		}
	}
}

// TestReserveEnglishSlotLosesNothing is the invariant behind the list surgery.
// Moving an element by shifting a range is exactly the kind of code that drops
// or duplicates one entry at a boundary, and the symptom — a candidate that
// silently disappears from a page — is nearly impossible to attribute later.
func TestReserveEnglishSlotLosesNothing(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5, 8, 13} {
		for enAt := 0; enAt < n; enAt++ {
			for _, pageSize := range []int{1, 2, 3, 5, 9} {
				in := make([]engine.Candidate, n)
				for i := range in {
					in[i] = engine.Candidate{Word: string(rune('a' + i))}
				}
				in[enAt].Source = engine.SourceEnglish

				got := reserveEnglishSlot(in, pageSize)
				if len(got) != n {
					t.Fatalf("n=%d en=%d page=%d: length %d → %d",
						n, enAt, pageSize, n, len(got))
				}
				seen := map[string]int{}
				for _, c := range got {
					seen[c.Word]++
				}
				for i := 0; i < n; i++ {
					if w := string(rune('a' + i)); seen[w] != 1 {
						t.Fatalf("n=%d en=%d page=%d: %q appears %d times in %s",
							n, enAt, pageSize, w, seen[w], joined(got))
					}
				}
				// The English candidate must end up visible whenever a page
				// has room to spare for it.
				if pageSize >= 2 && n > pageSize {
					at := -1
					for i, c := range got {
						if c.Source == engine.SourceEnglish {
							at = i
							break
						}
					}
					if at >= pageSize {
						t.Errorf("n=%d en=%d page=%d: English left at %d, off the first page",
							n, enAt, pageSize, at)
					}
					if at == 0 && enAt != 0 {
						t.Errorf("n=%d en=%d page=%d: English promoted to the committed slot",
							n, enAt, pageSize)
					}
				}
			}
		}
	}
}
