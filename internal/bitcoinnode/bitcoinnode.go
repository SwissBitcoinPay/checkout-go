// Package bitcoinnode is the merchant's Bitcoin node (SPEC.md §31, "The merchant's Bitcoin node"):
// a node of its own that serves mempool.space's API (an Umbrel, a server of its own), which the
// integration's channel names (bitcoinNode, SPEC.md §14) and the server then reads the blockchain
// from, rather than from mempool.space. Its URL follows a rule of its own, not the https URL's
// (SPEC.md §15): http is allowed for a host of a local network, and so is an IPv4 address. Nothing
// is repaired: a URL that is not in its one spelling is no node URL, and the channel then names no
// node.
package bitcoinnode

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
)

// Fallback is what the server does when the node does not answer a read.
type Fallback string

// The fallbacks of a node.
const (
	// Mempool is the same read of mempool.space.
	Mempool Fallback = "mempool"
	// None is never mempool.space: while the node does not answer, nothing is judged on-chain.
	None Fallback = "none"
)

// URL is a node URL, checked: where the node is read.
type URL struct {
	// URL is the node URL, as written.
	URL string
	// REST is <url>/api: the <explorer> of SPEC.md §31, "Explorers".
	REST string
	// WebSocket is <url> with ws for its leading http, then /api/v1/ws.
	WebSocket string
	// Local is a host of a local network: an IPv4 address of a local range, or a name under
	// .local.
	Local bool
	// Internet is a node of the Internet: https, and a name whose last label is not local.
	Internet bool
}

