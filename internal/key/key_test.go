package key

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/sattleme/checkout-go/internal/b64"
	"github.com/sattleme/checkout-go/internal/jsonv"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

func TestIntegrationKeyVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "integration-keys.json"), "cases")
	for _, c := range cases {
		name := tv.Str(t, c, "name")
		parsed, problem := Parse(tv.Str(t, c, "key"))
		expected := tv.Obj(t, c, "expected")
		if expected.Has("error") {
			if parsed != nil || problem == "" || tv.Str(t, expected, "error") != "invalid_key" {
				t.Errorf("%s: parsed %v", name, parsed)
			}
			continue
		}
		if parsed == nil {
			t.Errorf("%s: %s", name, problem)
			continue
		}
		relays := make([]any, len(parsed.Relays))
		for i, relay := range parsed.Relays {
			relays[i] = relay
		}
		got := string(jsonv.Marshal(jsonv.Members{
			{Name: "teamPublicKey", Value: parsed.TeamPublicKey},
			{Name: "integrationKey", Value: hex.EncodeToString(parsed.Key)},
			{Name: "relays", Value: relays},
		}))
		if want := string(jsonv.Marshal(expected)); got != want {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
	t.Logf("%d cases", len(cases))
}

// appKey is the app's vector's key (vectors/app/app.json, secretKey), a valid key of
// integration-keys.json: TestAppKeyIsThePinnedVectors checks it.
const appKey = "sattleme_ck_qEVejHuCZpF4XMmBSw5p7iZYxCtm20XxzT1fnJ9VB8o.uPTAz1d8bbQaw2Jx22Wwrga2vI2F8allY5NrPC3Aogk.cmVsYXkuZXhhbXBsZQ"

// The tests' constant is the pinned vectors' key: never a key of their own claimed to be the
// app's.
func TestAppKeyIsThePinnedVectors(t *testing.T) {
	app := tv.Load(t, "app/app.json")
	if got := tv.Str(t, app, "secretKey"); got != appKey {
		t.Errorf("app.json's key is %s", got)
	}
	valid := false
	for _, c := range tv.Objs(t, tv.Load(t, "integration-keys.json"), "cases") {
		if tv.Str(t, c, "key") == appKey && !tv.Obj(t, c, "expected").Has("error") {
			valid = true
		}
	}
	if !valid {
		t.Error("not a valid key of integration-keys.json")
	}
	parsed, _ := Parse(appKey)
	if hex.EncodeToString(parsed.Key) != tv.Str(t, tv.Obj(t, app, "parsed"), "integrationKey") {
		t.Error("another integration key than app.json's")
	}
}

// SPEC.md §8 step 5 and §2: the relays part is decoded strictly, a byte order mark kept, so
// U+FEFF then "relay.example" makes no relay address.
func TestKeyRelaysAfterAByteOrderMarkAreInvalid(t *testing.T) {
	parts := strings.Split(appKey, ".")
	bom := b64.EncodeURL([]byte("\xef\xbb\xbfrelay.example"))
	if parsed, problem := Parse(parts[0] + "." + parts[1] + "." + bom); parsed != nil || problem != Damaged {
		t.Errorf("parsed %v, %q", parsed, problem)
	}
}

func TestTheKeyIsNeverWritten(t *testing.T) {
	parsed, _ := Parse(appKey)
	secret := hex.EncodeToString(parsed.Key)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
		written := fmt.Sprintf(verb, *parsed) + fmt.Sprintf(verb, parsed)
		if strings.Contains(written, secret) || strings.Contains(written, fmt.Sprint(parsed.Key)) ||
			strings.Contains(strings.ToLower(written), "b8f4c0cf") || strings.Contains(written, "uPTAz1d8") {
			t.Errorf("%s writes the key: %s", verb, written)
		}
	}
	// Problems never hold the key.
	for _, text := range []string{appKey + ".x", "x" + appKey, appKey[:len(appKey)-20], strings.Replace(appKey, "uPTA", "uPT=", 1)} {
		if _, problem := Parse(text); problem == "" || strings.Contains(string(problem), "uPT") {
			t.Errorf("problem %q", problem)
		}
	}
}

func TestDamagedKeys(t *testing.T) {
	parts := strings.Split(strings.TrimPrefix(appKey, "sattleme_ck_"), ".")
	team, secret, relays := parts[0], parts[1], parts[2]
	raw, _ := b64.DecodeURL(relays)
	withBOM := b64.EncodeURL(append([]byte{0xef, 0xbb, 0xbf}, raw...))
	for name, text := range map[string]string{
		"empty":                    "",
		"another prefix":           strings.Replace(appKey, "sattleme_ck_", "sattleme_other_", 1),
		"a later format's prefix":  strings.Replace(appKey, "sattleme_ck_", "sattleme_ck2_", 1),
		"sattleme_ without ck_":    strings.Replace(appKey, "sattleme_ck_", "sattleme_", 1),
		"no prefix":                strings.TrimPrefix(appKey, "sattleme_ck_"),
		"a link holding its parts": "https://pay.example/pay#" + team + "." + secret + "." + relays,
		"a part missing":           "sattleme_ck_" + team + "." + secret,
		"a part too many":          appKey + "." + relays,
		"a key cut short":          appKey[:len(appKey)-3],
		"a team cut short":         "sattleme_ck_" + team[1:] + "." + secret + "." + relays,
		"a key too long":           "sattleme_ck_" + team + "." + secret + "AA." + relays,
		"padding":                  "sattleme_ck_" + team + "=." + secret + "." + relays,
		"standard base64":          "sattleme_ck_+" + team[1:] + "." + secret + "." + relays,
		"unused bits set":          "sattleme_ck_" + team[:len(team)-1] + "b." + secret + "." + relays,
		"no relay":                 "sattleme_ck_" + team + "." + secret + ".",
		"a relay that is not one":  "sattleme_ck_" + team + "." + secret + ".bG9jYWxob3N0",
		"spaces":                   " " + appKey,
		// Strict UTF-8 keeps it: U+FEFF then "relay…" is no relay's host (SPEC.md §8, step 5).
		"a relays part starting with a byte order mark": "sattleme_ck_" + team + "." + secret + "." + withBOM,
	} {
		parsed, problem := Parse(text)
		if parsed != nil || problem == "" {
			t.Errorf("%s: parsed", name)
		}
		if strings.Contains(string(problem), secret) || strings.Contains(string(problem), "b8f4c0cf577c6db41ac36271db65b0ae06b6bc8d85f1a96563936b3c2dc0a209") {
			t.Errorf("%s: the problem holds the key", name)
		}
	}
}
