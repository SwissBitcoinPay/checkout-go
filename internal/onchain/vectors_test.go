package onchain

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/httpx"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/key"
	"github.com/sattleme/checkout-go/internal/lnurl"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/order"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/secp"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
	"github.com/sattleme/checkout-go/internal/tolerance"
)

// The package against the on-chain conformance vectors (SPEC.md §31) of the pinned copy of the
// specification: the lot, the methods of an order, leases, witnesses, replacements and the
// statuses of an invoice with an address.

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// outcome is an error as the vectors write it: {error: code, param?}.
func outcome(err error) jsonv.Members {
	e, ok := errs.Of(err)
	if !ok {
		panic(err)
	}
	if e.Param != "" {
		return jsonv.O("error", string(e.Code), "param", e.Param)
	}
	return jsonv.O("error", string(e.Code))
}

// appSale is the sale of vectors/app/app.json's declaration, in registry.
func appSale(t *testing.T, registry derive.Registry) *derive.Sale {
	t.Helper()
	app := tv.Load(t, "app/app.json")
	integrationKey := unhex(t, tv.Str(t, tv.Obj(t, app, "parsed"), "integrationKey"))
	sale, err := derive.SaleOf(integrationKey, registry, tv.Str(t, tv.Obj(t, app, "declaration"), "ref"))
	if err != nil {
		t.Fatal(err)
	}
	return sale
}

func TestTheLotTheAppMadeOpensWithTheKeyAlone(t *testing.T) {
	v := tv.Load(t, "app/onchain-lot.json")
	k, problem := key.Parse(tv.Str(t, v, "secretKey"))
	if problem != "" {
		t.Fatal(problem)
	}
	_, encryptionKey := derive.ChannelKeys(k.Key)
	tv.Equal(t, "keys", jsonv.O("team", k.TeamPublicKey, "lotAddress", derive.LotAddress(k.Key), "channelEncryptionKey", hex.EncodeToString(encryptionKey)),
		jsonv.O("team", tv.Str(t, v, "team"), "lotAddress", tv.Str(t, v, "lotAddress"), "channelEncryptionKey", tv.Str(t, v, "channelEncryptionKey")))
	lot := tv.Obj(t, v, "lot")
	value := tv.Obj(t, lot, "value")
	opened, ok := nostr.Open(tv.Get(lot, "event"), nostr.OpenOptions{
		Type:          tv.Str(t, value, "t"),
		EncryptionKey: encryptionKey,
		Address:       tv.Str(t, v, "lotAddress"),
		Author:        tv.Str(t, v, "team"),
	}, func(value any, e *nostr.Event) ([]string, error) { return ParseLot(value, e.CreatedAt) })
	if !ok || opened.Newer {
		t.Fatal("the lot does not open")
	}
	tv.Equal(t, "addresses", opened.Value, tv.Get(tv.Obj(t, value, "v"), "addresses"))
}

func TestPaymentMethodVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "payment-methods.json"), "cases")
	for _, c := range cases {
		name := tv.Str(t, c, "name")
		sats, err := strconv.ParseInt(tv.Str(t, c, "sats"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		sendable := tv.Obj(t, c, "sendable")
		min, _ := strconv.ParseInt(tv.Str(t, sendable, "min"), 10, 64)
		max, _ := strconv.ParseInt(tv.Str(t, sendable, "max"), 10, 64)
		leased := LeaseOutcome{Status: tv.Str(t, c, "address")}
		if leased.Status == ExplorerFailed {
			leased.Err = errs.NewNetwork("The explorer did not answer", nil)
		}
		var got any
		checked, err := order.Check(order.Order{Ref: "A-1", Sats: sats, HasSats: true}, nil)
		if err == nil {
			var methods Methods
			if methods, err = PaymentMethods(checked.Sats, min, max, "sats"); err == nil {
				methods, err = MethodsWith(methods, leased)
			}
			list := []string{}
			if methods.Lightning {
				list = append(list, "lightning")
			}
			if methods.Onchain {
				list = append(list, "onchain")
			}
			got = jsonv.O("methods", list)
		}
		if err != nil {
			got = outcome(err)
		}
		tv.Equal(t, name, got, tv.Get(c, "expected"))
	}
	t.Logf("%d cases", len(cases))
}

type leaseVectors struct {
	file        *jsonv.Object
	registryKey []byte
	sale        *derive.Sale
}

func loadLeases(t *testing.T) leaseVectors {
	t.Helper()
	file := tv.Load(t, "onchain-leases.json")
	registry := declaration.RegistryOf(tv.Str(t, file, "registryKey"), tv.Str(t, file, "registryReader"))
	return leaseVectors{file: file, registryKey: registry.Key, sale: appSale(t, registry)}
}

func TestLeaseVectorsKeys(t *testing.T) {
	v := loadLeases(t)
	tv.Equal(t, "the sale of the app's declaration",
		jsonv.O("saleAuthor", v.sale.Author, "saleSecret", hex.EncodeToString(v.sale.SecretKey), "innerConversationKey", hex.EncodeToString(v.sale.InnerKey)),
		jsonv.O("saleAuthor", tv.Str(t, v.file, "saleAuthor"), "saleSecret", tv.Str(t, v.file, "saleSecret"), "innerConversationKey", tv.Str(t, v.file, "innerConversationKey")))
	for _, c := range tv.Objs(t, v.file, "derived") {
		d := derive.LeaseAddress(v.registryKey, tv.Str(t, c, "address"))
		tv.Equal(t, "the keys of "+tv.Str(t, c, "address"),
			jsonv.O("registryAddress", d, "outerKey", hex.EncodeToString(derive.OuterKey(v.registryKey, d, v.sale.Author))),
			jsonv.O("registryAddress", tv.Str(t, c, "registryAddress"), "outerKey", tv.Str(t, c, "outerKey")))
	}
}

func outsideJSON(outside LeaseOutside) any {
	switch outside.Kind {
	case OutsideNewer:
		return "newer"
	case OutsideWithdrawn:
		return jsonv.O("version", 1, "withdrawn", true)
	}
	return jsonv.O("version", 1, "end", outside.End, "sealed", outside.Sealed)
}

func TestLeaseVectorsEvents(t *testing.T) {
	v := loadLeases(t)
	events := tv.Objs(t, v.file, "events")
	for _, c := range events {
		name := tv.Str(t, c, "name")
		address := tv.Str(t, c, "address")
		event := tv.Get(c, "event")
		d := derive.LeaseAddress(v.registryKey, address)
		record, recordOK := OpenLeaseRecord(event, v.registryKey, d)
		whole, outside, wholeOK := OpenLeaseOutside(event, v.registryKey, d)
		var outsideGot any
		if wholeOK {
			outsideGot = outsideJSON(outside)
		}
		tv.Equal(t, name+": outside", outsideGot, tv.Get(c, "outside"))
		// A read keeps of it only its id, author, date and signature, and its outside's end.
		if recordOK != wholeOK || recordOK && record.Event != whole {
			t.Errorf("%s: the record keeps %+v", name, record)
		}
		if recordOK {
			id, _ := jsonv.Field(event, "id")
			pubkey, _ := jsonv.Field(event, "pubkey")
			sig, _ := jsonv.Field(event, "sig")
			createdAt, _ := jsonv.Field(event, "created_at")
			if record.Event.ID != id || record.Event.PubKey != pubkey || record.Event.Sig != sig || float64(record.Event.CreatedAt) != createdAt {
				t.Errorf("%s: the record's event is %+v", name, record.Event)
			}
			if record.Outside.Sealed != "" {
				t.Errorf("%s: the record keeps the sealed inside", name)
			}
		}
		// Opening it checks no signature; the record checks it when asked.
		signed := tv.Get(c, "signature") != false
		if e, ok := nostr.WithValidID(event); ok && nostr.SignatureChecks(e) != signed {
			t.Errorf("%s: the signature checks %v", name, !signed)
		}
		if recordOK && record.Signed() != signed {
			t.Errorf("%s: signed %v", name, !signed)
		}
		var terms LeaseTerms
		var inside *jsonv.Object
		opened := false
		if wholeOK && outside.Kind == OutsideLease {
			var err error
			terms, inside, err = OpenLeaseInside(outside, d, v.sale)
			opened = err == nil
		}
		wantInside := tv.Get(c, "inside")
		var insideGot any
		if opened {
			insideGot = inside
		}
		tv.Equal(t, name+": inside", insideGot, wantInside)
		if (wantInside != nil) != opened {
			continue
		}
		if opened {
			// The order's tolerance it gives: absent, or not one, none.
			wantTolerance := int64(0)
			if n, ok := tv.Get(c, "tolerance").(float64); ok {
				wantTolerance = int64(n)
			}
			if terms.Tolerance != wantTolerance {
				t.Errorf("%s: tolerance %d", name, terms.Tolerance)
			}
			// Confirmed: confirmed true alone (the vector's field; absent: true).
			wantConfirmed := tv.Get(c, "confirmed") != false
			if IsConfirmedLease(terms) != wantConfirmed {
				t.Errorf("%s: confirmed %v", name, !wantConfirmed)
			}
		}
	}
	t.Logf("%d events", len(events))
}

// A lease the package makes for the same sale and terms opens with the vector's keys into the
// vector's inside, byte for byte.
func TestLeaseVectorsMadeAgain(t *testing.T) {
	v := loadLeases(t)
	made := 0
	for _, c := range tv.Objs(t, v.file, "events") {
		inside, ok := jsonv.AsObject(tv.Get(c, "inside"))
		if !ok {
			continue
		}
		name := tv.Str(t, c, "name")
		// A tolerance that is not a whole number is no lease this package writes: it reads it as
		// none (TestLeaseVectorsEvents).
		toleranceValue := tv.Get(inside, "tolerance")
		if toleranceValue != nil && !jsonv.IsInteger(toleranceValue) {
			t.Logf("%s: not made (its tolerance %v is no whole number)", name, toleranceValue)
			continue
		}
		address := tv.Str(t, c, "address")
		saleValue := tv.Obj(t, inside, "sale")
		price, ok := declaration.ParsePrice(tv.Get(saleValue, "fiat"))
		if !ok {
			t.Fatalf("%s: price", name)
		}
		invoice, _ := tv.Get(inside, "invoice").(string)
		verifyURL, _ := tv.Get(inside, "verifyUrl").(string)
		terms := LeaseTerms{
			Address:   address,
			Sats:      tv.Int(t, inside, "sats"),
			Invoice:   invoice,
			VerifyURL: verifyURL,
			CreatedAt: tv.Int(t, inside, "createdAt"),
			ExpiresAt: tv.Int(t, inside, "expiresAt"),
			Price:     price,
			Note:      tv.Str(t, saleValue, "note"),
		}
		if n, ok := toleranceValue.(float64); ok {
			terms.Tolerance = int64(n)
		}
		if confirmed, has := inside.Get("confirmed"); has {
			terms.Confirmed, terms.HasConfirmed = confirmed == true, true
		}
		event := tv.Obj(t, c, "event")
		e, err := LeaseEventOf(v.sale, terms, tv.Int(t, event, "created_at"), testkit.Random("lease "+name))
		if err != nil {
			t.Fatal(err)
		}
		d := derive.LeaseAddress(v.registryKey, address)
		tv.Equal(t, name+": tags", e.Tags, [][]string{{"d", d}})
		if e.PubKey != tv.Str(t, v.file, "saleAuthor") {
			t.Errorf("%s: author %s", name, e.PubKey)
		}
		var outerKey string
		for _, k := range tv.Objs(t, v.file, "derived") {
			if tv.Str(t, k, "address") == address {
				outerKey = tv.Str(t, k, "outerKey")
			}
		}
		plaintext, err := nip44.Decrypt(e.Content, unhex(t, outerKey))
		if err != nil {
			t.Fatal(err)
		}
		if len(plaintext)%256 != 0 {
			t.Errorf("%s: a plaintext of %d bytes", name, len(plaintext))
		}
		record, err := jsonv.ParseString(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if typ, _ := jsonv.Field(record, "t"); typ != "registry-onchain" {
			t.Errorf("%s: type %v", name, typ)
		}
		value, _ := jsonv.Field(record, "v")
		end, _ := jsonv.Field(value, "end")
		wantEnd, _ := jsonv.Field(tv.Get(c, "outside"), "end")
		if end != wantEnd {
			t.Errorf("%s: end %v", name, end)
		}
		sealed, _ := jsonv.Field(value, "sealed")
		opened, err := nip44.Decrypt(sealed.(string), unhex(t, tv.Str(t, v.file, "innerConversationKey")))
		if err != nil {
			t.Fatal(err)
		}
		if opened != string(jsonv.Marshal(inside)) {
			t.Errorf("%s:\nsealed %s\nwant   %s", name, opened, jsonv.Marshal(inside))
		}
		reader := unhex(t, tv.Str(t, v.file, "registryReader"))
		key, err := nip44.ConversationKey(v.sale.SecretKey, reader)
		if err != nil || hex.EncodeToString(key) != tv.Str(t, v.file, "innerConversationKey") {
			t.Errorf("%s: the inner conversation key", name)
		}
		made++
	}
	t.Logf("%d leases made again", made)
}

// recordsOf is the records of a vector, as opened (one whose id is not its hash never is), their
// signatures counted in checked.
func recordsOf(t *testing.T, records []*jsonv.Object, checked *[]int) []*LeaseRecord {
	t.Helper()
	var out []*LeaseRecord
	for index, r := range records {
		if tv.Get(r, "id") == false {
			continue
		}
		id := fmt.Sprintf("%064d", index)
		if s, ok := tv.Get(r, "eventId").(string); ok {
			id = s
		}
		var outside LeaseOutside
		switch o := tv.Get(r, "outside").(type) {
		case string:
			outside.Kind = OutsideNewer
		case *jsonv.Object:
			if tv.Get(o, "withdrawn") == true {
				outside.Kind = OutsideWithdrawn
			} else {
				outside.Kind, outside.End = OutsideLease, tv.Int(t, o, "end")
			}
		}
		signed := tv.Get(r, "signature") != false
		i := index
		record := NewLeaseRecord(LeaseEvent{ID: id, PubKey: tv.Str(t, r, "author"), CreatedAt: tv.Int(t, r, "createdAt")}, "", outside)
		record.verify = func(string) bool {
			if checked != nil {
				*checked = append(*checked, i)
			}
			return signed
		}
		out = append(out, record)
	}
	return out
}

func TestLeaseVectorsLeased(t *testing.T) {
	v := loadLeases(t)
	cases := tv.Objs(t, v.file, "leased")
	for _, c := range cases {
		name := tv.Str(t, c, "name")
		var checked []int
		got := IsLeased(recordsOf(t, tv.Objs(t, c, "records"), &checked), tv.Int(t, c, "t"))
		if got != (tv.Get(c, "expected") == true) {
			t.Errorf("leased: %s: %v", name, got)
		}
		// The signatures checked: those of the records that would end an older one's hold.
		sorted := append([]int{}, checked...)
		for i := range sorted {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j] < sorted[i] {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		tv.Equal(t, "leased: "+name+": checked", intsOf(sorted), tv.Get(c, "checked"))
	}
	t.Logf("%d cases", len(cases))
}

func intsOf(list []int) []any {
	out := make([]any, len(list))
	for i, n := range list {
		out[i] = n
	}
	return out
}

func TestLeaseVectorsConfirmed(t *testing.T) {
	v := loadLeases(t)
	cases := tv.Objs(t, v.file, "confirmed")
	for _, c := range cases {
		name := tv.Str(t, c, "name")
		read := recordsOf(t, tv.Objs(t, c, "records"), nil)
		at := tv.Int(t, c, "t")
		// This lease, or one that is not among the records read.
		own := LeaseEvent{PubKey: tv.Str(t, c, "sale"), CreatedAt: at, ID: strings.Repeat("f", 64)}
		if lease, ok := tv.Get(c, "lease").(float64); ok {
			id := fmt.Sprintf("%064d", int(lease))
			found := false
			for _, r := range read {
				if r.Event.ID == id {
					own, found = r.Event, true
				}
			}
			if !found {
				t.Fatalf("%s: no lease %s", name, id)
			}
		}
		// As the call decides it: the read again of the address's leases, which answered, complete
		// or not as the vector says.
		again := &LeaseRead{Records: map[string][]*LeaseRecord{"the address": read}, Answered: true, Complete: tv.Get(c, "complete") == true}
		if got := again.Confirms("the address", own, at); got != (tv.Get(c, "expected") == true) {
			t.Errorf("confirmed: %s: %v", name, got)
		}
	}
	t.Logf("%d cases", len(cases))
}

func witnessValue(opened OpenedWitness, ok bool) any {
	if !ok {
		return nil
	}
	if opened.Newer {
		return "newer"
	}
	w := opened.Witness
	var sale, expiresAt any
	if w.Sale != "" {
		sale, expiresAt = w.Sale, w.ExpiresAt
	}
	return jsonv.O("version", 1, "address", w.Address, "txid", w.Txid, "sale", sale, "sats", w.Sats, "inputs", w.Inputs,
		"replacement", w.Replacement, "expiresAt", expiresAt)
}

func TestWitnessVectors(t *testing.T) {
	file := tv.Load(t, "onchain-witnesses.json")
	integrationKey := unhex(t, tv.Str(t, file, "integrationKey"))
	seen := SeenOf(integrationKey)
	keys := derive.SeenKeys(integrationKey)
	tv.Equal(t, "the seen keys",
		jsonv.O("seenSecret", hex.EncodeToString(keys.SecretKey), "seenAuthor", keys.Author, "seenAddressKey", hex.EncodeToString(keys.AddressKey)),
		tv.Get(file, "derived"))
	if hex.EncodeToString(seen.EncryptionKey) != tv.Str(t, file, "channelEncryptionKey") {
		t.Error("the channel's encryption key")
	}
	saleOf := func(v any) string { s, _ := v.(string); return s }
	for _, c := range tv.Objs(t, file, "addresses") {
		if got := WitnessAddress(seen.AddressKey, tv.Str(t, c, "address"), tv.Str(t, c, "txid"), saleOf(tv.Get(c, "sale"))); got != tv.Str(t, c, "witnessAddress") {
			t.Errorf("the witness address of %s, %v: %s", tv.Str(t, c, "txid"), tv.Get(c, "sale"), got)
		}
	}
	for _, c := range tv.Objs(t, file, "orderTags") {
		if got := OrderTag(seen.AddressKey, tv.Str(t, c, "address"), saleOf(tv.Get(c, "sale"))); got != tv.Str(t, c, "orderTag") {
			t.Errorf("the order tag of %s, %v: %s", tv.Str(t, c, "address"), tv.Get(c, "sale"), got)
		}
	}
	for _, c := range tv.Objs(t, file, "addressTags") {
		if got := AddressTag(seen.AddressKey, tv.Str(t, c, "address")); got != tv.Str(t, c, "addressTag") {
			t.Errorf("the address tag of %s: %s", tv.Str(t, c, "address"), got)
		}
	}
	// The addresses an event names when leasing (SPEC.md §31, "Leasing an address", step 3), as
	// ReadNamedAddresses keeps them.
	byTag := map[string]string{}
	for _, c := range tv.Objs(t, file, "addressTags") {
		byTag[AddressTag(seen.AddressKey, tv.Str(t, c, "address"))] = tv.Str(t, c, "address")
	}
	events := tv.Objs(t, file, "events")
	madeAgain := 0
	for _, c := range events {
		name := tv.Str(t, c, "name")
		event := tv.Get(c, "event")
		tv.Equal(t, name, witnessValue(OpenWitness(event, seen, nil)), tv.Get(c, "value"))
		names := []any{}
		for _, a := range AddressesNamed(event, keys.Author, byTag, nil) {
			names = append(names, a)
		}
		tv.Equal(t, name+": names", canonicalOrder(names), canonicalOrder(tv.Arr(t, c, "names")))
		// A witness the package makes for the same value has the vector's d and o tags (a
		// vector's other tags are ignored), and opens to it.
		value, ok := jsonv.AsObject(tv.Get(c, "value"))
		if !ok {
			continue
		}
		var inputs []string
		for _, coin := range tv.Arr(t, value, "inputs") {
			inputs = append(inputs, coin.(string))
		}
		w := Witness{
			Address:     tv.Str(t, value, "address"),
			Txid:        tv.Str(t, value, "txid"),
			Sale:        saleOf(tv.Get(value, "sale")),
			Sats:        tv.Int(t, value, "sats"),
			Inputs:      inputs,
			Replacement: tv.Get(value, "replacement") == true,
		}
		if n, ok := jsonv.AsSafeInteger(tv.Get(value, "expiresAt")); ok {
			w.ExpiresAt = n
		}
		createdAt, _ := jsonv.AsSafeInteger(tv.Get(tv.Obj(t, c, "event"), "created_at"))
		made, err := WitnessEvent(seen, w, createdAt, testkit.Random("witness "+name))
		if err != nil {
			t.Fatal(err)
		}
		if made.PubKey != keys.Author {
			t.Errorf("%s: made by %s", name, made.PubKey)
		}
		var wantTags [][]string
		for _, item := range tv.Arr(t, tv.Obj(t, c, "event"), "tags") {
			tag := item.([]any)
			if tag[0] == "d" || tag[0] == "o" {
				strs := make([]string, len(tag))
				for i, s := range tag {
					strs[i] = s.(string)
				}
				wantTags = append(wantTags, strs)
			}
		}
		tv.Equal(t, name+": tags", made.Tags, wantTags)
		tv.Equal(t, name+": made again", witnessValue(OpenWitness(testkit.EventValue(made), seen, nil)), value)
		madeAgain++
	}
	t.Logf("%d events, %d made again", len(events), madeAgain)
}

func TestReplacementVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "onchain-replacements.json"), "cases")
	for _, c := range cases {
		var txs []PaymentTx
		for _, tx := range tv.Objs(t, c, "txs") {
			sats, _ := strconv.ParseInt(tv.Str(t, tx, "sats"), 10, 64)
			var inputs []string
			for _, coin := range tv.Arr(t, tx, "inputs") {
				inputs = append(inputs, coin.(string))
			}
			p := PaymentTx{Txid: tv.Str(t, tx, "txid"), Sats: sats, Inputs: inputs, Listed: tv.Get(tx, "listed") == true}
			if n, ok := jsonv.AsSafeInteger(tv.Get(tx, "order")); ok {
				p.Order, p.HasOrder = n, true
			}
			txs = append(txs, p)
		}
		parts := jsonv.NewObject()
		for txid, list := range PaymentParts(txs) {
			items := make([]any, len(list))
			for i, part := range list {
				items[i] = jsonv.O("sats", strconv.FormatInt(part.Sats, 10), "root", part.Root)
			}
			parts.Set(txid, items)
		}
		tv.Equal(t, tv.Str(t, c, "name"), parts, tv.Get(c, "parts"))
	}
	t.Logf("%d cases", len(cases))
}

