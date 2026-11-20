package jsonv

import (
	"encoding/binary"
	"encoding/hex"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseReadsJSONAsJavaScriptDoes(t *testing.T) {
	v, err := ParseString(`{"settled":false,"Settled":true,"a":{"x":1},"a":{"y":2},"settled":true}`)
	if err != nil {
		t.Fatal(err)
	}
	o, _ := AsObject(v)
	// The last of members with one name counts; another case is another name.
	if got, _ := o.Get("settled"); got != true {
		t.Errorf("settled = %v", got)
	}
	if got, _ := o.Get("Settled"); got != true {
		t.Errorf("Settled = %v", got)
	}
	// Never merged: the last object, whole.
	a, _ := o.Get("a")
	if inner, _ := AsObject(a); inner.Has("x") || !inner.Has("y") {
		t.Errorf("a = %s", Marshal(a))
	}
	// First appearance's order, last value.
	if got := strings.Join(o.Keys(), ","); got != "settled,Settled,a" {
		t.Errorf("keys = %s", got)
	}
}

func TestParseNumbersAsDoubles(t *testing.T) {
	for text, want := range map[string]float64{
		"500": 500, "500.0": 500, "5e2": 500, "5E+2": 500, "50000e-2": 500, "-0": 0,
		"1e400": math.Inf(1), "-1e400": math.Inf(-1), "1e-400": 0,
		"9007199254740993": 9007199254740992, "0.30000000000000004": 0.30000000000000004,
	} {
		v, err := ParseString(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if v != want {
			t.Errorf("%s = %v, want %v", text, v, want)
		}
	}
	for _, text := range []string{"01", "+1", ".5", "5.", "1e", "1e+", "-", "NaN", "Infinity", "-Infinity", "0x10", "1_000", "\xd9\xa1"} {
		if _, err := ParseString(text); err == nil {
			t.Errorf("%q read as JSON", text)
		}
	}
	// A boolean is never a number.
	if _, ok := AsNumber(true); ok {
		t.Error("true read as a number")
	}
	if _, ok := AsSafeInteger(float64(MaxSafeInteger + 1)); ok {
		t.Error("2^53 read as a safe integer")
	}
	if n, ok := AsSafeInteger(float64(MaxSafeInteger)); !ok || n != MaxSafeInteger {
		t.Error("2^53 - 1 not a safe integer")
	}
	if _, ok := AsSafeInteger(1.5); ok {
		t.Error("1.5 read as an integer")
	}
}

func TestParseKeepsLoneSurrogatesDetectable(t *testing.T) {
	cases := []struct {
		text  string
		valid bool
		write string
	}{
		{`"%ud800"`, false, `"%ud800"`},
		{`"a%udc00b"`, false, `"a%udc00b"`},
		{`"%ud83d%ude00"`, true, "\"\xf0\x9f\x98\x80\""},
		{`"%uD83D%uDE00"`, true, "\"\xf0\x9f\x98\x80\""},
		{`"%ud83d%u0041"`, false, `"%ud83dA"`},
		{`"%ud83d"`, false, `"%ud83d"`},
		{`"%ude00%ud83d"`, false, `"%ude00%ud83d"`},
		{"\"\xc3\xa9%u00e9\"", true, "\"\xc3\xa9\xc3\xa9\""},
		{`"%ufffd"`, true, "\"\xef\xbf\xbd\""},
		{`"%u0000%u001f%u0009"`, true, `"%u0000%u001f\t"`},
	}
	for _, c := range cases {
		text, want := u(c.text), u(c.write)
		v, err := ParseString(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		s := v.(string)
		if utf8.ValidString(s) != c.valid {
			t.Errorf("%s: valid UTF-8 = %v", text, !c.valid)
		}
		if got := string(Marshal(s)); got != want {
			t.Errorf("%s written %s, want %s", text, got, want)
		}
	}
}

// u writes JSON's \u escapes of a test's text, given as %u (so that no tool reading this source
// turns an escape into its character).
func u(s string) string { return strings.ReplaceAll(s, "%u", "\\u") }

func TestParseRefusesWhatIsNotJSON(t *testing.T) {
	for _, text := range []string{
		"", " ", "\ufeff{}", "{}\ufeff", "{,}", "[1,]", `{"a":1,}`, "{'a':1}", `{"a" 1}`, `["a" "b"]`,
		"\"a\nb\"", "\"\x7f\x01\"", `"\x"`, `"\u12"`, `"\u12g4"`, `"abc`, "tru", "nul", "[1] 2",
		"\"\xff\"", "\"\xed\xa0\x80\"", "\"\xc0\xaf\"", "{\"a\":1}\x00", "\v1", "\f1",
	} {
		if _, err := ParseString(text); err == nil {
			t.Errorf("%q read as JSON", text)
		}
	}
	// Whitespace is space, tab, line feed and carriage return alone.
	if _, err := ParseString(" \t\n\r[1]\r\n\t "); err != nil {
		t.Error(err)
	}
	deep := strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth)
	if _, err := ParseString(deep); err != nil {
		t.Errorf("depth %d: %v", MaxDepth, err)
	}
	if _, err := ParseString("[" + deep + "]"); err == nil {
		t.Error("read deeper than MaxDepth")
	}
}

// SPEC.md §2: a text nested more than 64 deep (arrays and objects counted together) is not JSON,
// as a scan of the text before it is read tells, whatever else it holds.
func TestATextNestedMoreThan64DeepIsNotJSON(t *testing.T) {
	if MaxDepth != 64 {
		t.Fatalf("MaxDepth %d", MaxDepth)
	}
	arrays := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	mixed := func(n int) string {
		open, closing := "", ""
		for i := 0; i < n; i++ {
			if i%2 == 0 {
				open += `{"a":`
				closing = "}" + closing
			} else {
				open += "["
				closing = "]" + closing
			}
		}
		return open + "1" + closing
	}
	for _, text := range []string{arrays(64), mixed(64), `[` + strings.Repeat(`{"a":`, 63) + `1` + strings.Repeat(`}`, 63) + `]`} {
		if _, err := ParseString(text); err != nil {
			t.Errorf("%.40s…: %v", text, err)
		}
	}
	for _, text := range []string{arrays(65), mixed(65), "[" + mixed(64) + "]",
		// Deeper than 64, though not JSON for other reasons too: refused all the same.
		strings.Repeat("[", 65) + "x"} {
		if _, err := ParseString(text); err == nil {
			t.Errorf("%.40s… read as JSON", text)
		}
	}
	// Brackets in strings are no nesting; an escaped quote does not end a string.
	for _, text := range []string{
		`["` + strings.Repeat("[{", 100) + `"]`,
		`{"` + strings.Repeat("[", 100) + `":1}`,
		`["\"` + strings.Repeat("[", 100) + `", "\\", 1]`,
	} {
		if _, err := ParseString(text); err != nil {
			t.Errorf("%.40s…: %v", text, err)
		}
	}
	if depthWithin([]byte(`["\\"`+strings.Repeat("[", 65)), 64) {
		t.Error("a string ended by its escaped backslash: what follows nests")
	}
}

// SPEC.md §2: an integer of any length is a double; beyond the doubles, ±Infinity.
func TestAnIntegerOfAnyLengthIsADouble(t *testing.T) {
	long := "1" + strings.Repeat("0", 4999)
	for text, want := range map[string]float64{
		long:                            math.Inf(1),
		"-" + long:                      math.Inf(-1),
		strings.Repeat("9", 5000):       math.Inf(1),
		"1" + strings.Repeat("0", 299):  1e299,
		"-1" + strings.Repeat("0", 299): -1e299,
		"[" + long + "]":                math.Inf(1),
		"1e400":                         math.Inf(1),
		"-1E+400":                       math.Inf(-1),
	} {
		v, err := ParseString(text)
		if err != nil {
			t.Fatalf("%.20s…: %v", text, err)
		}
		if items, ok := v.([]any); ok {
			v = items[0]
		}
		if v != want {
			t.Errorf("%.20s… = %v, want %v", text, v, want)
		}
	}
}

// SPEC.md §2: a member name holding U+0000 is an ordinary name.
func TestAMemberNameHoldingNULIsAName(t *testing.T) {
	v, err := ParseString(u(`{"%u0000":1,"a%u0000b":2,"a":3}`))
	if err != nil {
		t.Fatal(err)
	}
	o, _ := AsObject(v)
	if got := strings.Join(o.Keys(), "|"); got != "\x00|a\x00b|a" {
		t.Errorf("keys %q", got)
	}
	if n, _ := o.Get("a\x00b"); n != float64(2) {
		t.Errorf("a NUL b = %v", n)
	}
	// Written back escaped.
	if got := string(Marshal(v)); got != u(`{"%u0000":1,"a%u0000b":2,"a":3}`) {
		t.Errorf("written %s", got)
	}
}

// SPEC.md §2: a leading zero, a trailing comma, or whitespace other than space, tab, line feed and
// carriage return (U+00A0, U+FEFF, U+2028…) is not JSON.
func TestLeadingZerosTrailingCommasAndOtherWhitespaceAreNotJSON(t *testing.T) {
	for _, text := range []string{
		"00", "-01", "-00", "012", "[01]", `{"a":01}`, "0.1e01x",
		"[1,]", "[1,,2]", `{"a":1,}`, `{"a":1,,"b":2}`, "[,1]",
		"\u00a0[1]", "[1]\u00a0", "[1,\u00a02]", "{\"a\":\u00a01}",
		"\ufeff[1]", "[\ufeff1]", "[1]\u2028", "\u2029[1]", "\u3000[1]", "\x0b[1]", "\x0c[1]",
	} {
		if _, err := ParseString(text); err == nil {
			t.Errorf("%q read as JSON", text)
		}
	}
	for text, want := range map[string]float64{"0": 0, "-0": 0, "0.10": 0.1, "0e1": 0, "10": 10} {
		if v, err := ParseString(text); err != nil || v != want {
			t.Errorf("%s: %v, %v", text, v, err)
		}
	}
}

func TestMarshalIsJSONStringify(t *testing.T) {
	v := Members{
		{"t", "checkout"},
		{"v", Members{
			{"version", 1},
			{"url", "https://shop.example/a?b=1&c=<2>"},
			{"note", "\"quoted\" \\ \b\f\n\r\t\x01\x7f é😀"},
			{"fiat", nil},
			{"list", []any{true, false, int64(-5), 2.5, []string{"x"}}},
		}},
	}
	want := `{"t":"checkout","v":{"version":1,"url":"https://shop.example/a?b=1&c=<2>",` +
		`"note":"\"quoted\" \\ \b\f\n\r\t\u0001` + "\x7f é😀" + `","fiat":null,` +
		`"list":[true,false,-5,2.5,["x"]]}}`
	if got := string(Marshal(v)); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// A parsed object is written back in its order, with its last values.
	parsed, _ := ParseString(`{"b":1,"a":2,"b":3}`)
	if got := string(Marshal(parsed)); got != `{"b":3,"a":2}` {
		t.Errorf("got %s", got)
	}
	if got := string(Marshal(math.Inf(1))); got != "null" {
		t.Errorf("Infinity written %s", got)
	}
}

func TestNumberStringIsECMAScripts(t *testing.T) {
	for _, c := range jsNumbers {
		raw, err := hex.DecodeString(c[0])
		if err != nil {
			t.Fatal(err)
		}
		f := math.Float64frombits(binary.BigEndian.Uint64(raw))
		if got := NumberString(f); got != c[1] {
			t.Errorf("%s: got %s, want %s", c[0], got, c[1])
		}
	}
	for f, want := range map[float64]string{0: "0", math.Copysign(0, -1): "0", 1e21: "1e+21", 1e-7: "1e-7", 123e-20: "1.23e-18", 100: "100"} {
		if got := NumberString(f); got != want {
			t.Errorf("%v: got %s, want %s", f, got, want)
		}
	}
}

// SPEC.md §13 (Limits): reading costs time linear in its size, whatever a relay or a server sends.
// A string of 1 MiB of escapes, alone or with a character of two bytes between each (a reader that
// indexes characters from the string's start at each escape is quadratic: minutes), is read in a
// few milliseconds, within 2 s on a busy machine with the race detector.
func TestReadingAStringOfEscapesTakesTimeLinearInItsSize(t *testing.T) {
	for name, c := range map[string]struct {
		text string
		// size is the string's, in bytes: a lone surrogate is held as its 3 bytes of WTF-8.
		size int
	}{
		"escapes":                      {`"` + strings.Repeat(`\u0000`, 1<<20/6) + `"`, 1 << 20 / 6},
		"escapes and other characters": {`"` + strings.Repeat(`\u0000é`, 1<<20/8) + `"`, 3 * (1 << 20 / 8)},
		"lone surrogates and others":   {`"` + strings.Repeat(`\ud800é`, 1<<20/8) + `"`, 5 * (1 << 20 / 8)},
	} {
		start := time.Now()
		v, err := ParseString(c.text)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s, _ := v.(string); len(s) != c.size {
			t.Errorf("%s: %d bytes read, want %d", name, len(s), c.size)
		}
		if elapsed > 2*time.Second {
			t.Errorf("%s: %d bytes read in %v", name, len(c.text), elapsed)
		}
	}
}
