package onchain

import (
	"context"
	"sort"
	"sync"

	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/httpx"
	"github.com/sattleme/checkout-go/internal/lnurl"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/tolerance"
)

// The payment of an invoice (SPEC.md §31, "The on-chain payment"): its on-chain part, from the
// explorer and the witnesses of its order, its rest part (the Lightning invoices of the rest of an
// underpayment), and how they combine with its Lightning part.

// The kinds of an on-chain part.
const (
	Underpaid   = "underpaid"
	Unconfirmed = "unconfirmed"
	Paid        = "paid"
)

// OnchainPart is the on-chain part of an invoice with an address: underpaid (less than the amount
// came in time: Final once the lease ended and nothing is undecided), unconfirmed, or paid (3
// confirmations on the explorer read).
type OnchainPart struct {
	Kind          string
	Received      int64
	Final         bool
	Confirmations int64
	Txids         []string
}

// OnchainCheck is what the on-chain part of an invoice needs.
type OnchainCheck struct {
	// Address, Sats and ExpiresAt are the invoice's deposit address, its amount and its end.
	Address   string
	Sats      int64
	ExpiresAt int64
	// Sale is the order's sale: its author (hex).
	Sale string
	// Clock is the server's clock, in Unix seconds, read once the explorer answered: a transaction
	// it lists was broadcast by then (SPEC.md §31, "The on-chain payment", step 3.4).
	Clock func() int64
	// HTTP asks BlockCypher.
	HTTP *httpx.Client
	// Explorer is the explorer of the call: mempool.space, or the merchant's Bitcoin node with its
	// fallback.
	Explorer *Explorer
	// ReadWitnesses reads the witnesses of the order (network when fewer than a majority
	// answered); everyRelay reads them on every relay, each until its timeout.
	ReadWitnesses func(everyRelay bool) (*OrderWitnesses, error)
	// WitnessAddress is the witness address of txid paying the address for the sale, or for none
	// ("").
	WitnessAddress func(txid, sale string) string
	// Publish publishes a witness on a majority of the merchant's relays, else network.
	Publish func(w Witness) error
	Logger  logx.Logger
	// PaidByRests is what the invoice's rest invoices paid (its rest part, checked at the same
	// time; one that failed counts as not paid): the on-chain part needs only the difference. nil:
	// 0.
	PaidByRests func() int64
	// Tolerance is the order's tolerance of on-chain underpayments (its lease's, basis points),
	// read with the sale's records at the same time. nil: 0 (none, or the records unread).
	Tolerance func() int64
	// ReadMerchantAcceptance reads the merchant's acceptance of the invoice (merchant_acceptance.go)
	// on every relay, at the same time as the witnesses, when some transaction pays the address
	// (network when fewer than a majority answered). nil: none.
	ReadMerchantAcceptance func() (MerchantAcceptance, error)
}

// The kinds of a transaction paying the address (SPEC.md §31, step 3).
const (
	kindInTime      = "in time"
	kindReplacement = "replacement"
	kindLate        = "late"
	kindUndecided   = "undecided"
	kindAnothers    = "another's"
)

type paying struct {
	tx   *ExplorerTx
	pays int64
}

// both runs two calls at once: the first error of the two.
func both(a, b func() error) error {
	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(1)
	go func() { defer wg.Done(); errA = a() }()
	errB = b()
	wg.Wait()
	if errA != nil {
		return errA
	}
	return errB
}

