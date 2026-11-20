package nostr

import (
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/text"
)

// ErrNewer is a record of a newer format than this package reads: ignored, never written over.
var ErrNewer = errors.New("nostr: record of a newer format")

// ErrInvalid is a record's value that is not valid: the record is ignored.
var ErrInvalid = errors.New("nostr: invalid record value")

// SealOptions is what a record is sealed with (SPEC.md §12).
type SealOptions struct {
	Type string
	// Value is the record's value, as jsonv writes it.
	Value any
	// Signer is the secret key that signs the event.
	Signer []byte
	// EncryptionKey is the NIP-44 conversation key (32 bytes).
	EncryptionKey []byte
	// Address is the d tag.
	Address   string
	CreatedAt int64
	// Tags come after the d tag: a checkout link's expiration (NIP-40), a witness's o tags.
	Tags [][]string
	// Random is the CSPRNG: the NIP-44 nonce and the signature's auxiliary randomness.
	Random io.Reader
}

// Plaintext is a record's plaintext: {"t": type, "v": value} followed by spaces (U+0020) up to the
// next multiple of 256 bytes (none when it is one already).
func Plaintext(recordType string, value any) string {
	json := string(jsonv.Marshal(jsonv.Members{{Name: "t", Value: recordType}, {Name: "v", Value: value}}))
	padding := (proto.RecordBlockBytes - len(json)%proto.RecordBlockBytes) % proto.RecordBlockBytes
	return json + strings.Repeat(" ", padding)
}

// Seal makes the signed event of a record. Encryption and signature are randomized.
func Seal(o SealOptions) (*Event, error) {
	content, err := nip44.Encrypt(Plaintext(o.Type, o.Value), o.EncryptionKey, o.Random)
	if err != nil {
		return nil, err
	}
	tags := append([][]string{{"d", o.Address}}, o.Tags...)
	return Sign(Template{CreatedAt: o.CreatedAt, Kind: proto.RecordKind, Tags: tags, Content: content}, o.Signer, o.Random)
}

// OpenOptions is what a record is opened as.
type OpenOptions struct {
	Type string
	// EncryptionKey is the NIP-44 conversation key (32 bytes).
	EncryptionKey []byte
	// Address is the d tag the event must have.
	Address string
	// Author is the author the event must have (hex).
	Author string
	// Memo, when given, remembers the signatures checked (a read's).
	Memo *SigMemo
}

// Opened is a record that opened: authentic, its value valid, or of a newer format (Newer: its
// value is then unread).
type Opened[T any] struct {
	Event *Event
	Value T
	Newer bool
}

// Open reads what a relay sent, if it is a record of the options (SPEC.md §12): kind, author, d
// tag, then a content that opens with the key and holds a value of the type, then the event's
// checks, then the value, validated by parse (ErrNewer for a newer format; any other error, or
// a value that is not one, makes it invalid). False for anything else: a relay is never trusted.
func Open[T any](v any, o OpenOptions, parse func(value any, e *Event) (T, error)) (Opened[T], bool) {
	var none Opened[T]
	claimed, ok := jsonv.AsObject(v)
	if !ok {
		return none, false
	}
	kind, _ := claimed.Get("kind")
	pubkey, _ := claimed.Get("pubkey")
	content, _ := claimed.Get("content")
	tags, _ := claimed.Get("tags")
	contentText, isText := content.(string)
	if kind != float64(proto.RecordKind) || pubkey != o.Author || !isText || !hasTag(tags, "d", o.Address) {
		return none, false
	}
	payload, newer, ok := readContent(contentText, o)
	if !ok && !newer {
		return none, false
	}
	event, ok := WithValidID(v)
	if !ok || !o.Memo.Check(event) {
		return none, false
	}
	if newer {
		return Opened[T]{Event: event, Newer: true}, true
	}
	value, err := parse(payload, event)
	if errors.Is(err, ErrNewer) {
		return Opened[T]{Event: event, Newer: true}, true
	}
	if err != nil {
		return none, false
	}
	return Opened[T]{Event: event, Value: value}, true
}

// hasTag tells whether tags (as a relay sent them) hold [name, value, …].
func hasTag(tags any, name, value string) bool {
	items, ok := tags.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if tag, ok := item.([]any); ok && len(tag) >= 2 && tag[0] == name && tag[1] == value {
			return true
		}
	}
	return false
}

// readContent returns the value inside a record's content (ok), or that it is of a newer NIP-44
// version (newer); neither when it does not open or is of another type.
func readContent(content string, o OpenOptions) (value any, newer, ok bool) {
	plaintext, err := nip44.Decrypt(content, o.EncryptionKey)
	if errors.Is(err, nip44.ErrNewerVersion) {
		return nil, true, false
	}
	if err != nil {
		return nil, false, false
	}
	envelope, err := jsonv.ParseString(plaintext)
	if err != nil {
		return nil, false, false
	}
	obj, isObject := jsonv.AsObject(envelope)
	if !isObject {
		return nil, false, false
	}
	v, hasV := obj.Get("v")
	t, _ := obj.Get("t")
	if !hasV || t != o.Type {
		return nil, false, false
	}
	return v, false, true
}

