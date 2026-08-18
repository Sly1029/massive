// Package irversion defines the independent semantic version carried by Graph
// IR artifacts. Transport schema versions intentionally remain separate.
package irversion

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a deliberately small major/minor Graph IR version. Patch releases
// belong to the implementation, not the serialized graph contract.
type Version struct {
	Major uint32
	Minor uint32
}

// Current is the Graph IR emitted by this compiler generation.
var Current = MustParse("0.1")

// CompilerRange declares the Graph IR versions accepted by the Go compiler.
// A later incompatible 0.x graph revision must update this range explicitly.
var CompilerRange = MustParseRange(">=0.1 <0.2")

func Parse(input string) (Version, error) {
	majorText, minorText, ok := strings.Cut(input, ".")
	if !ok || strings.Contains(minorText, ".") {
		return Version{}, fmt.Errorf("graph IR version %q must use canonical major.minor form", input)
	}
	major, err := parsePart(majorText)
	if err != nil {
		return Version{}, fmt.Errorf("graph IR version %q: invalid major: %w", input, err)
	}
	minor, err := parsePart(minorText)
	if err != nil {
		return Version{}, fmt.Errorf("graph IR version %q: invalid minor: %w", input, err)
	}
	return Version{Major: major, Minor: minor}, nil
}

func MustParse(input string) Version {
	version, err := Parse(input)
	if err != nil {
		panic(err)
	}
	return version
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

func (v Version) compare(other Version) int {
	if v.Major < other.Major || (v.Major == other.Major && v.Minor < other.Minor) {
		return -1
	}
	if v == other {
		return 0
	}
	return 1
}

func parsePart(input string) (uint32, error) {
	if input == "" || (len(input) > 1 && input[0] == '0') {
		return 0, fmt.Errorf("must be a non-negative integer without leading zeroes")
	}
	value, err := strconv.ParseUint(input, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("must be a non-negative uint32: %w", err)
	}
	return uint32(value), nil
}

type bound struct {
	version   Version
	inclusive bool
}

// Range is a conjunction of at most one lower and one upper version bound.
// It deliberately supports the small range language needed by IR consumers:
// >=0.1 <0.2, >0.1 <=0.2, or =0.1.
type Range struct {
	lower *bound
	upper *bound
}

func ParseRange(input string) (Range, error) {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return Range{}, fmt.Errorf("graph IR version range cannot be empty")
	}

	var parsed Range
	for _, field := range fields {
		operator, versionText := rangeTerm(field)
		version, err := Parse(versionText)
		if err != nil {
			return Range{}, err
		}

		switch operator {
		case "=":
			if parsed.lower != nil || parsed.upper != nil {
				return Range{}, fmt.Errorf("graph IR version range %q has conflicting exact bound", input)
			}
			parsed.lower = &bound{version: version, inclusive: true}
			parsed.upper = &bound{version: version, inclusive: true}
		case ">", ">=":
			if parsed.lower != nil {
				return Range{}, fmt.Errorf("graph IR version range %q has multiple lower bounds", input)
			}
			parsed.lower = &bound{version: version, inclusive: operator == ">="}
		case "<", "<=":
			if parsed.upper != nil {
				return Range{}, fmt.Errorf("graph IR version range %q has multiple upper bounds", input)
			}
			parsed.upper = &bound{version: version, inclusive: operator == "<="}
		default:
			return Range{}, fmt.Errorf("graph IR version range %q has unsupported operator %q", input, operator)
		}
	}

	if parsed.lower != nil && parsed.upper != nil {
		comparison := parsed.lower.version.compare(parsed.upper.version)
		if comparison > 0 || (comparison == 0 && (!parsed.lower.inclusive || !parsed.upper.inclusive)) {
			return Range{}, fmt.Errorf("graph IR version range %q is empty", input)
		}
	}
	return parsed, nil
}

func MustParseRange(input string) Range {
	value, err := ParseRange(input)
	if err != nil {
		panic(err)
	}
	return value
}

func (r Range) Contains(version Version) bool {
	if r.lower != nil {
		comparison := version.compare(r.lower.version)
		if comparison < 0 || (comparison == 0 && !r.lower.inclusive) {
			return false
		}
	}
	if r.upper != nil {
		comparison := version.compare(r.upper.version)
		if comparison > 0 || (comparison == 0 && !r.upper.inclusive) {
			return false
		}
	}
	return true
}

func (r Range) String() string {
	parts := make([]string, 0, 2)
	if r.lower != nil {
		operator := ">"
		if r.lower.inclusive {
			operator = ">="
		}
		parts = append(parts, operator+r.lower.version.String())
	}
	if r.upper != nil {
		operator := "<"
		if r.upper.inclusive {
			operator = "<="
		}
		parts = append(parts, operator+r.upper.version.String())
	}
	return strings.Join(parts, " ")
}

func rangeTerm(input string) (string, string) {
	for _, operator := range []string{">=", "<=", ">", "<", "="} {
		if version, found := strings.CutPrefix(input, operator); found {
			return operator, version
		}
	}
	return "=", input
}
