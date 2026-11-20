// Package key reads an integration's secret key, as the merchant's app writes it (SPEC.md §8):
// sattleme_ck_<team>.<key>.<relays>, each part in canonical base64url without padding. It stays
// on the integrator's server: never in a web page, a repository nor a log.
package key

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/sattleme/checkout-go/internal/b64"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/secp"
	"github.com/sattleme/checkout-go/internal/urls"
)

// IntegrationKey is what an integration's secret key holds.
type IntegrationKey struct {
	// TeamPublicKey is the team's public key (x only, hex): the author of the integration's
	// channel.
	TeamPublicKey string
	// Key is the integration's key (32 bytes): a secret. Never logged.
	Key []byte
	// Relays are where its channel was published when the key was given (wss:// URLs).
	Relays []string
}

// Format writes the key without its secret, whatever the verb.
func (k IntegrationKey) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "IntegrationKey{team %s, %d relays}", k.TeamPublicKey, len(k.Relays))
}

// Problem says why a text is not an integration key, without the key.
type Problem string

const (
	// NotAKey is another prefix, a part too many: not a sattle.me integration key.
	NotAKey Problem = "Not a sattle.me integration key"
	// CutShort is a key with a part missing.
	CutShort Problem = "Integration key cut short"
	// Damaged is a part that is not canonical base64url, a length other than 32 bytes, a team
	// key off the curve, an invalid relay list.
	Damaged Problem = "Damaged integration key"
)

// Parse reads an integration's secret key (SPEC.md §8, in its order). The problem, when it is
// not one, never holds the key.
func Parse(text string) (*IntegrationKey, Problem) {
	rest, ok := strings.CutPrefix(text, proto.IntegrationKeyPrefix)
	if !ok {
		return nil, NotAKey
	}
	parts := strings.Split(rest, ".")
	if len(parts) < 3 {
		return nil, CutShort
	}
	if len(parts) > 3 {
		return nil, NotAKey
	}
	team, okTeam := b64.DecodeURL(parts[0])
	secret, okKey := b64.DecodeURL(parts[1])
	relays, okRelays := urls.DecodeRelayList(parts[2])
	if !okTeam || !okKey || !okRelays || len(team) != proto.KeyBytes || len(secret) != proto.KeyBytes {
		return nil, Damaged
	}
	if !secp.IsPublicKeyBytes(team) {
		return nil, Damaged
	}
	return &IntegrationKey{TeamPublicKey: hex.EncodeToString(team), Key: secret, Relays: relays}, ""
}
