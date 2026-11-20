package channel

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/key"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// readOn reads the channel of k over a pool of its own, closed once it is done.
func readOn(t *testing.T, n *testkit.Net, k *key.IntegrationKey, timeout time.Duration, logger logx.Logger) (*Read, error) {
	t.Helper()
	pool := relay.NewPool(context.Background(), relay.Options{
		Dialer:  n.Dialer(),
		Timeout: timeout,
		Now:     func() int64 { return time.Now().Unix() },
		Random:  rand.Reader,
		Header:  http.Header{"User-Agent": {proto.UserAgent}},
	})
	defer pool.Close()
	return ReadChannel(context.Background(), pool, k, Options{Random: rand.Reader, Logger: logger})
}

func merchant(t *testing.T, others []string, behaviours map[string]testkit.Behaviour) *testkit.Merchant {
	t.Helper()
	m := testkit.NewMerchant()
	m.Start(others, behaviours)
	t.Cleanup(m.Stop)
	return m
}

func keyOf(t *testing.T, m *testkit.Merchant) *key.IntegrationKey {
	t.Helper()
	k, problem := key.Parse(m.Key())
	if problem != "" {
		t.Fatal(problem)
	}
	return k
}

func read(t *testing.T, m *testkit.Merchant, timeout time.Duration, logger logx.Logger) (*Read, error) {
	t.Helper()
	return readOn(t, m.Net, keyOf(t, m), timeout, logger)
}

