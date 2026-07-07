package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

const maxSafeInteger = 1<<53 - 1

var canonicalIntegerPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// CanonicalizeJSON parses a JSON field tree and writes the v0 canonical JSON
// representation from conformance/schema/hashing.md.
func CanonicalizeJSON(data []byte) ([]byte, error) {
	value, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}

	return canonicalize(value)
}

// DigestJSON returns the sha256:<hex> digest for a JSON field tree.
func DigestJSON(data []byte) (string, error) {
	canonical, err := CanonicalizeJSON(data)
	if err != nil {
		return "", err
	}

	return DigestBytes(canonical), nil
}

// DigestBytes returns the sha256:<hex> digest for an exact byte payload.
func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DigestJSONWithRootMemberExcluded applies the hashing self-exclusion rule:
// the named root object member is absent from the field tree before hashing.
func DigestJSONWithRootMemberExcluded(data []byte, member string) (string, error) {
	value, err := decodeJSON(data)
	if err != nil {
		return "", err
	}

	object, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("self-exclusion requires a root JSON object")
	}
	delete(object, member)

	canonical, err := canonicalize(object)
	if err != nil {
		return "", err
	}

	return DigestBytes(canonical), nil
}

// LessUTF16 compares strings by ECMAScript UTF-16 code-unit order.
func LessUTF16(a, b string) bool {
	unitsA := utf16.Encode([]rune(a))
	unitsB := utf16.Encode([]rune(b))
	for i := 0; i < len(unitsA) && i < len(unitsB); i++ {
		if unitsA[i] != unitsB[i] {
			return unitsA[i] < unitsB[i]
		}
	}
	return len(unitsA) < len(unitsB)
}

func decodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON field tree: %w", err)
	}
	if err := decoder.Decode(new(struct{})); err != io.EOF {
		return nil, fmt.Errorf("decode JSON field tree: trailing JSON content")
	}

	return value, nil
}

func canonicalize(value any) ([]byte, error) {
	var out bytes.Buffer
	if err := writeCanonicalJSON(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeCanonicalJSON(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
			return nil
		}
		out.WriteString("false")
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
		sort.Slice(keys, func(i, j int) bool { return LessUTF16(keys[i], keys[j]) })

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
		return fmt.Errorf("non-canonical number %q: v0 canonical field trees restrict numbers to canonical safe integers", text)
	}

	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("parse canonical integer %q: %w", text, err)
	}
	if parsed > maxSafeInteger || parsed < -maxSafeInteger {
		return fmt.Errorf("number %q exceeds the safe-integer range ±(2^53-1)", text)
	}

	return nil
}

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
				continue
			}
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.String()
}
