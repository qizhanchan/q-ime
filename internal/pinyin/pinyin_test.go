package pinyin

import (
	"reflect"
	"testing"
)

func TestTranspositions(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		why  string
	}{
		{"ping", []string{"ipng", "pnig", "pign"}, "one swap per adjacent pair"},
		{"zhi", []string{"hzi", "zih"}, "the h-position slips rime-ice lists as rules"},
		{"a", nil, "nothing to swap"},
		{"", nil, "empty"},
		{"aa", nil, "swapping equal letters would only duplicate the exact arc"},
		{"gaa", []string{"aga"}, "the equal pair is skipped, the differing one is not"},
	}
	for _, c := range cases {
		if got := Transpositions(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Transpositions(%q) = %v, want %v (%s)", c.in, got, c.want, c.why)
		}
	}
}

// TestTranspositionsIsItsOwnInverse is why one function serves both
// directions: the set of spellings s could be a mistyping OF is the same as
// the set s transposes INTO.
func TestTranspositionsIsItsOwnInverse(t *testing.T) {
	for _, s := range []string{"ping", "zhi", "guang", "jiong", "dao"} {
		for _, v := range Transpositions(s) {
			if !contains(Transpositions(v), s) {
				t.Errorf("%q transposes to %q but not back", s, v)
			}
		}
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func TestIsInitial(t *testing.T) {
	for _, s := range []string{"b", "zh", "ch", "sh", "y", "w", "a", "o", "e"} {
		if !IsInitial(s) {
			t.Errorf("IsInitial(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "i", "u", "v", "ni", "zz", "ng"} {
		if IsInitial(s) {
			t.Errorf("IsInitial(%q) = true, want false", s)
		}
	}
}

func TestFuzzyOffIsIdentity(t *testing.T) {
	var f Fuzzy
	if f.AnyEnabled() {
		t.Fatal("zero Fuzzy reports rules enabled")
	}
	got := f.Variants("zhang")
	if !reflect.DeepEqual(got, []string{"zhang"}) {
		t.Errorf("Variants = %v, want just the input", got)
	}
}

func TestFuzzyVariants(t *testing.T) {
	cases := []struct {
		name  string
		fuzzy Fuzzy
		in    string
		want  []string // must all be present
	}{
		{"initial zh↔z", Fuzzy{ZhZ: true}, "zhang", []string{"zhang", "zang"}},
		{"initial z↔zh", Fuzzy{ZhZ: true}, "zang", []string{"zang", "zhang"}},
		{"final ang↔an", Fuzzy{AngAn: true}, "shang", []string{"shang", "shan"}},
		{"final ing↔in", Fuzzy{IngIn: true}, "jing", []string{"jing", "jin"}},
		{"both axes combine", Fuzzy{ZhZ: true, AngAn: true}, "zhang",
			[]string{"zhang", "zang", "zhan", "zan"}},
	}
	for _, tc := range cases {
		got := tc.fuzzy.Variants(tc.in)
		index := map[string]bool{}
		for _, g := range got {
			index[g] = true
		}
		for _, w := range tc.want {
			if !index[w] {
				t.Errorf("%s: Variants(%q) = %v, missing %q", tc.name, tc.in, got, w)
			}
		}
		if got[0] != tc.in {
			t.Errorf("%s: Variants(%q)[0] = %q, want the input first", tc.name, tc.in, got[0])
		}
	}
}

// A fuzzy rule must not eat a whole syllable: "n" fuzzing to "" (or "ang"
// rewriting itself to "an") would produce readings nobody typed.
func TestFuzzyNeverConsumesTheWholeSyllable(t *testing.T) {
	f := Fuzzy{NL: true, AngAn: true, ZhZ: true}
	for _, in := range []string{"n", "l", "ang", "an", "zh", "z"} {
		for _, v := range f.Variants(in) {
			if v == "" {
				t.Errorf("Variants(%q) produced an empty syllable", in)
			}
		}
		if got := f.Variants("ang"); len(got) != 1 || got[0] != "ang" {
			t.Errorf(`Variants("ang") = %v, want it left alone`, got)
		}
	}
}

func TestNoDuplicateVariants(t *testing.T) {
	f := Fuzzy{ZhZ: true, ChC: true, ShS: true, AngAn: true, EngEn: true, IngIn: true}
	for _, in := range []string{"zhang", "sheng", "jing", "chan", "ni"} {
		seen := map[string]bool{}
		for _, v := range f.Variants(in) {
			if seen[v] {
				t.Errorf("Variants(%q) repeated %q", in, v)
			}
			seen[v] = true
		}
	}
}

// rime-ice spells ü as v, so a user typing the natural "nue"/"lue" has to
// reach the same entries as "nve"/"lve".
func TestNormalizeUmlaut(t *testing.T) {
	cases := map[string]string{
		"nue":  "nve",
		"lue":  "lve",
		"nve":  "nve",
		"nüe":  "nve",
		"nuan": "nuan", // a real syllable, must survive untouched
		"luan": "luan",
		"ni":   "ni",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripSeparators(t *testing.T) {
	cases := []struct {
		in     string
		clean  string
		breaks []int
	}{
		{"nihao", "nihao", nil},
		{"xi'an", "xian", []int{2}},
		{"a'b'c", "abc", []int{1, 2}},
		// A leading apostrophe marks no boundary — there is nothing before it.
		{"'ni", "ni", nil},
	}
	for _, tc := range cases {
		clean, breaks := StripSeparators(tc.in)
		if clean != tc.clean {
			t.Errorf("StripSeparators(%q) clean = %q, want %q", tc.in, clean, tc.clean)
		}
		for _, b := range tc.breaks {
			if !breaks[b] {
				t.Errorf("StripSeparators(%q) missing break at %d (got %v)", tc.in, b, breaks)
			}
		}
		if len(tc.breaks) != len(breaks) {
			t.Errorf("StripSeparators(%q) breaks = %v, want %v", tc.in, breaks, tc.breaks)
		}
	}
}