// SigMemo remembers the signatures checked in a read, once per id and signature, whatever relays
// send copies. Safe for concurrent use; a nil memo checks every time.
type SigMemo struct {
	mu      sync.Mutex
	checked map[string]bool
}

// NewSigMemo returns an empty memo.
func NewSigMemo() *SigMemo { return &SigMemo{checked: map[string]bool{}} }

// Check tells whether the signature of an event whose id was checked verifies.
func (m *SigMemo) Check(e *Event) bool {
	if m == nil {
		return SignatureChecks(e)
	}
	copyKey := e.ID + "/" + e.Sig
	m.mu.Lock()
	valid, seen := m.checked[copyKey]
	m.mu.Unlock()
	if seen {
		return valid
	}
	valid = SignatureChecks(e)
	m.mu.Lock()
	m.checked[copyKey] = valid
	m.mu.Unlock()
	return valid
}

// RecordsThatOpen is what a read of sealed records keeps of each event (SPEC.md §13: the event,
// when it opens): an event of kind 30078 whose content decrypts with keyOf of its first d tag
// (hex32, else "") and its author (NIP-44 v2, its MAC checked), or is of a newer NIP-44 version,
// and that checks out (§11). Its signature too: anybody may send a copy of a record under an id of
// its own, its content and signature left as they are, and only the signature tells it from the
// record. Checked once per id and signature (memo). keyOf gives nil for an author or an address
// the read does not open. Its value is not checked here: the read opens what it keeps. Of the
// event, only its NIP-01 members are kept (NIP01Members): a relay may add any to a record that
// checks out, its id and its signature covering none of them (SPEC.md §13, "Limits").
func RecordsThatOpen(keyOf func(d, author string) []byte, memo *SigMemo) func(any) (any, bool) {
	return func(v any) (any, bool) {
		o, ok := jsonv.AsObject(v)
		if !ok {
			return nil, false
		}
		kind, _ := o.Get("kind")
		content, _ := o.Get("content")
		pubkey, _ := o.Get("pubkey")
		contentText, ok1 := content.(string)
		author, ok2 := pubkey.(string)
		if kind != float64(proto.RecordKind) || !ok1 || !ok2 {
			return nil, false
		}
		key := keyOf(DTagOf(v), author)
		if key == nil {
			return nil, false
		}
		if _, err := nip44.Decrypt(contentText, key); err != nil && !errors.Is(err, nip44.ErrNewerVersion) {
			return nil, false
		}
		event, ok := WithValidID(v)
		if !ok || !memo.Check(event) {
			return nil, false
		}
		return NIP01Members(o), true
	}
}

// nip01Names are the members of a Nostr event (NIP-01).
var nip01Names = []string{"id", "pubkey", "created_at", "kind", "tags", "content", "sig"}

// NIP01Members is an event a relay sent, as JSON, with its NIP-01 members alone (id, pubkey,
// created_at, kind, tags, content, sig): nothing else a relay put in it is kept.
func NIP01Members(o *jsonv.Object) *jsonv.Object {
	kept := jsonv.NewObject()
	for _, name := range nip01Names {
		if v, ok := o.Get(name); ok {
			kept.Set(name, v)
		}
	}
	return kept
}

// DTagOf returns the d tag of a value a relay sent when it is hex32 (its first d tag); "" otherwise.
func DTagOf(v any) string {
	tags, _ := jsonv.Field(v, "tags")
	items, ok := tags.([]any)
	if !ok {
		return ""
	}
	for _, item := range items {
		tag, ok := item.([]any)
		if !ok || len(tag) == 0 || tag[0] != "d" {
			continue
		}
		if len(tag) > 1 {
			if d, ok := tag[1].(string); ok && text.IsHex32(d) {
				return d
			}
		}
		return ""
	}
	return ""
}

// Newest is the newest of the records relays sent.
type Newest[T any] struct {
	// Event and Value are the newest valid record; Event is nil when there is none.
	Event *Event
	Value T
	// NewestUnreadable is the created_at of the newest copy of a newer format; HasUnreadable
	// tells whether there is one.
	NewestUnreadable int64
	HasUnreadable    bool
}

// NewestRecord reads answers (the events of each relay): every copy is checked with open, once
// per event id that checked out (a copy with the id of one already checked is the same event);
// the newest by created_at wins (on a tie, the lowest event id: the one relays keep, NIP-01).
func NewestRecord[T any](answers [][]any, open func(any) (Opened[T], bool)) Newest[T] {
	checked := map[string]bool{}
	var result Newest[T]
	for _, events := range answers {
		for _, v := range events {
			if id, ok := jsonv.Field(v, "id"); ok {
				if s, ok := id.(string); ok && checked[s] {
					continue
				}
			}
			copy, ok := open(v)
			if !ok {
				continue
			}
			checked[copy.Event.ID] = true
			createdAt := copy.Event.CreatedAt
			if copy.Newer {
				if !result.HasUnreadable || createdAt > result.NewestUnreadable {
					result.NewestUnreadable, result.HasUnreadable = createdAt, true
				}
				continue
			}
			if result.Event == nil || Replaces(createdAt, copy.Event.ID, result.Event.CreatedAt, result.Event.ID) {
				result.Event, result.Value = copy.Event, copy.Value
			}
		}
	}
	return result
}
