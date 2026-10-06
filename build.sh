#!/bin/bash
# Build / install / uninstall q-ime (Go + cgo IMK input method, qui panel).
#
#   ./build.sh              compile the lexicon + QuiIME.app into ./build
#   ./build.sh install      compile, then install to ~/Library/Input Methods
#   ./build.sh install-system  same, but /Library/Input Methods (needs sudo)
#   ./build.sh check        ask the OS whether it knows about our input source
#   ./build.sh uninstall    kill the process and remove the installed bundle
#   ./build.sh logs         stream the running input method's NSLog output
#   ./build.sh lexicon      rebuild only the compiled dictionary
#
# Signing: ad-hoc by default, which is enough both to execute and to register
# (verified — the amfid "adhoc signed" log line is a red herring). Override
# with a real identity when it matters, e.g. for distribution:
#
#   IDENTITY="Developer ID Application: Your Name (TEAMID)" ./build.sh install
#
# Deliberately a hand-built bundle rather than an Xcode project: the point is
# to see exactly which Info.plist keys and which signing steps are
# load-bearing. The binary is Go (cgo); the IMK shim is bridge_darwin.m inside
# the same package.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
APP_NAME="QuiIME"
BUILD_DIR="$HERE/build"
APP="$BUILD_DIR/$APP_NAME.app"
INSTALL_DIR="$HOME/Library/Input Methods"
MIN_MACOS="13.0"
# The compiled dictionary and where its sources come from.
#
# rime-ice is a git submodule at third_party/rime-ice, checked out shallow.
# Override RIME_ICE to build against a different checkout.
LEXICON="lexicon.bin"
RIME_ICE="${RIME_ICE:-$HERE/third_party/rime-ice}"
# Optional extra source, outside the rime-ice checkout: the refined subset of
# the tencent word list. Absent is fine — the lexicon just loses the re-ranking
# it buys (see tools/dictc, tencentLite).
TENCENT_LITE="${TENCENT_LITE:-$HERE/third_party/tencent_lite.dict.yaml}"
# "-" is ad-hoc. Set IDENTITY to a Developer ID to sign for real.
IDENTITY="${IDENTITY:--}"

# rime_ice_present is the check for a usable checkout. It tests for a file
# rather than the directory: an uninitialised submodule leaves an empty
# third_party/rime-ice behind, which a `-d` test would accept.
rime_ice_present() {
    [ -f "$RIME_ICE/cn_dicts/base.dict.yaml" ]
}

# require_rime_ice fails with the one command that fixes the common case.
require_rime_ice() {
    rime_ice_present && return 0
    cat >&2 <<EOF
error: rime-ice not found at $RIME_ICE

If this is a fresh clone, initialise the submodule:

  git submodule update --init --depth 1

Or point RIME_ICE at an existing checkout:

  RIME_ICE=/path/to/rime-ice ./build.sh install
EOF
    return 1
}

# do_lexicon compiles the rime-ice dictionaries into the binary lexicon the
# input method mmaps. Skipped when the output is already newer than the
# compiler and the sources — it produces ~77MB, so rebuilding it on every code
# change would dominate the edit/test loop.
do_lexicon() {
    local out="$HERE/build/$LEXICON"
    mkdir -p "$HERE/build"
    if ! rime_ice_present; then
        if [ -f "$out" ]; then
            echo "note: rime-ice not found at $RIME_ICE; reusing existing $out" >&2
            return 0
        fi
        require_rime_ice || return 1
    fi
    local lite=()
    if [ -f "$TENCENT_LITE" ]; then
        lite=(-lite "$TENCENT_LITE")
    fi
    if [ -f "$out" ] &&
        [ ! "$HERE/tools/dictc/main.go" -nt "$out" ] &&
        [ ! "$RIME_ICE/cn_dicts/base.dict.yaml" -nt "$out" ] &&
        { [ ! -f "$TENCENT_LITE" ] || [ ! "$TENCENT_LITE" -nt "$out" ]; }; then
        echo "lexicon up to date ($(du -h "$out" | cut -f1))"
        return 0
    fi
    echo "compiling lexicon from $RIME_ICE ..."
    ( cd "$HERE" && go run ./tools/dictc -src "$RIME_ICE" "${lite[@]+"${lite[@]}"}" -out "$out" )
}

