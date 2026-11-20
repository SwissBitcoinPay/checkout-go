// Package jsonv reads and writes JSON as every implementation of the specification does (SPEC.md
// §2): as JavaScript's JSON.parse and JSON.stringify do.
//
// Reading: RFC 8259 only; of members with one name the last counts, names that differ in case
// being different names; a number is its value as an IEEE 754 double, whatever its spelling (500,
// 500.0 and 5e2 are one number); a boolean is never a number. A string escape of a lone surrogate
// ("\ud800") is kept, encoded as WTF-8 (the three bytes UTF-8 would give the code point), so that
// the string is not valid UTF-8: wherever a text is checked (SPEC.md §6) it is refused, as in
// JavaScript, never silently replaced by U+FFFD as encoding/json does.
//
// Values are nil (null), bool, float64, string, []any and *Object.
package jsonv

import (
	"errors"
	"strconv"
	"unicode/utf8"
)

// MaxDepth is the deepest nesting of arrays and objects, counted together, of a JSON text (SPEC.md
// §2): a text nested deeper is not JSON, whatever else it holds. No answer of a relay, a service
// or a record nests more than a few levels. Every implementation checks it alike, by a scan of the
// text before it is read (depthWithin).
const MaxDepth = 64

// ErrSyntax is the error of every document that is not JSON as this package reads it.
var ErrSyntax = errors.New("jsonv: not JSON")

// Object is a JSON object: its members in the order of their first appearance, each with the value
// of its last occurrence, as JSON.parse keeps them.
type Object struct {
	keys   []string
	values map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object {
	return &Object{values: map[string]any{}}
}

// Set gives the member `name` the value v (a new member goes last).
func (o *Object) Set(name string, v any) {
	if _, ok := o.values[name]; !ok {
		o.keys = append(o.keys, name)
	}
	o.values[name] = v
}

// Get returns the value of the member `name`, and whether the object has it.
func (o *Object) Get(name string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.values[name]
	return v, ok
}

// Has tells whether the object has a member `name` (null included).
func (o *Object) Has(name string) bool {
	_, ok := o.Get(name)
	return ok
}

// Keys returns the names of the members, in the order of their first appearance.
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.keys...)
}

// Len returns the number of members.
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Parse returns the value of the JSON text `data` (UTF-8, whitespace allowed around the value), or
// ErrSyntax. Invalid UTF-8 anywhere, a byte order mark, a trailing comma, a control character in a
// string, NaN, Infinity, a leading zero or a nesting deeper than MaxDepth are not JSON.
func Parse(data []byte) (any, error) {
	if !depthWithin(data, MaxDepth) {
		return nil, ErrSyntax
	}
	p := parser{data: data}
	p.skipSpace()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return nil, ErrSyntax
	}
	return v, nil
}

// ParseString is Parse of the bytes of s.
func ParseString(s string) (any, error) {
	return Parse([]byte(s))
}

// depthWithin tells whether the arrays and objects of data nest at most max deep, counted
// together, outside its strings: a quote opens a string and the next unescaped quote closes it, a
// backslash in a string escaping the byte after it. It reads any text, JSON or not.
func depthWithin(data []byte, max int) bool {
	depth := 0
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[', '{':
			depth++
			if depth > max {
				return false
			}
		case ']', '}':
			depth--
		}
	}
	return true
}

type parser struct {
	data []byte
	pos  int
}

func (p *parser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (any, error) {
	if p.pos >= len(p.data) {
		return nil, ErrSyntax
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		if depth >= MaxDepth {
			return nil, ErrSyntax
		}
		return p.object(depth + 1)
	case c == '[':
		if depth >= MaxDepth {
			return nil, ErrSyntax
		}
		return p.array(depth + 1)
	case c == '"':
		return p.string()
	case c == 't':
		return true, p.literal("true")
	case c == 'f':
		return false, p.literal("false")
	case c == 'n':
		return nil, p.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return nil, ErrSyntax
	}
}

func (p *parser) literal(word string) error {
	if len(p.data)-p.pos < len(word) || string(p.data[p.pos:p.pos+len(word)]) != word {
		return ErrSyntax
	}
	p.pos += len(word)
	return nil
}

func (p *parser) object(depth int) (any, error) {
	p.pos++ // {
	o := NewObject()
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return o, nil
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, ErrSyntax
		}
		name, err := p.string()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, ErrSyntax
		}
		p.pos++
		p.skipSpace()
		v, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		o.Set(name, v)
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, ErrSyntax
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return o, nil
		default:
			return nil, ErrSyntax
		}
	}
}

func (p *parser) array(depth int) (any, error) {
	p.pos++ // [
	items := []any{}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return items, nil
	}
	for {
		p.skipSpace()
		v, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, ErrSyntax
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return items, nil
		default:
			return nil, ErrSyntax
		}
	}
}

