package main

import (
	"testing"

	"github.com/qizhanchan/q-ime/internal/engine"
	"github.com/qizhanchan/qui"
	"github.com/qizhanchan/qui/widgets"
)

// TestPanelMeasuresToContent pins down the panel's width.
//
// The panel is an overlay window over somebody else's document, and a window
// receives mouse events across its WHOLE frame, transparent pixels included.
// So width that is not covered by the card is width stolen from the app
// underneath — measuring to content is a correctness property, not polish.
func TestPanelMeasuresToContent(t *testing.T) {
	cands := []engine.Candidate{
		{Word: "或嗯玲珑"}, {Word: "合理"}, {Word: "荷兰"}, {Word: "河流"}, {Word: "合力"},
	}
	p := &candidatePanel{}
	tree := p.buildTree()
	p.applyState(panelState{
		Preedit:    "huo'en'ling'long",
		Candidates: cands,
		Selected:   0,
		Total:      60,
		PageSize:   5,
	})
	got := tree.Measure(qui.Size{W: panelMaxWidth, H: 400})
	t.Logf("measured: W=%.1f H=%.1f (constraint W=%d)", got.W, got.H, panelMaxWidth)
	if got.W >= panelMaxWidth-2*panelMargin {
		t.Errorf("measured width %.1f expanded to the constraint; the panel is not sizing to content", got.W)
	}
	// Five candidates, one of them four characters wide, need ~335pt of row
	// plus the card padding and the transparent margin. The ceiling is what
	// catches a stray default padding creeping back in: widgets.NewLabel
	// ships 12pt of horizontal padding, and at two labels per chip that
	// alone put a third of the panel's width back.
	// Five candidates, one of them four characters wide, need ~323pt of row
	// plus the card padding and the transparent margin. The ceilings are
	// what catch a stray default padding creeping back in: qui.DefaultStyle
	// puts {4,6,4,6} on EVERY widget, and at four labels and two layout
	// containers that alone accounted for ~30% of both dimensions.
	if got.W > 380 {
		t.Errorf("measured width %.1f is wider than five candidates need (~351)", got.W)
	}
	if got.H > 85 {
		t.Errorf("measured height %.1f is taller than two rows need (~75)", got.H)
	}
}

// TestPanelWidthBreakdown prints where the panel's width actually goes, so a
// regression in one row cannot hide behind the total.
func TestPanelWidthBreakdown(t *testing.T) {
	cands := []engine.Candidate{
		{Word: "或嗯玲珑"}, {Word: "合理"}, {Word: "荷兰"}, {Word: "河流"}, {Word: "合力"},
	}
	p := &candidatePanel{}
	root := p.buildTree()
	p.applyState(panelState{
		Preedit: "huo'en'ling'long", Candidates: cands, Selected: 0,
		Total: 60, PageSize: 5,
	})
	avail := qui.Size{W: panelMaxWidth, H: 400}
	show := func(name string, w qui.Widget) {
		m := w.Measure(avail)
		t.Logf("%-10s W=%6.1f H=%6.1f", name, m.W, m.H)
	}
	show("root", root)
	show("header", p.header)
	show("row", p.row)
	show("preedit", p.preedit)
	show("pageLbl", p.pageLbl)
	for i, c := range p.row.ChildList() {
		show("chip "+cands[i].Word, c)
	}
}

func TestChipWidthBreakdown(t *testing.T) {
	p := &candidatePanel{}
	p.buildTree()
	avail := qui.Size{W: panelMaxWidth, H: 400}
	c := p.newChip(1, "合理", false)
	t.Logf("chip total W=%6.1f", c.Measure(avail).W)
	if cont, ok := c.(*chip); ok {
		t.Logf("  chip padding = %+v", cont.Style().Padding)
		for i, kid := range cont.ChildList() {
			lbl := kid.(*widgets.Label)
			t.Logf("  child %d text=%q font=%.1f padding=%+v W=%6.1f",
				i, lbl.Text(), lbl.Style().Font.Size, lbl.Style().Padding,
				kid.Measure(avail).W)
		}
	}
}
