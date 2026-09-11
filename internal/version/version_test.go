package version

import "testing"

func TestStableVersionOrdering(t *testing.T) {
	for _, tt := range []struct {
		a, b  string
		cmp   int
		valid bool
	}{
		{"1.10.0", "1.9.99", 1, true},
		{"2.0.0", "10.0.0", -1, true},
		{"1.2.3", "1.2.3", 0, true},
		{"0.0.0", "0.0.1", -1, true},
		{"18446744073709551616.0.0", "18446744073709551615.0.0", 1, true},
		{"1.0.0-beta.1", "1.0.0", 0, false},
		{"dev", "1.0.0", 0, false},
		{"01.2.3", "1.2.3", 0, false},
		{"1.2", "1.2.0", 0, false},
		{"1.2.3+build", "1.2.3", 0, false},
	} {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			cmp, valid := Compare(tt.a, tt.b)
			if cmp != tt.cmp || valid != tt.valid {
				t.Fatalf("Compare(%q, %q) = %d, %v", tt.a, tt.b, cmp, valid)
			}
		})
	}
}

func TestReleaseTagMustBeCanonicalStable(t *testing.T) {
	for _, tag := range []string{"1.2.3", "v1.2", "v01.2.3", "v1.2.3-beta.1", "v1.2.3+build", "v1.2.3\n", "v1.2.3/other"} {
		if _, ok := FromTag(tag); ok {
			t.Errorf("accepted invalid release tag %q", tag)
		}
	}
	if v, ok := FromTag("v1.2.3"); !ok || v != "1.2.3" {
		t.Fatalf("valid tag rejected: %s / %v", v, ok)
	}
}
