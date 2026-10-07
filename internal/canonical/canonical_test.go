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
				payload = canonicalFixturePayload(t, payload)

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
				// bytes (for example, insignificant whitespace).
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

func canonicalFixturePayload(t *testing.T, fixture []byte) []byte {
	t.Helper()
	if !bytes.HasSuffix(fixture, []byte("\n")) || bytes.HasSuffix(fixture, []byte("\r\n")) {
		t.Fatal("canonical fixture must use exactly one final LF as repository transport")
	}
	return fixture[:len(fixture)-1]
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

func TestMarshalCanonicalizesTypedValuesAndRawMessages(t *testing.T) {
	actual, err := Marshal(struct {
		Values []json.RawMessage `json:"values"`
		Unsafe string            `json:"unsafe"`
	}{
		Values: []json.RawMessage{json.RawMessage(`{"b":2,"a":1}`)},
		Unsafe: "<>&",
	})
	if err != nil {
		t.Fatal(err)
	}
	if expected := `{"unsafe":"<>&","values":[{"a":1,"b":2}]}`; string(actual) != expected {
		t.Fatalf("canonical JSON mismatch\nactual:   %s\nexpected: %s", actual, expected)
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

func TestCanonicalJSONRejectsMalformedUnicode(t *testing.T) {
	for name, input := range map[string][]byte{
		"lone high surrogate":    []byte(`{"value":"\ud800"}`),
		"lone low surrogate":     []byte(`{"value":"\udc00"}`),
		"unpaired surrogate key": []byte(`{"\ud800":1}`),
		"invalid UTF-8 value":    append(append([]byte(`{"value":"`), 0xff), []byte(`"}`)...),
		"invalid UTF-8 key":      append(append([]byte(`{"`), 0xff), []byte(`":1}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if result, err := CanonicalizeJSON(input); err == nil {
				t.Fatalf("malformed Unicode silently changed to %s", result)
			}
			if digest, err := DigestJSON(input); err == nil {
				t.Fatalf("malformed Unicode acquired identity %s", digest)
			}
			if digest, err := DigestJSONWithRootMemberExcluded(input, "self"); err == nil {
				t.Fatalf("self-exclusion accepted malformed Unicode: %s", digest)
			}
		})
	}
	valid := []byte(`{"value":"\ud83d\ude00"}`)
	actual, err := CanonicalizeJSON(valid)
	if err != nil || string(actual) != `{"value":"😀"}` {
		t.Fatalf("valid surrogate pair: %s, %v", actual, err)
	}
}

func TestCanonicalJSONRejectsDuplicateObjectNames(t *testing.T) {
	for _, input := range []string{
		`{"value":1,"value":2}`,
		`{"value":{"key":1,"key":2}}`,
		`{"key":1,"\u006bey":2}`,
	} {
		if canonical, err := CanonicalizeJSON([]byte(input)); err == nil {
			t.Errorf("duplicate names silently collapsed: %s -> %s", input, canonical)
		}
	}
}

func TestMarshalRejectsInvalidUTF8BeforeEncoding(t *testing.T) {
	invalid := string([]byte{0xff})
	for _, value := range []any{invalid, map[string]string{invalid: "value"}, map[string]string{"key": invalid}} {
		if result, err := Marshal(value); err == nil {
			t.Errorf("invalid Go string silently changed to %s", result)
		}
	}
}
