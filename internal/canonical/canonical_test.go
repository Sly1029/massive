package canonical

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDigestJSONGoldenVector(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", "hashing", "canonical-input.json"))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", "hashing", "canonical-input.sha256"))
	if err != nil {
		t.Fatal(err)
	}

	actual, err := DigestJSON(input)
	if err != nil {
		t.Fatal(err)
	}

	if actual != strings.TrimSpace(string(expected)) {
		t.Fatalf("digest mismatch\nactual:   %s\nexpected: %s", actual, strings.TrimSpace(string(expected)))
	}
}

func TestCanonicalizeJSONEscaping(t *testing.T) {
	input := []byte(`{"unsafe":"<>&\u2028\u2029","control":"\u0001\n"}`)

	actual, err := CanonicalizeJSON(input)
	if err != nil {
		t.Fatal(err)
	}

	expected := `{"control":"\u0001\n","unsafe":"<>&` + "\u2028" + "\u2029" + `"}`
	if string(actual) != expected {
		t.Fatalf("canonical JSON mismatch\nactual:   %q\nexpected: %q", actual, expected)
	}
}

func TestCanonicalizeJSONRejectsNonSafeIntegers(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "fraction", input: `{"n":1.5}`},
		{name: "exponent", input: `{"n":1e3}`},
		{name: "unsafe", input: `{"n":9007199254740992}`},
		{name: "negative unsafe", input: `{"n":-9007199254740992}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CanonicalizeJSON([]byte(test.input)); err == nil {
				t.Fatalf("expected canonicalization error for %s", test.input)
			}
		})
	}
}

func TestDigestJSONWithRootMemberExcluded(t *testing.T) {
	withMember, err := DigestJSONWithRootMemberExcluded([]byte(`{"a":1,"self":"ignored"}`), "self")
	if err != nil {
		t.Fatal(err)
	}
	withoutMember, err := DigestJSON([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}

	if withMember != withoutMember {
		t.Fatalf("self-excluded digest mismatch: %s != %s", withMember, withoutMember)
	}
}
