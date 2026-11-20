package onchain

import (
	"context"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/httpx"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/text"
)

// What the blockchain shows (SPEC.md §31, "Explorers"): one explorer of the Esplora API, the
// explorer, asked over HTTP: mempool.space, or the merchant's Bitcoin node when the channel names
// one (internal/bitcoinnode), mempool.space answering in its place when the node fails and its
// fallback is "mempool" (Explorer); and BlockCypher, for when a transaction was received, asked
// only about a transaction no check saw while its lease ran. Third parties: they see the server's
// IP address and what it asks about, never a key. Their answers are checked as strictly as a
// relay's: anything else is a failure (network). A payment counts as made on what the explorer
// read alone shows.

// Output is an output of a transaction: the sats it pays, and its address ("" without one).
type Output struct {
	Sats    int64
	Address string
}

// ExplorerTx is a transaction, as an explorer gives it, checked.
type ExplorerTx struct {
	Txid string
	// Coins are the coins it spends (<txid>:<vout>), coinbase inputs left out, in their order.
	Coins   []string
	Outputs []Output
	// BlockHeight is the height of its block; Confirmed false while it is not confirmed.
	BlockHeight int64
	Confirmed   bool
}

const maxVout = 4_294_967_295

func safeNatural(v any) (int64, bool) {
	n, ok := jsonv.AsSafeInteger(v)
	return n, ok && n >= 0
}

// ParseTransaction reads an Esplora transaction, checked; false when it is not one (SPEC.md §31,
// Explorers), as when its outputs together hold more than 2 100 000 000 000 000 sats.
func ParseTransaction(value any) (*ExplorerTx, bool) {
	o, ok := jsonv.AsObject(value)
	if !ok {
		return nil, false
	}
	txidValue, _ := o.Get("txid")
	if !text.IsHex32Value(txidValue) {
		return nil, false
	}
	vinValue, _ := o.Get("vin")
	voutValue, _ := o.Get("vout")
	statusValue, _ := o.Get("status")
	vin, ok1 := vinValue.([]any)
	vout, ok2 := voutValue.([]any)
	status, ok3 := jsonv.AsObject(statusValue)
	if !ok1 || !ok2 || !ok3 {
		return nil, false
	}
	tx := &ExplorerTx{Txid: txidValue.(string), Coins: []string{}, Outputs: []Output{}}
	for _, inputValue := range vin {
		input, ok := jsonv.AsObject(inputValue)
		if !ok {
			return nil, false
		}
		if coinbase, _ := input.Get("is_coinbase"); coinbase == true {
			continue
		}
		txid, _ := input.Get("txid")
		indexValue, _ := input.Get("vout")
		index, ok := safeNatural(indexValue)
		if !text.IsHex32Value(txid) || !ok || index > maxVout {
			return nil, false
		}
		tx.Coins = append(tx.Coins, txid.(string)+":"+strconv.FormatInt(index, 10))
	}
	// The sats of its outputs together: at most every bitcoin there will ever be, else it is no
	// transaction, whatever an explorer says (each output at most 2^53 - 1: no overflow).
	var total int64
	for _, outputValue := range vout {
		output, ok := jsonv.AsObject(outputValue)
		if !ok {
			return nil, false
		}
		satsValue, _ := output.Get("value")
		sats, ok := safeNatural(satsValue)
		if !ok {
			return nil, false
		}
		if total += sats; total > proto.MaxSats {
			return nil, false
		}
		addressValue, hasAddress := output.Get("scriptpubkey_address")
		addr, isText := addressValue.(string)
		if hasAddress && !isText {
			return nil, false
		}
		tx.Outputs = append(tx.Outputs, Output{Sats: sats, Address: addr})
	}
	confirmed, _ := status.Get("confirmed")
	isConfirmed, ok := confirmed.(bool)
	if !ok {
		return nil, false
	}
	if isConfirmed {
		heightValue, _ := status.Get("block_height")
		height, ok := safeNatural(heightValue)
		if !ok {
			return nil, false
		}
		tx.BlockHeight, tx.Confirmed = height, true
	}
	return tx, true
}

// PaysTo is what tx pays address: the sum of its outputs to it.
func PaysTo(tx *ExplorerTx, address string) int64 {
	var sum int64
	for _, o := range tx.Outputs {
		if o.Address == address {
			sum = AddSats(sum, o.Sats)
		}
	}
	return sum
}