// number reads RFC 8259's number: -? (0 | [1-9][0-9]*) (\.[0-9]+)? ([eE][+-]?[0-9]+)?, as the
// IEEE 754 double nearest its value; beyond the doubles, ±Inf (as JavaScript reads 1e400).
func (p *parser) number() (any, error) {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
	}
	if p.pos >= len(p.data) {
		return nil, ErrSyntax
	}
	switch c := p.data[p.pos]; {
	case c == '0':
		p.pos++
	case c >= '1' && c <= '9':
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	default:
		return nil, ErrSyntax
	}
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		if p.pos >= len(p.data) || !isDigit(p.data[p.pos]) {
			return nil, ErrSyntax
		}
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if p.pos >= len(p.data) || !isDigit(p.data[p.pos]) {
			return nil, ErrSyntax
		}
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	}
	f, err := strconv.ParseFloat(string(p.data[start:p.pos]), 64)
	if err != nil {
		// Syntax was checked above: only a value beyond the doubles (ErrRange) is left, which
		// ParseFloat gives as ±Inf or as the nearest denormal or zero, as JavaScript does.
		var numErr *strconv.NumError
		if !errors.As(err, &numErr) || numErr.Err != strconv.ErrRange {
			return nil, ErrSyntax
		}
	}
	return f, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// string reads a JSON string. Raw bytes must be valid UTF-8 and no control character; escapes are
// RFC 8259's. A surrogate pair of escapes is its code point; a lone surrogate escape is kept, as
// WTF-8.
func (p *parser) string() (string, error) {
	p.pos++ // "
	var out []byte
	start := p.pos
	for {
		if p.pos >= len(p.data) {
			return "", ErrSyntax
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			if out == nil {
				s := string(p.data[start:p.pos])
				p.pos++
				return s, nil
			}
			out = append(out, p.data[start:p.pos]...)
			p.pos++
			return string(out), nil
		case c == '\\':
			out = append(out, p.data[start:p.pos]...)
			var err error
			out, err = p.escape(out)
			if err != nil {
				return "", err
			}
			start = p.pos
		case c < 0x20:
			return "", ErrSyntax
		case c < utf8.RuneSelf:
			p.pos++
		default:
			r, size := utf8.DecodeRune(p.data[p.pos:])
			if r == utf8.RuneError && size <= 1 {
				return "", ErrSyntax
			}
			p.pos += size
		}
	}
}

func (p *parser) escape(out []byte) ([]byte, error) {
	p.pos++ // backslash
	if p.pos >= len(p.data) {
		return nil, ErrSyntax
	}
	c := p.data[p.pos]
	p.pos++
	switch c {
	case '"':
		return append(out, '"'), nil
	case '\\':
		return append(out, '\\'), nil
	case '/':
		return append(out, '/'), nil
	case 'b':
		return append(out, '\b'), nil
	case 'f':
		return append(out, '\f'), nil
	case 'n':
		return append(out, '\n'), nil
	case 'r':
		return append(out, '\r'), nil
	case 't':
		return append(out, '\t'), nil
	case 'u':
		unit, ok := p.hex4()
		if !ok {
			return nil, ErrSyntax
		}
		if unit >= 0xD800 && unit <= 0xDBFF {
			// A high surrogate: with a low one escaped right after, a pair.
			if p.pos+6 <= len(p.data) && p.data[p.pos] == '\\' && p.data[p.pos+1] == 'u' {
				save := p.pos
				p.pos += 2
				low, ok := p.hex4()
				if ok && low >= 0xDC00 && low <= 0xDFFF {
					r := 0x10000 + (rune(unit)-0xD800)<<10 + (rune(low) - 0xDC00)
					return utf8.AppendRune(out, r), nil
				}
				p.pos = save
			}
			return appendSurrogate(out, unit), nil
		}
		if unit >= 0xDC00 && unit <= 0xDFFF {
			return appendSurrogate(out, unit), nil
		}
		return utf8.AppendRune(out, rune(unit)), nil
	default:
		return nil, ErrSyntax
	}
}

func (p *parser) hex4() (uint16, bool) {
	if p.pos+4 > len(p.data) {
		return 0, false
	}
	var v uint16
	for i := 0; i < 4; i++ {
		c := p.data[p.pos+i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		v = v<<4 | uint16(d)
	}
	p.pos += 4
	return v, true
}

// appendSurrogate appends the WTF-8 encoding of a lone surrogate: the three bytes UTF-8 would give
// its code point, which no valid UTF-8 holds.
func appendSurrogate(out []byte, unit uint16) []byte {
	return append(out, 0xE0|byte(unit>>12), 0x80|byte(unit>>6)&0x3F, 0x80|byte(unit)&0x3F)
}

// IsSurrogateAt tells whether s[i:] begins with the WTF-8 encoding of a lone surrogate, and gives
// its code unit.
func IsSurrogateAt(s string, i int) (uint16, bool) {
	if i+3 > len(s) || s[i] != 0xED || s[i+1] < 0xA0 || s[i+1] > 0xBF || s[i+2] < 0x80 || s[i+2] > 0xBF {
		return 0, false
	}
	return uint16(s[i]&0x0F)<<12 | uint16(s[i+1]&0x3F)<<6 | uint16(s[i+2]&0x3F), true
}

// Object returns the members as an object, as JSON reads them back (of members with one name,
// the last counts).
func (m Members) Object() *Object {
	o := NewObject()
	for _, member := range m {
		o.Set(member.Name, member.Value)
	}
	return o
}