// canonicalOrder is items sorted by their JSON.
func canonicalOrder(items []any) []any {
	sorted := append([]any{}, items...)
	sort.Slice(sorted, func(i, j int) bool { return tv.Canon(sorted[i]) < tv.Canon(sorted[j]) })
	return sorted
}

func errorOf(code string) error {
	switch code {
	case "network":
		return errs.NewNetwork("The Lightning address did not answer", nil)
	case "invalid_invoice":
		return errs.NewInvalidInvoice("Not this invoice", nil)
	}
	return &errs.Error{Code: errs.Code(code), Message: code}
}

func lightningPartOf(v any) LightningResult {
	if code, ok := jsonv.Field(v, "error"); ok {
		return LightningResult{Err: errorOf(code.(string))}
	}
	state, _ := jsonv.Field(v, "state")
	preimage, _ := jsonv.Field(v, "preimage")
	p, _ := preimage.(string)
	return LightningResult{Part: lnurl.Part{State: state.(string), Preimage: p}}
}

// acceptanceKnown is what AcceptanceKnown, as a check of the invoice of address (of sats, ending
// at expiresAt) runs it at now, tells of the merchant's three relays when every one of them holds
// an acceptance record of accepted (nil: none), as the vector says, so that every majority that
// answers the read reads it: whether the pages know received.
func acceptanceKnown(t *testing.T, address string, sats, expiresAt, now, received int64, accepted *int64) bool {
	t.Helper()
	n := testkit.NewNet()
	defer n.Close()
	relays := []string{"wss://a.example", "wss://b.example", "wss://c.example"}
	for _, url := range relays {
		n.Add(url, testkit.Behaviour{})
	}
	keys := derive.RestKeys(bytes.Repeat([]byte{2}, 32), "R-1", address, expiresAt)
	if accepted != nil {
		e, err := AcceptanceRecordEvent(keys, *accepted, expiresAt, now-1, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		for _, url := range relays {
			n.Get(url).Take(e)
		}
	}
	pool := relay.NewPool(context.Background(), relay.Options{Dialer: n.Dialer(), Timeout: 5 * time.Second, Now: func() int64 { return now }, Random: rand.Reader})
	defer pool.Close()
	known, err := AcceptanceKnown(context.Background(), pool, ReaderOf(keys), sats, received, relays, rand.Reader, logx.Nop)
	if err != nil {
		t.Fatal(err)
	}
	return known
}

// vectors/app/merchant-acceptance.json (SPEC.md §31, "The merchant's acceptance"): the addresses
// of the merchant's acceptances of invoices of the app's integration, and records made by the app
// (records, read with secretKey) or by the specification's own code for a team of its own
// (made.records, read with made.secretKey: the same integration key), each read as a check reads
// it for its invoice: through the reading of one record (OpenMerchantAcceptance) and through the
// count of a read on every relay where each of 3 relays sent it (MerchantAcceptanceOf).
func TestMerchantAcceptanceVectors(t *testing.T) {
	v := tv.Load(t, "app/merchant-acceptance.json")
	made := tv.Obj(t, v, "made")
	parse := func(text string) *key.IntegrationKey {
		k, problem := key.Parse(text)
		if problem != "" {
			t.Fatalf("the vector's key: %s", problem)
		}
		return k
	}
	app, madeKey := parse(tv.Str(t, v, "secretKey")), parse(tv.Str(t, made, "secretKey"))
	_, encryptionKey := derive.ChannelKeys(app.Key)
	if app.TeamPublicKey != tv.Str(t, v, "team") || hex.EncodeToString(encryptionKey) != tv.Str(t, v, "channelEncryptionKey") {
		t.Error("another team, or another channel encryption key")
	}
	// made: a team of its own, for the same integration key.
	if madeKey.TeamPublicKey != tv.Str(t, made, "team") || !bytes.Equal(madeKey.Key, app.Key) ||
		secp.MustPublicKey(unhex(t, tv.Str(t, made, "teamSecret"))) != madeKey.TeamPublicKey {
		t.Error("made: another team, or another integration")
	}
	invoices := tv.Objs(t, v, "invoices")
	for _, inv := range invoices {
		saleKey, _, author := derive.SaleKeys(app.Key, tv.Str(t, inv, "ref"))
		if hex.EncodeToString(saleKey) != tv.Str(t, inv, "saleKey") || author != tv.Str(t, inv, "sale") {
			t.Errorf("the sale of %s", tv.Str(t, inv, "ref"))
		}
		if got := derive.MerchantAcceptanceAddress(app.Key, author, tv.Str(t, inv, "onchainAddress"), tv.Int(t, inv, "expiresAt")); got != tv.Str(t, inv, "acceptanceAddress") {
			t.Errorf("the merchant's acceptance address of %s at %d: %s", tv.Str(t, inv, "ref"), tv.Int(t, inv, "expiresAt"), got)
		}
	}
	read := func(records []*jsonv.Object, k *key.IntegrationKey) {
		for _, c := range records {
			t.Run(tv.Str(t, c, "name"), func(t *testing.T) {
				inv := invoices[tv.Int(t, c, "invoice")]
				keys := MerchantAcceptanceKeysOf(k.Key, k.TeamPublicKey, tv.Str(t, inv, "sale"), tv.Str(t, inv, "onchainAddress"), tv.Int(t, inv, "expiresAt"))
				sats, _ := strconv.ParseInt(tv.Str(t, inv, "sats"), 10, 64)
				terms := AcceptedInvoice{Sats: sats, ExpiresAt: tv.Int(t, inv, "expiresAt")}
				event := tv.Get(c, "event")
				expected := tv.Get(c, "expected")
				var got any
				if opened, ok := OpenMerchantAcceptance(event, keys, terms, nil); ok && opened.Newer && !opened.SetAside {
					got = "newer"
				} else if ok && !opened.Newer {
					got = jsonv.O("received", opened.Received, "createdAt", opened.Event.CreatedAt)
				}
				tv.Equal(t, "opened", got, expected)
				var want int64
				if received, ok := jsonv.Field(expected, "received"); ok {
					want, _ = jsonv.AsSafeInteger(received)
				}
				everyRelay := [][]any{{event}, {event}, {event}}
				if counted := MerchantAcceptanceOf(everyRelay, keys, terms, 3, nostr.NewSigMemo()); counted.Received != want {
					t.Errorf("counted %d, want %d", counted.Received, want)
				}
			})
		}
	}
	records, madeRecords := tv.Objs(t, v, "records"), tv.Objs(t, made, "records")
	read(records, app)
	read(madeRecords, madeKey)
	t.Logf("%d invoices, %d records, %d made", len(invoices), len(records), len(madeRecords))
}

func TestPaymentVectors(t *testing.T) {
	file := tv.Load(t, "onchain-payments.json")
	invoice := tv.Obj(t, file, "invoice")
	address := tv.Str(t, invoice, "onchainAddress")
	sats, _ := strconv.ParseInt(tv.Str(t, invoice, "sats"), 10, 64)
	expiresAt := tv.Int(t, invoice, "expiresAt")
	app := tv.Load(t, "app/app.json")
	sale := tv.Str(t, tv.Obj(t, app, "declaration"), "saleAuthor")
	integrationKey := unhex(t, tv.Str(t, tv.Obj(t, app, "parsed"), "integrationKey"))
	seen := SeenOf(integrationKey)
	witnessAt := func(txid, of string) string { return WitnessAddress(seen.AddressKey, address, txid, of) }
	// Another order's deposit address and sale: where a witness of another order is (`address`
	// "other") and whose it is (`sale` "other").
	const otherAddress = "bc1p4gmh64rd4m5y5nn8hwfnvtz2mm6lngpmwys5a257klyg03ju0pyqu65xe8"
	_, _, otherSale := derive.SaleKeys(integrationKey, "ANOTHER-ORDER")
	// eventOf is the event of a witness of the vector, as a relay sends it: made with the seen
	// secret as the client makes witnesses (WitnessEvent), at the invoice's address or, `address`
	// "other", at another order's; of the order's sale ("this", with the expiresAt of the invoice
	// it is of), another sale (with the invoice's expiresAt: nothing here depends on it) or none.
	// One of a newer format: a record of the seen secret at the witness address of its txid and
	// the order's sale ("newer") or none ("newer none"), with that address's o tags, dated newerAt
	// (the vector gives no date of it).
	eventOf := func(t *testing.T, w *jsonv.Object, newerAt int64) any {
		t.Helper()
		at := address
		if tv.Get(w, "address") == "other" {
			at = otherAddress
		}
		txid := tv.Str(t, w, "txid")
		var e *nostr.Event
		var err error
		switch kind := tv.Get(w, "sale"); kind {
		case "newer", "newer none":
			of := sale
			if kind == "newer none" {
				of = ""
			}
			e, err = nostr.Seal(nostr.SealOptions{
				Type:          proto.WitnessRecordType,
				Value:         jsonv.O("version", proto.WitnessVersion+1),
				Signer:        seen.SecretKey,
				EncryptionKey: seen.EncryptionKey,
				Address:       WitnessAddress(seen.AddressKey, at, txid, of),
				CreatedAt:     newerAt,
				Tags:          [][]string{{proto.OrderTag, OrderTag(seen.AddressKey, at, of)}, {proto.OrderTag, AddressTag(seen.AddressKey, at)}},
				Random:        rand.Reader,
			})
		default:
			witness := Witness{Address: at, Txid: txid, Sats: tv.Int(t, w, "sats"), Replacement: tv.Get(w, "replacement") == true, Inputs: []string{}}
			for _, coin := range tv.Arr(t, w, "inputs") {
				witness.Inputs = append(witness.Inputs, coin.(string))
			}
			switch kind {
			case "this":
				witness.Sale = sale
			case "other":
				witness.Sale = otherSale
			}
			if witness.Sale != "" {
				witness.ExpiresAt = expiresAt
				if n, ok := jsonv.AsSafeInteger(tv.Get(w, "expiresAt")); ok {
					witness.ExpiresAt = n
				}
			}
			e, err = WitnessEvent(seen, witness, tv.Int(t, w, "seenAt"), rand.Reader)
		}
		if err != nil {
			t.Fatal(err)
		}
		return testkit.EventValue(e)
	}
	// The merchant's acceptances (the team's records, SPEC.md §31, "The merchant's acceptance"),
	// made by a team of merchant-acceptance.json's own (made: its secret is in the vector) for the
	// app's integration key, so that they check out: at the address of the invoice of the order's
	// sale, sealed with the channel's key.
	made := tv.Obj(t, tv.Load(t, "app/merchant-acceptance.json"), "made")
	team, problem := key.Parse(tv.Str(t, made, "secretKey"))
	if problem != "" || !bytes.Equal(team.Key, integrationKey) {
		t.Fatal("made: another integration")
	}
	teamSecret := unhex(t, tv.Str(t, made, "teamSecret"))
	merchantKeys := MerchantAcceptanceKeysOf(team.Key, team.TeamPublicKey, sale, address, expiresAt)
	terms := AcceptedInvoice{Sats: sats, ExpiresAt: expiresAt}
	// The merchant's relays of the vector: 3, of which 2 are a majority.
	merchantRelays := []string{"wss://merchant-1.example", "wss://merchant-2.example", "wss://merchant-3.example"}
	// acceptanceEvents are the events of a case's merchant's acceptances, sealed and signed by the
	// team: of its received (a JSON number, whatever it is), or of version 2 for one of a newer
	// format, dated its createdAt. Where the vector gives two of one date ids, made again until
	// their ids sort as the vector's do (the lowest counts).
	acceptanceEvents := func(t *testing.T, records []*jsonv.Object) []*nostr.Event {
		t.Helper()
		for {
			events := make([]*nostr.Event, len(records))
			for i, r := range records {
				value := jsonv.O("version", proto.MerchantAcceptanceVersion+1)
				if r.Has("received") {
					received, err := strconv.ParseFloat(tv.Str(t, r, "received"), 64)
					if err != nil {
						t.Fatal(err)
					}
					value = jsonv.O("version", proto.MerchantAcceptanceVersion, "received", received)
				}
				e, err := nostr.Seal(nostr.SealOptions{
					Type:          proto.MerchantAcceptanceRecordType,
					Value:         value,
					Signer:        teamSecret,
					EncryptionKey: merchantKeys.EncryptionKey,
					Address:       merchantKeys.Address,
					CreatedAt:     tv.Int(t, r, "createdAt"),
					Random:        rand.Reader,
				})
				if err != nil {
					t.Fatal(err)
				}
				events[i] = e
			}
			sorted := true
			for i, a := range records {
				for j, b := range records {
					idA, okA := a.Get("id")
					idB, okB := b.Get("id")
					if okA && okB && (idA.(string) < idB.(string)) != (events[i].ID < events[j].ID) {
						sorted = false
					}
				}
			}
			if sorted {
				return events
			}
		}
	}
	// heldByRelay is what each of the 3 relays holds of a case's merchant's acceptances (absent:
	// none): each record on the first sentBy of them (absent: all), as a relay sends it.
	heldByRelay := func(t *testing.T, c *jsonv.Object) [][]any {
		t.Helper()
		var records []*jsonv.Object
		if c.Has("merchantAccepted") {
			records = tv.Objs(t, c, "merchantAccepted")
		}
		events := acceptanceEvents(t, records)
		held := make([][]any, len(merchantRelays))
		for index := range merchantRelays {
			held[index] = []any{}
			for i, r := range records {
				sentBy := int64(len(merchantRelays))
				if r.Has("sentBy") {
					sentBy = tv.Int(t, r, "sentBy")
				}
				if int64(index) < sentBy {
					held[index] = append(held[index], testkit.EventValue(events[i]))
				}
			}
		}
		return held
	}
	// countedRead is the check's read of the merchant's acceptance, through the package's count of
	// what the relays sent (MerchantAcceptanceOf), each relay answering with what it holds;
	// "unreadable": fewer than a majority answered (network, as ReadMerchantAcceptance fails).
	countedRead := func(t *testing.T, c *jsonv.Object) func() (MerchantAcceptance, error) {
		if tv.Get(c, "merchantAccepted") == "unreadable" {
			return func() (MerchantAcceptance, error) {
				return MerchantAcceptance{}, errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
			}
		}
		held := heldByRelay(t, c)
		return func() (MerchantAcceptance, error) {
			return MerchantAcceptanceOf(held, merchantKeys, terms, len(merchantRelays), nostr.NewSigMemo()), nil
		}
	}
	// relayRead runs run with the read over the merchant's 3 relays, local, each holding what
	// heldByRelay says, as they are (several at one address included); the check reads them on
	// every relay (ReadMerchantAcceptance). The first, which holds every record, answers 150 ms
	// after the others: a read that went on with the first majority to answer would miss what it
	// holds. "unreadable": the 2 others never answer.
	relayRead := func(t *testing.T, c *jsonv.Object, now int64, run func(read func() (MerchantAcceptance, error))) {
		t.Helper()
		unreadable := tv.Get(c, "merchantAccepted") == "unreadable"
		var held [][]any
		if !unreadable {
			held = heldByRelay(t, c)
		}
		n := testkit.NewNet()
		defer n.Close()
		for index, url := range merchantRelays {
			b := testkit.Behaviour{}
			if index == 0 {
				b.QueryDelay = 150 * time.Millisecond
			} else if unreadable {
				b.Mute = true
			}
			r := n.Add(url, b)
			if !unreadable {
				for _, v := range held[index] {
					r.Store(v)
				}
			}
		}
		pool := relay.NewPool(context.Background(), relay.Options{Dialer: n.Dialer(), Timeout: 500 * time.Millisecond, Now: func() int64 { return now }, Random: rand.Reader})
		run(func() (MerchantAcceptance, error) {
			return ReadMerchantAcceptance(context.Background(), pool, merchantKeys, terms, merchantRelays, rand.Reader)
		})
		pool.Close()
		queried := false
		for _, url := range merchantRelays {
			// Nothing published there: a check never publishes a merchant's acceptance, nor again.
			if got := n.Get(url).Received(); len(got) > 0 {
				t.Errorf("%s received %d events", url, len(got))
			}
			if len(n.Get(url).Queries()) > 0 {
				queried = true
			}
		}
		// Read on every relay, as the check reads it: each relay asked once, by its address alone.
		if queried && !unreadable {
			for _, url := range merchantRelays {
				queries := []any{}
				for _, q := range n.Get(url).Queries() {
					queries = append(queries, q)
				}
				tv.Equal(t, url+"'s queries", queries, []any{jsonv.O("kinds", []any{proto.RecordKind}, "authors", []string{merchantKeys.Team}, "#d", []string{merchantKeys.Address})})
			}
		}
	}
	cases := tv.Objs(t, file, "cases")
	// runCase runs one case through the package's payment check (CheckOnchainPart, CombineParts),
	// with readMerchantAcceptance as the check's read of the merchant's acceptance, reading the
	// explorer of a call whose channel names node (nil: mempool.space; the vector's explorer is the
	// explorer read, whichever it is); then the acceptance record it publishes
	// (AcceptanceToPublish, AcceptanceKnown).
	runCase := func(t *testing.T, c *jsonv.Object, readMerchantAcceptance func() (MerchantAcceptance, error), node *bitcoinnode.Node) {
		base := proto.MempoolURL
		if node != nil {
			at, _ := bitcoinnode.ParseURL(node.URL)
			base = at.REST
		}
		log := &testkit.Logger{}
		var readIt atomic.Bool
		var mu sync.Mutex
		asked := []any{}
		publishedList := []any{}
		explorer := tv.Get(c, "explorer")
		blockcypher := tv.Get(c, "blockcypher")
		_, explorerDown := jsonv.Field(explorer, "error")
		var mempoolAsked atomic.Bool
		fake := testkit.NewFakeHTTP(func(req *http.Request) testkit.Answer {
			url := req.URL.String()
			switch {
			case node != nil && node.Fallback == bitcoinnode.Mempool && strings.HasPrefix(url, proto.MempoolURL):
				// Asked only once the node failed: then down too, so that the result is the
				// vector's.
				mempoolAsked.Store(true)
				return testkit.Text(503, "down")
			case url == base+"/address/"+address+"/txs":
				if explorerDown {
					return testkit.Text(500, "down")
				}
				txs, _ := jsonv.Field(explorer, "txs")
				return testkit.JSON(txs)
			case url == base+"/blocks/tip/height":
				if explorerDown {
					return testkit.Text(500, "down")
				}
				height, _ := jsonv.Field(explorer, "height")
				return testkit.Text(200, jsonv.NumberString(height.(float64)))
			case req.URL.Host == "api.blockcypher.com":
				txid := req.URL.Path[strings.LastIndexByte(req.URL.Path, '/')+1:]
				mu.Lock()
				asked = append(asked, txid)
				mu.Unlock()
				if _, down := jsonv.Field(blockcypher, "error"); down {
					return testkit.Status(500, jsonv.O("error", "down"))
				}
				received, ok := jsonv.Field(blockcypher, txid)
				if !ok {
					return testkit.Status(404, jsonv.O("error", "Transaction "+txid+" not found."))
				}
				return testkit.JSON(jsonv.O("hash", txid, "received", received, "confirmations", 0))
			}
			t.Errorf("not asked in this vector: %s", url)
			return testkit.Answer{Err: errors.New("not asked")}
		})
		now := tv.Int(t, c, "now")
		// What the relays send to a read of the witnesses (`address` "other": of no invoice of
		// the read, which a relay sent anyway): the vector's, made here, on the test's
		// goroutine; nil when it is unreadable.
		sentOf := func(held any) []any {
			if held == "unreadable" {
				return nil
			}
			sent := []any{}
			for _, item := range held.([]any) {
				sent = append(sent, eventOf(t, item.(*jsonv.Object), now-1))
			}
			return sent
		}
		first := sentOf(tv.Get(c, "witnesses"))
		heldAgain, said := c.Get("witnessesAgain")
		again := first
		if said {
			again = sentOf(heldAgain)
		}
		// The witnesses the check published, as the relays then hold them.
		var publishedEvents []any
		reads := 0
		readWitnesses := func(bool) (*OrderWitnesses, error) {
			mu.Lock()
			defer mu.Unlock()
			// The first read, then the one after publishing: the vector's witnesses again,
			// unless it says, and those the check published.
			sent := first
			if reads > 0 {
				sent = again
				if !said && sent != nil {
					sent = append(append([]any(nil), sent...), publishedEvents...)
				}
			}
			reads++
			if sent == nil {
				return nil, errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
			}
			// Read as ReadOrderWitnesses reads it.
			return OrderWitnessesOf(sent, seen, address, sale, expiresAt, nostr.NewSigMemo()), nil
		}
		// The rest part: the invoices of the rest the relays hold, and the Lightning part of each.
		var rests *RestPart
		if list, ok := c.Get("rests"); ok {
			rests = &RestPart{}
			for _, item := range list.([]any) {
				r := item.(*jsonv.Object)
				decoded, ok := bolt11.Decode(tv.Str(t, r, "invoice"))
				if !ok {
					t.Fatal("a rest invoice that does not decode")
				}
				// What the vector writes of it is the invoice's own.
				tv.Equal(t, "rest invoice", jsonv.O("paymentHash", tv.Get(r, "paymentHash"), "sats", tv.Get(r, "sats"), "createdAt", tv.Get(r, "createdAt"), "expiresAt", tv.Get(r, "expiresAt")),
					jsonv.O("paymentHash", decoded.PaymentHash, "sats", strconv.FormatInt(decoded.AmountMsat/1000, 10), "createdAt", decoded.Timestamp, "expiresAt", decoded.Timestamp+decoded.ExpirySeconds))
				rests.Invoices = append(rests.Invoices, RestInvoice{
					Bolt11:      tv.Str(t, r, "invoice"),
					PaymentHash: decoded.PaymentHash,
					VerifyURL:   "https://breez.tips/lnurlp/verify/" + decoded.PaymentHash,
					Sats:        decoded.AmountMsat / 1000,
					CreatedAt:   decoded.Timestamp,
					ExpiresAt:   decoded.Timestamp + decoded.ExpirySeconds,
				})
				rests.Results = append(rests.Results, lightningPartOf(tv.Get(r, "lightning")))
			}
		}
		paidByRests := SummaryOf(rests).Paid
		orderTolerance := tolerance.Of(tv.Get(c, "tolerance"))
		part, err := CheckOnchainPart(context.Background(), OnchainCheck{
			Address:        address,
			Sats:           sats,
			ExpiresAt:      expiresAt,
			Sale:           sale,
			Clock:          func() int64 { return now },
			HTTP:           httpx.New(fake, time.Second),
			Explorer:       NewExplorer(httpx.New(fake, time.Second), node, nil, time.Now),
			ReadWitnesses:  readWitnesses,
			WitnessAddress: witnessAt,
			Publish: func(w Witness) error {
				if tv.Get(c, "publishFails") == true {
					return errs.NewNetwork("Fewer than a majority took the witness", nil)
				}
				if w.Address != address {
					t.Errorf("a witness of %s", w.Address)
				}
				// Of this invoice: its expiresAt; of none: none.
				if w.Sale != "" && w.ExpiresAt != expiresAt || w.Sale == "" && w.ExpiresAt != 0 {
					t.Errorf("a witness of expiresAt %d", w.ExpiresAt)
				}
				var of any
				if w.Sale != "" {
					of = "this"
				}
				e, err := WitnessEvent(seen, w, now, rand.Reader)
				if err != nil {
					t.Error(err)
					return err
				}
				mu.Lock()
				publishedList = append(publishedList, jsonv.O("txid", w.Txid, "sale", of, "replacement", w.Replacement))
				publishedEvents = append(publishedEvents, testkit.EventValue(e))
				mu.Unlock()
				return nil
			},
			Logger:      log,
			PaidByRests: func() int64 { return paidByRests },
			Tolerance:   func() int64 { return orderTolerance },
			ReadMerchantAcceptance: func() (MerchantAcceptance, error) {
				readIt.Store(true)
				return readMerchantAcceptance()
			},
		})
		if mempoolAsked.Load() && !errs.Is(err, errs.Network) {
			t.Errorf("mempool.space asked while the node answered")
		}
		onchain := &OnchainResult{Part: part, Err: err}
		var lightning *LightningResult
		if l := tv.Get(c, "lightning"); l != nil {
			r := lightningPartOf(l)
			lightning = &r
		}
		status, err := CombineParts(InvoiceTerms{Sats: sats, ExpiresAt: expiresAt, Address: address}, now, lightning, onchain, rests)
		var got any
		if err != nil {
			e, ok := errs.Of(err)
			if !ok {
				t.Fatal(err)
			}
			got = jsonv.O("error", string(e.Code))
		} else {
			got = testkit.StatusJSON(status)
		}
		tv.Equal(t, "status", got, tv.Get(c, "expected"))
		if err == nil {
			// Published at once, each on its own: in no order.
			tv.Equal(t, "published", canonicalOrder(publishedList), canonicalOrder(tv.Get(c, "published").([]any)))
		}
		tv.Equal(t, "asked", asked, tv.Get(c, "asked"))
		// The acceptance record the check publishes, unless the relays hold one of what it
		// received or less (SPEC.md §31, "Acceptance records"): as the check decides it, with
		// AcceptanceKnown, over relays each of which holds the vector's.
		var acceptance any
		if err == nil {
			if received, ok := AcceptanceToPublish(status, sats, expiresAt, now); ok {
				var accepted *int64
				if v, held := c.Get("accepted"); held {
					n, err := strconv.ParseInt(fmt.Sprint(v), 10, 64)
					if err != nil {
						t.Fatalf("accepted %v", v)
					}
					accepted = &n
				}
				if !acceptanceKnown(t, address, sats, expiresAt, now, received, accepted) {
					acceptance = strconv.FormatInt(received, 10)
				}
			}
		}
		tv.Equal(t, "acceptance", acceptance, tv.Get(c, "acceptance"))
		// A merchant's acceptance of a newer format is logged when the check read it, whatever
		// its date and however many relays sent it.
		newer := false
		if records, ok := tv.Get(c, "merchantAccepted").([]any); ok {
			for _, r := range records {
				if _, ok := jsonv.Field(r, "newer"); ok {
					newer = true
				}
			}
		}
		warned := 0
		for _, message := range log.Messages() {
			if strings.Contains(message, "a merchant's acceptance of this order has a newer format") {
				warned++
			}
		}
		if want := map[bool]int{true: 1}[newer && readIt.Load()]; warned != want {
			t.Errorf("warned %d times of a merchant's acceptance of a newer format, want %d", warned, want)
		}
	}
	endToEnd := 0
	for _, c := range cases {
		t.Run(tv.Str(t, c, "name"), func(t *testing.T) { runCase(t, c, countedRead(t, c), nil) })
	}
	// The explorer read is the merchant's Bitcoin node (SPEC.md §31): every case, the node's
	// answers the vector's; mempool.space never asked with "none", and with "mempool" only once
	// the node failed (then down too: network either way).
	for _, node := range []*bitcoinnode.Node{
		{URL: "http://192.168.1.10:3006", Fallback: bitcoinnode.None},
		{URL: "https://node.example/mempool", Fallback: bitcoinnode.Mempool, Payers: true},
	} {
		for _, c := range cases {
			t.Run("from "+node.URL+": "+tv.Str(t, c, "name"), func(t *testing.T) { runCase(t, c, countedRead(t, c), node) })
		}
	}
	for _, c := range cases {
		if !c.Has("merchantAccepted") {
			continue
		}
		endToEnd++
		t.Run("end to end on 3 local relays: "+tv.Str(t, c, "name"), func(t *testing.T) {
			relayRead(t, c, tv.Int(t, c, "now"), func(read func() (MerchantAcceptance, error)) { runCase(t, c, read, nil) })
		})
	}
	t.Logf("%d cases, each from mempool.space and from 2 nodes, %d of them end to end on local relays", len(cases), endToEnd)
}
