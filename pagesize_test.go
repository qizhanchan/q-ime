package main

import (
	"testing"

	"github.com/qizhanchan/q-ime/internal/engine"
)

// TestClampPageSize covers the values that reach clampPageSize from outside the
// program: a hand-edited preferences file, and a file written by a build whose
// bounds were different.
func TestClampPageSize(t *testing.T) {
	for _, tc := range []struct {
		in, want int
	}{
		{0, minPageSize}, // missing from the JSON entirely
		{3, minPageSize}, // the old floor, from a file this build inherited
		{5, 5},
		{8, 8},
		{9, maxPageSize}, // the old ceiling
		{999, maxPageSize},
		{-1, minPageSize},
	} {
		if got := clampPageSize(tc.in); got != tc.want {
			t.Errorf("clampPageSize(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestSetPageSizeShowsMoreCandidates is what the menu pick has to accomplish:
// a bigger page means more candidates on screen, on the digit keys, right away.
func TestSetPageSizeShowsMoreCandidates(t *testing.T) {
	s, _ := newShiftTestSession(t)
	s.cands = cands("一", "二", "三", "四", "五", "六", "七", "八", "九", "十")

	if got := len(s.visible()); got != 5 {
		t.Fatalf("the default page shows %d candidates, want 5", got)
	}
	s.setPageSize(8)
	if got := len(s.visible()); got != 8 {
		t.Errorf("after setting 8 the page shows %d candidates", got)
	}
	if got := s.pageSize(); got != 8 {
		t.Errorf("pageSize() = %d after the pick", got)
	}
}

// TestSetPageSizePersists: the menu is the only place this setting can be
// changed, so a pick that is not written down is a pick the user makes again
// after every login.
func TestSetPageSizePersists(t *testing.T) {
	s, _ := newShiftTestSession(t)
	var saved []prefs
	s.save = func(p prefs) { saved = append(saved, p) }

	s.setPageSize(7)
	if len(saved) != 1 || saved[0].PageSize != 7 {
		t.Fatalf("setPageSize wrote %v, want one save of 7", saved)
	}
	// Picking the row that is already checked changes nothing and writes
	// nothing — opening the menu to look at it is not an edit.
	s.setPageSize(7)
	if len(saved) != 1 {
		t.Errorf("a no-op pick wrote the file again: %v", saved)
	}
}

// TestSetPageSizeRepositionsTheReservedSlot is why the candidate list is
// rebuilt rather than merely redrawn.
//
// The menu is reachable mid-composition — macOS does not end the composition to
// open it — and the English slot is positioned at pageSize-1. A list built for
// five and shown on a page of eight would put the reserved English word in the
// middle of the page, which is neither where it was promised nor where the eye
// looks for it.
func TestSetPageSizeRepositionsTheReservedSlot(t *testing.T) {
	s, _ := newLearningSession(t)
	s.save = func(prefs) {}

	typeKeys(s, "song")
	at5 := indexOfSource(s.cands, engine.SourceEnglish)
	if at5 != 4 {
		t.Fatalf("on a page of 5 the English candidate is at %d, want the last slot (4): %v",
			at5, candWords(s.cands))
	}

	s.setPageSize(8)
	if at8 := indexOfSource(s.cands, engine.SourceEnglish); at8 != 7 {
		t.Errorf("on a page of 8 the English candidate is at %d, want the last slot (7): %v",
			at8, candWords(s.cands))
	}
}

func indexOfSource(cs []engine.Candidate, src engine.Source) int {
	for i, c := range cs {
		if c.Source == src {
			return i
		}
	}
	return -1
}
