// Package urls checks hosts and URLs in their one spelling (SPEC.md §9, §15, §30): relay
// addresses and lists, https URLs (what the package asks over HTTP), checkout origins and redirect
// URLs (where a checkout link sends the customer). Nothing is repaired: a URL that is not so is
// refused, so that no request goes to an IP address, to the machine itself (localhost and the
// names under it, RFC 6761) or to a host other than the one checked.
package urls

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sattleme/checkout-go/internal/b64"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/text"
)

var (
	// hostPattern: labels of a–z 0–9 - separated by ".", the last of at least two letters a–z.
	hostPattern = regexp.MustCompile(`^[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}$`)
	// urlCharacters: the characters of a redirect URL and of an https URL, RFC 3986's, and "%"
	// with two hex digits.
	urlCharacters = regexp.MustCompile(`^(?:[A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=]|%[0-9A-Fa-f]{2})+$`)
	// httpsPattern: "https://", the authority (up to the first "/", "?" or "#"), then anything.
	httpsPattern = regexp.MustCompile(`^https://([^/?#]*)(?:[/?#].*)?$`)
	// authorityPattern: a host and an optional port of 1 to 5 digits.
	authorityPattern = regexp.MustCompile(`^([^:]*)(?::([0-9]{1,5}))?$`)
	// originPattern: an https URL with nothing after its authority.
	originPattern = regexp.MustCompile(`^https://[^/?#]*$`)
	// appPattern: an app link's scheme, then "://".
	appPattern = regexp.MustCompile(`^([a-z][a-z0-9+.-]*)://`)
	// relayPattern: a host, an optional port, an optional path without whitespace (ECMAScript's
	// \s, which the merchant's app checks with), query, fragment nor comma.
	relayPattern = regexp.MustCompile(`^([a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,})(:[0-9]{1,5})?(/[^\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}?#,]*)?$`)
)

// RelayPrefix begins every relay address.
const RelayPrefix = "wss://"

// IsHost tells whether host is a host as in a relay address: lower case, at most 253
// characters, so never an IP address; its last label not localhost, which names the machine's
// own loopback (RFC 6761).
func IsHost(host string) bool {
	return len(host) <= proto.MaxHostLength && hostPattern.MatchString(host) &&
		host != "localhost" && !strings.HasSuffix(host, ".localhost")
}

// isPort tells whether the digits of a port (1 to 5 of them) name 1 to 65535.
func isPort(digits string) bool {
	n, err := strconv.Atoi(digits)
	return err == nil && n >= 1 && n <= 65535
}

// HTTPS is an https URL's host and port: 443 when it names none.
type HTTPS struct {
	Host string
	Port int
}

// ParseHTTPS reads s as an https URL (SPEC.md §15: rule 1 of a redirect URL): 1 to 2048
// characters of a redirect URL, "https://", a host, an optional port, then nothing or "/", "?"
// or "#" and anything. So no upper case in the scheme or the host, no user name nor password, no
// IP address, no whitespace, no "\". False when it is not one.
func ParseHTTPS(s string) (HTTPS, bool) {
	if len(s) > proto.MaxHTTPSURLLength || !urlCharacters.MatchString(s) {
		return HTTPS{}, false
	}
	m := httpsPattern.FindStringSubmatch(s)
	if m == nil {
		return HTTPS{}, false
	}
	a := authorityPattern.FindStringSubmatchIndex(m[1])
	if a == nil {
		return HTTPS{}, false
	}
	host := m[1][a[2]:a[3]]
	if !IsHost(host) {
		return HTTPS{}, false
	}
	port := 443
	if a[4] >= 0 {
		digits := m[1][a[4]:a[5]]
		if !isPort(digits) {
			return HTTPS{}, false
		}
		port, _ = strconv.Atoi(digits)
	}
	return HTTPS{Host: host, Port: port}, true
}

// IsHTTPS tells whether v is a string that is an https URL.
func IsHTTPS(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	_, ok = ParseHTTPS(s)
	return ok
}

