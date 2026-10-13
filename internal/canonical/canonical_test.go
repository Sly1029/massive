package canonical

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
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

func TestCanonicalJSONDoubleVectors(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", "canonical-json-v0", "numbers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []struct {
			Bits      string
			Canonical *string
			Hash      *string
		}
		Sources []struct {
			Source    string
			Canonical *string
		}
	}
	if err := json.Unmarshal(body, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors.Cases {
		t.Run(vector.Bits, func(t *testing.T) {
			bits, err := strconv.ParseUint(vector.Bits, 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			value := math.Float64frombits(bits)
			encoded, err := Marshal(value)
			if vector.Canonical == nil {
				if err == nil {
					t.Fatalf("non-finite double accepted: %s", encoded)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != *vector.Canonical || DigestBytes(encoded) != *vector.Hash {
				t.Fatalf("got %s (%s), want %s (%s)", encoded, DigestBytes(encoded), *vector.Canonical, *vector.Hash)
			}
			var decoded float64
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if value != 0 && math.Float64bits(decoded) != bits {
				t.Fatalf("double changed: %x", math.Float64bits(decoded))
			}
			if value == 0 && math.Signbit(decoded) {
				t.Fatal("negative zero survived normalization")
			}
			again, err := CanonicalizeJSON(encoded)
			if err != nil || !bytes.Equal(again, encoded) {
				t.Fatalf("round trip changed: %s, %v", again, err)
			}
		})
	}
	for _, vector := range vectors.Sources {
		encoded, err := CanonicalizeJSON([]byte(vector.Source))
		if vector.Canonical == nil {
			if err == nil {
				t.Fatalf("invalid number accepted: %s", vector.Source)
			}
		} else if err != nil || string(encoded) != *vector.Canonical {
			t.Fatalf("canonicalize %s = %s, %v; want %s", vector.Source, encoded, err, *vector.Canonical)
		}
	}
}

func FuzzCanonicalFloatRoundTrip(f *testing.F) {
	for _, bits := range []uint64{0, 1, 1 << 63, 0x7fefffffffffffff, 0x4340000000000000, 0x444b1ae4d6e2ef50} {
		f.Add(bits)
	}
	f.Fuzz(func(t *testing.T, bits uint64) {
		value := math.Float64frombits(bits)
		body, err := Marshal(value)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			if err == nil {
				t.Fatal("non-finite value accepted")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		var decoded float64
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		if value != 0 && math.Float64bits(decoded) != bits {
			t.Fatalf("%x -> %s -> %x", bits, body, math.Float64bits(decoded))
		}
		again, err := CanonicalizeJSON(body)
		if err != nil || !bytes.Equal(body, again) {
			t.Fatalf("canonical bytes changed: %s -> %s, %v", body, again, err)
		}
	})
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

func TestSelfExclusionRejectsNumericOverflowBeforeDeletingMember(t *testing.T) {
	for _, body := range []string{`{"a":1,"self":1e400}`, `{"a":1,"self":{"nested":-1e400}}`} {
		if digest, err := DigestJSONWithRootMemberExcluded([]byte(body), "self"); err == nil {
			t.Fatalf("overflow acquired identity %s", digest)
		}
	}
}
