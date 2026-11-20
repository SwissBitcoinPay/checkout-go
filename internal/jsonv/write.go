package jsonv

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Members is a JSON object to write, its members in this order (JSON.stringify writes an object's
// members in the order they were set).
type Members []Member

// Member is one member of Members.
type Member struct {
	Name  string
	Value any
}

// Raw is JSON text written as it is.
type Raw []byte

// Marshal writes v as JSON.stringify(v) does: no whitespace, members in their order, strings with
// only `"`, `\` and the control characters escaped (\b \t \n \f \r, others \u00xx), a lone
// surrogate as its \udxxx escape, numbers as ECMAScript writes them. v holds nil, bool, the
// integer types, float64, string, []string, [][]string, []any, Members, *Object and Raw; any
// other type panics: it is a programming error.
func Marshal(v any) []byte {
	return Append(nil, v)
}

// Append appends the JSON of v (as Marshal writes it) to dst.
func Append(dst []byte, v any) []byte {
	switch v := v.(type) {
	case nil:
		return append(dst, "null"...)
	case bool:
		if v {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case int:
		return strconv.AppendInt(dst, int64(v), 10)
	case int64:
		return strconv.AppendInt(dst, v, 10)
	case uint64:
		return strconv.AppendUint(dst, v, 10)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return append(dst, "null"...)
		}
		return append(dst, NumberString(v)...)
	case string:
		return AppendString(dst, v)
	case []string:
		dst = append(dst, '[')
		for i, s := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendString(dst, s)
		}
		return append(dst, ']')
	case [][]string:
		dst = append(dst, '[')
		for i, s := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = Append(dst, s)
		}
		return append(dst, ']')
	case []any:
		dst = append(dst, '[')
		for i, item := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = Append(dst, item)
		}
		return append(dst, ']')
	case Members:
		dst = append(dst, '{')
		for i, m := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendString(dst, m.Name)
			dst = append(dst, ':')
			dst = Append(dst, m.Value)
		}
		return append(dst, '}')
	case *Object:
		dst = append(dst, '{')
		for i, name := range v.keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendString(dst, name)
			dst = append(dst, ':')
			dst = Append(dst, v.values[name])
		}
		return append(dst, '}')
	case Raw:
		return append(dst, v...)
	default:
		panic(fmt.Sprintf("jsonv: cannot write %T", v))
	}
}

const hexDigits = "0123456789abcdef"

// AppendString appends s as a JSON string, as JSON.stringify writes it. A lone surrogate (WTF-8)
// is written as its escape; any other byte that is not UTF-8 (a programming error: no text of this
// package holds one) as U+FFFD.
func AppendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\r':
				dst = append(dst, '\\', 'r')
			default:
				if c < 0x20 {
					dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
				} else {
					dst = append(dst, c)
				}
			}
			i++
			continue
		}
		if unit, ok := IsSurrogateAt(s, i); ok {
			dst = append(dst, '\\', 'u',
				hexDigits[unit>>12], hexDigits[unit>>8&0xF], hexDigits[unit>>4&0xF], hexDigits[unit&0xF])
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			dst = append(dst, "\xef\xbf\xbd"...) // U+FFFD
			i++
			continue
		}
		dst = append(dst, s[i:i+size]...)
		i += size
	}
	return append(dst, '"')
}

// NumberString writes a finite double as ECMAScript's Number::toString does: the shortest digits
// that read back as the same double; plain from 10^-6 to below 10^21, with an exponent otherwise
// ("1e+21", "1.5e-7"); -0 as "0".
func NumberString(f float64) string {
	if f == 0 {
		return "0"
	}
	if f < 0 {
		return "-" + NumberString(-f)
	}
	// d.ddddde±x: the shortest digits and the exponent of the first one.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exponent, _ := strings.Cut(e, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	exp, _ := strconv.Atoi(exponent)
	k := len(digits)
	n := exp + 1 // the value is 0.digits × 10^n
	switch {
	case k <= n && n <= 21:
		return digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return "0." + strings.Repeat("0", -n) + digits
	}
	sign := "+"
	if n-1 < 0 {
		sign = "-"
	}
	abs := n - 1
	if abs < 0 {
		abs = -abs
	}
	if k == 1 {
		return digits + "e" + sign + strconv.Itoa(abs)
	}
	return digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(abs)
}

// Indent writes v as JSON.stringify(v, null, indent) does: each member and item on its own line,
// indent repeated by depth, an empty object or array as {} or [].
func Indent(v any, indent string) []byte {
	return appendIndent(nil, v, indent, "")
}

func appendIndent(dst []byte, v any, indent, prefix string) []byte {
	inner := prefix + indent
	switch v := v.(type) {
	case *Object:
		if v.Len() == 0 {
			return append(dst, "{}"...)
		}
		dst = append(dst, '{')
		for i, name := range v.keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, '\n')
			dst = append(dst, inner...)
			dst = AppendString(dst, name)
			dst = append(dst, ": "...)
			dst = appendIndent(dst, v.values[name], indent, inner)
		}
		dst = append(dst, '\n')
		dst = append(dst, prefix...)
		return append(dst, '}')
	case Members:
		if len(v) == 0 {
			return append(dst, "{}"...)
		}
		dst = append(dst, '{')
		for i, m := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, '\n')
			dst = append(dst, inner...)
			dst = AppendString(dst, m.Name)
			dst = append(dst, ": "...)
			dst = appendIndent(dst, m.Value, indent, inner)
		}
		dst = append(dst, '\n')
		dst = append(dst, prefix...)
		return append(dst, '}')
	case []any:
		if len(v) == 0 {
			return append(dst, "[]"...)
		}
		dst = append(dst, '[')
		for i, item := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, '\n')
			dst = append(dst, inner...)
			dst = appendIndent(dst, item, indent, inner)
		}
		dst = append(dst, '\n')
		dst = append(dst, prefix...)
		return append(dst, ']')
	default:
		return Append(dst, v)
	}
}

// O makes Members of names and values, alternately: O("t", "checkout", "v", 1). It panics on an
// odd count or a name that is not a string: a programming error.
func O(namesAndValues ...any) Members {
	if len(namesAndValues)%2 != 0 {
		panic("jsonv: O takes names and values")
	}
	members := make(Members, 0, len(namesAndValues)/2)
	for i := 0; i < len(namesAndValues); i += 2 {
		name, ok := namesAndValues[i].(string)
		if !ok {
			panic(fmt.Sprintf("jsonv: O's name %v is not a string", namesAndValues[i]))
		}
		members = append(members, Member{Name: name, Value: namesAndValues[i+1]})
	}
	return members
}
