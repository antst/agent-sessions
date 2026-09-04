package productruntime

import "testing"

func TestVersionAtLeastUsesStrictPortableNumericCores(t *testing.T) {
	for _, test := range []struct {
		value, floor string
		want         bool
	}{
		{"1.18.25", "1.18.25", true}, {"v1.18.25+build.01", "1.18.25", true},
		{"2.0.0", "1.18.25", true}, {"1.19.0", "1.18.25", true},
		{"1.18.26-rc.1+build-2", "1.18.25", true}, {"1.18.25-rc.1", "1.18.25", true},
		{"1.18.24", "1.18.25", false}, {"1.18", "1.18.25", false},
		{"1.18.+25", "1.18.25", false}, {"1.18.25-", "1.18.25", false},
		{"1.18.25+", "1.18.25", false}, {"1.18.25-rc..1", "1.18.25", false},
		{"1.18.25-01", "1.18.25", false}, {"1.18.25+one+two", "1.18.25", false},
		{"1.18.25+bad_1", "1.18.25", false}, {"1.018.25", "1.18.25", false},
		{"4294967295.0.0", "1.18.25", true}, {"4294967296.0.0", "1.18.25", false},
		{"1.18.25", "1.18.25+", false},
	} {
		if got := VersionAtLeast(test.value, test.floor); got != test.want {
			t.Errorf("VersionAtLeast(%q, %q) = %t, want %t", test.value, test.floor, got, test.want)
		}
	}
}
