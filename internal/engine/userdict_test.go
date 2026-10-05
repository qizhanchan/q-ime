package engine

import (
	"path/filepath"
	"testing"
	"time"
)

// TestEvictionKeepsEarnedEntriesOverStaleOnes pins what "least valuable"
// means. Eviction used to sort on Last alone, which threw away a word picked
// fifty times three months ago before a word mistyped once two months ago —
// deleting exactly the long-lived vocabulary the dictionary exists to keep.
// The eviction key is now the boost arithmetic itself, so an entry's earned
// frequency counts for something even after its recency has decayed.
func TestEvictionKeepsEarnedEntriesOverStaleOnes(t *testing.T) {
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	now := time.Now()

	// A word this user genuinely lives in, last touched three months ago.
	u.clock = func() time.Time { return now.Add(-90 * 24 * time.Hour) }
	for i := 0; i < 50; i++ {
		u.Record([]string{"shen", "zhen"}, "深圳")
	}
	// A one-off, more recent but barely used.
	u.clock = func() time.Time { return now.Add(-60 * 24 * time.Hour) }
	u.Record([]string{"shi"}, "试")

	u.clock = func() time.Time { return now }
	u.mu.Lock()
	u.evictLocked(1)
	u.mu.Unlock()

	if _, ok := u.Boost([]string{"shen", "zhen"}, "深圳"); !ok {
		t.Error("eviction dropped the fifty-pick word instead of the one-off")
	}
	if _, ok := u.Boost([]string{"shi"}, "试"); ok {
		t.Error("the one-off survived eviction ahead of a far better-earned entry")
	}
}

// TestEvictionRebuildsTheDerivedIndexes: whatever the eviction order, the
// literal/successor/phrase indexes are derived data and must not keep pointing
// at entries that are gone.
func TestEvictionRebuildsTheDerivedIndexes(t *testing.T) {
	u := NewUserDict(filepath.Join(t.TempDir(), "user.json"))
	now := time.Now()
	u.clock = func() time.Time { return now.Add(-30 * 24 * time.Hour) }
	u.RecordLiteral("gh", "GitHub")
	u.clock = func() time.Time { return now }
	u.RecordLiteral("af", "AfterShip")

	u.mu.Lock()
	u.evictLocked(1)
	u.mu.Unlock()

	if ls := u.LiteralsFor("gh"); len(ls) != 0 {
		t.Errorf("LiteralsFor(gh) = %v after its entry was evicted", ls)
	}
	if ls := u.LiteralsFor("af"); len(ls) != 1 || ls[0].Word != "AfterShip" {
		t.Errorf("LiteralsFor(af) = %v, want the surviving literal", ls)
	}
}