func wantCode(t *testing.T, err error, code errs.Code) {
	t.Helper()
	if !errs.Is(err, code) {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func TestReadingTheChannelOfTheAppsVectorsWithItsKeyAlone(t *testing.T) {
	n := testkit.NewNet()
	t.Cleanup(n.Close)
	r := n.Add("wss://relay.example", testkit.Behaviour{})
	app := tv.Load(t, "app/app.json")
	r.Store(tv.Get(tv.Obj(t, app, "channel"), "event"))
	k, problem := key.Parse(tv.Str(t, app, "secretKey"))
	if problem != "" {
		t.Fatal(problem)
	}
	got, err := readOn(t, n, k, 2*time.Second, logx.Nop)
	if err != nil {
		t.Fatal(err)
	}
	c := got.Channel
	registryKey := tv.Str(t, tv.Obj(t, tv.Obj(t, tv.Obj(t, app, "channel"), "value"), "v"), "registryKey")
	if c.Removed || c.BusinessName != "Sunrise Bakery" || c.LightningAddress != "sattleme02c6047f94@breez.tips" || c.RegistryKey != registryKey {
		t.Errorf("%+v", c)
	}
	tv.Equal(t, "relays", got.Relays, []string{"wss://relay.example"})
	var queries []any
	for _, q := range r.Queries() {
		queries = append(queries, q)
	}
	tv.Equal(t, "queries", queries, []any{jsonv.O(
		"kinds", []any{30078},
		"authors", []any{tv.Str(t, tv.Obj(t, app, "parsed"), "team")},
		"#d", []any{tv.Str(t, tv.Obj(t, app, "derived"), "channelAddress")},
	)})
}

func TestTheNewestCopyIsKeptAndOneSignedByAnotherKeyIgnored(t *testing.T) {
	m := merchant(t, nil, nil)
	m.PublishChannel(m.ChannelValue(100), 100)
	m.PublishChannel(m.ChannelValue(200, jsonv.Member{Name: "profile", Value: jsonv.O("businessName", "Old", "lightningAddress", "a@b.test", "currency", "CHF")}), 200,
		// On a majority, as the app publishes it: any majority that answers holds it.
		"wss://relay-a.test", "wss://relay-b.test")
	// Newer, but by another author: whoever saw the address could publish there.
	other := m.ChannelEvent(m.ChannelValue(300, jsonv.Member{Name: "status", Value: "removed"}), 300, testkit.Bytes(66, 32))
	for _, url := range m.KeyRelays {
		m.Net.Get(url).StoreEvent(other)
	}
	got, err := read(t, m, 2*time.Second, logx.Nop)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel.BusinessName != "Old" || got.Channel.UpdatedAt != 200 {
		t.Errorf("%+v", got.Channel)
	}
}

func TestACopyWhoseDateIsNotItsUpdatedAtOrThatIsDamagedIsIgnored(t *testing.T) {
	m := merchant(t, nil, nil)
	m.PublishChannel(m.ChannelValue(100), 100)
	m.PublishChannel(m.ChannelValue(150), 200)
	r := m.Net.Get("wss://relay-b.test")
	good := testkit.EventValue(m.ChannelEvent(m.ChannelValue(300), 300, nil)).(*jsonv.Object)
	damaged := testkit.EventValue(m.ChannelEvent(m.ChannelValue(300), 300, nil)).(*jsonv.Object)
	content, _ := damaged.Get("content")
	damaged.Set("content", "B"+strings.TrimPrefix(content.(string), "A"))
	r.Store(damaged)
	good.Set("sig", strings.Repeat("00", 64))
	r.Store(good)
	got, err := read(t, m, 2*time.Second, logx.Nop)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel.UpdatedAt != 100 {
		t.Errorf("updatedAt %d", got.Channel.UpdatedAt)
	}
}

func TestAChannelWithAnAddressOnAnIPOrUnderLocalhostOrALoneSurrogateIsIgnored(t *testing.T) {
	m := merchant(t, nil, nil)
	m.PublishChannel(m.ChannelValue(100), 100)
	for i, profile := range []jsonv.Members{
		jsonv.O("businessName", "Sunrise Bakery", "lightningAddress", "shop@10.0.0.1"),
		jsonv.O("businessName", "Sunrise Bakery", "lightningAddress", "shop@pay.localhost"),
		jsonv.O("businessName", "Sunrise \xed\xa0\x80", "lightningAddress", testkit.LightningAddress),
	} {
		at := int64(200 + i)
		m.PublishChannel(m.ChannelValue(at, jsonv.Member{Name: "profile", Value: profile}), at)
	}
	got, err := read(t, m, 2*time.Second, logx.Nop)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel.UpdatedAt != 100 {
		t.Errorf("updatedAt %d", got.Channel.UpdatedAt)
	}
}

func TestARemovedIntegrationIsRevoked(t *testing.T) {
	m := merchant(t, nil, nil)
	m.PublishChannel(m.ChannelValue(100), 100)
	m.PublishChannel(jsonv.O("version", 1, "status", "removed", "updatedAt", 200), 200)
	_, err := read(t, m, 2*time.Second, logx.Nop)
	wantCode(t, err, errs.Revoked)
}

func TestANewerFormatIsWarnedOfAndIgnored(t *testing.T) {
	m := merchant(t, nil, nil)
	m.PublishChannel(m.ChannelValue(100), 100)
	m.PublishChannel(m.ChannelValue(200, jsonv.Member{Name: "version", Value: 2}), 200)
	logger := &testkit.Logger{}
	got, err := read(t, m, 2*time.Second, logger)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel.UpdatedAt != 100 || len(logger.Messages()) != 1 {
		t.Errorf("updatedAt %d, warnings %v", got.Channel.UpdatedAt, logger.Messages())
	}
	// Only one of a newer format: not found, and newer.
	only := merchant(t, nil, nil)
	only.PublishChannel(only.ChannelValue(200, jsonv.Member{Name: "version", Value: 2}), 200)
	_, err = read(t, only, 2*time.Second, logx.Nop)
	e, ok := errs.Of(err)
	if !ok || e.Code != errs.ChannelNotFound || !e.NewerFormat || e.Retryable() {
		t.Errorf("got %v", err)
	}
}

func TestTheChannelIsFollowedWhereItMoved3TimesAtMost(t *testing.T) {
	m := testkit.NewMerchant()
	lists := [][]string{m.KeyRelays, {"wss://second.test"}, {"wss://third.test", "wss://third-b.test"}, {"wss://fourth.test"}, {"wss://fifth.test"}}
	var others []string
	for _, list := range lists[1:] {
		others = append(others, list...)
	}
	m.Start(others, nil)
	t.Cleanup(m.Stop)
	for i := range lists {
		next := lists[i]
		if i+1 < len(lists) {
			next = lists[i+1]
		}
		at := int64(100 + i)
		m.PublishChannel(m.ChannelValue(at,
			jsonv.Member{Name: "relays", Value: next},
			jsonv.Member{Name: "profile", Value: jsonv.O("businessName", "List "+string(rune('0'+i)), "lightningAddress", testkit.LightningAddress, "currency", "CHF")},
		), at, lists[i]...)
	}
	got, err := read(t, m, 2*time.Second, logx.Nop)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel.BusinessName != "List 3" {
		t.Errorf("read %s", got.Channel.BusinessName)
	}
	tv.Equal(t, "relays", got.Relays, []string{"wss://fourth.test"})
	if n := m.Net.Connections("wss://fifth.test"); n != 0 {
		t.Errorf("%d connections to the fifth list", n)
	}
}

func TestARemovalOnTheListTheChannelMovedToIsSeen(t *testing.T) {
	m := merchant(t, []string{"wss://new.test"}, nil)
	m.PublishChannel(m.ChannelValue(100, jsonv.Member{Name: "relays", Value: []string{"wss://new.test"}}), 100)
	m.PublishChannel(jsonv.O("version", 1, "status", "removed", "updatedAt", 200), 200, "wss://new.test")
	_, err := read(t, m, 2*time.Second, logx.Nop)
	wantCode(t, err, errs.Revoked)
}

func TestAListTheChannelMovedToThatDoesNotAnswerIsNetwork(t *testing.T) {
	m := merchant(t, nil, nil)
	m.PublishChannel(m.ChannelValue(100, jsonv.Member{Name: "relays", Value: []string{"wss://gone.test"}}), 100)
	_, err := read(t, m, 2*time.Second, logx.Nop)
	wantCode(t, err, errs.Network)
}

func TestRelaysThatAnswerWithoutTheChannel(t *testing.T) {
	m := merchant(t, nil, nil)
	_, err := read(t, m, 2*time.Second, logx.Nop)
	wantCode(t, err, errs.ChannelNotFound)
	if e, _ := errs.Of(err); !e.Retryable() || e.NewerFormat {
		t.Error("retryable when not newer")
	}
}

func TestAReadGoesOnOnceAMajorityAnsweredPastASilentRelayAndOneAskingToAuthenticate(t *testing.T) {
	m := merchant(t, nil, map[string]testkit.Behaviour{
		"wss://relay-a.test": {Mute: true},
		"wss://relay-b.test": {RequireAuth: true},
	})
	m.PublishChannel(m.ChannelValue(100), 100, "wss://relay-b.test")
	started := time.Now()
	// relay-b and relay-c are a majority: the silent relay's 30 s are not waited for.
	got, err := read(t, m, 30*time.Second, logx.Nop)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Errorf("took %v", elapsed)
	}
	if got.Channel.UpdatedAt != 100 {
		t.Errorf("updatedAt %d", got.Channel.UpdatedAt)
	}
	// A key of this read alone: never the team's nor a sale's.
	authenticated := m.Net.Get("wss://relay-b.test").Authenticated()
	if len(authenticated) == 0 || authenticated[0] == m.Team {
		t.Errorf("authenticated %v", authenticated)
	}
}

func TestARelayThatNeverCompletesTheHandshakeIsClosed(t *testing.T) {
	m := merchant(t, nil, nil)
	m.Net.AddBlackhole("wss://hole.test")
	m.PublishChannel(m.ChannelValue(100), 100, "wss://relay-b.test")
	k := keyOf(t, m)
	on := func(list ...string) (*Read, error) {
		moved := *k
		moved.Relays = list
		return readOn(t, m.Net, &moved, 300*time.Millisecond, logx.Nop)
	}
	// Read on the two others, a majority of three; the hole's connection closed meanwhile.
	got, err := on("wss://hole.test", "wss://relay-b.test", "wss://relay-c.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel.UpdatedAt != 100 {
		t.Errorf("updatedAt %d", got.Channel.UpdatedAt)
	}
	// One of two is no majority: no answer is no absence.
	_, err = on("wss://hole.test", "wss://relay-b.test")
	wantCode(t, err, errs.Network)
}

func TestNoRelayThatAnswersIsNetworkNotAnAbsence(t *testing.T) {
	m := merchant(t, nil, map[string]testkit.Behaviour{"wss://relay-a.test": {Mute: true}})
	m.Net.Down("wss://relay-b.test")
	m.Net.Down("wss://relay-c.test")
	started := time.Now()
	_, err := read(t, m, 300*time.Millisecond, logx.Nop)
	wantCode(t, err, errs.Network)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("took %v", elapsed)
	}
}
