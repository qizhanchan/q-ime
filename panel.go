package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/qizhanchan/q-ime/internal/engine"
	"github.com/qizhanchan/qui"
	"github.com/qizhanchan/qui/widgets"
)

// panelState is everything the panel draws. Passed as one value rather than
// as separate arguments so that adding something to the display cannot
// silently leave one of the update paths behind.
type panelState struct {
	// English says the buffer is a literal English word rather than pinyin.
	// Shown, because it changes what every letter key does and a mode with no
	// visible sign of itself is a mode users fight instead of use.
	English    bool
	Preedit    string
	Candidates []engine.Candidate
	Selected   int // index within Candidates; -1 for none
	PageStart  int // absolute index of Candidates[0], for the page counter
	Total      int
	PageSize   int
}

// candidatePanel is the qui overlay panel showing the preedit and candidates.
//
// One per process, not one per text client — see the note on imeSession.
type candidatePanel struct {
	win     *qui.Window
	preedit *widgets.Label
	pageLbl *widgets.Label
	row     *qui.Container
	header  *qui.Container

	// onPick is called with the index of a clicked candidate WITHIN THE
	// CURRENT PAGE. Clicking is a real path, not a nicety: it is the only way
	// to select a candidate that does not go through the keyboard, and it is
	// what proves the panel takes mouse input without taking focus.
	onPick func(int)

	state panelState
	// noticeSeq invalidates a pending notice-hide timer whenever anything
	// else takes the panel over.
	noticeSeq uint64
}

// Padding around the card, in logical points. The window is sized to the
// card plus this margin so the rounded corners and drop shadow have
// transparent pixels to sit in — and, more importantly, so the window's
// frame stays close to the visible card. A window swallows mouse events
// across its whole frame, transparent or not, so slack here is slack stolen
// from the app underneath.
const panelMargin = 6

// panelMaxWidth bounds the measurement so a long candidate list is clipped by
// the layout rather than producing a window wider than the screen.
const panelMaxWidth = 900

// Colours. One dark card rather than anything theme-derived: this window
// floats over somebody else's document, so it has to read as an overlay in
// every app, on every wallpaper.
var (
	panelBackground = qui.Color{R: 0.11, G: 0.12, B: 0.14, A: 0.97}
	panelBorder     = qui.Color{R: 1, G: 1, B: 1, A: 0.14}
	panelPreedit    = qui.Color{R: 0.60, G: 0.66, B: 0.75, A: 1}
	panelMuted      = qui.Color{R: 0.42, G: 0.47, B: 0.55, A: 1}
	panelText       = qui.Color{R: 0.93, G: 0.95, B: 0.97, A: 1}
	panelIndex      = qui.Color{R: 0.48, G: 0.54, B: 0.63, A: 1}
	panelSelectBg   = qui.Color{R: 0.16, G: 0.42, B: 0.88, A: 1}
	panelSelectText = qui.Color{R: 1, G: 1, B: 1, A: 1}
)

func newCandidatePanel(app *qui.App) (*candidatePanel, error) {
	// The initial size is a placeholder; every update() resizes to content.
	win, err := app.NewOverlayPanel(320, 72)
	if err != nil {
		return nil, err
	}
	win.SetRenderer(qui.NewGLRenderer())

	p := &candidatePanel{win: win}
	win.SetRoot(p.buildTree())
	return p, nil
}

