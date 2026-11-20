package testkit

import (
	"encoding/hex"
	"errors"

	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
)

// OpenAsOwner opens a declaration as the merchant's app does, with the registry key and the
// reading key: its inside.
func OpenAsOwner(e *nostr.Event, registryKey, readerSecret []byte) (*jsonv.Object, error) {
	return OpenRecordAsOwner(e, registryKey, readerSecret, proto.DeclarationRecordType)
}

// OpenRecordAsOwner opens a record of the registry of recordType (a declaration, a lease) as the
// merchant's app does: its inside.
func OpenRecordAsOwner(e *nostr.Event, registryKey, readerSecret []byte, recordType string) (*jsonv.Object, error) {
	d, _ := e.DTag()
	plaintext, err := nip44.Decrypt(e.Content, derive.OuterKey(registryKey, d, e.PubKey))
	if err != nil {
		return nil, err
	}
	outside, err := jsonv.ParseString(plaintext)
	if err != nil {
		return nil, err
	}
	if t, _ := jsonv.Field(outside, "t"); t != recordType {
		return nil, errors.New("testkit: not a record of type " + recordType)
	}
	v, _ := jsonv.Field(outside, "v")
	sealed, _ := jsonv.Field(v, "sealed")
	sealedText, ok := sealed.(string)
	if !ok {
		return nil, errors.New("testkit: nothing sealed")
	}
	author, _ := hex.DecodeString(e.PubKey)
	key, err := nip44.ConversationKey(readerSecret, author)
	if err != nil {
		return nil, err
	}
	insideText, err := nip44.Decrypt(sealedText, key)
	if err != nil {
		return nil, err
	}
	inside, err := jsonv.ParseString(insideText)
	if err != nil {
		return nil, err
	}
	o, ok := jsonv.AsObject(inside)
	if !ok {
		return nil, errors.New("testkit: not an object")
	}
	return o, nil
}
