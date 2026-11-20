// Package nostr holds Nostr events (NIP-01, SPEC.md §11) and sealed records (NIP-78 and NIP-44,
// SPEC.md §12): their ids, signatures and checks, sealing and opening.
package nostr

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/secp"
	"github.com/sattleme/checkout-go/internal/text"
)

// Event is a signed Nostr event (NIP-01), checked or made here.
type Event struct {
	// ID is the hex SHA-256 of the serialized event.
	ID string
	// PubKey is the hex x-only public key of the author.
	PubKey string
	// CreatedAt is in Unix seconds.
	CreatedAt int64
	// Kind is an integer, as a double (JSON's number: a relay may write it 30078.0).
	Kind    float64
	Tags    [][]string
	Content string
	// Sig is the hex BIP-340 signature of the id.
	Sig string
}

// Template is what is signed of an event.
type Template struct {
	CreatedAt int64
	Kind      int64
	Tags      [][]string
	Content   string
}

// hash is the event id: SHA-256 of the NIP-01 serialization, [0, pubkey, created_at, kind, tags,
// content] as JSON.stringify writes it (numbers as integers whatever their spelling).
func hash(pubkey string, createdAt int64, kind float64, tags [][]string, content string) [32]byte {
	serialized := jsonv.Marshal([]any{0, pubkey, createdAt, kind, tags, content})
	return sha256.Sum256(serialized)
}

// Sign signs a template with secret (BIP-340, 32 fresh bytes of auxiliary randomness from
// random, as the BIP recommends).
func Sign(t Template, secret []byte, random io.Reader) (*Event, error) {
	pubkey, err := secp.PublicKey(secret)
	if err != nil {
		return nil, err
	}
	id := hash(pubkey, t.CreatedAt, float64(t.Kind), t.Tags, t.Content)
	var aux [32]byte
	if _, err := io.ReadFull(random, aux[:]); err != nil {
		return nil, err
	}
	sig, err := secp.Sign(secret, id[:], aux)
	if err != nil {
		return nil, err
	}
	tags := t.Tags
	if tags == nil {
		tags = [][]string{}
	}
	return &Event{
		ID:        hex.EncodeToString(id[:]),
		PubKey:    pubkey,
		CreatedAt: t.CreatedAt,
		Kind:      float64(t.Kind),
		Tags:      tags,
		Content:   t.Content,
		Sig:       hex.EncodeToString(sig[:]),
	}, nil
}

// JSON is the event as a relay takes it.
func (e *Event) JSON() jsonv.Members {
	return jsonv.Members{
		{Name: "id", Value: e.ID},
		{Name: "pubkey", Value: e.PubKey},
		{Name: "created_at", Value: e.CreatedAt},
		{Name: "kind", Value: e.Kind},
		{Name: "tags", Value: e.Tags},
		{Name: "content", Value: e.Content},
		{Name: "sig", Value: e.Sig},
	}
}

// WithValidID reads what a relay sent as an event whose id is its hash: what is checked of a
// record before anything else where anybody may write (SPEC.md §31, "Opening a lease"), its
// signature only where it decides. id and pubkey hex32, sig 128 lowercase hex characters,
// created_at a safe integer, kind an integer, content a string, tags an array of arrays of
// strings, no string of content or tags holding a lone surrogate: its UTF-8, which the hash needs,
// does not exist. False otherwise.
func WithValidID(v any) (*Event, bool) {
	o, ok := jsonv.AsObject(v)
	if !ok {
		return nil, false
	}
	id, _ := o.Get("id")
	pubkey, _ := o.Get("pubkey")
	sig, _ := o.Get("sig")
	idText, ok1 := id.(string)
	pubText, ok2 := pubkey.(string)
	sigText, ok3 := sig.(string)
	if !ok1 || !ok2 || !ok3 || !text.IsHex32(idText) || !text.IsHex32(pubText) || !text.IsHex64(sigText) {
		return nil, false
	}
	createdAtValue, _ := o.Get("created_at")
	createdAt, ok := jsonv.AsSafeInteger(createdAtValue)
	if !ok {
		return nil, false
	}
	kindValue, _ := o.Get("kind")
	if !jsonv.IsInteger(kindValue) {
		return nil, false
	}
	contentValue, _ := o.Get("content")
	content, ok := contentValue.(string)
	if !ok || !text.IsWellFormed(content) {
		return nil, false
	}
	tagsValue, _ := o.Get("tags")
	tagItems, ok := tagsValue.([]any)
	if !ok {
		return nil, false
	}
	tags := make([][]string, len(tagItems))
	for i, item := range tagItems {
		parts, ok := item.([]any)
		if !ok {
			return nil, false
		}
		tag := make([]string, len(parts))
		for j, part := range parts {
			s, ok := part.(string)
			if !ok || !text.IsWellFormed(s) {
				return nil, false
			}
			tag[j] = s
		}
		tags[i] = tag
	}
	kind := kindValue.(float64)
	h := hash(pubText, createdAt, kind, tags, content)
	if hex.EncodeToString(h[:]) != idText {
		return nil, false
	}
	return &Event{ID: idText, PubKey: pubText, CreatedAt: createdAt, Kind: kind, Tags: tags, Content: content, Sig: sigText}, true
}

// SignatureChecks tells whether the signature of an event, whose id was checked, verifies
// (BIP-340).
func SignatureChecks(e *Event) bool {
	pub, err1 := hex.DecodeString(e.PubKey)
	id, err2 := hex.DecodeString(e.ID)
	sig, err3 := hex.DecodeString(e.Sig)
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	return secp.Verify(pub, id, sig)
}

// Valid reads what a relay sent as an event that checks out (SPEC.md §11): its id is its hash
// and its signature verifies. Relays are never trusted.
func Valid(v any) (*Event, bool) {
	e, ok := WithValidID(v)
	if !ok || !SignatureChecks(e) {
		return nil, false
	}
	return e, true
}

// DTag returns the first "d" tag of an event (its value, when the tag has one).
func (e *Event) DTag() (string, bool) {
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == "d" {
			if len(tag) > 1 {
				return tag[1], true
			}
			return "", false
		}
	}
	return "", false
}

// TagValues returns the values of the tags named name (their second item), in order.
func (e *Event) TagValues(name string) []string {
	var values []string
	for _, tag := range e.Tags {
		if len(tag) > 1 && tag[0] == name {
			values = append(values, tag[1])
		}
	}
	return values
}

// Replaces tells whether a replaces b as relays keep them: a later created_at, then the lowest id
// (NIP-01).
func Replaces(aCreatedAt int64, aID string, bCreatedAt int64, bID string) bool {
	return aCreatedAt > bCreatedAt || aCreatedAt == bCreatedAt && aID < bID
}