// buildTree constructs the widget tree and records the pieces applyState
// mutates.
//
// Separate from newCandidatePanel so the layout can be measured in a test,
// without an OS window or a GPU context. The panel's width is a correctness
// property — a window takes mouse events across its whole frame, transparent
// or not, so width the card does not cover is width taken away from the app
// underneath — and that deserves a test rather than an eyeball.
func (p *candidatePanel) buildTree() qui.Widget {
	p.preedit = newTightLabel("", 12, panelPreedit)
	p.pageLbl = newTightLabel("", 11, panelMuted)

	// JustifySpaceBetween rather than a Grow spacer between the two: a
	// spacer would be one more widget to measure, and space-between already
	// means "first at the start, last at the end" once the container has
	// been sized to its content.
	p.header = qui.NewContainer(
		qui.FlexLayout{
			Direction:  qui.Horizontal,
			Gap:        12,
			AlignItems: qui.AlignCenter,
			Justify:    qui.JustifySpaceBetween,
		},
		p.preedit, p.pageLbl,
	)
	// Zeroed explicitly. qui.DefaultStyle() gives EVERY widget — plain
	// Containers included — a padding of {4,6,4,6}, which is a sensible
	// document default and dead weight on a pure layout row: 12pt of width
	// and 8pt of height per container, invisible and paid twice here.
	p.header.Style().Padding = qui.Insets{}

	p.row = qui.NewContainer(qui.FlexLayout{Direction: qui.Horizontal, Gap: 2})
	p.row.Style().Padding = qui.Insets{}

	card := qui.NewContainer(qui.FlexLayout{Direction: qui.Vertical, Gap: 3}, p.header, p.row)
	card.Style().Background = panelBackground
	card.Style().Radius = 10
	card.Style().Border = panelBorder
	card.Style().BorderSize = 1
	card.Style().Padding = qui.Insets{Top: 6, Right: 8, Bottom: 6, Left: 8}

	root := qui.NewContainer(qui.FlexLayout{Direction: qui.Vertical}, card)
	root.Style().Background = qui.ColorTransparent
	root.Style().Padding = qui.Insets{
		Top: panelMargin, Right: panelMargin, Bottom: panelMargin, Left: panelMargin,
	}
	return root
}

// update rebuilds the chips for a new composition state and resizes the
// window to fit. Called on every keystroke, so it must not allocate an OS
// resource or touch the GPU context — only the widget tree.
func (p *candidatePanel) update(st panelState) {
	// A composition is taking the panel over; a notice timer still in flight
	// must not pull the window out from under it.
	p.noticeSeq++
	p.applyState(st)
	p.fitToContent()
}

// windowless reports a panel with no OS window behind it — the panel a test
// constructs to exercise the widget tree or the key state machine.
//
// The same convention UserDict uses for its nil receiver, and for the same
// reason: the alternative is threading an interface through the session purely
// so a test can supply a stub, when what actually needs testing is the state
// machine, not the drawing. Every method that touches p.win checks this; the
// methods that only touch the widget tree do not need to.
//
// In production it is never true: main.go treats a panel it cannot create as
// fatal, because an input method with no candidate window has nothing to show.
func (p *candidatePanel) windowless() bool { return p == nil || p.win == nil }

// applyState fills the tree in from st, without touching the OS window.
func (p *candidatePanel) applyState(st panelState) {
	p.state = st
	p.preedit.SetText(preeditLine(st))
	p.pageLbl.SetText(pageLabel(st))

	chips := make([]qui.Widget, 0, len(st.Candidates))
	for i, c := range st.Candidates {
		chips = append(chips, p.newChip(i, c.Word, i == st.Selected))
	}
	// SetChildren rather than clear-then-add: one layout invalidation
	// instead of one per chip, on a path that runs every keystroke.
	p.row.SetChildren(chips...)
}

// newTightLabel builds a label with NO padding of its own.
//
// widgets.NewLabel arrives with theme padding ({4,6,4,6}), which is right for
// a label sitting in a document and wrong for one inside a chip: at two
// labels per candidate it added 24pt of dead width to every chip — about a
// third of the panel — and the chip's own padding is already the spacing
// that should be there.
func newTightLabel(text string, size float32, fg qui.Color) *widgets.Label {
	l := widgets.NewLabel(text)
	l.Style().Font.Size = size
	l.Style().Foreground = fg
	l.Style().Padding = qui.Insets{}
	return l
}

// preeditLine is the line above the candidates: the letters as typed, plus
// the spelling the highlighted candidate was read as, when a transposition was
// assumed to get there ("pign → ping").
//
// Only on the PANEL. The marked text in the user's document stays exactly what
// they typed — that invariant is pinned by preedit_test and is not negotiable,
// because letters nobody pressed must never enter a document. But the panel is
// ours, and it is the only place a correction can be shown at all: the
// candidate reads 平 while the typed letters read "pign", and with nothing
// bridging the two, a correction is something that silently happens to you.
func preeditLine(st panelState) string {
	if st.Selected < 0 || st.Selected >= len(st.Candidates) {
		return st.Preedit
	}
	c := st.Candidates[st.Selected]
	if !c.Corrected || len(c.Reading) == 0 {
		return st.Preedit
	}
	return st.Preedit + " → " + strings.Join(c.Reading, "'")
}

