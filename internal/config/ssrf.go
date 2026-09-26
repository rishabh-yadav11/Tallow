package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
)

// M14: any base_url was accepted with no restriction on where it points, so a
// config - or a hot-reloaded config - could aim the gateway at an internal host.
// Because the upstream request carries both the provider `Authorization: Bearer
// <secret>` header and the full user prompt, that turns the gateway into a
// credentialed HTTP client for whatever the config points it at.
//
// A note on the trust model, because M14's severity is deployment dependent and
// the design follows from it. base_url is operator-supplied, and the default
// listen address is 127.0.0.1, so an operator who can write base_url is usually
// already inside the same trust boundary as the provider secret. That is why
// the audit filed this as a Medium rather than a High, and it is why the fix is
// a default-on guard with an explicit opt-out rather than a hard prohibition.
//
// The exposure worth closing is the ESCALATION: a config delivered by a
// mechanism that can write only part of the config - a templating system, a CI
// job, a control plane, a synced dotfiles repo - may be able to set base_url
// without being able to read the keystore. It would then hold a credentialed
// request primitive into the internal network, carrying the operator's real
// provider credentials and the user's prompt to whatever address it named.

// AllowPrivateBaseURL permits base_url hosts that resolve to private, loopback,
// link-local, or otherwise non-public address space.
//
// It exists because a private upstream is a legitimate deployment: a vLLM server
// on 10.0.0.5, Ollama on localhost, a corporate gateway, a peer link. A guard
// that made those unreachable would be a guard operators disable, and a disabled
// guard protects nobody.
//
// With it enabled the operator is asserting a private upstream is intended, and
// the credentialed-request-into-the-internal-network risk is accepted as a
// consequence. It is off by default, so the safe posture is the default posture
// and opting out is a deliberate act.
//
// It is a package variable rather than a config field on purpose: it is a
// property of the trust relationship between the operator and the deployment,
// not a per-provider routing preference, and making it per-provider would invite
// a single compromised provider entry to opt itself out of the policy.
//
// It is atomic rather than a plain bool because a test that exercises BOTH
// postures must be able to flip it without racing the dialer goroutines it is
// testing, and because -race must stay clean when it does.
var allowPrivateBaseURL atomic.Bool

// SetAllowPrivateBaseURL enables or disables private base_url hosts.
//
// Setting it is a deliberate, deployment-wide assertion. It is not a per-request
// or per-provider switch, and there is no config-file spelling of it: an operator
// who can write the config should not be able to opt a single provider out of
// the policy by editing one entry.
func SetAllowPrivateBaseURL(allow bool) { allowPrivateBaseURL.Store(allow) }

// PrivateBaseURLAllowed reports whether private base_url hosts are permitted.
func PrivateBaseURLAllowed() bool { return allowPrivateBaseURL.Load() }

// ValidateBaseURL checks a provider base_url for the structural problems that
// make the upstream request impossible or nonsensical.
//
// This is the half that catches operator mistakes at load time with a message
// worth reading: a missing or non-HTTP scheme, a URL with no host, embedded
// credentials, or a query string that would be silently dropped when the proxy
// appends "/chat/completions".
func ValidateBaseURL(provider, raw string) error {
	if raw == "" {
		return fmt.Errorf("provider %q: base_url required", provider)
	}
	if strings.TrimSpace(raw) != raw {
		return fmt.Errorf("provider %q: base_url has leading or trailing whitespace", provider)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("provider %q: base_url: %w", provider, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return fmt.Errorf("provider %q: base_url must include a scheme, e.g. https://api.example.com (got %q)", provider, raw)
	default:
		return fmt.Errorf("provider %q: base_url scheme must be http or https, got %q", provider, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("provider %q: base_url has no host", provider)
	}
	if u.User != nil {
		return fmt.Errorf("provider %q: base_url must not embed credentials", provider)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("provider %q: base_url must not carry a query or fragment", provider)
	}
	if _, _, err := net.SplitHostPort(u.Host); err == nil {
		// A host:port is fine and normal. Nothing to do.
		return nil
	}
	if strings.Contains(u.Host, ":") {
		// Bare IPv6 without brackets, or a port on a bracketed literal.
		if _, err := netip.ParseAddr(strings.Trim(u.Host, "[]")); err != nil {
			return fmt.Errorf("provider %q: base_url has an unparseable host %q: %w", provider, u.Host, err)
		}
	}
	return nil
}

// IsPublicAddr reports whether addr is in globally routable unicast space.
//
// Everything that is not global unicast is treated as disallowed, which is the
// safe default: loopback, RFC1918, CGNAT, link-local, unique-local, multicast,
// unspecified, and the IPv4-mapped forms of all of them. An address that cannot
// be positively identified as a public unicast address is not connected to.
//
// Being deliberately conservative here costs nothing in practice: no real
// OpenAI-compatible provider is served from any of the excluded ranges, and the
// only deployments that need them are the private-upstream ones that
// AllowPrivateBaseURL exists for.
func IsPublicAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	// Unmap first: an IPv4-mapped IPv6 address must be judged by its IPv4 value,
	// or ::ffff:127.0.0.1 would read as a public IPv6 address.
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}
	// IsGlobalUnicast is true for private ranges, so they are excluded by hand.
	if addr.IsPrivate() {
		return false
	}
	if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return false
	}
	if addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	// 100.64.0.0/10, carrier-grade NAT. Globally routable in form, not in fact,
	// and a classic SSRF target in cloud environments.
	if v4 := addr; v4.Is4() {
		if cgnat.Contains(v4) {
			return false
		}
	}
	// 169.254.0.0/16 is covered by IsLinkLocalUnicast, but the cloud metadata
	// address 169.254.169.254 deserves to be named explicitly in the policy so
	// that a future change to the link-local rule cannot quietly re-open it.
	if addr == netip.AddrFrom4([4]byte{169, 254, 169, 254}) {
		return false
	}
	// 0.0.0.0/8 and 240.0.0.0/4: "this network" and reserved space. IsUnspecified
	// covers only 0.0.0.0 exactly, so the rest of 0.0.0.0/8 is named here.
	if addr.Is4() && (addr.As4()[0] == 0 || addr.As4()[0] >= 240) {
		return false
	}
	return true
}