// CheckOnchainPart checks the on-chain part of an invoice at now, the clock once the explorer
// answered (SPEC.md §31, "The on-chain payment"): each transaction paying its address in time
// (while the lease ran, as a witness of the sale or BlockCypher's time says), a replacement of
// payments in time, late, or undecided; what counts of them; and, once enough counts, their
// confirmations on the explorer. It publishes the witnesses it finds (of the sale on a majority,
// else network; of none for the merchant's app only, a failure logged), never over a witness of a
// newer format, which it logs (SPEC.md §12). What is due on-chain is the least the order's
// tolerance accepts of the amount (the amount without tolerance), or the received of the
// merchant's acceptance a majority of the relays sent when less, less what its rest invoices paid,
// a sat at least (tolerance.DueOnchain): when they pay the amount, the on-chain part is paid
// without transactions. network when the explorer (the node and, with "mempool", mempool.space),
// BlockCypher or the relays fail: with "none" and the node down, never final underpaid.
func CheckOnchainPart(ctx context.Context, c OnchainCheck) (OnchainPart, error) {
	end := c.ExpiresAt + proto.GraceSeconds
	// The address's transactions and the chain's height, of one explorer.
	read, err := ReadAddress(ctx, c.Explorer, c.Address)
	if err != nil {
		return OnchainPart{}, err
	}
	txs, height := read.Txs, read.Height
	// After the explorer's answer, never before: listed now, a transaction was broadcast by now.
	now := c.Clock()
	var pays []paying
	listed := map[string]bool{}
	for _, tx := range txs {
		amount := PaysTo(tx, c.Address)
		if amount > 0 && !listed[tx.Txid] {
			listed[tx.Txid] = true
			pays = append(pays, paying{tx: tx, pays: amount})
		}
	}
	// The witnesses, and the merchant's acceptance with them (on every relay), when some
	// transaction pays the address.
	witnesses := &OrderWitnesses{Sale: map[string]SaleWitness{}, Others: map[string]SaleWitness{}, None: map[string]bool{}, Newer: map[string]bool{}}
	var accepted MerchantAcceptance
	if len(pays) > 0 {
		err := both(func() error {
			var err error
			witnesses, err = c.ReadWitnesses(false)
			return err
		}, func() error {
			if c.ReadMerchantAcceptance == nil {
				return nil
			}
			var err error
			accepted, err = c.ReadMerchantAcceptance()
			return err
		})
		if err != nil {
			return OnchainPart{}, err
		}
	}
	if len(witnesses.Newer) > 0 {
		c.Logger.Warn("github.com/sattleme/checkout-go: a witness of this address has a newer format than this version reads; update the package")
	}
	if accepted.Newer {
		c.Logger.Warn("github.com/sattleme/checkout-go: a merchant's acceptance of this order has a newer format than this version reads; " +
			"update the package")
	}

	// The parts: from the witnesses of this invoice and of other invoices (in the order they were
	// seen), then the transactions no such witness names (their order unknown): a replacement of
	// another invoice's payment stands for it.
	partsOf := func(read *OrderWitnesses) map[string][]Part {
		var list []PaymentTx
		for _, named := range []map[string]SaleWitness{read.Sale, read.Others} {
			txids := make([]string, 0, len(named))
			for txid := range named {
				txids = append(txids, txid)
			}
			// The order of a map's entries means nothing: PaymentParts sorts by when each was seen.
			sort.Strings(txids)
			for _, txid := range txids {
				w := named[txid]
				list = append(list, PaymentTx{Txid: txid, Sats: w.Sats, Inputs: w.Inputs, Order: w.SeenAt, HasOrder: true, Listed: listed[txid]})
			}
		}
		for _, p := range pays {
			if _, ok := read.Sale[p.tx.Txid]; ok {
				continue
			}
			if _, ok := read.Others[p.tx.Txid]; ok {
				continue
			}
			list = append(list, PaymentTx{Txid: p.tx.Txid, Sats: p.pays, Inputs: p.tx.Coins, Listed: true})
		}
		return PaymentParts(list)
	}
	// anothers is another invoice's: a witness of another invoice names it, or the root of one of
	// its parts.
	anothers := func(read *OrderWitnesses, parts map[string][]Part, txid string) bool {
		if _, ok := read.Others[txid]; ok {
			return true
		}
		for _, part := range parts[txid] {
			if _, ok := read.Others[part.Root]; ok {
				return true
			}
		}
		return false
	}
	parts := partsOf(witnesses)
	newerNames := func(txid string) bool { return witnesses.Newer[c.WitnessAddress(txid, c.Sale)] }
	// A witness of a newer format that names no transaction listed may be of a payment replaced.
	newerElsewhere := false
	for d := range witnesses.Newer {
		names := false
		for _, p := range pays {
			if d == c.WitnessAddress(p.tx.Txid, c.Sale) || d == c.WitnessAddress(p.tx.Txid, "") {
				names = true
				break
			}
		}
		if !names {
			newerElsewhere = true
		}
	}

	kinds := map[string]string{}
	// Witnesses of the sale to publish (on a majority), and of none (logged when they fail).
	var ofSale, ofNone []Witness
	witnessOf := func(tx *ExplorerTx, amount int64, forSale, replacement bool) Witness {
		inputs := tx.Coins
		if len(inputs) > proto.MaxWitnessInputs {
			inputs = inputs[:proto.MaxWitnessInputs]
		}
		w := Witness{Address: c.Address, Txid: tx.Txid, Sats: amount, Inputs: inputs, Replacement: replacement}
		if forSale {
			w.Sale, w.ExpiresAt = c.Sale, c.ExpiresAt
		}
		return w
	}
	for _, p := range pays {
		txid := p.tx.Txid
		// Another invoice's: a transaction counts for one invoice at most.
		if anothers(witnesses, parts, txid) {
			kinds[txid] = kindAnothers
			continue
		}
		witness, named := witnesses.Sale[txid]
		if named && !witness.Replacement {
			kinds[txid] = kindInTime
			continue
		}
		if newerNames(txid) {
			kinds[txid] = kindUndecided
			continue
		}
		if now <= end {
			// Listed now, it was broadcast by now: while the lease runs.
			kinds[txid] = kindInTime
			ofSale = append(ofSale, witnessOf(p.tx, p.pays, true, false))
			continue
		}
		if named {
			// An earlier check found it a replacement, and BlockCypher's time of it late.
			kinds[txid] = kindReplacement
			continue
		}
		// No check saw it in time.
		own := parts[txid]
		replaces, ofItself := false, false
		for _, part := range own {
			if part.Root != txid {
				replaces = true
			} else {
				ofItself = true
			}
		}
		if ofItself {
			ms, known, err := ReceivedAt(ctx, c.HTTP, txid)
			if err != nil {
				return OnchainPart{}, err
			}
			if known && ms <= end*1000 {
				kinds[txid] = kindInTime
				ofSale = append(ofSale, witnessOf(p.tx, p.pays, true, false))
				continue
			}
		}
		switch {
		case newerElsewhere:
			kinds[txid] = kindUndecided
		case replaces:
			kinds[txid] = kindReplacement
			ofSale = append(ofSale, witnessOf(p.tx, p.pays, true, true))
		default:
			// Its payment reaches the merchant's wallet, but no order. A witness none names it, or
			// one of a newer format at that witness's address: never written over (SPEC.md §12).
			kinds[txid] = kindLate
			if !witnesses.None[txid] && !witnesses.Newer[c.WitnessAddress(txid, "")] {
				ofNone = append(ofNone, witnessOf(p.tx, p.pays, false, false))
			}
		}
	}
	if err := publishAll(c, ofSale, ofNone); err != nil {
		return OnchainPart{}, err
	}
	// Its witnesses on a majority, read again on every relay: of two checks that publish witnesses
	// of one transaction at once, the later reader sees both, whatever one relay hides.
	if len(ofSale) > 0 {
		again, err := c.ReadWitnesses(true)
		if err != nil {
			return OnchainPart{}, err
		}
		partsAgain := partsOf(again)
		for _, p := range pays {
			if anothers(again, partsAgain, p.tx.Txid) {
				kinds[p.tx.Txid] = kindAnothers
			}
		}
	}

	inTime := func(root string) bool {
		if listed[root] {
			return kinds[root] == kindInTime
		}
		w, ok := witnesses.Sale[root]
		return ok && !w.Replacement
	}
	counted := map[string]int64{}
	var received int64
	for _, p := range pays {
		kind := kinds[p.tx.Txid]
		if kind == kindUndecided || kind == kindAnothers {
			continue
		}
		var sum int64
		for _, part := range parts[p.tx.Txid] {
			if inTime(part.Root) {
				sum = AddSats(sum, part.Sats)
			}
		}
		if sum > 0 {
			counted[p.tx.Txid] = sum
			received = AddSats(received, sum)
		}
	}
	var paidByRests, orderTolerance int64
	if c.PaidByRests != nil {
		paidByRests = c.PaidByRests()
	}
	if c.Tolerance != nil {
		orderTolerance = c.Tolerance()
	}
	if err := ctx.Err(); err != nil {
		return OnchainPart{}, err
	}
	// Nothing due when the rest invoices paid it all; else the least the order's tolerance
	// accepts, or what the merchant accepted when less, less what they paid, at least a sat.
	due := tolerance.DueOnchain(c.Sats, orderTolerance, paidByRests, accepted.Received)
	if due == 0 {
		return OnchainPart{Kind: Paid, Received: received, Txids: []string{}}, nil
	}
	if received < due {
		undecided := false
		for _, kind := range kinds {
			if kind == kindUndecided {
				undecided = true
			}
		}
		return OnchainPart{Kind: Underpaid, Received: received, Final: now > end && !undecided}, nil
	}
	var order []*ExplorerTx
	txids := []string{}
	for _, p := range pays {
		if _, ok := counted[p.tx.Txid]; ok {
			order = append(order, p.tx)
			txids = append(txids, p.tx.Txid)
		}
	}
	confirmations := int64(-1)
	for _, tx := range order {
		if n := ConfirmationsOf(tx, height); confirmations < 0 || n < confirmations {
			confirmations = n
		}
	}
	// 3 confirmations on the explorer read: a payment counts as made on what it alone shows.
	if confirmations >= proto.Confirmations {
		return OnchainPart{Kind: Paid, Received: received, Txids: txids}, nil
	}
	return OnchainPart{Kind: Unconfirmed, Received: received, Confirmations: confirmations, Txids: txids}, nil
}

