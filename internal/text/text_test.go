package text

import "testing"

func TestCodePoints(t *testing.T) {
	for s, want := range map[string]int{
		"":                         0,
		"abc":                      3,
		"\xc3\xa9":                 1, // é
		"\xf0\x9f\x98\x80":         1, // an emoji: 2 UTF-16 units, 4 bytes, 1 code point
		"a\xed\xa0\x80b":           3, // a lone surrogate (as jsonv keeps it) counts as one
		"\xe2\x84\xaa\xe2\x84\xaa": 2, // two Kelvin signs
	} {
		if got := CodePoints(s); got != want {
			t.Errorf("CodePoints(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestWellFormed(t *testing.T) {
	for s, want := range map[string]bool{
		"":                 true,
		"a\xc3\xa9":        true,
		"\xf0\x9f\x98\x80": true,
		"\xed\xa0\x80":     false, // lone high surrogate
		"\xed\xb0\x80":     false, // lone low surrogate
		"\xff":             false,
		"\xc0\xaf":         false, // overlong
	} {
		if got := IsWellFormed(s); got != want {
			t.Errorf("IsWellFormed(%q) = %v", s, got)
		}
	}
}

func TestTrimmingIsASCIIOnly(t *testing.T) {
	for s, want := range map[string]string{
		" \t\n\ra b\r\n\t ": "a b",
		"\xc2\xa0a\xc2\xa0": "\xc2\xa0a\xc2\xa0", // no-break space kept
		"\fa\f":             "\fa\f",             // form feed is not ASCII whitespace here
		"\va\v":             "\va\v",
		"\xe2\x80\x83a":     "\xe2\x80\x83a", // em space kept
		"   ":               "",
	} {
		if got := TrimASCII(s); got != want {
			t.Errorf("TrimASCII(%q) = %q, want %q", s, got, want)
		}
	}
	if got := TrimSpaces(" \ta\t "); got != "\ta\t" {
		t.Errorf("TrimSpaces trimmed %q", got)
	}
}

func TestCaseIsASCIIOnly(t *testing.T) {
	// U+212A KELVIN SIGN stays as it is; Unicode case mapping would lower it to "k".
	kelvin := "\xe2\x84\xaa"
	if got := LowerASCII("LNBC" + kelvin + "\xc3\x89"); got != "lnbc"+kelvin+"\xc3\x89" {
		t.Errorf("LowerASCII = %q", got)
	}
	if LowerASCII("abc") != "abc" {
		t.Error("lowercase text changed")
	}
	if !EqualFoldASCII("Shop.Example", "shop.example") || EqualFoldASCII(kelvin, "k") || EqualFoldASCII("a", "ab") {
		t.Error("EqualFoldASCII")
	}
}

func TestControlAndHex(t *testing.T) {
	if !HasControl("a\x00") || !HasControl("\x1f") || !HasControl("\x7f") || HasControl("a\xc2\x80b") || HasControl("a b") {
		t.Error("HasControl")
	}
	hex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !IsHex32(hex) || IsHex32(hex[:63]) || IsHex32(hex+"0") || IsHex32("0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef") {
		t.Error("IsHex32")
	}
	if !IsHex64(hex+hex) || IsHex64(hex) {
		t.Error("IsHex64")
	}
	if IsHex32Value(1.0) || !IsHex32Value(hex) {
		t.Error("IsHex32Value")
	}
	if !IsDigits("0123") || IsDigits("") || IsDigits("1a") || IsDigits("\xd9\xa1") || IsDigits("-1") {
		t.Error("IsDigits")
	}
}
