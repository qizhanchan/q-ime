package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveAppendsInsteadOfRewriting pins the point of the journal: once a
// snapshot exists, learning one more word must not rewrite it.
func TestSaveAppendsInsteadOfRewriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	u := NewUserDict(path)
	u.ForgetAll() // forces the first save to write a snapshot
	u.Record([]string{"ni", "hao"}, "你好")
	if err := u.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no snapshot after a compacting save: %v", err)
	}

	u.Record([]string{"shi", "jie"}, "世界")
	if err := u.Save(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("an ordinary save rewrote the snapshot")
	}
	j, err := os.ReadFile(path + ".journal")
	if err != nil {
		t.Fatalf("no journal: %v", err)
	}
	if lines := strings.Count(string(j), "\n"); lines != 1 {
		t.Errorf("journal has %d lines, want 1:\n%s", lines, j)
	}

	r := NewUserDict(path)
	if r.Len() != 2 {
		t.Fatalf("reloaded %d entries, want 2", r.Len())
	}
	if _, ok := r.Boost([]string{"shi", "jie"}, "世界"); !ok {
		t.Error("journaled entry lost on reload")
	}
}

// TestJournalReplaysDeletes: Forget is journaled as a deletion and must not
// come back from the snapshot on the next launch.
func TestJournalReplaysDeletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	u := NewUserDict(path)
	u.ForgetAll()
	u.Record([]string{"guan"}, "关")
	if err := u.Save(); err != nil {
		t.Fatal(err)
	}
	if u.Forget("", []string{"guan"}, "关") == 0 {
		t.Fatal("nothing forgotten")
	}
	if err := u.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".journal"); err != nil {
		t.Fatalf("forget was not journaled: %v", err)
	}
	if n := NewUserDict(path).Len(); n != 0 {
		t.Errorf("reloaded %d entries, want the forgotten one gone", n)
	}
}

// TestCompactionFoldsTheJournal: past the threshold, the next save rewrites
// the snapshot and the journal goes away with nothing lost.
func TestCompactionFoldsTheJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	u := NewUserDict(path)
	u.Record([]string{"a"}, "啊")
	if err := u.Save(); err != nil {
		t.Fatal(err)
	}
	u.journalSize = journalCompactBytes
	u.Record([]string{"b"}, "不")
	if err := u.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".journal"); !os.IsNotExist(err) {
		t.Errorf("journal survived compaction: %v", err)
	}
	if n := NewUserDict(path).Len(); n != 2 {
		t.Errorf("reloaded %d entries after compaction, want 2", n)
	}
}

// TestTornJournalTail: a save cut off mid-line loses that record and nothing
// else, and the next save compacts rather than appending onto the fragment.
func TestTornJournalTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	u := NewUserDict(path)
	u.Record([]string{"ni"}, "你")
	if err := u.Save(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path+".journal", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"k":"wo\u0000我","n":`)
	f.Close()

	r := NewUserDict(path)
	if r.Len() != 1 {
		t.Fatalf("reloaded %d entries, want the intact one only", r.Len())
	}
	if !r.compact {
		t.Error("a torn journal did not schedule a compaction")
	}
	r.Record([]string{"ta"}, "他")
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if n := NewUserDict(path).Len(); n != 2 {
		t.Errorf("reloaded %d entries, want 2", n)
	}
}

// TestSaveWithoutChangesWritesNothing: an idle input method must not touch
// the disk at all.
func TestSaveWithoutChangesWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	u := NewUserDict(path)
	if err := u.Save(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + ".journal"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s written with nothing to save", filepath.Base(p))
		}
	}
}