# do_english regenerates the embedded English word list from the same rime-ice
# checkout.
#
# NOT part of do_build, unlike do_lexicon. The output is a committed source
# file that go:embed compiles into the binary, so it is regenerated when the
# dictionaries are updated — a deliberate act, with a diff to review — rather
# than on every build. A build that silently rewrote a tracked file would make
# "did the word list change?" unanswerable from git.
do_english() {
    require_rime_ice || return 1
    ( cd "$HERE" && go run ./tools/engc \
        -src "$RIME_ICE/en_dicts" \
        -o ./internal/english/data/english.txt )
}

do_build() {
    do_lexicon
    rm -rf "$APP"
    mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
    cp "$HERE/Info.plist" "$APP/Contents/Info.plist"
    printf 'APPL????' > "$APP/Contents/PkgInfo"
    # The icon is not decoration: an input method whose icon key points at a
    # missing file is silently absent from System Settings. The .lproj
    # bundles supply the input source's readable name. See docs/arch.md.
    cp -R "$HERE/Resources/"* "$APP/Contents/Resources/"
    # The lexicon rides in Resources, which is where engine.go looks first.
    cp "$HERE/build/$LEXICON" "$APP/Contents/Resources/$LEXICON"

    # CGO_CFLAGS carries the deployment target; the frameworks come from
    # the #cgo directives in bridge.go.
    ( cd "$HERE" && CGO_CFLAGS="-mmacosx-version-min=$MIN_MACOS" \
        go build -o "$APP/Contents/MacOS/$APP_NAME" . )

    # Apple Silicon refuses to execute an unsigned binary at all. Ad-hoc
    # is enough for both execution and input-method registration (verified);
    # distribution needs Developer ID + notarization.
    if [ "$IDENTITY" = "-" ]; then
        codesign --force --sign - "$APP" >/dev/null
    else
        codesign --force --options runtime --timestamp \
            --sign "$IDENTITY" "$APP" >/dev/null
    fi

    echo "built $APP (identity: $IDENTITY)"
}

do_check() {
    local bin="$BUILD_DIR/tis-list"
    local src="$HERE/tools/tis-list.m"
    mkdir -p "$BUILD_DIR"
    if [ ! -x "$bin" ] || [ "$src" -nt "$bin" ]; then
        clang -fobjc-arc -Wno-deprecated-declarations \
            -framework Carbon -framework Foundation -o "$bin" "$src"
    fi
    "$bin" "$@"
}

do_install() {
    do_build
    mkdir -p "$INSTALL_DIR"
    # A running old copy holds the mach connection name; it must die first
    # or the freshly installed one never gets the port.
    killall "$APP_NAME" 2>/dev/null || true
    sleep 0.3
    rm -rf "$INSTALL_DIR/$APP_NAME.app"
    cp -R "$APP" "$INSTALL_DIR/"
    # TIS reads bundle metadata through LaunchServices, so refresh that
    # first — otherwise a re-install can register stale Info.plist contents.
    LSREGISTER=/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister
    [ -x "$LSREGISTER" ] && "$LSREGISTER" -f "$INSTALL_DIR/$APP_NAME.app"

    # Registration does not take effect reliably on the first call after the
    # bundle is replaced — it lands somewhere between "queued" and "lost".
    # Poll instead of hoping, so `install` either succeeds or says it didn't.
    local ok=0
    for _ in 1 2 3 4 5; do
        "$INSTALL_DIR/$APP_NAME.app/Contents/MacOS/$APP_NAME" --register >/dev/null
        sleep 1.5
        if do_check >/dev/null 2>&1; then ok=1; break; fi
    done
    open "$INSTALL_DIR/$APP_NAME.app"
    if [ "$ok" != 1 ]; then
        echo "WARNING: registered without error but the input source is still" >&2
        echo "not in the TIS database. See docs/arch.md, \"the .inputmethod. trap\"." >&2
    fi
    cat <<EOF

installed to $INSTALL_DIR/$APP_NAME.app

Verify the OS actually took it (registration fails SILENTLY otherwise):
  ./build.sh check

FIRST INSTALL ONLY: log out and back in. TISRegisterInputSource gives a
transient entry; the durable registration happens during login, and the
services that would refresh it live-side (imklaunchagent, hiservices) are
SIP-protected and cannot be restarted. Later re-installs need no logout —
just switch away from the input method and back to pick up the new binary.

Then enable it by hand (this part cannot be scripted):
  System Settings > Keyboard > Input Sources > Edit… > + > Chinese, Simplified
  pick "qui 拼音", then switch to it with Ctrl-Space.

Then in any text field: type  nihao  and press space.
EOF
}

