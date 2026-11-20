package testkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
)

// The blockchain for tests: deposit addresses (taproot, bech32m) made from a seed, and fakes of
// the explorers (mempool.space and merchants' Bitcoin nodes, Esplora REST), of BlockCypher and of
// the explorers' WebSockets (mempool.space's track-address, and a node's). Never the network.

// DepositAddress is the deposit address (bc1p…) of a 32-byte program derived from seed.
func DepositAddress(seed string) string {
	program := sha256.Sum256([]byte("deposit address " + seed))
	words := append([]int{1}, BytesToWords(program[:])...)
	return EncodeBech32("bc", words, 0x2bc830a3)
}

// Txid is a txid made from seed.
func Txid(seed string) string {
	h := sha256.Sum256([]byte("transaction " + seed))
	return hex.EncodeToString(h[:])
}

// Vin is an input of a transaction as Esplora writes it.
type Vin struct {
	Txid     string
	Vout     int64
	Coinbase bool
}

// Vout is an output of a transaction as Esplora writes it: Address "" has no
// scriptpubkey_address.
type Vout struct {
	Value   int64
	Address string
}

// EsploraTx is a transaction as Esplora writes it.
type EsploraTx struct {
	Txid        string
	Vin         []Vin
	Vout        []Vout
	Confirmed   bool
	BlockHeight int64
}

// JSON is the transaction as an explorer answers it.
func (tx *EsploraTx) JSON() jsonv.Members {
	vin := make([]any, len(tx.Vin))
	for i, in := range tx.Vin {
		if in.Coinbase {
			vin[i] = jsonv.O("is_coinbase", true)
		} else {
			vin[i] = jsonv.O("txid", in.Txid, "vout", in.Vout, "is_coinbase", false)
		}
	}
	vout := make([]any, len(tx.Vout))
	for i, out := range tx.Vout {
		if out.Address == "" {
			vout[i] = jsonv.O("value", out.Value)
		} else {
			vout[i] = jsonv.O("value", out.Value, "scriptpubkey_address", out.Address)
		}
	}
	status := jsonv.O("confirmed", false)
	if tx.Confirmed {
		status = jsonv.O("confirmed", true, "block_height", tx.BlockHeight)
	}
	return jsonv.O("txid", tx.Txid, "vin", vin, "vout", vout, "status", status)
}

// Payment is a transaction id paying address sats, spending coins (<txid>:<vout>; the coin of
// its id when none), with some change; confirmed at height when it is above 0.
func Payment(id, address string, sats int64, coins []string, height int64) *EsploraTx {
	if coins == nil {
		coins = []string{Txid("coin of "+id) + ":0"}
	}
	tx := &EsploraTx{Txid: id, Vout: []Vout{{Value: sats, Address: address}, {Value: 5000, Address: DepositAddress("change")}}}
	for _, coin := range coins {
		spent, vout, _ := strings.Cut(coin, ":")
		n, _ := strconv.ParseInt(vout, 10, 64)
		tx.Vin = append(tx.Vin, Vin{Txid: spent, Vout: n})
	}
	if height > 0 {
		tx.Confirmed, tx.BlockHeight = true, height
	}
	return tx
}

// The services of the chain that may be down; a node is the service of its URL (AddNode).
const (
	// Explorer is mempool.space.
	Explorer    = "explorer"
	BlockCypher = "blockcypher"
)

// FakeChain is the explorers (mempool.space, and the nodes added) and BlockCypher, as a test sets
// them: every explorer shows the same chain. Safe for concurrent use.
type FakeChain struct {
	mu sync.Mutex
	// height is the chain's height; heights the height an explorer shows instead, by service.
	height  int64
	heights map[string]int64
	// nodes are the REST URL (<url>/api) of each node, by its URL.
	nodes map[string]string
	// heightDown are the explorers whose chain's height fails (their address reads answer).
	heightDown map[string]bool
	// txs are the transactions of each address, in the explorer's order (newest first).
	txs map[string][]*EsploraTx
	// received is when BlockCypher received each transaction it knows (RFC 3339).
	received map[string]string
	down     map[string]bool
	asked    []string
}

