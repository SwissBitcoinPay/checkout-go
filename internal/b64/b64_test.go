package b64

import (
	"bytes"
	"testing"
)

func TestBase64URLIsStrict(t *testing.T) {
	for _, b := range [][]byte{{}, {0}, {0xff}, {0xfb, 0xff}, {1, 2, 3}, {0xfb, 0xef, 0xbe, 0xff}} {
		s := EncodeURL(b)
		got, ok := DecodeURL(s)
		if !ok || !bytes.Equal(got, b) {
			t.Errorf("%x: %q read back as %x, %v", b, s, got, ok)
		}
	}
	if EncodeURL([]byte{0xfb, 0xff}) != "-_8" {
		t.Error("alphabet")
	}
	for _, s := range []string{
		"A",        // one character left: no byte
		"AB",       // unused trailing bits not zero ("AA" is the 0 byte)
		"AAB",      // likewise for two bytes
		"AA==",     // padding
		"+/8",      // the standard alphabet
		"AA AA",    // whitespace
		"AA\nAA",   // a line break, which encoding/base64 skips
		"AA\rAA",   //
		"AAA\x00",  //
		"\xc3\xa9", // non-ASCII
	} {
		if b, ok := DecodeURL(s); ok {
			t.Errorf("%q read as %x", s, b)
		}
	}
}

func TestBase64IsStrict(t *testing.T) {
	for _, b := range [][]byte{{}, {0}, {0xfb, 0xff}, {1, 2, 3}, {0xfb, 0xef, 0xbe, 0xff}} {
		s := EncodeStd(b)
		got, ok := DecodeStd(s)
		if !ok || !bytes.Equal(got, b) {
			t.Errorf("%x: %q read back as %x, %v", b, s, got, ok)
		}
	}
	if EncodeStd([]byte{0xfb, 0xff}) != "+/8=" {
		t.Error("alphabet")
	}
	for _, s := range []string{
		"AA", "AAA", // padding missing
		"AB==", // unused trailing bits not zero
		"AAB=", //
		"A===", // too much padding
		"=AAA", // padding first
		"AA=A", // padding inside
		"-_8=", // the url alphabet
		"AA==\n", "AA\n==", "AAAA\r\nAAAA", "AAAA AAAA",
		"AAAA====",
	} {
		if b, ok := DecodeStd(s); ok {
			t.Errorf("%q read as %x", s, b)
		}
	}
}
