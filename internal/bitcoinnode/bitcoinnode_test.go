package bitcoinnode

import (
	"strings"
	"testing"

	"github.com/sattleme/checkout-go/internal/jsonv"
)

// The rule of a node URL (SPEC.md §31, "A node URL"), case by case: the bitcoin-nodes.json
// vectors run through the same functions (vectors_test.go).
func TestNodeURLs(t *testing.T) {
	valid := map[string]URL{
		"http://192.168.1.10:3006":  {REST: "http://192.168.1.10:3006/api", WebSocket: "ws://192.168.1.10:3006/api/v1/ws", Local: true},
		"http://umbrel.local":       {REST: "http://umbrel.local/api", WebSocket: "ws://umbrel.local/api/v1/ws", Local: true},
		"http://10.0.0.1":           {REST: "http://10.0.0.1/api", WebSocket: "ws://10.0.0.1/api/v1/ws", Local: true},
		"http://172.16.0.1":         {REST: "http://172.16.0.1/api", WebSocket: "ws://172.16.0.1/api/v1/ws", Local: true},
		"http://172.31.255.255":     {REST: "http://172.31.255.255/api", WebSocket: "ws://172.31.255.255/api/v1/ws", Local: true},
		"http://100.64.0.1":         {REST: "http://100.64.0.1/api", WebSocket: "ws://100.64.0.1/api/v1/ws", Local: true},
		"http://100.127.255.254":    {REST: "http://100.127.255.254/api", WebSocket: "ws://100.127.255.254/api/v1/ws", Local: true},
		"https://192.168.1.10":      {REST: "https://192.168.1.10/api", WebSocket: "wss://192.168.1.10/api/v1/ws", Local: true},
		"https://8.8.8.8":           {REST: "https://8.8.8.8/api", WebSocket: "wss://8.8.8.8/api/v1/ws"},
		"https://node.example":      {REST: "https://node.example/api", WebSocket: "wss://node.example/api/v1/ws", Internet: true},
		"https://umbrel.local:4443": {REST: "https://umbrel.local:4443/api", WebSocket: "wss://umbrel.local:4443/api/v1/ws", Local: true},
		"https://node.example:65535/mempool": {REST: "https://node.example:65535/mempool/api", WebSocket: "wss://node.example:65535/mempool/api/v1/ws",
			Internet: true},
		"https://node.example/x/apis": {REST: "https://node.example/x/apis/api", WebSocket: "wss://node.example/x/apis/api/v1/ws", Internet: true},
		"https://a-1.b2.example":      {REST: "https://a-1.b2.example/api", WebSocket: "wss://a-1.b2.example/api/v1/ws", Internet: true},
	}
	for text, want := range valid {
		got, ok := ParseURL(text)
		want.URL = text
		if !ok || got != want {
			t.Errorf("%s: %+v, %v", text, got, ok)
		}
	}
	long := "https://node.example/" + strings.Repeat("a", 179)
	if _, ok := ParseURL(long); !ok || len(long) != 200 {
		t.Errorf("200 characters: refused")
	}
	invalid := []any{
		// http to a host that is not local, and the machine itself.
		"http://node.example", "http://8.8.8.8", "http://172.32.0.1", "http://100.128.0.1", "http://192.169.0.1",
		"https://127.0.0.1", "https://0.0.0.0", "https://0.1.2.3", "https://localhost", "https://node.localhost", "http://localhost.local.localhost",
		// Not in its one spelling.
		"HTTPS://node.example", "https://Node.example", "https://node.example.", "https://node..example", "https://node", "https://node.123",
		"https://-node.example", "https://node-.example", "https://192.168.01.10", "https://256.1.1.1", "https://1.2.3",
		"https://[::1]", "https://user@node.example", "https://user:pass@node.example", "https://node.example?x", "https://node.example#x",
		"https://node.example/", "https://node.example/api", "https://node.example/API", "https://node.example/x/Api", "https://node.example/a/",
		"https://node.example/a?b", "https://node.example/a#b", "https://node.example/a@b", `https://node.example/a\b`, "https://node.example/a b",
		"https://node.example:0", "https://node.example:65536", "https://node.example:080", "https://node.example:", "https://node.example:123456",
		" https://node.example", "https://node.example ", "wss://node.example", "ftp://node.example", "//node.example", "node.example",
		"https://nöde.example", "https://" + strings.Repeat("a", 64) + ".example", long + "a",
		"", nil, 1.0, true, jsonv.O("url", "https://node.example"), []any{"https://node.example"},
	}
	for _, v := range invalid {
		if got, ok := ParseURL(v); ok {
			t.Errorf("%v: %+v", v, got)
		}
	}
}

