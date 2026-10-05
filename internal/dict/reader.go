package dict

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"syscall"
)

// Reader is a read-only view over an mmap'd compiled lexicon.
//
// data aliases the mapped region: the lexicon never enters the Go heap, and
// because the region holds no pointers the GC never walks it. A Reader is
// safe for concurrent readers and is never mutated after Open.
type Reader struct {
	data       []byte
	syllOff    int
	trieOff    int // also the root node offset
	nodeCount  int
	entryCount int

	syls      []string // ascending; index is the syllable ID
	maxSyll   int
	refWeight uint32
}

// RefWeight is the scale the compiler normalized weights against, i.e. about
// what the most common word scores. Dividing by it turns a stored weight into
// something a caller can take the log of and get a probability-like quantity,
// which is what ranking multi-word candidates requires.
func (r *Reader) RefWeight() uint32 {
	if r.refWeight == 0 {
		return 1
	}
	return r.refWeight
}

// Candidate is one word produced by a lookup.
type Candidate struct {
	Word   string
	Weight uint32
}

// Node is a position in the trie. The zero Node is invalid; start from Root.
type Node struct {
	off int
}

// ID is a stable identifier for this node within its dictionary, suitable as
// a map key. Callers memoizing a search need to recognize a node they have
// already expanded, and the byte offset is exactly that identity.
func (n Node) ID() uint32 { return uint32(n.off) }

var (
	ErrBadFormat  = errors.New("dict: not a q-ime dictionary")
	ErrBadVersion = errors.New("dict: dictionary was built by a different version; rebuild it")
)

// Open mmaps the compiled dictionary at path.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := int(fi.Size())
	if size < headerSize {
		return nil, ErrBadFormat
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*Reader, error) {
		_ = syscall.Munmap(data)
		return nil, e
	}
	if string(data[hMagic:hMagic+len(magic)]) != magic {
		return fail(ErrBadFormat)
	}
	if v := le.Uint32(data[hVersion:]); v != FormatVersion {
		return fail(fmt.Errorf("%w (file v%d, want v%d)", ErrBadVersion, v, FormatVersion))
	}
	r := &Reader{
		data:       data,
		syllOff:    int(le.Uint32(data[hSyllOff:])),
		trieOff:    int(le.Uint32(data[hTrieOff:])),
		nodeCount:  int(le.Uint32(data[hNodeCount:])),
		entryCount: int(le.Uint32(data[hEntryCount:])),
		refWeight:  le.Uint32(data[hRefWeight:]),
	}
	r.decodeSyllables()
	return r, nil
}

// Close unmaps the file. Reading through the Reader afterwards is a fault,
// exactly as it would be in C — callers must drop it.
func (r *Reader) Close() error {
	if r.data == nil {
		return nil
	}
	err := syscall.Munmap(r.data)
	r.data = nil
	return err
}

func (r *Reader) decodeSyllables() {
	pos := r.syllOff
	n := int(le.Uint16(r.data[pos:]))
	pos += 2
	r.syls = make([]string, 0, n)
	for i := 0; i < n; i++ {
		ln := int(r.data[pos])
		pos++
		s := string(r.data[pos : pos+ln]) // ~416 short strings, copied once
		pos += ln
		r.syls = append(r.syls, s)
		if ln > r.maxSyll {
			r.maxSyll = ln
		}
	}
}

// Syllables returns the alphabet, ascending; the index of a syllable is its ID.
func (r *Reader) Syllables() []string { return r.syls }

// MaxSyllableLen is the length of the longest syllable, which bounds how far
// the segmenter has to look ahead.
func (r *Reader) MaxSyllableLen() int { return r.maxSyll }

// Stats reports the compiled size, for the startup log line.
func (r *Reader) Stats() (nodes, entries, bytes int) {
	return r.nodeCount, r.entryCount, len(r.data)
}

// Syllable returns the text of a syllable ID.
func (r *Reader) Syllable(id uint16) string {
	if int(id) >= len(r.syls) {
		return ""
	}
	return r.syls[id]
}

// SyllableID looks up an exact syllable.
func (r *Reader) SyllableID(s string) (uint16, bool) {
	i := sort.SearchStrings(r.syls, s)
	if i < len(r.syls) && r.syls[i] == s {
		return uint16(i), true
	}
	return 0, false
}

