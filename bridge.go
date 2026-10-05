package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework InputMethodKit -framework Carbon
#include <stdlib.h>
#include "bridge_darwin.h"
*/
import "C"

import (
	"log"
	"os"
	"path/filepath"
	"unsafe"
)

// The IMK side calls the three quiIMEGo* functions below; everything else
// here is Go calling into ObjC. See bridge_darwin.h for the contract, in
// particular that `client` is valid only for the duration of one callback.

// session is the process-wide input state. It is a singleton on purpose:
// IMK creates one IMKInputController per text client, but there is exactly
// one user, one composition and one candidate panel. Keeping state here
// rather than on the controller is what stops each text field from getting
// its own private half of the input method.
var session *imeSession

// --- ObjC -> Go ------------------------------------------------------

//export qimeBridgeGoKey
func qimeBridgeGoKey(client unsafe.Pointer, chars *C.char, keyCode C.int, mods C.uint) C.int {
	if session == nil {
		return 0
	}
	return cBool(session.handleText(client, C.GoString(chars), int(keyCode), uint(mods)))
}

//export qimeBridgeGoCommand
func qimeBridgeGoCommand(client unsafe.Pointer, selector *C.char) C.int {
	if session == nil {
		return 0
	}
	return cBool(session.handleCommand(client, C.GoString(selector)))
}

// qimeBridgeGoFlags receives modifier transitions (NSEventTypeFlagsChanged),
// which exist only on the raw-NSEvent path. Returning 1 would swallow the
// event; handleFlags never does, because the host app needs to see every
// modifier change.
//
//export qimeBridgeGoFlags
func qimeBridgeGoFlags(client unsafe.Pointer, mods C.uint, keyCode C.int) C.int {
	if session == nil {
		return 0
	}
	return cBool(session.handleFlags(client, uint(mods), int(keyCode)))
}

// qimeBridgeGoLog lets the ObjC side write to the same log file as the Go
// side, so a diagnostic about a host app lands where `build.sh logs` shows it
// rather than in the unified log, where NSLog'd strings come back redacted.
//
//export qimeBridgeGoLog
func qimeBridgeGoLog(msg *C.char) {
	logf("%s", C.GoString(msg))
}

// qimeBridgeGoComposedString answers IMK's composedString: with the text
// currently underlined in the host app.
//
// The returned buffer is allocated by C.CString and MUST be freed by the
// caller — the ObjC side does that immediately after copying it into an
// NSString. Returning a Go string directly is not an option: cgo cannot hand a
// Go pointer to C and have it outlive the call.
//
//export qimeBridgeGoComposedString
func qimeBridgeGoComposedString() *C.char {
	if session == nil {
		return nil
	}
	return C.CString(session.preedit())
}

// qimeBridgeGoPageSize answers the input-source menu with the candidate count
// in force, so the menu can put the check mark on the right row.
//
//export qimeBridgeGoPageSize
func qimeBridgeGoPageSize() C.int {
	if session == nil {
		return C.int(defaultPrefs().PageSize)
	}
	return C.int(session.pageSize())
}

// qimeBridgeGoPageSizeBounds writes the range the candidate count may be set
// to, so the menu builds its rows from what Go allows rather than from a
// second copy of the numbers on the ObjC side.
//
// Writing through C pointers is the one direction cgo allows without ceremony:
// the memory belongs to the caller's stack frame in ObjC, and Go only stores
// into it.
//
//export qimeBridgeGoPageSizeBounds
func qimeBridgeGoPageSizeBounds(lo, hi *C.int) {
	if lo != nil {
		*lo = C.int(minPageSize)
	}
	if hi != nil {
		*hi = C.int(maxPageSize)
	}
}

// qimeBridgeGoSetPageSize applies a pick from the input-source menu.
//
// Runs on the main thread — a menu action is dispatched from the same run loop
// that delivers key events — so it may touch the session and the panel
// directly, exactly like a keystroke does.
//
//export qimeBridgeGoSetPageSize
func qimeBridgeGoSetPageSize(n C.int) {
	if session == nil {
		return
	}
	session.setPageSize(int(n))
	// Both numbers, deliberately. The menu item makes a round trip through
	// another process as a serialized copy, so "asked for 8, now showing 5"
	// would be the visible symptom of a tag that did not survive it — and
	// without the first number that reads like the setting simply not working.
	logf("menu asked for %d candidates; now showing %d", int(n), session.pageSize())
}

//export qimeBridgeGoDeactivate
func qimeBridgeGoDeactivate(client unsafe.Pointer) {
	if session == nil {
		return
	}
	session.deactivate(client)
}

func cBool(v bool) C.int {
	if v {
		return 1
	}
	return 0
}

// --- Go -> ObjC ------------------------------------------------------

func startIMKServer() bool { return C.qimeBridgeServerStart() != 0 }

func registerInputSource() bool { return C.qimeBridgeRegisterSelf() != 0 }

// retainClient takes a reference so the pointer survives past the IMK
// callback that produced it. Every retainClient needs exactly one
// releaseClient — see imeSession.setClient.
func retainClient(client unsafe.Pointer) unsafe.Pointer {
	if client == nil {
		return nil
	}
	return unsafe.Pointer(C.qimeBridgeRetainClient(client))
}

func releaseClient(client unsafe.Pointer) {
	if client == nil {
		return
	}
	C.qimeBridgeReleaseClient(client)
}

func setMarkedText(client unsafe.Pointer, text string) {
	c := C.CString(text)
	defer C.free(unsafe.Pointer(c))
	C.qimeBridgeSetMarkedText(client, c)
}

func insertText(client unsafe.Pointer, text string) {
	if text == "" {
		return
	}
	c := C.CString(text)
	defer C.free(unsafe.Pointer(c))
	C.qimeBridgeInsertText(client, c)
}

// caretRect reports the host app's insertion point in global logical
// points, top-left origin — ready for qui.Window.ShowAt. ok is false when
// the client won't say, which happens in some Electron and Java apps; the
// caller must then fall back to a position of its own rather than showing
// the panel at (0,0).
func caretRect(client unsafe.Pointer) (x, y, w, h int, ok bool) {
	var cx, cy, cw, ch C.int
	if C.qimeBridgeCaretRect(client, &cx, &cy, &cw, &ch) == 0 {
		return 0, 0, 0, 0, false
	}
	return int(cx), int(cy), int(cw), int(ch), true
}

// LogPath is where the Go half of the input method logs.
//
// A file, not stderr and not the unified log. launchd starts an input method
// with no stderr anyone can read, and NSLog'ing Go strings through the
// unified log came back as "<decode: missing data>" — the one channel that
// has to work while debugging an invisible background process should not
// itself be the thing that is broken. `build.sh logs` tails this.
func LogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/QuiIME.log"
	}
	return filepath.Join(home, "Library", "Logs", "QuiIME.log")
}

// installLogging must run before anything logs.
func installLogging() {
	path := LogPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return // keep the default stderr writer rather than dying over logging
	}
	log.SetOutput(f)
	log.SetFlags(log.Ltime | log.Lmicroseconds)
}

func logf(format string, args ...any) {
	log.Printf("q-ime: "+format, args...)
}
