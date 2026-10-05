// bridge_darwin.h — the C surface between the InputMethodKit shim and Go.
//
// Division of labor: the ObjC side owns nothing but the IMK objects and the
// conversions AppKit insists on. Every decision — what the preedit is, what
// the candidates are, whether a key is swallowed — happens in Go. That
// keeps the untestable half of the process as small as possible.
//
// Threading: every function here runs on the process main thread. IMK
// delivers key events as run-loop sources, which are serviced inside the
// main loop's event pump, so a callback into Go arrives on the same OS
// thread qui pinned at startup and may touch the widget tree directly.
//
// Lifetime: the `client` pointer is an id<IMKTextInput> owned by IMK. It is
// safe to use for the duration of the callback that produced it. Holding it
// longer — which the panel's MOUSE path needs, since a click arrives outside
// any IMK callback — requires taking a reference: qimeBridgeRetainClient /
// qimeBridgeReleaseClient. ARC's __bridge cast does not retain, so caching a
// bare pointer is a dangling read waiting for the host app to close a text
// field mid-composition.

// Naming: everything here is prefixed qimeBridge, NOT quiIME. Go's //export
// symbols have C linkage across the WHOLE binary, and root qui already owns
// quiIME* for its own IME-CLIENT bridge (ime_darwin.m — qui apps receiving
// marked text, the opposite direction from this file). Reusing that prefix
// links as a duplicate-symbol error whose message names two anonymous .o
// files and mentions neither package.

#ifndef QUI_IME_BRIDGE_H
#define QUI_IME_BRIDGE_H

// --- process setup ---------------------------------------------------

// qimeBridgeServerStart creates the IMKServer using InputMethodConnectionName
// from Info.plist. Returns 1 on success.
int qimeBridgeServerStart(void);

// qimeBridgeRegisterSelf runs TISRegisterInputSource on our own bundle. Only
// makes the input source visible for the current login session — the
// durable registration happens at login. See README.
int qimeBridgeRegisterSelf(void);

// --- client lifetime -------------------------------------------------

// qimeBridgeRetainClient takes a +1 reference so the pointer stays valid
// after the callback returns. Pair with qimeBridgeReleaseClient exactly once.
void *qimeBridgeRetainClient(void *client);

// qimeBridgeReleaseClient drops a reference taken by qimeBridgeRetainClient.
void qimeBridgeReleaseClient(void *client);

// --- talking back to the host app ------------------------------------

// qimeBridgeSetMarkedText shows utf8 as the in-progress (underlined) text in
// the host app. Passing "" clears it.
//
// The string is sent ATTRIBUTED, built by IMK's markForStyle:atRange:. A bare
// NSString gets "default marking" with no clause segment, which clients that
// maintain their own composition model cannot read. See the definition.
void qimeBridgeSetMarkedText(void *client, const char *utf8);

// qimeBridgeInsertText commits utf8 into the host app and ends composition.
void qimeBridgeInsertText(void *client, const char *utf8);

// qimeBridgeCaretRect reports where the host app's insertion point is, in
// GLOBAL LOGICAL POINTS with a TOP-LEFT origin — already converted out of
// Cocoa's bottom-left screen space so the result can go straight to
// qui.Window.ShowAt. Returns 0 when the client cannot say.
int qimeBridgeCaretRect(void *client, int *x, int *y, int *w, int *h);

#endif
