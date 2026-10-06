package dict

import (
	"fmt"
	"os"
	"sort"
)

// Entry is one candidate word with its weight.
type Entry struct {
	Word   string
	Weight uint32
}

// Builder accumulates (syllable sequence → words) and serializes a trie.
//
// This runs only in the offline compiler, never in the resident input
// method, so it is free to be heap-hungry.
type Builder struct {
	syls      []string          // ascending; index is the syllable ID
	sylID     map[string]uint16 //
	nodes     []buildNode
	entries   int
	refWeight uint32
}

// SetRefWeight records the scale the compiler normalized weights against —
// roughly the weight of the most common word.
//
// It is stored in the file rather than agreed on as a constant in two places
// because the reader needs it to turn a weight back into a log-PROBABILITY,
// and a reader using a different reference than the compiler would silently
// mis-rank every multi-word candidate.
func (b *Builder) SetRefWeight(w uint32) { b.refWeight = w }

type buildKid struct {
	syl  uint16
	node int32
}

type buildNode struct {
	kids  []buildKid // kept sorted by syl
	posts []Entry
}

// NewBuilder creates a builder over the given syllable alphabet. The
// alphabet is sorted here rather than trusted from the caller: the
// contiguous-prefix-range property that abbreviation lookup depends on is
// only true if IDs are assigned in ascending byte order.
func NewBuilder(syllables []string) *Builder {
	syls := append([]string(nil), syllables...)
	sort.Strings(syls)
	b := &Builder{
		syls:  syls,
		sylID: make(map[string]uint16, len(syls)),
		nodes: make([]buildNode, 1, 1<<20), // node 0 is the root
	}
	for i, s := range syls {
		b.sylID[s] = uint16(i)
	}
	return b
}

// SyllableID reports the ID of a syllable, and whether it is in the alphabet.
func (b *Builder) SyllableID(s string) (uint16, bool) {
	id, ok := b.sylID[s]
	return id, ok
}

// Add records word under the given syllable sequence. Re-adding the same
// word at the same key keeps the higher weight, which is what lets a later
// source promote an overlapping entry without dropping anything.
func (b *Builder) Add(syls []uint16, word string, weight uint32) {
	if word == "" || len(syls) == 0 || len(syls) > MaxSyllables {
		return
	}
	cur := int32(0)
	for _, s := range syls {
		cur = b.descend(cur, s)
	}
	n := &b.nodes[cur]
	for i := range n.posts {
		if n.posts[i].Word == word {
			if weight > n.posts[i].Weight {
				n.posts[i].Weight = weight
			}
			return
		}
	}
	n.posts = append(n.posts, Entry{Word: word, Weight: weight})
	b.entries++
}

// descend returns the child of node cur along syllable s, creating it if
// needed. Children stay sorted by syllable ID — the reader binary-searches
// them, and the whole abbreviation scheme depends on that order.
func (b *Builder) descend(cur int32, s uint16) int32 {
	kids := b.nodes[cur].kids
	i := sort.Search(len(kids), func(i int) bool { return kids[i].syl >= s })
	if i < len(kids) && kids[i].syl == s {
		return kids[i].node
	}
	b.nodes = append(b.nodes, buildNode{})
	child := int32(len(b.nodes) - 1)
	kids = append(kids, buildKid{})
	copy(kids[i+1:], kids[i:])
	kids[i] = buildKid{syl: s, node: child}
	b.nodes[cur].kids = kids
	return child
}

// Stats reports what has been accumulated so far.
func (b *Builder) Stats() (nodes, entries int) { return len(b.nodes), b.entries }

// TrimPostings caps how many words are kept per key, dropping the lowest
// weights. Long tails on a single key are dead weight: no user pages 300
// candidates deep on one syllable, and the postings section is the largest
// part of the file.
func (b *Builder) TrimPostings(max int) (dropped int) {
	if max <= 0 {
		return 0
	}
	for i := range b.nodes {
		p := b.nodes[i].posts
		if len(p) <= max {
			continue
		}
		sort.SliceStable(p, func(a, c int) bool { return p[a].Weight > p[c].Weight })
		dropped += len(p) - max
		b.nodes[i].posts = p[:max]
	}
	b.entries -= dropped
	return dropped
}

// pool interns strings into one byte buffer.
type pool struct {
	buf  []byte
	seen map[string]uint32
}

func newPool() *pool { return &pool{seen: make(map[string]uint32, 1<<20)} }