// NewFakeChain is a chain at height 970 000.
func NewFakeChain() *FakeChain {
	return &FakeChain{height: 970_000, heights: map[string]int64{}, nodes: map[string]string{}, heightDown: map[string]bool{},
		txs: map[string][]*EsploraTx{}, received: map[string]string{}, down: map[string]bool{}}
}

// SetHeight sets the chain's height.
func (c *FakeChain) SetHeight(height int64) {
	c.mu.Lock()
	c.height = height
	c.mu.Unlock()
}

// AddNode serves the chain at a merchant's Bitcoin node of url (a node URL: <url>/api), the
// service url.
func (c *FakeChain) AddNode(url string) {
	c.mu.Lock()
	c.nodes[url] = url + "/api"
	c.mu.Unlock()
}

// SetExplorerHeight sets the height the explorer service shows (0: the chain's).
func (c *FakeChain) SetExplorerHeight(service string, height int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if height == 0 {
		delete(c.heights, service)
	} else {
		c.heights[service] = height
	}
}

// SetHeightDown makes the explorer service's height fail (HTTP 503), its address reads answering;
// or answer again.
func (c *FakeChain) SetHeightDown(service string, down bool) {
	c.mu.Lock()
	c.heightDown[service] = down
	c.mu.Unlock()
}

// Height is the chain's height.
func (c *FakeChain) Height() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.height
}

// Add adds tx to the transactions of address, first (the explorer lists the newest first).
func (c *FakeChain) Add(address string, tx *EsploraTx) {
	c.mu.Lock()
	c.txs[address] = append([]*EsploraTx{tx}, c.txs[address]...)
	c.mu.Unlock()
}

// Drop drops id from the mempool (replaced, or dropped).
func (c *FakeChain) Drop(address, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var kept []*EsploraTx
	for _, tx := range c.txs[address] {
		if tx.Txid != id {
			kept = append(kept, tx)
		}
	}
	c.txs[address] = kept
}

// Confirm confirms id at height.
func (c *FakeChain) Confirm(address, id string, height int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tx := range c.txs[address] {
		if tx.Txid == id {
			tx.Confirmed, tx.BlockHeight = true, height
		}
	}
}

// SetReceived sets when BlockCypher received txid (RFC 3339); "" forgets it.
func (c *FakeChain) SetReceived(txid, at string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at == "" {
		delete(c.received, txid)
	} else {
		c.received[txid] = at
	}
}

// SetDown makes a service fail (HTTP 5xx), or answer again: Explorer (mempool.space), BlockCypher,
// or a node's URL.
func (c *FakeChain) SetDown(service string, down bool) {
	c.mu.Lock()
	c.down[service] = down
	c.mu.Unlock()
}

// Asked are the URLs asked, in order.
func (c *FakeChain) Asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

// BlockCypherAsked are the URLs of BlockCypher asked.
func (c *FakeChain) BlockCypherAsked() []string {
	var out []string
	for _, u := range c.Asked() {
		if strings.HasPrefix(u, proto.BlockCypherURL) {
			out = append(out, u)
		}
	}
	return out
}

var addressPath = regexp.MustCompile(`^/address/([a-z0-9]+)/txs$`)

// explorerOf is the explorer service of url and its REST URL, false when it is none's.
func (c *FakeChain) explorerOf(url string) (service, base string, ok bool) {
	if strings.HasPrefix(url, proto.MempoolURL+"/") {
		return Explorer, proto.MempoolURL, true
	}
	for node, rest := range c.nodes {
		if strings.HasPrefix(url, rest+"/") {
			return node, rest, true
		}
	}
	return "", "", false
}