// AddSats is a + b, two amounts of 0 or more, at most math.MaxInt64: never wrapped (SPEC.md §7).
// An explorer's answer may claim outputs of 2^53 − 1 sats each, a thousand of them in far less
// than the 1 MiB of a body; their sum stops at math.MaxInt64, which is still at least whatever is
// due (21 million bitcoins at most): every comparison with an amount is the exact sum's.
func AddSats(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// ConfirmationsOf is its confirmations at the chain's height: 0 unconfirmed, else height − its
// block's + 1, 1 at least.
func ConfirmationsOf(tx *ExplorerTx, height int64) int64 {
	if !tx.Confirmed {
		return 0
	}
	if c := height - tx.BlockHeight + 1; c > 1 {
		return c
	}
	return 1
}

// AddressTransactions is the transactions of address on the explorer at base: its first answer
// (a deposit address is paid a few times at most), in the explorer's order.
func AddressTransactions(ctx context.Context, client *httpx.Client, base, address string) ([]*ExplorerTx, error) {
	u := base + "/address/" + address + "/txs"
	a, err := client.GetJSON(ctx, u, nil)
	if err != nil {
		return nil, err
	}
	if !a.OK {
		return nil, errs.NewNetwork(httpx.HostOf(u)+" answered HTTP "+strconv.Itoa(a.Status), nil)
	}
	list, ok := a.Body.([]any)
	if !ok {
		return nil, errs.NewNetwork(httpx.HostOf(u)+" gave no transactions", nil)
	}
	txs := make([]*ExplorerTx, 0, len(list))
	for _, v := range list {
		tx, ok := ParseTransaction(v)
		if !ok {
			return nil, errs.NewNetwork(httpx.HostOf(u)+" gave a transaction that is not one", nil)
		}
		txs = append(txs, tx)
	}
	return txs, nil
}

var heightPattern = regexp.MustCompile(`^[0-9]{1,9}$`)

// ChainHeight is the chain's height on the explorer at base: a body of 1 to 9 digits (plain text,
// not JSON).
func ChainHeight(ctx context.Context, client *httpx.Client, base string) (int64, error) {
	u := base + "/blocks/tip/height"
	status, body, isText, err := client.GetText(ctx, u)
	if err != nil {
		return 0, err
	}
	if status < 200 || status > 299 {
		return 0, errs.NewNetwork(httpx.HostOf(u)+" answered HTTP "+strconv.Itoa(status), nil)
	}
	if !isText || !heightPattern.MatchString(body) {
		return 0, errs.NewNetwork(httpx.HostOf(u)+" gave no height", nil)
	}
	height, _ := strconv.ParseInt(body, 10, 64)
	return height, nil
}

// blockcypherTime is a time as BlockCypher writes it: UTC, a fraction of 1 to 9 digits or none,
// Z (2026-10-10T13:05:12.345Z). Another spelling of RFC 3339 is not BlockCypher's.
var blockcypherTime = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]{1,9}))?Z$`)

// BlockCypherMilliseconds reads the Unix milliseconds of a time as BlockCypher writes it, the
// fraction past milliseconds dropped (never rounded); false when it is not one, or not a real
// date and time (a month of 01 to 12, a day of that month, hours 00 to 23, minutes and seconds 00
// to 59: no leap second).
func BlockCypherMilliseconds(s string) (int64, bool) {
	m := blockcypherTime.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n := make([]int, 6)
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	year, month, day, hour, minute, second := n[0], n[1], n[2], n[3], n[4], n[5]
	if month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59 {
		return 0, false
	}
	fraction := m[7]
	if len(fraction) > 3 {
		fraction = fraction[:3]
	}
	for len(fraction) < 3 {
		fraction += "0"
	}
	ms, _ := strconv.Atoi(fraction)
	t := time.Date(year, time.Month(month), day, hour, minute, second, ms*int(time.Millisecond), time.UTC)
	// An overflowing day is carried over to the next month: a day that is not one.
	if day < 1 || t.Day() != day {
		return 0, false
	}
	return t.UnixMilli(), true
}

// ReceivedAt is when BlockCypher first received txid, in Unix milliseconds (it never changes);
// known false when it does not know it yet (HTTP 404: it receives it later). Without a token,
// BlockCypher takes 100 requests an hour from an IP address: it is asked only about a transaction
// no check saw in time.
func ReceivedAt(ctx context.Context, client *httpx.Client, txid string) (ms int64, known bool, err error) {
	u := proto.BlockCypherURL + "/txs/" + txid + "?limit=1&includeHex=false"
	a, err := client.GetJSON(ctx, u, nil)
	if err != nil {
		return 0, false, err
	}
	if a.Status == 404 {
		return 0, false, nil
	}
	if !a.OK {
		return 0, false, errs.NewNetwork(httpx.HostOf(u)+" answered HTTP "+strconv.Itoa(a.Status), nil)
	}
	hash, _ := jsonv.Field(a.Body, "hash")
	received, _ := jsonv.Field(a.Body, "received")
	receivedText, isText := received.(string)
	if _, isObject := jsonv.AsObject(a.Body); !isObject || hash != txid || !isText {
		return 0, false, errs.NewNetwork(httpx.HostOf(u)+" gave no time for the transaction", nil)
	}
	ms, ok := BlockCypherMilliseconds(receivedText)
	if !ok || ms <= 0 {
		return 0, false, errs.NewNetwork(httpx.HostOf(u)+" gave no time for the transaction", nil)
	}
	return ms, true, nil
}

// The explorers a read may ask.
const (
	// NodeExplorer is the merchant's Bitcoin node.
	NodeExplorer = "node"
	// MempoolExplorer is mempool.space.
	MempoolExplorer = "mempool"
)

// ExplorersToAsk are the explorers one read asks, in order (SPEC.md §31, fallback): without a node
// (hasNode false), mempool.space; with "none", the node alone; with "mempool", the node then
// mempool.space, or mempool.space first when the node failed a read less than 60 s ago in the same
// call (failedRecently). The next one is asked only when the one before failed: a read gives the
// answer of the first that answers, and fails (network) when every one asked failed.
func ExplorersToAsk(hasNode bool, fallback bitcoinnode.Fallback, failedRecently bool) []string {
	switch {
	case !hasNode:
		return []string{MempoolExplorer}
	case fallback == bitcoinnode.None:
		return []string{NodeExplorer}
	case failedRecently:
		return []string{MempoolExplorer, NodeExplorer}
	}
	return []string{NodeExplorer, MempoolExplorer}
}

// NodeFailures is when the nodes a call read failed a read, by node URL: of one call alone
// (CreateInvoice, GetStatus, one WaitForPayment), never kept between calls. Safe for concurrent
// use: the checks of a call and its WebSocket share it.
type NodeFailures struct {
	mu sync.Mutex
	at map[string]time.Time
}

// NewNodeFailures is the failures of a new call: none.
func NewNodeFailures() *NodeFailures {
	return &NodeFailures{at: map[string]time.Time{}}
}

func (f *NodeFailures) failedAt(url string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.at[url]
	return at, ok
}

func (f *NodeFailures) failed(url string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.at[url] = at
}

// Source is an explorer a read asks: its name, its REST URL (<explorer>), its WebSocket's, and the
// HTTP client its reads go through (plain http allowed for a node of a local network only).
type Source struct {
	Name      string
	REST      string
	WebSocket string
	HTTP      *httpx.Client
}

// Explorer is the explorer of a call (SPEC.md §31): mempool.space, or the channel's node with its
// fallback. Each read follows the fallback on its own; a node that fails is remembered in the
// call's NodeFailures, which its reads (and its WebSocket) share, so that for 60 s it is asked
// after mempool.space.
type Explorer struct {
	mempool  Source
	node     *Source
	url      string
	fallback bitcoinnode.Fallback
	failures *NodeFailures
	now      func() time.Time
}

// NewExplorer is the explorer of a call whose channel names node (nil: none, mempool.space), its
// failures shared by the call (a new call's own when nil), over client, with the clock now.
func NewExplorer(client *httpx.Client, node *bitcoinnode.Node, failures *NodeFailures, now func() time.Time) *Explorer {
	if failures == nil {
		failures = NewNodeFailures()
	}
	x := &Explorer{
		mempool:  Source{Name: MempoolExplorer, REST: proto.MempoolURL, WebSocket: proto.MempoolWebSocketURL, HTTP: client},
		failures: failures,
		now:      now,
	}
	if node != nil {
		if at, ok := bitcoinnode.ParseURL(node.URL); ok {
			x.node = &Source{Name: NodeExplorer, REST: at.REST, WebSocket: at.WebSocket, HTTP: client.ForNode()}
			x.url, x.fallback = at.URL, node.Fallback
		}
	}
	return x
}

// Node is the node the explorer reads ("" for none) and its fallback.
func (x *Explorer) Node() (string, bitcoinnode.Fallback) {
	return x.url, x.fallback
}

// Sources are the explorers the next read asks, in order (ExplorersToAsk).
func (x *Explorer) Sources() []Source {
	if x.node == nil {
		return []Source{x.mempool}
	}
	at, failed := x.failures.failedAt(x.url)
	recently := failed && x.now().Sub(at) < proto.NodeFailedRecentlyMs*time.Millisecond
	names := ExplorersToAsk(true, x.fallback, recently)
	sources := make([]Source, len(names))
	for i, name := range names {
		if name == NodeExplorer {
			sources[i] = *x.node
		} else {
			sources[i] = x.mempool
		}
	}
	return sources
}

// NodeFailed records that the node failed a read (or its WebSocket a connection), for the call.
func (x *Explorer) NodeFailed() {
	if x.node != nil {
		x.failures.failed(x.url, x.now())
	}
}

// ReadExplorer is one read of the explorer x: run with each explorer of its Sources, in order,
// until one answers. Each is asked under a context of its own, ended once it failed: what run
// asked of it at the same time (the chain's height with the address's transactions) ends then, and
// the whole read is asked again of the next one, never part of it. network when every one failed;
// an error of another kind (a cancellation: ctx's error) is returned as it is.
func ReadExplorer[T any](ctx context.Context, x *Explorer, run func(ctx context.Context, source Source) (T, error)) (T, error) {
	var failures []error
	var zero T
	for _, source := range x.Sources() {
		attempt, end := context.WithCancel(ctx)
		value, err := run(attempt, source)
		end()
		if err == nil {
			return value, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if !errs.Is(err, errs.Network) {
			return zero, err
		}
		if source.Name == NodeExplorer {
			x.NodeFailed()
		}
		failures = append(failures, err)
	}
	if len(failures) == 1 {
		return zero, failures[0]
	}
	messages := make([]string, len(failures))
	for i, err := range failures {
		messages[i] = err.Error()
	}
	return zero, errs.NewNetwork("Neither the merchant's Bitcoin node nor mempool.space answered ("+strings.Join(messages, "; ")+")", failures[len(failures)-1])
}

// AddressRead is an address's transactions and the chain's height, from one explorer.
type AddressRead struct {
	Txs    []*ExplorerTx
	Height int64
}

// ReadAddress is step 1 of a check (SPEC.md §31, "The on-chain payment"): the transactions of
// address and the chain's height, asked at once of one explorer, never one from each (the
// confirmations of a transaction compare its block_height with that explorer's height). They are
// one read, which fails when either fails: the other's request then ends, and with "mempool" both
// are asked again of mempool.space.
func ReadAddress(ctx context.Context, x *Explorer, address string) (AddressRead, error) {
	return ReadExplorer(ctx, x, func(ctx context.Context, source Source) (AddressRead, error) {
		var read AddressRead
		err := bothEnding(ctx, func(ctx context.Context) error {
			var err error
			read.Txs, err = AddressTransactions(ctx, source.HTTP, source.REST, address)
			return err
		}, func(ctx context.Context) error {
			var err error
			read.Height, err = ChainHeight(ctx, source.HTTP, source.REST)
			return err
		})
		return read, err
	})
}

// ReadTransactions is an address's transactions (when an address is leased: SPEC.md §31,
// "Leasing an address"), one read of the explorer.
func ReadTransactions(ctx context.Context, x *Explorer, address string) ([]*ExplorerTx, error) {
	return ReadExplorer(ctx, x, func(ctx context.Context, source Source) ([]*ExplorerTx, error) {
		return AddressTransactions(ctx, source.HTTP, source.REST, address)
	})
}

// bothEnding runs a and b at once, under a context ended as soon as either fails: the first error
// of the two, once both returned.
func bothEnding(ctx context.Context, a, b func(ctx context.Context) error) error {
	ctx, end := context.WithCancel(ctx)
	defer end()
	var wg sync.WaitGroup
	var errA error
	wg.Add(1)
	go func() {
		defer wg.Done()
		if errA = a(ctx); errA != nil {
			end()
		}
	}()
	errB := b(ctx)
	if errB != nil {
		end()
	}
	wg.Wait()
	// The one that failed first, not the other's end.
	if errA != nil && errB != nil && ctx.Err() != nil {
		if errs.Is(errA, errs.Network) && !errs.Is(errB, errs.Network) {
			return errA
		}
		if errs.Is(errB, errs.Network) && !errs.Is(errA, errs.Network) {
			return errB
		}
	}
	if errA != nil {
		return errA
	}
	return errB
}
