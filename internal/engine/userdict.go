package engine

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// UserDict remembers what this user picked, so the input method stops
// offering the same wrong first candidate forever.
//
// Small on purpose — it holds only choices the user actually made, not the
// lexicon — so unlike the compiled dictionary it lives on the Go heap and is
// saved as plain JSON. Being readable matters more than being compact here:
// it is the one file a user might want to inspect, edit or delete. Saves
// append to a journal beside it rather than rewriting it — see
// userdict_store.go.
//
// Guarded by a mutex because saving happens on a timer off the input
// method's goroutine.
type UserDict struct {
	mu      sync.Mutex
	path    string
	entries map[string]*userEntry
	dirty   bool
	// changed holds the keys written since the last save, which is all a
	// journal append has to write.
	changed map[string]struct{}
	// compact makes the next save rewrite the snapshot instead of appending —
	// after a change too broad to journal, or a snapshot that failed to load.
	compact bool
	// journalSize is the journal's length on disk. Touched only by loading and
	// by Save, which saveMu serializes.
	journalSize int64
	saveMu      sync.Mutex
	// literals indexes the literal entries by code — see literalKey.
	//
	// Pure derived data, rebuilt from entries, and it exists for one reason:
	// looking a literal up must not scan the history. An exact lookup happens
	// on EVERY pinyin keystroke, and a map that may hold twenty thousand
	// entries is not something to walk between a key going down and the
	// candidate list being drawn.
	literals map[string][]string
	// successors indexes the bigram entries by the word they follow, for the
	// same reason literals is indexed by code: the context lookup happens on the
	// keystroke path and must not scan the history.
	successors map[string][]bigramRef
	// phrases indexes the MULTI-SYLLABLE reading entries by the first letter of
	// their first syllable, so the lookup that produces a word the lexicon does
	// not have costs one bucket rather than a walk of twenty thousand readings.
	//
	// Multi-syllable only, and that is not an optimization but a statement about
	// what can be in there. A single-syllable reading entry is always a lexicon
	// character — a lone character cannot be committed from anywhere else, since
	// the literal path writes to its own namespace — so the ranking boost
	// already covers it and indexing it here would only produce duplicates. What
	// is left is exactly the words this user BUILT, which is the hundreds-scale
	// the literal index also lives at.
	//
	// Keyed by first LETTER rather than first syllable so a query still finds a
	// phrase whose first syllable is only half typed — `y`, `ye` and the
	// per-syllable initials `ywc` all have to reach a phrase read ye'wu'ce,
	// and all of them start with the same letter. See learnedPhrases.
	phrases map[byte][]phraseRef
	// gen counts context writes — see Generation.
	gen uint64
	// clock is swappable so tests can exercise recency without sleeping.
	clock func() time.Time
}

// bigramRef is one remembered successor, without the score: the entry it points
// at holds the counts, so nothing here can go stale except by eviction.
type bigramRef struct {
	reading []string
	word    string
}

// phraseRef is one multi-syllable reading entry, indexed by first letter. Same
// shape and same reasoning as bigramRef.
type phraseRef struct {
	reading []string
	word    string
}

type userEntry struct {
	Count uint32 `json:"n"`
	// Last is a unix timestamp of the most recent pick. Recency is tracked
	// separately from count so a word used twice today outranks one used
	// three times last year.
	Last int64 `json:"t"`
}

// userFile is the serialized form. A version field means a future change to
// the scoring can discard incompatible history instead of misreading it.
type userFile struct {
	Version int                   `json:"version"`
	Entries map[string]*userEntry `json:"entries"`
}

const userFileVersion = 1

// maxUserEntries bounds the file. Well above what a heavy user produces in
// years, and low enough that loading it is never something you notice.
const maxUserEntries = 20000

