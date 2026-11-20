package checkout_test

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// nilDialer is a WebSocketDialer of the integrator's, as a nil pointer.
type nilDialer struct{}

func (*nilDialer) Dial(context.Context, string, http.Header) (checkout.WebSocketConn, error) {
	return nil, fmt.Errorf("never dialed")
}

// The integration's secret key, as the client takes it; every case of integration-keys.json is
// internal/key's.

func TestTheClientRefusesAKeyAtConstruction(t *testing.T) {
	app := tv.Load(t, "app/app.json")
	key := tv.Str(t, app, "secretKey")
	parts := strings.Split(strings.TrimPrefix(key, "sattleme_ck_"), ".")
	secret := tv.Str(t, tv.Obj(t, app, "parsed"), "integrationKey")
	for _, text := range []string{"", "nope", strings.Replace(key, "sattleme_ck_", "sattleme_other_", 1), key + "." + parts[2], key[:len(key)-3], " " + key,
		"https://pay.example/pay#" + strings.Join(parts, ".")} {
		_, err := checkout.New(text)
		e := wantCode(t, err, checkout.CodeInvalidKey)
		if strings.Contains(e.Message, parts[1]) || strings.Contains(e.Message, secret) || e.Retryable() {
			t.Errorf("%q: %s", text, e.Message)
		}
	}
}

func TestTheKeyStaysOutOfJSONAndOfWhatIsPrinted(t *testing.T) {
	app := tv.Load(t, "app/app.json")
	key := tv.Str(t, app, "secretKey")
	secret := tv.Str(t, tv.Obj(t, app, "parsed"), "integrationKey")
	keyPart := strings.Split(key, ".")[1]
	c, err := checkout.New(key, checkout.WithLogger(quiet()))
	must(t, err)
	data, err := json.Marshal(c)
	must(t, err)
	same(t, "json", jsonOf(t, c), map[string]any{"teamPublicKey": tv.Str(t, tv.Obj(t, app, "parsed"), "team"), "relays": []string{"wss://relay.example"}})
	for _, printed := range []string{string(data), fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s %q %x", c, c, c, c, c, c), c.String(), c.GoString(), fmt.Sprintf("%v", *c)} {
		if strings.Contains(printed, secret) || strings.Contains(printed, keyPart) {
			t.Errorf("the key in %s", printed)
		}
	}
}

// No serializer an application commonly calls on what it logs or returns writes a secret: of the
// client (as a pointer or a value, alone or inside the application's own values) and of its
// options, encoding/json, fmt's verbs, log/slog's handlers, encoding/gob and encoding/xml write
// neither the integration key (as its text, hex, bytes) nor the CoinGecko key.
func TestNoSerializerWritesTheKeys(t *testing.T) {
	app := tv.Load(t, "app/app.json")
	key := tv.Str(t, app, "secretKey")
	secretHex := tv.Str(t, tv.Obj(t, app, "parsed"), "integrationKey")
	secret, err := hex.DecodeString(secretHex)
	must(t, err)
	const demoKey = "CG-demo-key-of-the-shop"
	secrets := []string{strings.Split(key, ".")[1], secretHex, fmt.Sprint(secret), fmt.Sprintf("%#v", secret), string(secret), demoKey}
	options := []checkout.Option{checkout.WithCoinGeckoAPIKey(demoKey), checkout.WithLogger(quiet())}
	c, err := checkout.New(key, options...)
	must(t, err)
	type held struct {
		Client  *checkout.Client
		Value   checkout.Client
		Options []checkout.Option
		Any     any
	}
	// An application's own values holding it, in fields fmt cannot call its methods on too.
	type shop struct {
		client  checkout.Client
		pointer *checkout.Client
		options []checkout.Option
	}
	values := []any{c, *c, held{Client: c, Value: *c, Any: c}, &held{Client: c, Any: *c}, []any{c, *c}, map[string]any{"client": c},
		shop{client: *c, pointer: c, options: options}, &shop{client: *c}, options, options[0]}
	var written []string
	for _, v := range values {
		if data, err := json.Marshal(v); err == nil {
			written = append(written, string(data))
		} else {
			written = append(written, err.Error())
		}
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
			written = append(written, fmt.Sprintf(verb, v))
		}
		var text, structured bytes.Buffer
		slog.New(slog.NewTextHandler(&text, nil)).Info("logged", "value", v)
		slog.New(slog.NewJSONHandler(&structured, nil)).Info("logged", "value", v)
		written = append(written, text.String(), structured.String())
		var encoded bytes.Buffer
		if err := gob.NewEncoder(&encoded).Encode(v); err == nil {
			written = append(written, encoded.String())
		} else {
			written = append(written, err.Error())
		}
		if data, err := xml.Marshal(v); err == nil {
			written = append(written, string(data))
		} else {
			written = append(written, err.Error())
		}
	}
	for _, w := range written {
		for _, s := range secrets {
			if strings.Contains(w, s) {
				t.Errorf("a secret written: %s", w)
			}
		}
	}
}

