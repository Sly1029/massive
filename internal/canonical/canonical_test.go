package canonical

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalJSONV0Corpus(t *testing.T) {
	root := filepath.Join("..", "..", "conformance", "fixtures", "canonical-json-v0")
	expectedHashBytes, err := os.ReadFile(filepath.Join(root, "hashes.json"))
	if err != nil {
		t.Fatal(err)
	}
	expectedHashes := map[string]string{}
	if err := json.Unmarshal(expectedHashBytes, &expectedHashes); err != nil {
		t.Fatal(err)
	}
	validFixtureCount := 0

	for _, kind := range []string{"valid", "invalid"} {
		entries, err := os.ReadDir(filepath.Join(root, kind))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s corpus is empty", kind)
		}

		fixtureCount := 0
		for _, entry := range entries {
			if entry.IsDir() {
				t.Fatalf("unexpected nested corpus directory %s", entry.Name())
			}
			fixtureCount++
			if kind == "valid" {
				validFixtureCount++
			}
			t.Run(kind+"/"+entry.Name(), func(t *testing.T) {
				payload, err := os.ReadFile(filepath.Join(root, kind, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				payload = trimFixtureNewline(payload)

				canonicalPayload, err := CanonicalizeJSON(payload)
				if kind == "valid" {
					if err != nil {
						t.Fatalf("canonicalize valid corpus payload: %v", err)
					}
					if !bytes.Equal(canonicalPayload, payload) {
						t.Fatalf("valid corpus payload changed\nactual:   %q\nexpected: %q", canonicalPayload, payload)
					}
					if actual := DigestBytes(canonicalPayload); actual != expectedHashes[entry.Name()] {
						t.Fatalf("valid corpus digest mismatch\nactual:   %s\nexpected: %s", actual, expectedHashes[entry.Name()])
					}
					return
				}

				// Artifact publication accepts a body only when this same
				// canonicalizer reproduces its bytes exactly. An invalid fixture
				// may fail canonicalization outright, or normalize to different
				// bytes (for example, whitespace and lone-surrogate escapes).
				if err == nil && bytes.Equal(canonicalPayload, payload) {
					t.Fatalf("invalid corpus payload was accepted by the canonical byte boundary: %q", payload)
				}
			})
		}
		if fixtureCount == 0 {
			t.Fatalf("%s corpus is empty", kind)
		}
	}
	if len(expectedHashes) != validFixtureCount {
		t.Fatalf("canonical JSON valid fixture hashes = %d, want %d", len(expectedHashes), validFixtureCount)
	}
}

func trimFixtureNewline(payload []byte) []byte {
	if bytes.HasSuffix(payload, []byte("\r\n")) {
		return payload[:len(payload)-2]
	}
	return bytes.TrimSuffix(payload, []byte("\n"))
}

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