// NewUserDict loads the user dictionary at path, or starts an empty one if
// it does not exist or cannot be parsed.
//
// A corrupt history is not worth failing to start over: the input method
// still works without it, and refusing to launch would be a far worse
// outcome than losing learned frequencies.
func NewUserDict(path string) *UserDict {
	u := &UserDict{
		path:       path,
		entries:    make(map[string]*userEntry),
		changed:    make(map[string]struct{}),
		literals:   make(map[string][]string),
		successors: make(map[string][]bigramRef),
		phrases:    make(map[byte][]phraseRef),
		clock:      time.Now,
	}
	u.loadLocked()
	u.migrateLiteralsLocked()
	u.rebuildIndexesLocked()
	return u
}

// migrateLiteralsLocked moves English picks recorded before the literal
// namespace existed into it.
//
// Those entries were written by the English candidate path under a reading key,
// where nothing has ever read them back: an English word is looked up by the
// letters typed, not by a reading. Rewriting them costs one pass over a file
// that is already in memory, and the alternative is telling a user their
// history is fine but they have to type each of those words once more before it
// counts.
//
// An ASCII word is the discriminator, and it is exact rather than a heuristic:
// the only words that reach the reading namespace come from the Chinese
// lexicon, whose entries are never ASCII, and the only ASCII words this input
// method has ever recorded came from the English path.
func (u *UserDict) migrateLiteralsLocked() {
	for k, e := range u.entries {
		code, word, found := strings.Cut(k, "\x00")
		// A reading key always starts with a pinyin letter, so anything else is
		// some other namespace's key and none of this function's business.
		// Tested against the letter range rather than against the markers in
		// use, so adding a fourth namespace cannot silently feed it to this.
		if !found || k[0] < 'a' || k[0] > 'z' {
			continue
		}
		if !isASCII(word) || strings.Contains(code, "'") {
			continue
		}
		delete(u.entries, k)
		// Keep whichever record is richer if both spellings somehow exist.
		if old, ok := u.entries[literalKey(code, word)]; !ok || old.Count < e.Count {
			u.entries[literalKey(code, word)] = e
		}
		u.rewriteLocked()
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// DefaultUserDictPath is where the history lives: alongside other
// application support data, not next to the app bundle, which the user may
// replace on every reinstall.
func DefaultUserDictPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "quiime-user.json")
	}
	return filepath.Join(home, "Library", "Application Support", "QuiIME", "user.json")
}

// key is the reading and the word together. Keying on the reading as well as
// the word is what keeps 行 learned as háng from affecting 行 as xíng.
func userKey(reading []string, word string) string {
	return strings.Join(reading, "'") + "\x00" + word
}

// literalKey is the key for a LITERAL pick: a word committed as the letters
// themselves rather than as a reading of Chinese — an English word, a brand, a
// piece of jargon, a command name.
//
// A namespace of its own, because the two kinds of memory answer different
// questions. A reading key asks "which word did you mean by these sounds"; a
// literal key asks "how do you spell the thing you type as these letters", and
// "AfterShip" is not a reading of anything. Reading keys also PRODUCE now,
// through the phrases index and LearnedPhrases.
//
// The \x01 prefix cannot collide with a reading key, whose first byte is always
// a letter, so both live in one map and one file.
func literalKey(code, word string) string {
	return "\x01" + code + "\x00" + word
}

// coinedKey is the key for a word the user BUILT: a composition finished by
// several deliberate picks, recorded as one word.
//
// The fourth namespace, and it has to be a namespace rather than a flag on the
// reading entries, because those cannot be trusted to produce: reading entries
// also hold accidental sentence commits, and reading them to produce candidates
// would promote those to full-coverage matches the lexicon can never outrank.
//
// Only coinPhrase writes here, and only when every piece of the composition was
// deliberately picked. That is the difference between "this text passed through"
// and "the user assembled this on purpose".
func coinedKey(reading []string, word string) string {
	return "\x03" + strings.Join(reading, "'") + "\x00" + word
}

func splitCoinedKey(k string) (reading []string, word string, ok bool) {
	if k == "" || k[0] != '\x03' {
		return nil, "", false
	}
	sep := strings.IndexByte(k, 0)
	if sep <= 0 {
		return nil, "", false
	}
	return strings.Split(k[1:sep], "'"), k[sep+1:], true
}