func TestInvalidOptionsAreRefusedAtConstruction(t *testing.T) {
	m := newMerchant(t)
	for _, c := range []struct {
		param string
		opt   checkout.Option
	}{
		{"options", nil},
		{"httpTransport", checkout.WithHTTPTransport(nil)},
		{"webSocketDialer", checkout.WithWebSocketDialer(nil)},
		{"logger", checkout.WithLogger(nil)},
		{"now", checkout.WithClock(nil)},
		{"random", checkout.WithRandom(nil)},
		{"relayTimeoutMs", checkout.WithRelayTimeout(99_000_000)},
		{"relayTimeoutMs", checkout.WithRelayTimeout(60_001_000_000)},
		{"relayTimeoutMs", checkout.WithRelayTimeout(1_500_000_000 + 1)},
		{"relayTimeoutMs", checkout.WithRelayTimeout(0)},
		{"httpTimeoutMs", checkout.WithHTTPTimeout(-1)},
		{"httpTimeoutMs", checkout.WithHTTPTimeout(60_001_000_000)},
	} {
		_, err := checkout.New(m.Key(), c.opt)
		wantCode(t, err, checkout.CodeInvalidOption, c.param)
	}
	// A typed nil (a nil pointer as an interface, which != nil does not see) would panic at the
	// first request, in a goroutine of the call where no recover of the caller reaches.
	var transport *http.Transport
	var dialer *nilDialer
	var logger *testkit.Logger
	var random *bytes.Reader
	for _, c := range []struct {
		param string
		opt   checkout.Option
	}{
		{"httpTransport", checkout.WithHTTPTransport(transport)},
		{"webSocketDialer", checkout.WithWebSocketDialer(dialer)},
		{"webSocketDialer", checkout.WithWebSocketDialer(checkout.NewWebSocketDialer(transport))},
		{"logger", checkout.WithLogger(logger)},
		{"logger", checkout.WithLogger(checkout.LoggerFunc(nil))},
		{"random", checkout.WithRandom(random)},
	} {
		_, err := checkout.New(m.Key(), c.opt)
		wantCode(t, err, checkout.CodeInvalidOption, c.param)
	}
	// Wrapped in a dialer of the integrator's, NewWebSocketDialer over a typed nil dials nothing.
	if conn, err := checkout.NewWebSocketDialer(transport).Dial(ctx, m.KeyRelays[0], nil); err == nil || conn != nil {
		t.Errorf("dialed over a nil transport: %v", err)
	}
	// The edges.
	for _, opt := range []checkout.Option{checkout.WithRelayTimeout(100_000_000), checkout.WithRelayTimeout(60_000_000_000),
		checkout.WithHTTPTimeout(100_000_000), checkout.WithHTTPTimeout(60_000_000_000), checkout.WithCoinGeckoAPIKey("!~")} {
		_, err := checkout.New(m.Key(), opt)
		must(t, err)
	}
}
