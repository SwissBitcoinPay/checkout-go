package nostr

import (
	"strings"
	"testing"

	"github.com/sattleme/checkout-go/internal/jsonv"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// nostr-events.json (SPEC.md §11): an event's id, through the check every event a relay sends goes
// through first (WithValidID): its strings written as JSON.stringify writes them, its numbers as
// integers whatever their spelling. Each vector's event, as a relay would send it with that id,
// checks out; with another id (its last digit changed), it does not.
func TestEventIDVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "nostr-events.json"), "cases")
	for _, c := range cases {
		name := tv.Str(t, c, "name")
		event := tv.Obj(t, c, "event")
		id := tv.Str(t, c, "id")
		sent := func(id string) any {
			v := jsonv.NewObject()
			for _, member := range event.Keys() {
				value, _ := event.Get(member)
				v.Set(member, value)
			}
			v.Set("id", id)
			v.Set("sig", strings.Repeat("0", 128))
			return v
		}
		e, ok := WithValidID(sent(id))
		if !ok {
			t.Errorf("%s: the id %s is not the event's", name, id)
			continue
		}
		if e.ID != id || e.CreatedAt != tv.Int(t, event, "created_at") {
			t.Errorf("%s: %+v", name, e)
		}
		other := id[:63] + "0"
		if id[63] == '0' {
			other = id[:63] + "1"
		}
		if _, ok := WithValidID(sent(other)); ok {
			t.Errorf("%s: another id checks out", name)
		}
	}
	t.Logf("%d cases", len(cases))
}