// PrefixRange returns the half-open ID range [lo, hi) of every syllable
// starting with p.
//
// This is the whole reason IDs are assigned in ascending byte order: it makes
// "any syllable beginning with n" — which is what typing a bare initial means
// — a contiguous range, so abbreviation lookup is a binary search rather than
// a scan of the alphabet at every position.
func (r *Reader) PrefixRange(p string) (lo, hi uint16) {
	a := sort.SearchStrings(r.syls, p)
	b := a
	for b < len(r.syls) && hasPrefix(r.syls[b], p) {
		b++
	}
	return uint16(a), uint16(b)
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// Root is the trie's root node.
func (r *Reader) Root() Node { return Node{off: r.trieOff} }

// Child follows the edge labelled syl, if any.
func (r *Reader) Child(n Node, syl uint16) (Node, bool) {
	base := n.off + nodeHeadSize
	cnt := int(le.Uint16(r.data[n.off+6:]))
	i := sort.Search(cnt, func(i int) bool {
		return le.Uint16(r.data[base+i*childRecSize:]) >= syl
	})
	if i < cnt && le.Uint16(r.data[base+i*childRecSize:]) == syl {
		return Node{off: int(le.Uint32(r.data[base+i*childRecSize+2:]))}, true
	}
	return Node{}, false
}

// ChildrenInRange calls fn for every child whose syllable ID is in [lo, hi).
// Children are stored sorted by ID, so this is a binary search to the start
// of the range plus a walk — no allocation, and no scan of absent syllables.
// fn returning false stops the walk.
func (r *Reader) ChildrenInRange(n Node, lo, hi uint16, fn func(syl uint16, child Node) bool) {
	base := n.off + nodeHeadSize
	cnt := int(le.Uint16(r.data[n.off+6:]))
	i := sort.Search(cnt, func(i int) bool {
		return le.Uint16(r.data[base+i*childRecSize:]) >= lo
	})
	for ; i < cnt; i++ {
		syl := le.Uint16(r.data[base+i*childRecSize:])
		if syl >= hi {
			return
		}
		child := Node{off: int(le.Uint32(r.data[base+i*childRecSize+2:]))}
		if !fn(syl, child) {
			return
		}
	}
}

// HasChildren reports whether n has any outgoing edge — i.e. whether the
// syllable sequence reaching n is a prefix of some longer key.
func (r *Reader) HasChildren(n Node) bool {
	return le.Uint16(r.data[n.off+6:]) > 0
}

// PostCount is how many words end exactly at n.
func (r *Reader) PostCount(n Node) int { return int(le.Uint16(r.data[n.off+4:])) }

// Post returns the i'th word ending at n. Postings are weight-descending, so
// a caller wanting the best few just reads a prefix.
func (r *Reader) Post(n Node, i int) Candidate {
	base := int(le.Uint32(r.data[n.off:]))
	p := base + i*postRecSize
	wo := int(le.Uint32(r.data[p+4:]))
	wl := int(le.Uint16(r.data[p+8:]))
	return Candidate{
		Word:   string(r.data[wo : wo+wl]),
		Weight: le.Uint32(r.data[p:]),
	}
}

// PostWeight returns just the weight of the i'th posting, without
// materializing the word. Ranking looks at far more postings than it keeps,
// and this is what stops the discarded ones from allocating.
func (r *Reader) PostWeight(n Node, i int) uint32 {
	base := int(le.Uint32(r.data[n.off:]))
	return le.Uint32(r.data[base+i*postRecSize:])
}

// Lookup walks an exact syllable-ID sequence and returns up to limit
// candidates, best first. limit <= 0 means all.
func (r *Reader) Lookup(syls []uint16, limit int) []Candidate {
	n := r.Root()
	for _, s := range syls {
		var ok bool
		if n, ok = r.Child(n, s); !ok {
			return nil
		}
	}
	cnt := r.PostCount(n)
	if limit > 0 && limit < cnt {
		cnt = limit
	}
	out := make([]Candidate, cnt)
	for i := 0; i < cnt; i++ {
		out[i] = r.Post(n, i)
	}
	return out
}