// SameHost tells whether two https URLs are on the same host: their hosts and their ports are
// equal.
func SameHost(a, b HTTPS) bool { return a.Host == b.Host && a.Port == b.Port }

// IsCheckoutOrigin tells whether v is a checkout origin (SPEC.md §30, "The web app's origin"):
// "https://", a host as in a relay address, an optional port, nothing else.
func IsCheckoutOrigin(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	_, ok = ParseHTTPS(s)
	return ok && originPattern.MatchString(s)
}

// RedirectShown returns what the page shows of a redirect URL ("Redirecting to …"): the host
// and port of an https URL as written, the scheme and "://" of an app's link. False when s is not
// a redirect URL (SPEC.md §30, "Redirect URLs"): nothing is repaired.
func RedirectShown(s string) (string, bool) {
	if len(s) > proto.MaxRedirectURLLength || !urlCharacters.MatchString(s) {
		return "", false
	}
	if strings.HasPrefix(s, "https://") {
		if _, ok := ParseHTTPS(s); !ok {
			return "", false
		}
		return httpsPattern.FindStringSubmatch(s)[1], true
	}
	m := appPattern.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	scheme := m[1]
	if len(scheme) > proto.MaxRedirectSchemeLength || scheme == "https" || proto.IsRefusedRedirectScheme(scheme) {
		return "", false
	}
	return scheme + "://", true
}

// IsRelayURL tells whether s is a relay address in its one spelling (SPEC.md §9): "wss://", a
// lowercase host name (not localhost nor a name under it), an optional port (1 to 65535) and an
// optional path without a trailing slash, 120 code points at most (the app counts UTF-16 units:
// never fewer, so whatever it writes is read).
func IsRelayURL(s string) bool {
	rest, ok := strings.CutPrefix(s, RelayPrefix)
	if !ok {
		return false
	}
	m := relayPattern.FindStringSubmatch(rest)
	if m == nil {
		return false
	}
	port, path := m[2], m[3]
	return IsHost(m[1]) &&
		(port == "" || isPort(port[1:])) &&
		!strings.HasSuffix(path, "/") &&
		// A lone surrogate (a JSON escape in a channel makes one), or bytes that are not UTF-8,
		// are no text (SPEC.md §6).
		text.IsWellFormed(path) &&
		text.CodePoints(s) <= proto.MaxRelayURLLength
}

// ParseRelayList reads a JSON value as a relay list: 1 to 10 addresses in their one spelling,
// none twice. False when it is not one.
func ParseRelayList(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	list := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		list = append(list, s)
	}
	return CheckRelayList(list)
}

// CheckRelayList returns list when it is a relay list: 1 to 10 addresses in their one spelling,
// none twice.
func CheckRelayList(list []string) ([]string, bool) {
	if len(list) == 0 || len(list) > proto.MaxRelays {
		return nil, false
	}
	for i, relay := range list {
		if !IsRelayURL(relay) {
			return nil, false
		}
		for _, earlier := range list[:i] {
			if earlier == relay {
				return nil, false
			}
		}
	}
	return append([]string(nil), list...), true
}

// DecodeRelayList reads the relays part of a key or a checkout link: base64url of the UTF-8 of
// the hosts without "wss://", joined by ",". Strict UTF-8: a byte order mark is kept, so a part
// starting with one names no relay.
func DecodeRelayList(part string) ([]string, bool) {
	b, ok := b64.DecodeURL(part)
	if !ok || len(b) == 0 || !utf8.Valid(b) {
		return nil, false
	}
	hosts := strings.Split(string(b), ",")
	list := make([]string, len(hosts))
	for i, host := range hosts {
		list[i] = RelayPrefix + host
	}
	return CheckRelayList(list)
}

// EncodeRelayList writes relays as a key or a checkout link writes them.
func EncodeRelayList(relays []string) string {
	hosts := make([]string, len(relays))
	for i, relay := range relays {
		hosts[i] = strings.TrimPrefix(relay, RelayPrefix)
	}
	return b64.EncodeURL([]byte(strings.Join(hosts, ",")))
}

// SameRelayList tells whether two lists hold the same relays in the same order.
func SameRelayList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
