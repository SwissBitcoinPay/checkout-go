// Package derive holds the keys an integration's server derives (SPEC.md §10, §30, §31):
// HKDF-SHA256 (RFC 5869), 32 bytes, salt and info in UTF-8, and HMAC-SHA256. They never change:
// the integration's channel, its sales and their declarations derive from them, in the merchant's
// app as here (the tests check them against the app's vectors, spec/vectors/app/app.json).
//
// Every key here is a secret but the authors and the addresses: never logged, never in an error.
package derive

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"

	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/secp"
)

// Key is HKDF-SHA256 of secret with salt and info, 32 bytes.
func Key(secret []byte, salt, info string) []byte {
	key, err := hkdf.Key(sha256.New, secret, []byte(salt), info, proto.KeyBytes)
	if err != nil {
		// Only a length above 255 × 32 bytes fails: never 32.
		panic("derive: " + err.Error())
	}
	return key
}

// ChannelKeys returns the integration's channel's d tag (hex) and its NIP-44 conversation key.
func ChannelKeys(integrationKey []byte) (address string, encryptionKey []byte) {
	return hex.EncodeToString(Key(integrationKey, proto.IntegrationSalt, proto.ChannelAddressInfo)),
		Key(integrationKey, proto.IntegrationSalt, proto.ChannelEncryptionInfo)
}

// LotAddress is where the integration's lot of deposit addresses is: its d tag (hex).
func LotAddress(integrationKey []byte) string {
	return hex.EncodeToString(Key(integrationKey, proto.IntegrationSalt, proto.LotAddressInfo))
}

// Seen is what the integration's witnesses are made with: the seen secret (it signs them), its
// public key (their author) and the key of their addresses and order tags. Secrets, like the
// integration key, but the author.
type Seen struct {
	SecretKey  []byte
	Author     string
	AddressKey []byte
}

// SeenKeys derives the integration's witness keys.
func SeenKeys(integrationKey []byte) Seen {
	secret := Key(integrationKey, proto.IntegrationSalt, proto.SeenSecretInfo)
	return Seen{
		SecretKey:  secret,
		Author:     secp.MustPublicKey(secret),
		AddressKey: Key(integrationKey, proto.IntegrationSalt, proto.SeenAddressKeyInfo),
	}
}

// TakerKey is the key that marks the integration's declarations ("By" in the app).
func TakerKey(integrationKey []byte) []byte {
	return Key(integrationKey, proto.IntegrationSalt, proto.TakerInfo)
}

// SaleKeys returns the sale of an order: its own key, its Nostr secret and public key (the
// declarations' author).
func SaleKeys(integrationKey []byte, ref string) (key, secretKey []byte, author string) {
	key = Key(integrationKey, proto.IntegrationSalt, proto.SaleInfo(ref))
	secretKey = Key(key, proto.SaleSalt, proto.SaleNostrSecretInfo)
	return key, secretKey, secp.MustPublicKey(secretKey)
}

// Rest is the keys of the rest records of an invoice (SPEC.md §31, "The rest of an
// underpayment"): the rest key (its checkout links' records carry it), the rest secret (it signs
// the records; only the integration key derives it) and its public key, the author, and the
// records' encryption key. Secrets but Author.
type Rest struct {
	Key           []byte
	SecretKey     []byte
	Author        string
	EncryptionKey []byte
}

// RestKeys derives the rest keys of the invoice of the order ref with the deposit address and the
// end expiresAt.
func RestKeys(integrationKey []byte, ref, address string, expiresAt int64) Rest {
	key := Key(integrationKey, proto.IntegrationSalt, proto.RestKeyInfo(ref, address, expiresAt))
	secret := Key(integrationKey, proto.IntegrationSalt, proto.RestSecretInfo(ref, address, expiresAt))
	return Rest{
		Key:           key,
		SecretKey:     secret,
		Author:        secp.MustPublicKey(secret),
		EncryptionKey: RestEncryptionKey(key),
	}
}

// MerchantAcceptanceAddress is the d tag (hex) of the merchant's acceptance of the invoice of the
// sale saleAuthor with the deposit address and the end expiresAt (SPEC.md §31, "The merchant's
// acceptance"): one per invoice.
func MerchantAcceptanceAddress(integrationKey []byte, saleAuthor, address string, expiresAt int64) string {
	return hex.EncodeToString(Key(integrationKey, proto.IntegrationSalt, proto.MerchantAcceptanceInfo(saleAuthor, address, expiresAt)))
}