func splitLiteralKey(k string) (code, word string, ok bool) {
	if k == "" || k[0] != '\x01' {
		return "", "", false
	}
	sep := strings.IndexByte(k, 0)
	if sep < 0 {
		return "", "", false
	}
	return k[1:sep], k[sep+1:], true
}

// Boost reports the score bonus for a remembered pick.
//
// Frequency contributes logarithmically — the difference between one pick and
// two should be large, between twenty and twenty-one negligible — and recency
// adds a decaying slot on top so a word used this week outranks a stale one
// with the same count.
func (u *UserDict) Boost(reading []string, word string) (float64, bool) {
	if u == nil {
		return 0, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	e, ok := u.entries[userKey(reading, word)]
	if !ok {
		return 0, false
	}
	return u.boostLocked(e), true
}

func (u *UserDict) boostLocked(e *userEntry) float64 {
	return u.scaledBoostLocked(e, userBase, userScale)
}

func (u *UserDict) scaledBoostLocked(e *userEntry, base, scale float64) float64 {
	boost := base + scale*math.Log(float64(e.Count)+1)
	if age := u.clock().Unix() - e.Last; age >= 0 {
		// Half-life of a fortnight: recent picks feel sticky, old ones fade
		// back toward pure frequency rather than pinning a word forever.
		const halfLife = float64(14 * 24 * 60 * 60)
		boost += recentSlot * math.Exp2(-float64(age)/halfLife)
	}
	return boost
}

// bigramKey is the key for a word FOLLOWING another word.
//
// The third namespace, and the first one that is about context rather than
// about a single word. Its \x02 marker cannot collide with either of the
// others: a reading key starts with a pinyin letter and a literal key with
// \x01.
//
// # Why the reading is in the key
//
// Because ranking is not enough — the same wall the English brands ran into.
// Typing 真 then `b` and expecting 不错 asks for a two-syllable word out of one
// letter, and nothing in the search produces it: the completion path walks ONE
// extra syllable and takes only the first handful of children, so 不错 is not a
// low-ranked candidate for `b`, it is not a candidate at all.
//
// So the context has to be able to OFFER the word, which means knowing how it
// is spelled in pinyin. Keeping the reading here costs a few bytes per pair and
// removes the need for a word→reading index over a 77MB lexicon that is
// organised the other way round.
func bigramKey(prev string, reading []string, word string) string {
	return "\x02" + prev + "\x00" + strings.Join(reading, "'") + "\x00" + word
}

func splitBigramKey(k string) (prev string, reading []string, word string, ok bool) {
	if k == "" || k[0] != '\x02' {
		return "", nil, "", false
	}
	rest := k[1:]
	p, rest, found := strings.Cut(rest, "\x00")
	if !found {
		return "", nil, "", false
	}
	r, w, found := strings.Cut(rest, "\x00")
	if !found || w == "" {
		return "", nil, "", false
	}
	return p, strings.Split(r, "'"), w, true
}

// Successor is a word this user has typed after some particular word, with
// enough to offer it again: how it is spelled, and what it has earned.
type Successor struct {
	Reading []string
	Word    string
	Boost   float64
}

// RecordBigram notes that the user committed word directly after prev.
//
// This is the one piece of context this input method keeps. It is deliberately
// the USER's own, not a corpus: a shipped n-gram table is a second large data
// source with a second licence, while this costs a few bytes per pair, needs no
// download, and adapts to the words a particular person actually strings
// together — which for the phrases anyone types daily is the stronger signal.
//
// What it cannot do is help the first time. That is the honest cost, and it is
// why this is a complement to a corpus model rather than a replacement for one.
func (u *UserDict) RecordBigram(prev string, reading []string, word string) {
	if u == nil || prev == "" || word == "" || len(reading) == 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	k := bigramKey(prev, reading, word)
	e, ok := u.entries[k]
	if !ok {
		if len(u.entries) >= maxUserEntries {
			u.evictLocked(maxUserEntries / 10)
		}
		e = &userEntry{}
		u.entries[k] = e
		u.successors[prev] = append(u.successors[prev], bigramRef{reading, word})
	}
	e.Count++
	e.Last = u.clock().Unix()
	u.touchLocked(k)
	u.gen++
}

// Forget removes everything this user's history says that could have put word
// at the front for this reading: the pick, the pair with prev, and the literal
// spelling if it was learned as one. Returns how many entries went.
//
// All three namespaces at once, deliberately. Half-forgetting is worse than not
// offering the gesture at all: if a pick and the pair with prev were written by
// the same accidental commits, dropping only the pair leaves the word first on
// its unigram boost, so nothing visibly happens. What the user is asking for is
// "stop letting my own history put this here", and the answer has to be
// complete enough that the candidate actually falls back to what the lexicon
// says.
//
// Deleting a key that was never there is free, so the caller does not have to
// know which namespace a candidate came from.
func (u *UserDict) Forget(prev string, reading []string, word string) int {
	if u == nil || word == "" || len(reading) == 0 {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	keys := []string{userKey(reading, word)}
	if prev != "" {
		keys = append(keys, bigramKey(prev, reading, word))
	}
	if len(reading) == 1 {
		// An English or literal pick is keyed by the letters, not by a reading.
		keys = append(keys, literalKey(reading[0], word))
	}
	if len(reading) > 1 {
		keys = append(keys, coinedKey(reading, word))
	}
	gone := 0
	for _, k := range keys {
		if _, ok := u.entries[k]; ok {
			delete(u.entries, k)
			u.touchLocked(k)
			gone++
		}
	}
	if gone == 0 {
		return 0
	}
	// literals and successors are derived from entries, so they have to follow
	// — the same reason evictLocked rebuilds them.
	u.rebuildIndexesLocked()
	// A context write, so the engine's cached successor list is stale.
	u.gen++
	return gone
}

// Successors returns what this user has typed after prev, best first.
func (u *UserDict) Successors(prev string) []Successor {
	if u == nil || prev == "" {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	refs := u.successors[prev]
	out := make([]Successor, 0, len(refs))
	for _, ref := range refs {
		e, ok := u.entries[bigramKey(prev, ref.reading, ref.word)]
		if !ok {
			continue // evicted; the index is rebuilt lazily
		}
		out = append(out, Successor{
			Reading: ref.reading,
			Word:    ref.word,
			Boost:   u.scaledBoostLocked(e, bigramBase, bigramScale),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Boost != out[j].Boost {
			return out[i].Boost > out[j].Boost
		}
		return out[i].Word < out[j].Word
	})
	return out
}

// Generation counts writes to the context memory, so a caller that caches one
// word's successors can tell when its copy went stale.
//
// Needed because the cache is refreshed on a context CHANGE, and committing the
// same word twice running (好, 好) does not change it — without this the pair
// just recorded would be invisible until some other word intervened.
func (u *UserDict) Generation() uint64 {
	if u == nil {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.gen
}

// Record notes that the user chose word for reading.
func (u *UserDict) Record(reading []string, word string) {
	if u == nil || word == "" || len(reading) == 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	k := userKey(reading, word)
	e, ok := u.entries[k]
	if !ok {
		if len(u.entries) >= maxUserEntries {
			u.evictLocked(maxUserEntries / 10)
		}
		e = &userEntry{}
		u.entries[k] = e
	}
	e.Count++
	e.Last = u.clock().Unix()
	u.touchLocked(k)
}

// RecordCoined notes a word the user assembled out of several picks. See
// coinedKey for why this is its own namespace and not a reading entry.
func (u *UserDict) RecordCoined(reading []string, word string) {
	if u == nil || word == "" || len(reading) < 2 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	k := coinedKey(reading, word)
	e, ok := u.entries[k]
	if !ok {
		if len(u.entries) >= maxUserEntries {
			u.evictLocked(maxUserEntries / 10)
		}
		e = &userEntry{}
		u.entries[k] = e
		u.indexPhraseLocked(reading, word)
	}
	e.Count++
	e.Last = u.clock().Unix()
	u.touchLocked(k)
}

// RecordLiteral notes that the user committed word after typing code — the
// letters lowercased, exactly as they were pressed.
//
// Code and word are recorded separately because they differ in the cases worth
// remembering: "aftership" → "AfterShip", "goland" → "GoLand", "buenosaires" →
// "Buenos Aires". When they are the same the entry costs a line and earns
// nothing back, but the caller cannot know that in advance — a literal that
// looks redundant today is the completion for a longer prefix tomorrow.
func (u *UserDict) RecordLiteral(code, word string) {
	if u == nil || code == "" || word == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	k := literalKey(code, word)
	e, ok := u.entries[k]
	if !ok {
		if len(u.entries) >= maxUserEntries {
			u.evictLocked(maxUserEntries / 10)
		}
		e = &userEntry{}
		u.entries[k] = e
		u.literals[code] = append(u.literals[code], word)
	}
	e.Count++
	e.Last = u.clock().Unix()
	u.touchLocked(k)
}

// LiteralBoost is Boost for the literal namespace: what a word committed under
// these letters has earned.
//
// The English side of the ranking goes through here rather than through Boost,
// so a word the table happens to contain and a word only this user knows are
// promoted by the same arithmetic. Splitting them would mean picking "song"
// over 送 twenty times sticks, while picking "AfterShip" twenty times does not.
func (u *UserDict) LiteralBoost(code, word string) (float64, bool) {
	if u == nil {
		return 0, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	e, ok := u.entries[literalKey(code, word)]
	if !ok {
		return 0, false
	}
	return u.boostLocked(e), true
}

// Literal is one remembered literal pick, with the boost already resolved so a
// caller can rank it without a second lookup.
type Literal struct {
	Code  string
	Word  string
	Boost float64
}

// LiteralsFor returns the words remembered for exactly this code, best first.
func (u *UserDict) LiteralsFor(code string) []Literal {
	if u == nil || code == "" {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return sortLiterals(u.collectLocked(code, nil))
}

// LiteralsWithPrefix returns remembered words whose CODE starts with prefix,
// best first, at most limit of them.
//
// Prefix matching is safe here in a way it is not for the English table:
// nothing is guessed about a word the user has never typed. Every hit is
// something they committed themselves, which is the same standard the reading
// side already applies — see Boost.
func (u *UserDict) LiteralsWithPrefix(prefix string, limit int) []Literal {
	if u == nil || prefix == "" || limit == 0 {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []Literal
	// Over the literal index only, which holds the words this user typed by
	// hand — hundreds at the outside, against a history that may hold twenty
	// thousand readings.
	for code := range u.literals {
		if strings.HasPrefix(code, prefix) {
			out = u.collectLocked(code, out)
		}
	}
	out = sortLiterals(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (u *UserDict) collectLocked(code string, out []Literal) []Literal {
	for _, word := range u.literals[code] {
		e, ok := u.entries[literalKey(code, word)]
		if !ok {
			continue // evicted; the index is rebuilt lazily
		}
		out = append(out, Literal{Code: code, Word: word, Boost: u.boostLocked(e)})
	}
	return out
}

// sortLiterals orders by boost, then alphabetically, so a list does not
// reshuffle between keystrokes that score the same.
func sortLiterals(ls []Literal) []Literal {
	sort.SliceStable(ls, func(i, j int) bool {
		if ls[i].Boost != ls[j].Boost {
			return ls[i].Boost > ls[j].Boost
		}
		return ls[i].Word < ls[j].Word
	})
	return ls
}

// rebuildIndexesLocked regenerates both derived indexes from entries, which is
// the single source of truth. Cheap enough to do wholesale: it is one pass over
// a map that is bounded at maxUserEntries, and it runs only at load and after
// an eviction.
func (u *UserDict) rebuildIndexesLocked() {
	u.literals = make(map[string][]string)
	u.successors = make(map[string][]bigramRef)
	u.phrases = make(map[byte][]phraseRef)
	for k := range u.entries {
		if code, word, ok := splitLiteralKey(k); ok {
			u.literals[code] = append(u.literals[code], word)
			continue
		}
		if prev, reading, word, ok := splitBigramKey(k); ok {
			u.successors[prev] = append(u.successors[prev], bigramRef{reading, word})
			continue
		}
		if reading, word, ok := splitCoinedKey(k); ok {
			u.indexPhraseLocked(reading, word)
		}
	}
}

// indexPhraseLocked adds a reading entry to the phrase index if it is one of the
// multi-syllable words the index is for. See the field comment.
func (u *UserDict) indexPhraseLocked(reading []string, word string) {
	if len(reading) < 2 || reading[0] == "" {
		return
	}
	c := reading[0][0]
	u.phrases[c] = append(u.phrases[c], phraseRef{reading, word})
}

// LearnedPhrase is a multi-syllable word this user built, with what it has
// earned. Distinct from Successor only in what it is keyed by.
type LearnedPhrase struct {
	Reading []string
	Word    string
	Boost   float64
}

// LearnedPhrases returns the multi-syllable words whose reading starts with this
// letter, best first, at most limit of them. limit <= 0 means all, as in
// dict.Lookup.
//
// This is the reading namespace's answer to LiteralsWithPrefix, and it exists
// because of the same wall: until now a reading key could only ever RANK words
// the lexicon already knew, so a phrase the user built out of two commits —
// 业务侧, a project name, a piece of jargon — was recorded and then never looked
// up again. Ranking cannot conjure a word the lexicon does not contain.
func (u *UserDict) LearnedPhrases(first byte, limit int) []LearnedPhrase {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	refs := u.phrases[first]
	out := make([]LearnedPhrase, 0, len(refs))
	for _, ref := range refs {
		e, ok := u.entries[coinedKey(ref.reading, ref.word)]
		if !ok {
			continue // evicted; the index is rebuilt lazily
		}
		out = append(out, LearnedPhrase{
			Reading: ref.reading,
			Word:    ref.word,
			Boost:   u.boostLocked(e),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Boost != out[j].Boost {
			return out[i].Boost > out[j].Boost
		}
		return out[i].Word < out[j].Word
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// evictLocked drops the n least valuable entries to keep the file bounded.
//
// "Least valuable" is the boost arithmetic itself — ln(count+1) plus the
// decaying recency slot — not raw age. Sorting on Last alone would throw away a
// word picked many times months ago before a word mistyped once, which deletes
// exactly the history the dictionary is FOR: the long-lived vocabulary of
// whoever uses the input method most. The bases differ per namespace but are
// constants, so ranking every entry with the same formula preserves each
// namespace's internal order, which is all an eviction has to get right.
func (u *UserDict) evictLocked(n int) {
	type kv struct {
		k     string
		e     *userEntry
		worth float64
	}
	all := make([]kv, 0, len(u.entries))
	for k, e := range u.entries {
		all = append(all, kv{k, e, u.scaledBoostLocked(e, 0, userScale)})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].worth != all[j].worth {
			return all[i].worth < all[j].worth
		}
		if all[i].e.Last != all[j].e.Last {
			return all[i].e.Last < all[j].e.Last
		}
		// Key order last, so an eviction is deterministic even between
		// entries with identical histories.
		return all[i].k < all[j].k
	})
	if n > len(all) {
		n = len(all)
	}
	for _, x := range all[:n] {
		delete(u.entries, x.k)
	}
	// A tenth of the dictionary at once: cheaper as one snapshot than as that
	// many journal records.
	u.rewriteLocked()
	u.rebuildIndexesLocked()
}

// Len reports how many picks are remembered.
func (u *UserDict) Len() int {
	if u == nil {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.entries)
}

// ForgetAll drops everything, for a "clear learned words" action. The
// single-candidate counterpart is Forget.
func (u *UserDict) ForgetAll() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.entries = make(map[string]*userEntry)
	u.literals = make(map[string][]string)
	u.successors = make(map[string][]bigramRef)
	u.phrases = make(map[byte][]phraseRef)
	u.gen++
	u.rewriteLocked()
	u.mu.Unlock()
}
