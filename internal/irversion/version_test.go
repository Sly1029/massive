package irversion

import "testing"

func TestParseVersion(t *testing.T) {
	version, err := Parse("0.1")
	if err != nil {
		t.Fatal(err)
	}
	if version.String() != "0.1" {
		t.Fatalf("version = %q, want 0.1", version.String())
	}
}

func TestParseVersionRejectsNonCanonicalForms(t *testing.T) {
	for _, input := range []string{"", "v0.1", "0", "0.1.0", "00.1", "0.01", "1.-1"} {
		if _, err := Parse(input); err == nil {
			t.Fatalf("Parse(%q) succeeded, want error", input)
		}
	}
}

func TestRangeContainsCurrentVersionOnly(t *testing.T) {
	range01 := MustParseRange(">=0.1 <0.2")
	if !range01.Contains(MustParse("0.1")) {
		t.Fatal("range should include 0.1")
	}
	if range01.Contains(MustParse("0.2")) {
		t.Fatal("range should exclude 0.2")
	}
}
