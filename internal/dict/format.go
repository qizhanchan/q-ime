// Package dict defines the on-disk format of the compiled pinyin lexicon and
// a reader that mmaps it.
//
// # Why a compiled binary instead of parsing rime-ice YAML at startup
//
// The merged lexicon is ~1.9M entries / ~50MB of text. An input method is a
// resident background process, so that must not live on the Go heap: the GC
// would scan it on every cycle and the RSS would be indefensible for
// something that sits in the menu bar all day. So the lexicon is compiled
// once (tools/dictc) into a flat, offset-addressed blob that the runtime
// mmaps. The OS pages it in on demand, the bytes never enter the Go heap
// (they contain no pointers, so the GC ignores the region entirely), and a
// lookup allocates only the handful of strings it actually returns.
//
// # Why a trie over syllable IDs, not a map from pinyin strings
//
// A string-keyed map handles "nihao" but not the two things users actually
// rely on: abbreviations ("zhg" → 中国) and mixed input ("nhao" → 你好).
// Both mean "syllable at position i is ANY syllable with this prefix", which
// is a set-valued query no exact-match index can answer.
//
// A trie whose edges are syllable IDs answers it directly. Syllable IDs are
// assigned in alphabetical order, so "every syllable starting with n" is a
// CONTIGUOUS ID range — and since each node's children are sorted by ID, that
// query is a binary search plus a linear scan of adjacent children. Fuzzy
// rules (z↔zh, n↔l, …) are not contiguous, so they are probed as explicit
// extra IDs; there are at most a few per position.
//
// # Layout
//
// All integers little-endian; all offsets absolute from the start of file.
//
//	Header     48 bytes
//	Syllables  count u16, then count×(len u8, bytes) — ascending, index == ID
//	Trie       nodes, root at trieOff; each node is
//	             postOff u32, postCount u16, childCount u16,
//	             then childCount×(sylID u16, nodeOff u32) sorted by sylID
//	Postings   records of postRecSize: weight u32, wordOff u32, wordLen u16
//	             sorted by weight DESC within a node, so a reader takes a prefix
//	Pool       raw UTF-8 word bytes
package dict

import "encoding/binary"

const (
	magic      = "QIMEDIC1"
	headerSize = 48

	nodeHeadSize = 8  // postOff(4) postCount(2) childCount(2)
	childRecSize = 6  // sylID(2) nodeOff(4)
	postRecSize  = 10 // weight(4) wordOff(4) wordLen(2)
)

// Header field offsets.
const (
	hMagic      = 0  // [8]byte
	hVersion    = 8  // u32
	hSyllOff    = 12 // u32
	hTrieOff    = 16 // u32 — also the root node's offset
	hPostOff    = 20 // u32
	hPoolOff    = 24 // u32
	hNodeCount  = 28 // u32
	hEntryCount = 32 // u32
	hRefWeight  = 36 // u32 — the scale weights were normalized against
	// 40..48 reserved
)

// FormatVersion is bumped whenever the layout changes so a stale compiled
// dictionary is rejected loudly instead of being misread.
const FormatVersion = 2

// MaxSyllables caps how many syllables one key may have. Keys longer than
// this are not useful for input (nobody types a 24-syllable run without
// committing) and the cap keeps the search bounded.
const MaxSyllables = 24

var le = binary.LittleEndian
