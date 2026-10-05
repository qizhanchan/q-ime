// q-ime is a macOS input method whose candidate window is drawn by qui.
//
// Step 3 of the plan in README.md: the IMK plumbing proven in step 1 and the
// non-activating transparent panel proven in step 2, wired together. The
// pinyin "engine" is still a lookup table (engine.go) — this step is about
// whether the three parts can coexist in one process at all:
//
//   - InputMethodKit needs the AppKit run loop serviced so its mach port
//     delivers key events;
//   - qui needs the same thread to pump events and paint frames;
//   - the process must never become the active application, or the app the
//     user is typing into loses focus the moment the input method loads.
//
// Run modes:
//
//	QuiIME              serve as the input method (how macOS starts it)
//	QuiIME --register   TISRegisterInputSource on our own bundle, then exit
package main

import (
	"log"
	"os"

	"github.com/qizhanchan/qui"
)

func main() {
	// Before anything else that might log — see installLogging.
	installLogging()

	if len(os.Args) > 1 && os.Args[1] == "--register" {
		if !registerInputSource() {
			os.Exit(1)
		}
		return
	}

	// Not a preference — two hard requirements. Only the cocoa backend can
	// make a non-activating window (GLFW returns ErrOverlayPanelUnsupported),
	// and only it honors the activation policy below. Set before NewApp,
	// which is when the backend is chosen.
	if err := os.Setenv("QUI_PLATFORM", "cocoa"); err != nil {
		log.Fatalf("q-ime: %v", err)
	}

	// THE most important line in the file. An input method that becomes the
	// active app pulls focus off the text field the user is typing into, at
	// which point every keystroke goes nowhere. Must precede NewApp: on
	// macOS an app that has already activated cannot quietly un-activate.
	qui.SetActivationPolicy(qui.ActivationProhibited)

	app, err := qui.NewApp()
	if err != nil {
		log.Fatalf("q-ime: NewApp: %v", err)
	}

	panel, err := newCandidatePanel(app)
	if err != nil {
		log.Fatalf("q-ime: candidate panel: %v", err)
	}

	// The lexicon is mmap'd, so this is a few page faults rather than a read
	// of 77MB — but it can still fail (missing bundle resource, a dictionary
	// built by an older dictc), and an input method that silently swallows
	// every keystroke is worse than one that refuses to start.
	prefs := loadPrefs()
	eng, user, lexicon, err := newEngine()
	if err != nil {
		log.Fatalf("q-ime: lexicon: %v", err)
	}
	startUserDictSaver(user)

	session = newIMESession(panel, eng, user, prefs)

	// Start IMK only after the panel and engine exist. IMK can deliver a key
	// event as soon as the server is up, and a callback arriving before they
	// are built would be a nil dereference on the user's first keystroke.
	if !startIMKServer() {
		log.Fatal("q-ime: could not start IMKServer")
	}
	logf("ready; %s", describeEngine(lexicon, user, eng.Fuzzy()))

	// qui's own loop, not [NSApp run]. Both service the default run loop
	// mode, which is what IMK's mach port needs, but only this one also
	// steps qui's frames — and it means the IME does not need a second
	// thread or a display-link shim to get its panel painted.
	//
	// The panel window is never closed, so Run does not fall out of its
	// loop; the process exits via the menu's terminate: or a signal.
	app.Run()
}
