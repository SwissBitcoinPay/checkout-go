// Package secp holds what the protocol needs of secp256k1 (SPEC.md §10, §11, §12), from
// btcec/v2 and its schnorr package (no curve or Schnorr code of its own): BIP-340 signatures
// with 32 bytes of auxiliary randomness, their verification, x-only public keys and lift_x, and
// the x coordinate of an ECDH, unhashed, as NIP-44 needs it.
package secp

import (
	"encoding/hex"
	"errors"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// verified counts the signatures verified: each costs some 100 µs, and what relays send must
// never make a read check one per event they hold (SPEC.md §13, "Limits").
var verified atomic.Uint64

// Verified is the number of signatures Verify checked so far, in this process.
func Verified() uint64 { return verified.Load() }

// ErrInvalidKey is a secret key out of range (0, or the curve's order or above), or a public key
// that is not the x coordinate of a point of the curve.
var ErrInvalidKey = errors.New("secp: invalid key")

// secretKey reads 32 bytes as a secret key: from 1 to the curve's order minus 1, never reduced.
func secretKey(secret []byte) (*btcec.PrivateKey, error) {
	if len(secret) != 32 {
		return nil, ErrInvalidKey
	}
	var k btcec.ModNScalar
	if overflow := k.SetByteSlice(secret); overflow || k.IsZero() {
		return nil, ErrInvalidKey
	}
	return btcec.PrivKeyFromScalar(&k), nil
}

// IsSecretKey tells whether 32 bytes are a valid secret key (from 1 to the order minus 1).
func IsSecretKey(secret []byte) bool {
	_, err := secretKey(secret)
	return err == nil
}

// PublicKey returns the x-only public key (BIP-340), in hex, of a secret key.
func PublicKey(secret []byte) (string, error) {
	k, err := secretKey(secret)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(schnorr.SerializePubKey(k.PubKey())), nil
}

// MustPublicKey is PublicKey for a secret derived by HKDF, which is a valid key but with
// negligible probability (SPEC.md §10: about 2^-128); it panics on the impossible.
func MustPublicKey(secret []byte) string {
	pub, err := PublicKey(secret)
	if err != nil {
		panic("secp: derived secret key out of range")
	}
	return pub
}

// liftX returns the point of the curve with the x coordinate xOnly (32 bytes) and an even y
// (BIP-340 lift_x).
func liftX(xOnly []byte) (*btcec.PublicKey, error) {
	if len(xOnly) != 32 {
		return nil, ErrInvalidKey
	}
	pub, err := schnorr.ParsePubKey(xOnly)
	if err != nil {
		return nil, ErrInvalidKey
	}
	return pub, nil
}

// IsPublicKeyBytes tells whether 32 bytes are the x coordinate of a point of secp256k1.
func IsPublicKeyBytes(xOnly []byte) bool {
	_, err := liftX(xOnly)
	return err == nil
}

// IsPublicKey tells whether v is an x-only public key: lowercase hex of 32 bytes, the x
// coordinate of a point of secp256k1 (BIP-340 lift_x).
func IsPublicKey(v any) bool {
	s, ok := v.(string)
	if !ok || !isLowerHex64(s) {
		return false
	}
	b, _ := hex.DecodeString(s)
	return IsPublicKeyBytes(b)
}

func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Sign signs the 32-byte message (an event id) with BIP-340, with the 32 bytes of auxiliary
// randomness aux (fresh from the CSPRNG, as the BIP recommends).
func Sign(secret, message []byte, aux [32]byte) ([64]byte, error) {
	var sig [64]byte
	k, err := secretKey(secret)
	if err != nil {
		return sig, err
	}
	s, err := schnorr.Sign(k, message, schnorr.CustomNonce(aux))
	if err != nil {
		return sig, err
	}
	copy(sig[:], s.Serialize())
	return sig, nil
}

// Verify tells whether sig (64 bytes) is a BIP-340 signature of the 32-byte message by the
// x-only public key pub (32 bytes). Its s must be below the curve's order: the library would
// reduce it, which BIP-340 forbids.
func Verify(pub, message, sig []byte) bool {
	verified.Add(1)
	if len(sig) != 64 || len(message) != 32 {
		return false
	}
	var s btcec.ModNScalar
	if overflow := s.SetByteSlice(sig[32:]); overflow {
		return false
	}
	key, err := liftX(pub)
	if err != nil {
		return false
	}
	parsed, err := schnorr.ParseSignature(sig)
	if err != nil {
		return false
	}
	return parsed.Verify(message, key)
}

// SharedX returns the x coordinate (32 bytes, unhashed) of the ECDH of a secret key with the
// x-only public key pub (its point with an even y): NIP-44's shared secret.
func SharedX(secret, pub []byte) ([]byte, error) {
	k, err := secretKey(secret)
	if err != nil {
		return nil, err
	}
	point, err := liftX(pub)
	if err != nil {
		return nil, err
	}
	return btcec.GenerateSharedSecret(k, point), nil
}