# Install system-wide. A registration into ~/Library/Input Methods is transient:
# the entry can disappear a minute after --register, while a bundle under
# /Library/Input Methods (root-owned) keeps its registration. That is also why
# the secure-input fallback works from there. See docs/arch.md.
#
# NOT doing what the sample's README says ("sudo chmod -R 777
# /Library/Input Methods"). A world-writable directory that the OS loads
# input methods out of means any local process can drop in something that
# sees every keystroke in every app. The bundle is copied with sudo and left
# owned by root:wheel instead.
do_install_system() {
    do_build
    local dest="/Library/Input Methods"
    killall "$APP_NAME" 2>/dev/null || true
    sleep 0.3
    sudo mkdir -p "$dest"
    sudo rm -rf "$dest/$APP_NAME.app"
    sudo cp -R "$APP" "$dest/"
    sudo chown -R root:wheel "$dest/$APP_NAME.app"
    sudo chmod -R go-w "$dest/$APP_NAME.app"

    LSREGISTER=/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister
    [ -x "$LSREGISTER" ] && "$LSREGISTER" -f "$dest/$APP_NAME.app"
    local ok=0
    for _ in 1 2 3 4 5; do
        "$dest/$APP_NAME.app/Contents/MacOS/$APP_NAME" --register >/dev/null
        sleep 1.5
        if do_check >/dev/null 2>&1; then ok=1; break; fi
    done
    open "$dest/$APP_NAME.app"
    echo "installed to $dest/$APP_NAME.app (root:wheel)"
    if [ "$ok" != 1 ]; then
        echo "WARNING: still not in the TIS database — log out and back in," >&2
        echo "then re-run './build.sh check'." >&2
    fi
}

do_uninstall() {
    killall "$APP_NAME" 2>/dev/null || true
    rm -rf "$INSTALL_DIR/$APP_NAME.app"
    echo "removed $INSTALL_DIR/$APP_NAME.app"
    if [ -d "/Library/Input Methods/$APP_NAME.app" ]; then
        sudo rm -rf "/Library/Input Methods/$APP_NAME.app"
        echo "removed /Library/Input Methods/$APP_NAME.app"
    fi
    echo "the input source may linger in System Settings until logout — remove it there with '-'"
}

# The Go half logs to a file (see bridge.go LogPath) because launchd gives an
# input method no readable stderr and NSLog'd Go strings came back from the
# unified log as "<decode: missing data>". The ObjC half's NSLog lines are in
# the unified log; both are shown.
do_logs() {
    local f="$HOME/Library/Logs/QuiIME.log"
    echo "--- unified log (ObjC side), last 2m ---"
    /usr/bin/log show --last 2m --style compact \
        --predicate 'process == "QuiIME"' 2>/dev/null | grep -E "q-ime" || true
    echo
    echo "--- $f (Go side, following; Ctrl-C to stop) ---"
    touch "$f"
    tail -f -n 40 "$f"
}

case "${1:-build}" in
    build)     do_build ;;
    lexicon)   do_lexicon ;;
    english)   do_english ;;
    install)   do_install ;;
    install-system) do_install_system ;;
    check)     shift; do_check "$@" ;;
    uninstall) do_uninstall ;;
    logs)      do_logs ;;
    *) echo "usage: $0 [build|lexicon|english|install|install-system|check|uninstall|logs]" >&2; exit 2 ;;
esac