// Route answers url when it is an explorer's or BlockCypher's.
func (c *FakeChain) Route(url string) (Answer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if service, base, ok := c.explorerOf(url); ok {
		c.asked = append(c.asked, url)
		if c.down[service] {
			return Text(503, "down"), true
		}
		path := strings.TrimPrefix(url, base)
		if path == "/blocks/tip/height" {
			if c.heightDown[service] {
				return Text(503, "down"), true
			}
			height, ok := c.heights[service]
			if !ok {
				height = c.height
			}
			return Text(200, strconv.FormatInt(height, 10)), true
		}
		if m := addressPath.FindStringSubmatch(path); m != nil {
			list := make([]any, 0, len(c.txs[m[1]]))
			for _, tx := range c.txs[m[1]] {
				list = append(list, tx.JSON())
			}
			return JSON(list), true
		}
		return Text(404, "Not found"), true
	}
	if strings.HasPrefix(url, proto.BlockCypherURL) {
		c.asked = append(c.asked, url)
		if c.down[BlockCypher] {
			return Status(500, jsonv.O("error", "down")), true
		}
		path := url
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		id := path[strings.LastIndexByte(path, '/')+1:]
		received, ok := c.received[id]
		if !ok {
			return Status(404, jsonv.O("error", "Transaction "+id+" not found.")), true
		}
		return JSON(jsonv.O("hash", id, "received", received, "confirmations", 0)), true
	}
	return Answer{}, false
}

// FakeExplorerSocket is mempool.space's WebSocket for tests: it records what each client sends
// and the headers of each handshake, and pushes what a test says changed.
type FakeExplorerSocket struct {
	mu       sync.Mutex
	messages []any
	headers  []http.Header
	conns    []*websocket.Conn
	open     int
}

// Open is how many connections it has open now.
func (s *FakeExplorerSocket) Open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

func (s *FakeExplorerSocket) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	c, err := websocket.Accept(w, req, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	s.mu.Lock()
	s.headers = append(s.headers, req.Header.Clone())
	s.conns = append(s.conns, c)
	s.open++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.open--
		s.mu.Unlock()
	}()
	defer c.CloseNow()
	for {
		_, data, err := c.Read(req.Context())
		if err != nil {
			return
		}
		v, err := jsonv.Parse(data)
		if err != nil {
			continue
		}
		s.mu.Lock()
		s.messages = append(s.messages, v)
		s.mu.Unlock()
	}
}

// Messages are what clients sent, as JSON.
func (s *FakeExplorerSocket) Messages() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.messages...)
}

// Handshakes are the headers of each connection's handshake.
func (s *FakeExplorerSocket) Handshakes() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

// Connections is how many connections it took.
func (s *FakeExplorerSocket) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.headers)
}

// Push pushes message to every client.
func (s *FakeExplorerSocket) Push(message any) {
	s.mu.Lock()
	conns := append([]*websocket.Conn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.Write(ctx, websocket.MessageText, jsonv.Marshal(message))
		cancel()
	}
}

// Drop drops every client's connection.
func (s *FakeExplorerSocket) Drop() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		c.CloseNow()
	}
}

// AddExplorerSocket serves the explorer's WebSocket (wss://mempool.space/api/v1/ws) on the
// network.
func (n *Net) AddExplorerSocket() *FakeExplorerSocket {
	s := &FakeExplorerSocket{}
	n.AddServer(proto.MempoolWebSocketURL, s)
	return s
}

// NodeSocketURL is the WebSocket of a merchant's Bitcoin node of url (a node URL): ws:// for
// http://, wss:// for https://, then /api/v1/ws.
func NodeSocketURL(url string) string {
	return "ws" + strings.TrimPrefix(url, "http") + "/api/v1/ws"
}

// AddNodeSocket serves the WebSocket of a merchant's Bitcoin node of url on the network.
func (n *Net) AddNodeSocket(url string) *FakeExplorerSocket {
	s := &FakeExplorerSocket{}
	n.AddServer(NodeSocketURL(url), s)
	return s
}