// pageLabel is the "2/7 ▸" counter, shown only when there is more than one
// page — a counter that always reads "1/1" is noise on a panel this small.
func pageLabel(st panelState) string {
	label := pageCounter(st)
	if !st.English {
		return label
	}
	if label == "" {
		return "EN"
	}
	return "EN  " + label
}

func pageCounter(st panelState) string {
	size := st.PageSize
	if size <= 0 {
		size = len(st.Candidates)
	}
	if size <= 0 || st.Total <= size {
		return ""
	}
	page := st.PageStart/size + 1
	pages := (st.Total + size - 1) / size
	label := fmt.Sprintf("%d/%d", page, pages)
	if page > 1 {
		label = "◂ " + label
	}
	if page < pages {
		label += " ▸"
	}
	return label
}

// chip is one candidate: its digit key and its text, highlighted when
// selected.
//
// A widget of its own rather than a widgets.Button because a Button carries
// hover and pressed states tuned for a real UI. Here the selection is driven
// by the keyboard, and a chip lighting up under a resting mouse pointer would
// fight with the highlight the user is actually steering.
type chip struct {
	widgets.Box
	onClick func()
	armed   bool
}

// Handle turns a press-and-release inside the chip into a pick.
//
// Acting on mouse UP rather than DOWN, and only when the down landed here
// too: this panel sits over the user's document, and a click that started
// somewhere else must not commit a candidate.
func (c *chip) Handle(event qui.Event) bool {
	if me, ok := event.(qui.MouseEvent); ok {
		switch me.Type() {
		case qui.EventMouseDown:
			if me.Button == qui.MouseButtonLeft {
				c.armed = true
				return true
			}
		case qui.EventMouseUp:
			if c.armed {
				c.armed = false
				if c.onClick != nil {
					c.onClick()
				}
				return true
			}
		case qui.EventMouseLeave:
			c.armed = false
		}
	}
	return c.Box.Handle(event)
}

func (p *candidatePanel) newChip(i int, word string, selected bool) qui.Widget {
	idxColor, textColor := panelIndex, panelText
	if selected {
		idxColor, textColor = panelSelectText, panelSelectText
	}
	idx := newTightLabel(fmt.Sprintf("%d", i+1), 11, idxColor)
	text := newTightLabel(word, 17, textColor)

	c := &chip{}
	c.BaseWidget = qui.NewBaseWidget()
	c.LayoutEngine = qui.FlexLayout{
		Direction: qui.Horizontal, Gap: 3, AlignItems: qui.AlignCenter,
	}
	// Container subclasses must call SetSelf, or event dispatch skips the
	// ancestor chain and the click never reaches Handle above.
	c.SetSelf(c)
	c.AddChild(idx)
	c.AddChild(text)

	c.Style().Padding = qui.Insets{Top: 2, Right: 7, Bottom: 2, Left: 5}
	c.Style().Radius = 5
	if selected {
		c.Style().Background = panelSelectBg
	}
	c.onClick = func() {
		if p.onPick != nil {
			p.onPick(i)
		}
	}
	return c
}

// fitToContent measures the widget tree and resizes the OS window to match.
//
// Without this the panel would keep whatever size it was created with: too
// small clips the candidates, too large means a big rectangle of invisible
// window sitting over the user's document eating clicks.
func (p *candidatePanel) fitToContent() {
	if p.windowless() {
		return
	}
	// No margin added here: the root container's own padding IS panelMargin,
	// so it is already inside want. Adding it again made the frame 2×
	// panelMargin larger than the card on both axes — invisible window
	// swallowing clicks meant for the document underneath.
	want := p.win.Root().Measure(qui.Size{W: panelMaxWidth, H: 400})
	w := int(want.W)
	h := int(want.H)
	if w < 100 {
		w = 100
	}
	if h < 44 {
		h = 44
	}
	p.win.SetSize(w, h)
	p.win.InvalidateLayout()
}