// publishAll publishes the witnesses of the sale (the first failure is the check's) and of none
// (each failure logged), all at once.
func publishAll(c OnchainCheck, ofSale, ofNone []Witness) error {
	var wg sync.WaitGroup
	errors := make([]error, len(ofSale))
	for i, w := range ofSale {
		wg.Add(1)
		go func(i int, w Witness) {
			defer wg.Done()
			errors[i] = c.Publish(w)
		}(i, w)
	}
	var mu sync.Mutex
	var noneErr error
	for _, w := range ofNone {
		wg.Add(1)
		go func(w Witness) {
			defer wg.Done()
			if err := c.Publish(w); err != nil {
				if errs.Is(err, errs.Network) {
					c.Logger.Warn("github.com/sattleme/checkout-go: the witness of a late payment was not published: " + err.Error())
					return
				}
				mu.Lock()
				noneErr = err
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return noneErr
}

// LightningResult is a Lightning part's outcome: its part, or the error it failed with.
type LightningResult struct {
	Part lnurl.Part
	Err  error
}

// OnchainResult is an on-chain part's outcome.
type OnchainResult struct {
	Part OnchainPart
	Err  error
}

// RestPart is the rest part of an invoice: its rest invoices, and the Lightning part of each, in
// order.
type RestPart struct {
	Invoices []RestInvoice
	Results  []LightningResult
	// Err is the sale's records that could not be read: whether a rest invoice was paid cannot be
	// told.
	Err error
	// Unrecorded are the payment hashes of the rest invoices whose rest record is not known on a
	// majority of the relays: never the status's bolt11 (a checkout link's page might never see
	// their payment).
	Unrecorded map[string]bool
}

// InvoiceTerms is what an invoice is, for its status: its amount, its end and its address ("":
// none).
type InvoiceTerms struct {
	Sats      int64
	ExpiresAt int64
	Address   string
}

// RestSummary is what the rest invoices paid (each payment hash once; one that failed counts as
// not paid), their preimages in order, whether each is final, and the first error.
type RestSummary struct {
	Paid      int64
	Preimages []string
	Hashes    map[string]bool
	Final     bool
	Err       error
}

// SummaryOf summarizes the rest part (nil: none).
func SummaryOf(rests *RestPart) RestSummary {
	s := RestSummary{Preimages: []string{}, Hashes: map[string]bool{}, Final: true}
	if rests == nil {
		return s
	}
	if rests.Err != nil {
		s.Final, s.Err = false, rests.Err
	}
	// In the order of the rest invoices (by date, then payment hash), whatever the order given.
	type entry struct {
		invoice RestInvoice
		result  LightningResult
	}
	order := make([]entry, len(rests.Invoices))
	for i := range rests.Invoices {
		order[i] = entry{rests.Invoices[i], rests.Results[i]}
	}
	sort.SliceStable(order, func(i, j int) bool { return RestBefore(order[i].invoice, order[j].invoice) })
	for _, e := range order {
		if e.result.Err != nil {
			s.Final = false
			if s.Err == nil {
				s.Err = e.result.Err
			}
			continue
		}
		switch e.result.Part.State {
		case lnurl.Paid:
			if s.Hashes[e.invoice.PaymentHash] {
				continue
			}
			s.Hashes[e.invoice.PaymentHash] = true
			s.Paid = AddSats(s.Paid, e.invoice.Sats)
			s.Preimages = append(s.Preimages, e.result.Part.Preimage)
		case lnurl.Open:
			s.Final = false
		}
	}
	return s
}

// CombineParts is the status of an invoice from its parts (SPEC.md §31, "The on-chain payment"):
// paid in Lightning, else paid by its rest invoices alone, else paid on-chain (mixed when rest
// invoices paid part of it), else unconfirmed on-chain, else an error of a part (the Lightning
// part's first, then the rest part's, then the on-chain part's), else expired once every part is
// final, else open, with its rest. A part is nil when the invoice has none (no Lightning invoice,
// no address). Received counts what the rest invoices paid.
func CombineParts(invoice InvoiceTerms, now int64, lightning *LightningResult, onchain *OnchainResult, rests *RestPart) (*model.PaymentStatus, error) {
	if lightning != nil && lightning.Err == nil && lightning.Part.State == lnurl.Paid {
		return &model.PaymentStatus{State: model.Paid, Method: model.Lightning, Preimage: lightning.Part.Preimage}, nil
	}
	summary := SummaryOf(rests)
	onchainOK := onchain != nil && onchain.Err == nil
	if summary.Paid >= invoice.Sats {
		// What came on-chain in time counts too (0 when the on-chain part failed).
		var onchainReceived int64
		if onchainOK {
			onchainReceived = onchain.Part.Received
		}
		return &model.PaymentStatus{State: model.Paid, Method: model.Mixed, Received: AddSats(onchainReceived, summary.Paid), Txids: []string{}, Preimages: summary.Preimages}, nil
	}
	if onchainOK && onchain.Part.Kind == Paid {
		received := AddSats(onchain.Part.Received, summary.Paid)
		if summary.Paid > 0 {
			return &model.PaymentStatus{State: model.Paid, Method: model.Mixed, Received: received, Txids: onchain.Part.Txids, Preimages: summary.Preimages}, nil
		}
		return &model.PaymentStatus{State: model.Paid, Method: model.Onchain, Received: received, Txids: onchain.Part.Txids}, nil
	}
	if onchainOK && onchain.Part.Kind == Unconfirmed {
		return &model.PaymentStatus{State: model.Unconfirmed, Received: AddSats(onchain.Part.Received, summary.Paid), Confirmations: int(onchain.Part.Confirmations), Txids: onchain.Part.Txids}, nil
	}
	if lightning != nil && lightning.Err != nil {
		return nil, lightning.Err
	}
	if summary.Err != nil {
		return nil, summary.Err
	}
	if onchain != nil && onchain.Err != nil {
		return nil, onchain.Err
	}
	var underpaid *OnchainPart
	if onchainOK && onchain.Part.Kind == Underpaid {
		underpaid = &onchain.Part
	}
	received := summary.Paid
	if underpaid != nil {
		received = AddSats(received, underpaid.Received)
	}
	lightningFinal := lightning == nil || lightning.Part.State == lnurl.Expired
	onchainFinal := onchain == nil || underpaid != nil && underpaid.Final
	if lightningFinal && onchainFinal && summary.Final {
		return &model.PaymentStatus{State: model.Expired, Received: received}, nil
	}
	status := &model.PaymentStatus{State: model.Open, ExpiresAt: invoice.ExpiresAt, Received: received}
	if invoice.Address != "" {
		var shown []RestInvoice
		if rests != nil {
			for _, r := range rests.Invoices {
				if !rests.Unrecorded[r.PaymentHash] {
					shown = append(shown, r)
				}
			}
		}
		status.Rest = RestOf(invoice.Sats, invoice.ExpiresAt, invoice.Address, received, now, shown, summary.Hashes)
	}
	return status, nil
}
