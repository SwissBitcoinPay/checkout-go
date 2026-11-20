package nip44

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/secp"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The official vectors of NIP-44 (https://github.com/paulmillr/nip44, see `source`), the app's
// copy.
func TestOfficialVectors(t *testing.T) {
	v := tv.Load(t, "nip44/nip44.vectors.json")
	counts := map[string]int{}

	for _, item := range tv.Arr(t, v, "calc_padded_len") {
		pair, _ := jsonv.AsArray(item)
		n, _ := jsonv.AsSafeInteger(pair[0])
		want, _ := jsonv.AsSafeInteger(pair[1])
		if got := PaddedLength(int(n)); got != int(want) {
			t.Errorf("PaddedLength(%d) = %d, want %d", n, got, want)
		}
		counts["calc_padded_len"]++
	}

	for _, c := range tv.Objs(t, v, "get_conversation_key") {
		key, err := ConversationKey(unhex(t, tv.Str(t, c, "sec1")), unhex(t, tv.Str(t, c, "pub2")))
		if err != nil || hex.EncodeToString(key) != tv.Str(t, c, "conversation_key") {
			t.Errorf("conversation key of %s: %x, %v", tv.Str(t, c, "sec1"), key, err)
		}
		counts["get_conversation_key"]++
	}
	for _, c := range tv.Objs(t, v, "invalid_get_conversation_key") {
		if key, err := ConversationKey(unhex(t, tv.Str(t, c, "sec1")), unhex(t, tv.Str(t, c, "pub2"))); err == nil {
			t.Errorf("%s: conversation key %x", tv.Str(t, c, "note"), key)
		}
		counts["invalid_get_conversation_key"]++
	}

	mk := tv.Obj(t, v, "get_message_keys")
	conversation := unhex(t, tv.Str(t, mk, "conversation_key"))
	for _, c := range tv.Objs(t, mk, "keys") {
		chachaKey, chachaNonce, hmacKey, err := MessageKeys(conversation, unhex(t, tv.Str(t, c, "nonce")))
		if err != nil ||
			hex.EncodeToString(chachaKey) != tv.Str(t, c, "chacha_key") ||
			hex.EncodeToString(chachaNonce) != tv.Str(t, c, "chacha_nonce") ||
			hex.EncodeToString(hmacKey) != tv.Str(t, c, "hmac_key") {
			t.Errorf("message keys of nonce %s", tv.Str(t, c, "nonce"))
		}
		counts["get_message_keys"]++
	}

	for _, c := range tv.Objs(t, v, "encrypt_decrypt") {
		key := unhex(t, tv.Str(t, c, "conversation_key"))
		plaintext, payload := tv.Str(t, c, "plaintext"), tv.Str(t, c, "payload")
		got, err := EncryptWithNonce(plaintext, key, unhex(t, tv.Str(t, c, "nonce")))
		if err != nil || got != payload {
			t.Errorf("encrypt %q: %s, %v", plaintext, got, err)
		}
		text, err := Decrypt(payload, key)
		if err != nil || text != plaintext {
			t.Errorf("decrypt %q: %q, %v", plaintext, text, err)
		}
		counts["encrypt_decrypt"]++
	}

	for _, c := range tv.Objs(t, v, "invalid_decrypt") {
		if text, err := Decrypt(tv.Str(t, c, "payload"), unhex(t, tv.Str(t, c, "conversation_key"))); err == nil {
			t.Errorf("%s: decrypted %q", tv.Str(t, c, "note"), text)
		}
		counts["invalid_decrypt"]++
	}
	t.Logf("cases: %v", counts)
}

func TestTellsAnotherVersionApart(t *testing.T) {
	if _, err := Decrypt("#future", make([]byte, 32)); !errors.Is(err, ErrNewerVersion) {
		t.Errorf("#future: %v", err)
	}
	if _, err := Decrypt("", make([]byte, 32)); !errors.Is(err, ErrNewerVersion) {
		t.Errorf("empty: %v", err)
	}
	// A payload whose version byte is not 2.
	key := make([]byte, 32)
	payload, _ := EncryptWithNonce("hello", key, make([]byte, 32))
	raw := []byte(payload)
	raw[0] = 'A' // the version byte 0
	raw[1] = 'w' // keep the rest decodable: "Aw" is 0x03 then the nonce's first bits
	if _, err := Decrypt(string(raw), key); !errors.Is(err, ErrNewerVersion) {
		t.Errorf("version 3: %v", err)
	}
}

func TestBothPartiesShareTheConversationKey(t *testing.T) {
	a, b := make([]byte, 32), make([]byte, 32)
	rand.Read(a)
	rand.Read(b)
	pubA, _ := secp.PublicKey(a)
	pubB, _ := secp.PublicKey(b)
	ab, err1 := ConversationKey(a, unhex(t, pubB))
	ba, err2 := ConversationKey(b, unhex(t, pubA))
	if err1 != nil || err2 != nil || hex.EncodeToString(ab) != hex.EncodeToString(ba) {
		t.Error("the parties' conversation keys differ")
	}
}

// UTF-8 is decoded strictly (SPEC.md §2): a plaintext's leading byte order mark is kept.
func TestKeepsALeadingByteOrderMark(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	payload, err := Encrypt("\xef\xbb\xbf{}", key, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if text, err := Decrypt(payload, key); err != nil || text != "\xef\xbb\xbf{}" {
		t.Errorf("decrypted %q, %v", text, err)
	}
}

func TestRefusesInvalidUTF8(t *testing.T) {
	if _, err := EncryptWithNonce("\xff", make([]byte, 32), make([]byte, 32)); err == nil {
		t.Error("invalid UTF-8 encrypted")
	}
	if _, err := EncryptWithNonce("", make([]byte, 32), make([]byte, 32)); err == nil {
		t.Error("an empty plaintext encrypted")
	}
}

func TestDrawsAFreshNonceEachTime(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	a, _ := Encrypt("Sunrise Bakery", key, rand.Reader)
	b, _ := Encrypt("Sunrise Bakery", key, rand.Reader)
	if a == b {
		t.Error("the same payload twice")
	}
}