var (
	// nodeURL is <scheme>://<host>[:<port>][<path>]: the host up to ':' or '/' (nothing that may
	// start a user name, a query or a fragment), a port of 1 to 5 digits without a leading zero, a
	// path of visible ASCII. The parts are then checked one by one.
	nodeURL = regexp.MustCompile(`^(https?)://([^/:?#@\\\s]+)(?::([1-9][0-9]{0,4}))?(/[\x21-\x7e]*)?$`)
	// ipv4 is an IPv4 address: 4 numbers from 0 to 255, none with a leading zero.
	ipv4 = regexp.MustCompile(`^(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.` +
		`(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])$`)
	// label is a label of a name: 1 to 63 of a–z 0–9 -, neither starting nor ending with -.
	label = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

// maxNameLength is a name's length, at most.
const maxNameLength = 253

// octetsOf is the four numbers of an IPv4 address; false when host is not one.
func octetsOf(host string) ([4]int, bool) {
	m := ipv4.FindStringSubmatch(host)
	if m == nil {
		return [4]int{}, false
	}
	var octets [4]int
	for i := range octets {
		octets[i], _ = strconv.Atoi(m[i+1])
	}
	return octets, true
}

// isLocalIPv4 tells whether an IPv4 address is of a local range: 10.0.0.0/8, 172.16.0.0/12,
// 192.168.0.0/16, or 100.64.0.0/10 (the shared address space of RFC 6598, which Tailscale uses).
func isLocalIPv4(o [4]int) bool {
	a, b := o[0], o[1]
	return a == 10 || a == 172 && b >= 16 && b <= 31 || a == 192 && b == 168 || a == 100 && b >= 64 && b <= 127
}

// isName tells whether host is a name: 2 labels or more, the last starting with a letter a–z,
// 253 characters at most, never localhost nor a name under it.
func isName(host string) bool {
	if len(host) > maxNameLength {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !label.MatchString(l) {
			return false
		}
	}
	last := labels[len(labels)-1]
	return last[0] >= 'a' && last[0] <= 'z' && last != "localhost"
}

// isNodePath tells whether path (after the authority, "" when absent) is a node URL's: no '?',
// '#', '@' nor '\', not ending with '/' (so '/' alone is not one), nor with /api compared ignoring
// ASCII case (the node's URL, not its API's).
func isNodePath(path string) bool {
	if path == "" {
		return true
	}
	if strings.ContainsAny(path, `?#@\`) || strings.HasSuffix(path, "/") {
		return false
	}
	return len(path) < 4 || !strings.EqualFold(path[len(path)-4:], "/api")
}

// ParseURL reads value as a node URL (SPEC.md §31, "A node URL"), with where the node is read;
// false when it is not one. https, or http for a local host; a host in lowercase, an IPv4 address
// (never in 0.0.0.0/8 nor 127.0.0.0/8) or a name (never localhost nor under it); a port from 1 to
// 65535; a path that ends neither with / nor with /api; 200 characters at most. So no user name
// nor password, no query, no fragment, no IPv6 address, no whitespace.
func ParseURL(value any) (URL, bool) {
	s, ok := value.(string)
	if !ok || len(s) > proto.MaxNodeURLLength {
		return URL{}, false
	}
	m := nodeURL.FindStringSubmatch(s)
	if m == nil {
		return URL{}, false
	}
	scheme, host, port, path := m[1], m[2], m[3], m[4]
	if port != "" {
		if n, _ := strconv.Atoi(port); n > 65535 {
			return URL{}, false
		}
	}
	if !isNodePath(path) {
		return URL{}, false
	}
	octets, isIPv4 := octetsOf(host)
	var local bool
	if isIPv4 {
		// Never the machine itself, nor "this network".
		if octets[0] == 0 || octets[0] == 127 {
			return URL{}, false
		}
		local = isLocalIPv4(octets)
	} else {
		if !isName(host) {
			return URL{}, false
		}
		local = strings.HasSuffix(host, ".local")
	}
	if scheme == "http" && !local {
		return URL{}, false
	}
	return URL{
		URL:       s,
		REST:      s + "/api",
		WebSocket: "ws" + strings.TrimPrefix(s, "http") + "/api/v1/ws",
		Local:     local,
		Internet:  scheme == "https" && !isIPv4 && !local,
	}, true
}

// fallbackOf reads a fallback: exactly "mempool" or "none".
func fallbackOf(v any) (Fallback, bool) {
	switch v {
	case string(Mempool):
		return Mempool, true
	case string(None):
		return None, true
	}
	return "", false
}

// Node is the channel's bitcoinNode, valid.
type Node struct {
	// URL is a node URL (ParseURL), as the channel writes it.
	URL      string
	Fallback Fallback
	// Payers tells whether the pages of checkout links read the node too (a node of the Internet
	// only).
	Payers bool
}

// Of reads the channel's bitcoinNode (SPEC.md §14, §31): an object, not an array, whose url is a
// node URL, whose fallback is exactly "mempool" or "none" and whose payers is a JSON boolean, true
// only for a node of the Internet; its other members ignored. nil when it is absent or invalid in
// any way: there is none, the server reads mempool.space, and the channel stays valid.
func Of(value any) *Node {
	o, ok := jsonv.AsObject(value)
	if !ok {
		return nil
	}
	urlValue, _ := o.Get("url")
	fallbackValue, _ := o.Get("fallback")
	payersValue, _ := o.Get("payers")
	u, ok := ParseURL(urlValue)
	if !ok {
		return nil
	}
	fallback, ok := fallbackOf(fallbackValue)
	if !ok {
		return nil
	}
	payers, ok := payersValue.(bool)
	// A link must never make each customer's browser probe its own local network.
	if !ok || payers && !u.Internet {
		return nil
	}
	return &Node{URL: u.URL, Fallback: fallback, Payers: payers}
}

// RecordNode is the node a checkout link's record names (SPEC.md §30): its URL and its fallback.
type RecordNode struct {
	URL      string
	Fallback Fallback
}

// ForRecord is what a checkout link's record of version 2 holds of node (SPEC.md §30): its url
// and fallback, copied as they are, never payers; nil (the field absent) when the channel names no
// node or its payers is false.
func ForRecord(node *Node) *RecordNode {
	if node == nil || !node.Payers {
		return nil
	}
	return &RecordNode{URL: node.URL, Fallback: node.Fallback}
}

// JSON is the record's bitcoinNode: {"url": …, "fallback": …}.
func (n *RecordNode) JSON() jsonv.Members {
	return jsonv.O("url", n.URL, "fallback", string(n.Fallback))
}

// OfRecord reads the node a page reads from a record of version 2's bitcoinNode (SPEC.md §30,
// "Reading a record"): an object whose url is a node URL of a node of the Internet and whose
// fallback is exactly "mempool" or "none" (its other members ignored); anything else (null
// included) is set aside, nil: the page reads mempool.space, and the record stays valid.
func OfRecord(value any) *RecordNode {
	o, ok := jsonv.AsObject(value)
	if !ok {
		return nil
	}
	urlValue, _ := o.Get("url")
	fallbackValue, _ := o.Get("fallback")
	u, ok := ParseURL(urlValue)
	if !ok || !u.Internet {
		return nil
	}
	fallback, ok := fallbackOf(fallbackValue)
	if !ok {
		return nil
	}
	return &RecordNode{URL: u.URL, Fallback: fallback}
}
