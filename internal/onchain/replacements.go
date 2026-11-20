package onchain

import "sort"

// PaymentTx is a transaction paying the address, as the parts are made of it (SPEC.md §31,
// "Replacements").
type PaymentTx struct {
	Txid string
	// Sats is what it pays the address.
	Sats int64
	// Inputs are the coins it spends (<txid>:<vout>), or those recorded of it.
	Inputs []string
	// Order is when it was seen (a witness's created_at); HasOrder false: unknown, after every
	// known one.
	Order    int64
	HasOrder bool
	// Listed tells whether the explorer lists it now.
	Listed bool
}

// Part is a part of a listed transaction: sats it pays as its root (itself, for a part of its
// own).
type Part struct {
	Sats int64
	Root string
}

// PaymentParts is the parts of each listed transaction, by txid: a txid given twice is taken once
// (its first entry); the transactions in order (unknown last, then by txid); the roots of each are
// those before it that spend one of its coins (their roots, put together), else itself; each root
// gives at most its sats, in that order, then what a transaction still pays is a part of its own.
func PaymentParts(txs []PaymentTx) map[string][]Part {
	seen := map[string]bool{}
	var unique []PaymentTx
	for _, tx := range txs {
		if !seen[tx.Txid] {
			seen[tx.Txid] = true
			unique = append(unique, tx)
		}
	}
	sort.SliceStable(unique, func(i, j int) bool {
		a, b := unique[i], unique[j]
		if a.HasOrder != b.HasOrder {
			return a.HasOrder
		}
		if a.HasOrder && a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.Txid < b.Txid
	})
	position := map[string]int{}
	for i, tx := range unique {
		position[tx.Txid] = i
	}
	roots := map[string][]string{}
	for i, tx := range unique {
		coins := map[string]bool{}
		for _, coin := range tx.Inputs {
			coins[coin] = true
		}
		together := map[string]bool{}
		var list []string
		for _, other := range unique[:i] {
			spendsOne := false
			for _, coin := range other.Inputs {
				if coins[coin] {
					spendsOne = true
					break
				}
			}
			if !spendsOne {
				continue
			}
			for _, root := range roots[other.Txid] {
				if !together[root] {
					together[root] = true
					list = append(list, root)
				}
			}
		}
		if len(list) == 0 {
			roots[tx.Txid] = []string{tx.Txid}
			continue
		}
		sort.Slice(list, func(a, b int) bool { return position[list[a]] < position[list[b]] })
		roots[tx.Txid] = list
	}
	toGive := map[string]int64{}
	for _, tx := range unique {
		toGive[tx.Txid] = tx.Sats
	}
	parts := map[string][]Part{}
	for _, tx := range unique {
		if !tx.Listed {
			continue
		}
		own := roots[tx.Txid]
		owed := tx.Sats
		var list []Part
		if len(own) == 1 && own[0] == tx.Txid {
			// A payment of its own: all its sats, which it no longer has to give.
			toGive[tx.Txid] = 0
		} else {
			for _, root := range own {
				left := toGive[root]
				take := owed
				if left < take {
					take = left
				}
				if take > 0 {
					list = append(list, Part{Sats: take, Root: root})
					toGive[root] = left - take
					owed -= take
				}
			}
		}
		if owed > 0 {
			list = append(list, Part{Sats: owed, Root: tx.Txid})
		}
		parts[tx.Txid] = list
	}
	return parts
}