// showBelowCaret places the panel under the host app's insertion point,
// flipping above it and sliding sideways rather than running off screen.
//
// The caret rect arrives in top-left global points (converted in the ObjC
// bridge), so this is plain arithmetic — no coordinate-space guessing at the
// call site, which is exactly why the conversion lives on the other side.
func (p *candidatePanel) showBelowCaret(caretX, caretY, caretH int) {
	if p.windowless() {
		return
	}
	const gap = 2
	size := p.win.WindowSize()
	w, h := int(size.W), int(size.H)

	// Offset by the transparent margin so the CARD, not the window frame,
	// lines up under the caret.
	x := caretX - panelMargin
	y := caretY + caretH + gap - panelMargin

	if area, ok := workAreaAt(caretX, caretY); ok {
		if right := int(area.X + area.W); x+w > right {
			x = right - w // slide left rather than hang off the edge
		}
		if left := int(area.X); x < left {
			x = left
		}
		if bottom := int(area.Y + area.H); y+h > bottom {
			// No room below: put the panel above the caret instead. A
			// candidate list clipped by the bottom of the screen is a
			// candidate list the user cannot read.
			y = caretY - h + panelMargin - gap
		}
		if top := int(area.Y); y < top {
			y = top
		}
	}
	p.win.ShowAt(x, y)
}

// workAreaAt returns the usable bounds of the display containing the point,
// falling back to the primary display when the point is off every screen
// (which happens transiently while a window is being dragged between them).
func workAreaAt(x, y int) (qui.Rect, bool) {
	fx, fy := float32(x), float32(y)
	for _, m := range qui.Monitors() {
		a := m.WorkArea
		if fx >= a.X && fx < a.X+a.W && fy >= a.Y && fy < a.Y+a.H {
			return a, true
		}
	}
	if a := qui.PrimaryMonitor().WorkArea; a.W > 0 && a.H > 0 {
		return a, true
	}
	return qui.Rect{}, false
}

// noticeDuration is how long the mode indicator stays up: long enough to
// read two characters, short enough to be gone before it is in the way of
// the text it floats over.
const noticeDuration = 900 * time.Millisecond

// showNotice flashes a transient label — the language mode — near the caret,
// then takes it away.
//
// Reuses the candidate panel rather than making a second overlay window:
// creating an OS window and a GPU context to show two characters for under a
// second would cost more than the gesture it is reporting on.
func (p *candidatePanel) showNotice(text string, caretX, caretY, caretH int) {
	if p.windowless() {
		return
	}
	p.noticeSeq++
	seq := p.noticeSeq

	p.preedit.SetText("")
	p.pageLbl.SetText("")
	p.row.SetChildren(newTightLabel(text, 15, panelText))
	p.fitToContent()
	p.showBelowCaret(caretX, caretY, caretH)

	// The timer fires on its own goroutine, so the hide is handed back to the
	// UI thread — the widget tree and the OS window belong to it. The
	// sequence check makes a stale timer (from an earlier notice, or from one
	// racing a composition that has since started) a no-op rather than a
	// window that vanishes mid-typing.
	time.AfterFunc(noticeDuration, func() {
		p.win.TryPostJob(func() {
			if p.noticeSeq == seq {
				p.hide()
			}
		})
	})
}

func (p *candidatePanel) hide() {
	if p.windowless() {
		return
	}
	p.noticeSeq++ // any pending notice timer is now stale
	p.win.Hide()
}

// deferToUI runs fn on the UI thread after d.
//
// PostJob rather than TryPostJob: the one caller is the deferred language
// switch, and jobs.go is explicit that a bounded lane is for work that may be
// dropped while state transitions must be lossless. A dropped mode switch is a
// mode the user asked for and did not get, with nothing on screen to say so.
//
// The window is the only thing here that knows how to reach the UI thread,
// which is why this sits on the panel rather than in session.go.
func (p *candidatePanel) deferToUI(d time.Duration, fn func()) {
	if p.windowless() {
		// No window means no UI thread to hand this to, so run it where it
		// lands. Dropping it instead would be a silent failure — Shift would
		// simply stop switching language, with nothing anywhere to say why —
		// and the caller has already decided the work must happen.
		time.AfterFunc(d, fn)
		return
	}
	time.AfterFunc(d, func() { p.win.PostJob(fn) })
}

// show makes the panel visible where it already is.
//
// Exists so the session never touches p.win itself: the one caller is
// refresh's fallback for hosts that refuse to report a caret rectangle, and
// leaving that as a raw field access made the whole key path untestable for
// the sake of one line.
func (p *candidatePanel) show() {
	if p.windowless() {
		return
	}
	p.win.Show()
}
