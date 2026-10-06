package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// The history is stored as a snapshot plus a journal.
//
// The snapshot (user.json) is the whole dictionary, the file a user might open.
// The journal (user.json.journal) is JSON lines, one per key written since the
// snapshot, each carrying that key's COMPLETE state rather than a delta. A save
// appends the handful of keys that changed; only once the journal has grown
// past journalCompactBytes, or after a change too broad to journal (eviction,
// forget-all), is the snapshot rewritten and the journal dropped.
//
// Records are absolute states, so replaying one twice is harmless. That is what
// makes compaction crash-safe without fsync choreography: a crash between
// renaming the new snapshot into place and removing the journal replays records
// the snapshot already contains, which changes nothing.

// journalCompactBytes is how large the journal may grow before the next save
// folds it into the snapshot. At a few dozen bytes a record that is thousands
// of writes, so the snapshot is rewritten a few hundred times less often than
// before, and replaying it at launch is still instant.
const journalCompactBytes = 512 << 10

// journalRecord is one key's state. Del marks a key that is gone; otherwise N
// and T are the entry.
type journalRecord struct {
	K   string `json:"k"`
	N   uint32 `json:"n,omitempty"`
	T   int64  `json:"t,omitempty"`
	Del bool   `json:"d,omitempty"`
}

func (u *UserDict) journalPath() string {
	return u.path + ".journal"
}

// loadLocked reads the snapshot, then replays the journal over it.
//
// Neither file is required, and a damaged one is not an error: see NewUserDict
// for why a lost history is better than an input method that will not start.
func (u *UserDict) loadLocked() {
	data, err := os.ReadFile(u.path)
	switch {
	case os.IsNotExist(err):
		// A fresh install, or one that has only ever journaled.
	case err != nil:
		u.compact = true
	default:
		var f userFile
		if err := json.Unmarshal(data, &f); err != nil {
			// Unreadable, so the next save has to replace it rather than append
			// to a journal that would be replayed over nothing. The journal's
			// records are absolute states, so they are still worth applying.
			u.compact = true
			break
		}
		if f.Version != userFileVersion {
			// Scored under different rules; the journal was written against the
			// same history, so it goes too.
			u.compact = true
			return
		}
		for k, v := range f.Entries {
			if v != nil {
				u.entries[k] = v
			}
		}
	}
	u.replayJournalLocked()
}

func (u *UserDict) replayJournalLocked() {
	data, err := os.ReadFile(u.journalPath())
	if err != nil {
		return
	}
	u.journalSize = int64(len(data))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		// A save was cut off mid-line. Appending after the fragment would glue
		// the next record onto it, so the next save compacts instead.
		u.compact = true
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var r journalRecord
		if err := json.Unmarshal(line, &r); err != nil || r.K == "" {
			continue // the torn tail, or damage; lose the one record
		}
		if r.Del {
			delete(u.entries, r.K)
		} else {
			u.entries[r.K] = &userEntry{Count: r.N, Last: r.T}
		}
	}
}

// touchLocked notes that key k was written, so the next save journals it.
func (u *UserDict) touchLocked(k string) {
	u.changed[k] = struct{}{}
	u.dirty = true
}

// rewriteLocked notes a change too broad to journal key by key.
func (u *UserDict) rewriteLocked() {
	u.compact = true
	u.dirty = true
}

// Save persists the history if anything changed: an append to the journal
// normally, a rewrite of the snapshot when compaction is due.
func (u *UserDict) Save() error {
	if u == nil {
		return nil
	}
	// One save at a time: an append racing a compaction could land in a
	// journal the compaction is about to delete.
	u.saveMu.Lock()
	defer u.saveMu.Unlock()

	u.mu.Lock()
	if !u.dirty {
		u.mu.Unlock()
		return nil
	}
	compact := u.compact || u.journalSize >= journalCompactBytes
	var data []byte
	var err error
	if compact {
		data, err = json.Marshal(userFile{Version: userFileVersion, Entries: u.entries})
	} else {
		data, err = u.journalLinesLocked()
	}
	u.dirty, u.compact = false, false
	u.changed = make(map[string]struct{})
	u.mu.Unlock()

	if err == nil {
		if compact {
			err = u.writeSnapshot(data)
		} else {
			err = u.appendJournal(data)
		}
	}
	if err != nil {
		// Whatever was lost from the journal is still in memory, and a failed
		// append may have left a partial line, so the retry rewrites it all.
		u.mu.Lock()
		u.rewriteLocked()
		u.mu.Unlock()
	}
	return err
}

// journalLinesLocked serializes the current state of every changed key.
// Sorted so a save is deterministic.
func (u *UserDict) journalLinesLocked() ([]byte, error) {
	keys := make([]string, 0, len(u.changed))
	for k := range u.changed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for _, k := range keys {
		r := journalRecord{K: k, Del: true}
		if e, ok := u.entries[k]; ok {
			r = journalRecord{K: k, N: e.Count, T: e.Last}
		}
		line, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// writeSnapshot replaces the snapshot, then drops the journal it now contains.
// Writes to a temporary file and renames, so an interrupted save cannot leave a
// truncated file that the next launch would discard.
func (u *UserDict) writeSnapshot(data []byte) error {
	if err := os.MkdirAll(filepath.Dir(u.path), 0o755); err != nil {
		return err
	}
	tmp := u.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, u.path); err != nil {
		return err
	}
	if err := os.Remove(u.journalPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	u.journalSize = 0
	return nil
}

func (u *UserDict) appendJournal(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(u.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(u.journalPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	n, werr := f.Write(data)
	u.journalSize += int64(n)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}