// RestEncryptionKey is the encryption key of the rest records of the rest key.
func RestEncryptionKey(restKey []byte) []byte {
	return Key(restKey, proto.CheckoutSalt, proto.RestEncryptionInfo)
}

// RestRecordAddress is the d tag (hex) of the rest record of the invoice whose payment hash is
// paymentHash.
func RestRecordAddress(restKey []byte, paymentHash string) string {
	return hex.EncodeToString(Key(restKey, proto.CheckoutSalt, proto.RestAddressInfo(paymentHash)))
}

// AcceptanceRecordAddress is the d tag (hex) of the acceptance record of the invoice whose rest
// key is restKey (SPEC.md §31, "Acceptance records"): one per invoice, a newer record replaces an
// older one.
func AcceptanceRecordAddress(restKey []byte) string {
	return hex.EncodeToString(Key(restKey, proto.CheckoutSalt, proto.AcceptanceAddressInfo))
}

// RegistryAddress is d(H): where the declarations of the payment hash (hex) are.
func RegistryAddress(registryKey []byte, paymentHash string) string {
	h, _ := hex.DecodeString(paymentHash)
	return hex.EncodeToString(hmacSHA256(registryKey, h))
}

// OuterKey is the outer key of a declaration at address by author (hex).
func OuterKey(registryKey []byte, address, author string) []byte {
	return Key(registryKey, proto.DeclarationSalt, proto.OuterInfo(address, author))
}

// TakerTag is who took a sale: the first 16 bytes of HMAC-SHA256(taker key, author), hex.
func TakerTag(takerKey []byte, saleAuthor string) string {
	author, _ := hex.DecodeString(saleAuthor)
	return hex.EncodeToString(hmacSHA256(takerKey, author)[:proto.TakerTagBytes])
}

// CheckoutKeys returns a checkout link's d tag (hex) and its NIP-44 conversation key, from its
// link key (SPEC.md §30).
func CheckoutKeys(linkKey []byte) (address string, encryptionKey []byte) {
	return hex.EncodeToString(Key(linkKey, proto.CheckoutSalt, proto.CheckoutAddressInfo)),
		Key(linkKey, proto.CheckoutSalt, proto.CheckoutEncryptionInfo)
}

// LeaseAddress is the registry address of a deposit address: HMAC(registry key, "onchain/v1/" +
// the address), hex (SPEC.md §31, "Leases").
func LeaseAddress(registryKey []byte, address string) string {
	return hex.EncodeToString(hmacSHA256(registryKey, []byte(proto.LeaseAddressPrefix+address)))
}

// HMAC is HMAC-SHA256 of data with key.
func HMAC(key, data []byte) []byte { return hmacSHA256(key, data) }

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// Registry is the merchant's registry, as the integration's channel gives it.
type Registry struct {
	// Key is the registry key (32 bytes): a secret.
	Key []byte
	// Reader is the public half of the merchant's reading key (hex).
	Reader string
}

// Sale is everything the sale of ref needs, derived once: its keys, its taker tag and the
// conversation key (ECDH) of its declarations' inside with the merchant's reading key.
type Sale struct {
	Ref       string
	Key       []byte
	SecretKey []byte
	Author    string
	Taker     string
	Registry  Registry
	InnerKey  []byte
}

// SaleOf derives the sale of ref. An error when the registry's reader is not a public key (a
// channel never gives one: it is checked as it is read).
func SaleOf(integrationKey []byte, registry Registry, ref string) (*Sale, error) {
	key, secret, author := SaleKeys(integrationKey, ref)
	reader, err := hex.DecodeString(registry.Reader)
	if err != nil {
		return nil, secp.ErrInvalidKey
	}
	inner, err := nip44.ConversationKey(secret, reader)
	if err != nil {
		return nil, err
	}
	return &Sale{
		Ref:       ref,
		Key:       key,
		SecretKey: secret,
		Author:    author,
		Taker:     TakerTag(TakerKey(integrationKey), author),
		Registry:  registry,
		InnerKey:  inner,
	}, nil
}
