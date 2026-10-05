package main

import "testing"

// The whole audit rests on the in-syllable / cross-boundary split: it is what
// turns "76% recovered" — a number no engine change could move, because most of
// the misses are unreachable by construction — into "94.8% of the slips the arcs
// can actually see". A wrong classifier would quietly move damage into the
// column nobody reads, so it gets a test even though the tool it lives in
// cannot have one.
func TestSlipIsPlacedRelativeToTheSyllables(t *testing.T) {
	// bei|jing — ends after "bei" (3) and after "jing" (7).
	r := reading{typed: "beijing", ends: []int{3, 7}}

	for _, tc := range []struct {
		name  string
		at    int
		want  bool
		width int
	}{
		{"inside the first syllable", 0, true, 2},
		{"inside the second", 4, true, 2},
		{"straddling the boundary", 2, false, 2},
		{"last letter of the word", 5, true, 2},
		{"a single-byte edit at the boundary", 3, true, 1},
	} {
		if got := r.within(tc.at, tc.width); got != tc.want {
			t.Errorf("%s: within(%d, %d) = %v, want %v", tc.name, tc.at, tc.width, got, tc.want)
		}
	}
}

func TestTransposeAllCoversEveryAdjacentPair(t *testing.T) {
	got := transposeAll("ping")
	want := []string{"ipng", "pnig", "pign"}
	if len(got) != len(want) {
		t.Fatalf("transposeAll(\"ping\") = %v, want %v", texts(got), want)
	}
	for i, v := range got {
		if v.text != want[i] {
			t.Errorf("variant %d is %q, want %q", i, v.text, want[i])
		}
		if v.at != i || v.width != 2 {
			t.Errorf("variant %q reports at=%d width=%d, want at=%d width=2", v.text, v.at, v.width, i)
		}
	}

	// A doubled letter has no distinct swap, and reporting one would count a
	// no-op as a slip the engine failed to recover from.
	if got := transposeAll("aa"); len(got) != 0 {
		t.Errorf("transposeAll(\"aa\") = %v, want none", texts(got))
	}
}

func texts(vs []variant) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.text
	}
	return out
}
