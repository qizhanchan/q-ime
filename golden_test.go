package main

import (
	"os"
	"testing"

	"github.com/qizhanchan/q-ime/internal/golden"
)

// TestGoldenRankings replays testdata/golden.tsv — every pinned "this input
// answers this word" decision — against the real lexicon. Same file, same
// runner as `qime-audit -mode golden`; this is just the version the machine
// runs on every `go test .`.
//
// Skips where the lexicon is absent, like every other test in this package
// that needs it: the lexicon is GPL data compiled on the developer's machine
// and is not in the repo.
func TestGoldenRankings(t *testing.T) {
	d, err := openLexicon()
	if err != nil {
		t.Skipf("no compiled lexicon: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	data, err := os.ReadFile("testdata/golden.tsv")
	if err != nil {
		t.Fatal(err)
	}
	cases, err := golden.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 40 {
		// The file shrinking below its starting size means cases were deleted;
		// that needs the same justification as deleting a test, and this makes
		// it impossible to do silently.
		t.Fatalf("golden.tsv holds %d cases, expected at least 40", len(cases))
	}

	e, err := golden.NewEngine(d)
	if err != nil {
		t.Fatal(err)
	}
	hasLite := golden.HasTencentLite(d)
	failures, skipped := golden.Run(e, cases, hasLite)
	if skipped > 0 {
		t.Logf("skipped %d lite-only cases: lexicon built without tencent_lite", skipped)
	}
	for _, f := range failures {
		t.Errorf("%s", f)
	}
}
