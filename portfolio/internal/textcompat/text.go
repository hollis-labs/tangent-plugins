// Package textcompat preserves tracker string behavior across the Go boundary.
package textcompat

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Lower uses language-neutral full Unicode lowercasing, including contextual
// final sigma and expanding dotted I. Simple rune lowercasing loses matches.
func Lower(s string) string { return cases.Lower(language.Und).String(s) }

// Terms splits ECMAScript whitespace, rather than treating U+0085 as whitespace
// or leaving the ECMAScript BOM inside a search term.
func Terms(s string) []string { return strings.FieldsFunc(Lower(s), Space) }

// Space is the whitespace set accepted by ECMAScript trim and regular expressions.
func Space(r rune) bool {
	return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' || r == ' ' || r == 0xa0 || r == 0x1680 || (r >= 0x2000 && r <= 0x200a) || r == 0x2028 || r == 0x2029 || r == 0x202f || r == 0x205f || r == 0x3000 || r == 0xfeff
}

// Trim preserves non-ECMAScript whitespace and trims the BOM as Node does.
func Trim(s string) string { return strings.TrimFunc(s, Space) }

// Less compares UTF-16 code units, as JavaScript's relational string operator.
func Less(a, b string) bool {
	av, bv := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(av) && i < len(bv); i++ {
		if av[i] != bv[i] {
			return av[i] < bv[i]
		}
	}
	return len(av) < len(bv)
}

// String converts JSON values as String(value) does for tracker filters and
// searchable text/label entries. Scalar numbers elsewhere remain unsearchable.
func String(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		n, err := x.Float64()
		if err != nil {
			return string(x)
		}
		if n == 0 {
			return "0"
		}
		if math.Abs(n) >= 1e-6 && math.Abs(n) < 1e21 {
			return strconv.FormatFloat(n, 'f', -1, 64)
		}
		s := strconv.FormatFloat(n, 'e', -1, 64)
		if i := strings.IndexByte(s, 'e'); i >= 0 {
			e, _ := strconv.Atoi(s[i+1:])
			sign := ""
			if e >= 0 {
				sign = "+"
			}
			return s[:i] + "e" + sign + strconv.Itoa(e)
		}
		return s
	case []any:
		parts := []string{}
		for _, v := range x {
			if v == nil {
				parts = append(parts, "")
			} else {
				parts = append(parts, String(v))
			}
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return fmt.Sprint(v)
	}
}

// Truthy is JavaScript JSON-value truthiness; empty arrays/objects are true.
func Truthy(v any) bool {
	if v == nil {
		return false
	}
	switch t := v.(type) {
	case string:
		return t != ""
	case bool:
		return t
	case json.Number:
		n, _ := t.Float64()
		return n != 0
	}
	return true
}

// Integer parses JSON integer values exactly, including decimal/exponent
// notation. Float64 conversion can round a fractional revision into an integer
// or an out-of-range value into a different counter, so revisions never use it.
func Integer(n json.Number) (int64, bool) {
	raw := string(n)
	if len(raw) > 1024 {
		return 0, false
	}
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(raw[i+1:])
		if err != nil || exponent > 1024 || exponent < -1024 {
			coefficient := strings.Trim(raw[:i], "-+0.")
			if coefficient == "" {
				return 0, true
			}
			return 0, false
		}
	}
	value, ok := new(big.Rat).SetString(raw)
	if !ok || !value.IsInt() || !value.Num().IsInt64() {
		return 0, false
	}
	return value.Num().Int64(), true
}