func TestTheChannelsNode(t *testing.T) {
	node := func(url string, fallback any, payers any) jsonv.Members {
		return jsonv.O("url", url, "fallback", fallback, "payers", payers)
	}
	cases := []struct {
		value any
		want  *Node
	}{
		{node("http://192.168.1.10:3006", "mempool", false), &Node{URL: "http://192.168.1.10:3006", Fallback: Mempool}},
		{node("http://umbrel.local", "none", false), &Node{URL: "http://umbrel.local", Fallback: None}},
		{node("https://node.example", "none", true), &Node{URL: "https://node.example", Fallback: None, Payers: true}},
		// Its other members ignored.
		{append(node("https://node.example", "mempool", false), jsonv.Member{Name: "label", Value: "Umbrel"}), &Node{URL: "https://node.example", Fallback: Mempool}},
		// Invalid in any way: none.
		{nil, nil},
		{"http://192.168.1.10:3006", nil},
		{[]any{node("https://node.example", "mempool", false)}, nil},
		{node("http://node.example", "mempool", false), nil},
		{node("https://node.example", "Mempool", false), nil},
		{node("https://node.example", nil, false), nil},
		{jsonv.O("url", "https://node.example", "payers", false), nil},
		{jsonv.O("url", "https://node.example", "fallback", "mempool"), nil},
		{node("https://node.example", "mempool", "true"), nil},
		{node("https://node.example", "mempool", 1.0), nil},
		{node("https://node.example", "mempool", nil), nil},
		// payers true only for a node of the Internet.
		{node("http://192.168.1.10:3006", "mempool", true), nil},
		{node("https://192.168.1.10", "mempool", true), nil},
		{node("https://8.8.8.8", "mempool", true), nil},
		{node("https://umbrel.local", "mempool", true), nil},
	}
	for _, c := range cases {
		value, _ := jsonv.ParseString(string(jsonv.Marshal(c.value)))
		got := Of(value)
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("%s: %+v", jsonv.Marshal(c.value), got)
		}
	}
}

func TestTheRecordsNode(t *testing.T) {
	if ForRecord(nil) != nil || ForRecord(&Node{URL: "http://umbrel.local", Fallback: Mempool}) != nil {
		t.Error("a node in the record without payers")
	}
	got := ForRecord(&Node{URL: "https://node.example", Fallback: None, Payers: true})
	if got == nil || string(jsonv.Marshal(got.JSON())) != `{"url":"https://node.example","fallback":"none"}` {
		t.Errorf("%+v", got)
	}
	read := func(v any) *RecordNode {
		value, _ := jsonv.ParseString(string(jsonv.Marshal(v)))
		return OfRecord(value)
	}
	if got := read(jsonv.O("url", "https://node.example", "fallback", "mempool", "payers", false)); got == nil || *got != (RecordNode{URL: "https://node.example", Fallback: Mempool}) {
		t.Errorf("%+v", got)
	}
	for _, v := range []any{nil, "https://node.example", jsonv.O("url", "http://umbrel.local", "fallback", "mempool"),
		jsonv.O("url", "https://umbrel.local", "fallback", "none"), jsonv.O("url", "https://192.168.1.10", "fallback", "none"),
		jsonv.O("url", "https://node.example", "fallback", "Mempool"), jsonv.O("url", "https://node.example")} {
		if got := read(v); got != nil {
			t.Errorf("%s: %+v", jsonv.Marshal(v), got)
		}
	}
}
