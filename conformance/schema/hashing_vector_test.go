package schema_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestCanonicalHashingGoldenVector(t *testing.T) {
	inputBytes, err := os.ReadFile(filepath.Join("..", "fixtures", "hashing", "canonical-input.json"))
	if err != nil {
		t.Fatal(err)
	}
	expectedBytes, err := os.ReadFile(filepath.Join("..", "fixtures", "hashing", "canonical-input.sha256"))
	if err != nil {
		t.Fatal(err)
	}

	decoder := json.NewDecoder(bytes.NewReader(inputBytes))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}

	canonical, err := canonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(canonical)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	expected := strings.TrimSpace(string(expectedBytes))
	if actual != expected {
		t.Fatalf("canonical hash mismatch\nactual:   %s\nexpected: %s\njson:     %s", actual, expected, canonical)
	}
}

func canonicalJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	if err := writeCanonicalJSON(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// canonicalIntegerPattern is the v0 number restriction from
// conformance/schema/hashing.md: canonical safe integers only.
var canonicalIntegerPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

const maxSafeInteger = 1<<53 - 1

func writeCanonicalJSON(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case json.Number:
		if err := validateCanonicalInteger(typed); err != nil {
			return err
		}
		out.WriteString(typed.String())
	case string:
		out.WriteString(jsonQuote(typed))
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonicalJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		// ECMAScript Object.keys().sort() order (UTF-16 code units), per
		// conformance/schema/hashing.md — not UTF-8 byte order.
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })

		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			out.WriteString(jsonQuote(key))
			out.WriteByte(':')
			if err := writeCanonicalJSON(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value type %T", typed)
	}
	return nil
}

func validateCanonicalInteger(number json.Number) error {
	text := number.String()
	if !canonicalIntegerPattern.MatchString(text) {
		return fmt.Errorf("non-canonical number %q: v0 canonical field trees restrict numbers to canonical safe integers (conformance/schema/hashing.md)", text)
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parsed > maxSafeInteger || parsed < -maxSafeInteger {
		return fmt.Errorf("number %q exceeds the safe-integer range ±(2^53-1) (conformance/schema/hashing.md)", text)
	}
	return nil
}

// lessUTF16 compares strings by UTF-16 code units, matching ECMAScript
// string comparison used by Object.keys().sort().
func lessUTF16(a, b string) bool {
	unitsA := utf16.Encode([]rune(a))
	unitsB := utf16.Encode([]rune(b))
	for i := 0; i < len(unitsA) && i < len(unitsB); i++ {
		if unitsA[i] != unitsB[i] {
			return unitsA[i] < unitsB[i]
		}
	}
	return len(unitsA) < len(unitsB)
}

// jsonQuote replicates JSON.stringify string serialization exactly: named
// escapes for \" \\ \b \t \n \f \r, \u00XX for other control characters
// below U+0020, and everything else raw — including < > & U+2028 U+2029.
func jsonQuote(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}

// The realized-environment vectors are checked here with an independent
// canonicalizer: each recipe field tree is rebuilt from the record's JSON.
func TestRealizedEnvironmentHashingVectors(t *testing.T) {
	for _, name := range []string{"realized-environment-locked-v1.json", "realized-environment-direct-v1.json"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "fixtures", "hashing", name))
			if err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			var record map[string]any
			if err := decoder.Decode(&record); err != nil {
				t.Fatal(err)
			}
			requirement := record["requirement"].(map[string]any)
			dependencies, ok := requirement["dependencies"]
			if !ok {
				dependencies = []any{}
			}
			requirementTree := map[string]any{
				"recipe": "python-requirement", "recipeVersion": json.Number("1"),
				"requiresPython": requirement["requiresPython"], "lockHash": requirement["lockHash"],
				"dependencies": dependencies,
			}
			realizationTree := map[string]any{"recipe": "existing-python", "recipeVersion": json.Number("1"), "cacheTag": nil}
			for member, value := range record["realization"].(map[string]any) {
				realizationTree[member] = value
			}
			for tree, member := range map[*map[string]any]string{&requirementTree: "requirementHash", &realizationTree: "realizationHash"} {
				canonical, err := canonicalJSON(*tree)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(canonical)
				if actual := "sha256:" + hex.EncodeToString(sum[:]); actual != record[member] {
					t.Fatalf("%s = %s, record has %s\njson: %s", member, actual, record[member], canonical)
				}
			}
		})
	}
}