// cgnat is 100.64.0.0/10.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// DialControl is the net.Dialer control function that enforces the M14 address
// policy against the address the network is actually about to use.
//
// It is applied per-dial rather than per-request for three reasons: it also
// covers a pooled idle connection that would otherwise skip revalidation, it
// covers a name that re-resolves to a different address between requests
// (DNS rebinding), and it is the only place where the resolved address exists
// at all. A string check on base_url at load time cannot do any of those - it
// would judge the name the operator typed, not the address the dialer used.
//
// The check runs before the socket is created, so a refused connection never
// reaches the internal host.
func DialControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("ssrf guard: cannot parse dial address %q: %w", address, err)
	}
	if allowPrivateBaseURL.Load() {
		return nil
	}
	ips, err := resolveGuardAddrs(host)
	if err != nil {
		// A name that does not resolve is a configuration problem, not a policy
		// violation, so the resolver's own message is the useful thing to return.
		return err
	}
	return checkAllPublic(ips, network)
}

// checkAllPublic enforces that EVERY address a name resolved to is permitted.
//
// This is a separate function, taking a resolved set rather than a host, for
// one reason: it is the only part of the guard that a test can reach without
// controlling DNS. Judging just the first address would let a name resolving to
// one public and one private address pass and then connect to the private one,
// and that branch is precisely the one an attacker controls - they choose what
// their domain resolves to.
func checkAllPublic(ips []netip.Addr, network string) error {
	if len(ips) == 0 {
		// Fail closed: an empty set is not evidence of a public destination.
		return fmt.Errorf("ssrf guard: no addresses to check for the %s connection", network)
	}
	for _, ip := range ips {
		if !IsPublicAddr(ip) {
			return &PrivateAddrError{Addr: ip, Network: network}
		}
	}
	return nil
}

// PrivateAddrError is returned when a base_url resolves to non-public address
// space. It is a distinct type so callers can recognise the policy refusal
// rather than pattern-matching a message.
type PrivateAddrError struct {
	Addr    netip.Addr
	Network string
}

func (e *PrivateAddrError) Error() string {
	return fmt.Sprintf("ssrf guard: refusing to connect to %s over %s: base_url resolved to a private, "+
		"loopback, link-local, or otherwise non-public address, and the upstream request carries the "+
		"provider Authorization header and the full user prompt. Point base_url at a public endpoint, "+
		"or call config.SetAllowPrivateBaseURL(true) if a private upstream is intended",
		e.Addr, e.Network)
}

// resolveGuardAddrs resolves host to the set of addresses a dial could use.
//
// A literal address is returned as-is. A name is resolved through the system
// resolver, matching what the dialer will do, so the guard judges the real
// answer rather than a re-resolution that could differ.
func resolveGuardAddrs(host string) ([]netip.Addr, error) {
	// An IPv6 zone ("fe80::1%eth0") identifies an interface, not a policy
	// difference, so it is stripped before judging the address.
	if h, _, ok := strings.Cut(host, "%"); ok {
		host = h
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("ssrf guard: cannot resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("ssrf guard: %q resolved to no addresses", host)
	}
	out := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, a.Unmap())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ssrf guard: %q resolved to no usable addresses", host)
	}
	return out, nil
}