func (p *pool) intern(s string) (off uint32, ln uint16) {
	if o, ok := p.seen[s]; ok {
		return o, uint16(len(s))
	}
	o := uint32(len(p.buf))
	p.buf = append(p.buf, s...)
	p.seen[s] = o
	return o, uint16(len(s))
}

// WriteFile serializes the trie to path.
//
// Two passes: the first computes every node's byte offset (a node's size
// depends only on its child count, which is already known), the second emits
// bytes now that child offsets can be filled in. Without the sizing pass a
// node could not reference a child that has not been written yet.
func (b *Builder) WriteFile(path string) error {
	// postCount is a u16, so clamp before anything is sized against it. Done
	// up front rather than at emit time so the sizing pass and the emit pass
	// agree on how many records each node has.
	for i := range b.nodes {
		if ps := b.nodes[i].posts; len(ps) > 0xffff {
			sort.SliceStable(ps, func(a, c int) bool { return ps[a].Weight > ps[c].Weight })
			b.nodes[i].posts = ps[:0xffff]
		}
	}

	// --- pass 1: sizes and offsets ---
	syllSize := 2
	for _, s := range b.syls {
		if len(s) > 255 {
			return fmt.Errorf("dict: syllable %q too long", s)
		}
		syllSize += 1 + len(s)
	}
	syllOff := uint32(headerSize)
	trieOff := syllOff + uint32(syllSize)

	nodeOff := make([]uint32, len(b.nodes))
	var pos uint32 = trieOff
	var totalPosts int
	for i := range b.nodes {
		nodeOff[i] = pos
		pos += uint32(nodeHeadSize + childRecSize*len(b.nodes[i].kids))
		totalPosts += len(b.nodes[i].posts)
	}
	postOff := pos
	poolOff := postOff + uint32(totalPosts*postRecSize)

	// --- intern words; postings order is fixed here ---
	p := newPool()
	type postRec struct {
		weight  uint32
		wordOff uint32
		wordLen uint16
	}
	postsOfNode := make([][]postRec, len(b.nodes))
	for i := range b.nodes {
		ps := b.nodes[i].posts
		if len(ps) == 0 {
			continue
		}
		sort.SliceStable(ps, func(a, c int) bool { return ps[a].Weight > ps[c].Weight })
		recs := make([]postRec, len(ps))
		for j, e := range ps {
			wo, wl := p.intern(e.Word)
			recs[j] = postRec{weight: e.Weight, wordOff: wo, wordLen: wl}
		}
		postsOfNode[i] = recs
	}

	out := make([]byte, poolOff+uint32(len(p.buf)))

	// --- header ---
	copy(out[hMagic:], magic)
	le.PutUint32(out[hVersion:], FormatVersion)
	le.PutUint32(out[hSyllOff:], syllOff)
	le.PutUint32(out[hTrieOff:], trieOff)
	le.PutUint32(out[hPostOff:], postOff)
	le.PutUint32(out[hPoolOff:], poolOff)
	le.PutUint32(out[hNodeCount:], uint32(len(b.nodes)))
	le.PutUint32(out[hEntryCount:], uint32(b.entries))
	le.PutUint32(out[hRefWeight:], b.refWeight)

	// --- syllables ---
	q := int(syllOff)
	le.PutUint16(out[q:], uint16(len(b.syls)))
	q += 2
	for _, s := range b.syls {
		out[q] = byte(len(s))
		q++
		q += copy(out[q:], s)
	}

	// --- trie + postings ---
	postCursor := postOff
	for i := range b.nodes {
		n := &b.nodes[i]
		q = int(nodeOff[i])
		recs := postsOfNode[i]
		if len(recs) > 0 {
			le.PutUint32(out[q:], postCursor)
		} else {
			le.PutUint32(out[q:], 0)
		}
		le.PutUint16(out[q+4:], uint16(len(recs)))
		le.PutUint16(out[q+6:], uint16(len(n.kids)))
		q += nodeHeadSize
		for _, k := range n.kids {
			le.PutUint16(out[q:], k.syl)
			le.PutUint32(out[q+2:], nodeOff[k.node])
			q += childRecSize
		}
		for _, r := range recs {
			pq := int(postCursor)
			le.PutUint32(out[pq:], r.weight)
			le.PutUint32(out[pq+4:], poolOff+r.wordOff)
			le.PutUint16(out[pq+8:], r.wordLen)
			postCursor += postRecSize
		}
	}

	// --- pool ---
	copy(out[poolOff:], p.buf)

	return os.WriteFile(path, out, 0o644)
}
