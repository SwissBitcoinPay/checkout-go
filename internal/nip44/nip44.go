// Package nip44 is NIP-44 version 2 (https://github.com/nostr-protocol/nips/blob/master/44.md),
// as the merchant's app implements it: ChaCha20 (RFC 8439, a 12-byte nonce, counter 0),
// HMAC-SHA256 over the nonce and the ciphertext, per-message keys from a random 32-byte nonce,
// plaintext padded. Checked against the official vectors. The conversation key is a 32-byte HKDF
// output used as it is, except inside a registry declaration: the ECDH of ConversationKey.
package nip44

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"unicode/utf8"

	"golang.org/x/crypto/chacha20"

	"github.com/sattleme/checkout-go/internal/b64"
	"github.com/sattleme/checkout-go/internal/secp"
)

const (
	version      = 2
	minPlaintext = 1
	maxPlaintext = 0xffff
)

var (
	// ErrNewerVersion is a payload of another NIP-44 version: written by a newer client.
	ErrNewerVersion = errors.New("nip44: payload of an unknown version")
	// ErrInvalid is a payload that is malformed, or not sealed with the key.
	ErrInvalid = errors.New("nip44: invalid payload")
)

// ConversationKey is NIP-44's get_conversation_key: the x coordinate of the ECDH of secret with
// the x-only public key pub (32 bytes), through HKDF-extract with the salt "nip44-v2". An error
// for a key out of range or off the curve.
func ConversationKey(secret, pub []byte) ([]byte, error) {
	shared, err := secp.SharedX(secret, pub)
	if err != nil {
		return nil, err
	}
	return hkdf.Extract(sha256.New, shared, []byte("nip44-v2"))
}

// PaddedLength is NIP-44's calc_padded_len: the padded length of a plaintext of n bytes.
func PaddedLength(n int) int {
	if n <= 32 {
		return 32
	}
	nextPower := 1
	for nextPower < n {
		nextPower <<= 1
	}
	// nextPower is the smallest power of two at least n: 1 << (floor(log2(n - 1)) + 1).
	chunk := 32
	if nextPower > 256 {
		chunk = nextPower / 8
	}
	return chunk * ((n-1)/chunk + 1)
}

// MessageKeys is NIP-44's get_message_keys: the ChaCha20 key and nonce and the HMAC key of one
// message.
func MessageKeys(conversationKey, nonce []byte) (chachaKey, chachaNonce, hmacKey []byte, err error) {
	if len(conversationKey) != 32 || len(nonce) != 32 {
		return nil, nil, nil, ErrInvalid
	}
	keys, err := hkdf.Expand(sha256.New, conversationKey, string(nonce), 76)
	if err != nil {
		return nil, nil, nil, err
	}
	return keys[0:32], keys[32:44], keys[44:76], nil
}

func pad(plaintext string) ([]byte, error) {
	n := len(plaintext)
	if n < minPlaintext || n > maxPlaintext {
		return nil, ErrInvalid
	}
	padded := make([]byte, 2+PaddedLength(n))
	binary.BigEndian.PutUint16(padded, uint16(n))
	copy(padded[2:], plaintext)
	return padded, nil
}

// unpad returns the text of a padded plaintext. UTF-8 is decoded strictly: a leading byte order
// mark is kept (a record's plaintext starting with one is then no JSON, SPEC.md §12).
func unpad(padded []byte) (string, error) {
	if len(padded) < 2 {
		return "", ErrInvalid
	}
	n := int(binary.BigEndian.Uint16(padded))
	if n < minPlaintext || len(padded) != 2+PaddedLength(n) {
		return "", ErrInvalid
	}
	text := padded[2 : 2+n]
	if !utf8.Valid(text) {
		return "", ErrInvalid
	}
	return string(text), nil
}

func mac(hmacKey, nonce, ciphertext []byte) []byte {
	h := hmac.New(sha256.New, hmacKey)
	h.Write(nonce)
	h.Write(ciphertext)
	return h.Sum(nil)
}

// Encrypt seals plaintext (valid UTF-8, 1 to 65535 bytes) under the conversation key, with a
// nonce of 32 bytes read from random (the CSPRNG).
func Encrypt(plaintext string, conversationKey []byte, random io.Reader) (string, error) {
	nonce := make([]byte, 32)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return "", err
	}
	return EncryptWithNonce(plaintext, conversationKey, nonce)
}

// EncryptWithNonce is Encrypt with a given nonce: for the official vectors only.
func EncryptWithNonce(plaintext string, conversationKey, nonce []byte) (string, error) {
	if !utf8.ValidString(plaintext) {
		return "", ErrInvalid
	}
	chachaKey, chachaNonce, hmacKey, err := MessageKeys(conversationKey, nonce)
	if err != nil {
		return "", err
	}
	padded, err := pad(plaintext)
	if err != nil {
		return "", err
	}
	cipher, err := chacha20.NewUnauthenticatedCipher(chachaKey, chachaNonce)
	if err != nil {
		return "", err
	}
	ciphertext := make([]byte, len(padded))
	cipher.XORKeyStream(ciphertext, padded)
	payload := make([]byte, 0, 1+32+len(ciphertext)+32)
	payload = append(payload, version)
	payload = append(payload, nonce...)
	payload = append(payload, ciphertext...)
	payload = append(payload, mac(hmacKey, nonce, ciphertext)...)
	return b64.EncodeStd(payload), nil
}

// Decrypt returns the text of a NIP-44 v2 payload: ErrNewerVersion for another version,
// ErrInvalid when the payload is malformed or not sealed with the conversation key.
func Decrypt(payload string, conversationKey []byte) (string, error) {
	if payload == "" || payload[0] == '#' {
		return "", ErrNewerVersion
	}
	if len(payload) < 132 || len(payload) > 87472 {
		return "", ErrInvalid
	}
	data, ok := b64.DecodeStd(payload)
	if !ok || len(data) < 99 || len(data) > 65603 {
		return "", ErrInvalid
	}
	if data[0] != version {
		return "", ErrNewerVersion
	}
	nonce := data[1:33]
	ciphertext := data[33 : len(data)-32]
	chachaKey, chachaNonce, hmacKey, err := MessageKeys(conversationKey, nonce)
	if err != nil {
		return "", err
	}
	// Constant time: the comparison does not stop at the first differing byte.
	if !hmac.Equal(mac(hmacKey, nonce, ciphertext), data[len(data)-32:]) {
		return "", ErrInvalid
	}
	cipher, err := chacha20.NewUnauthenticatedCipher(chachaKey, chachaNonce)
	if err != nil {
		return "", err
	}
	padded := make([]byte, len(ciphertext))
	cipher.XORKeyStream(padded, ciphertext)
	return unpad(padded)
}
