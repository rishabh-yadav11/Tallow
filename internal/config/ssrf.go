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

var allowPrivateBaseURL atomic.Bool

func SetAllowPrivateBaseURL(allow bool) { allowPrivateBaseURL.Store(allow) }

func PrivateBaseURLAllowed() bool { return allowPrivateBaseURL.Load() }

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

		return nil
	}
	if strings.Contains(u.Host, ":") {

		if _, err := netip.ParseAddr(strings.Trim(u.Host, "[]")); err != nil {
			return fmt.Errorf("provider %q: base_url has an unparseable host %q: %w", provider, u.Host, err)
		}
	}
	return nil
}

func IsPublicAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}

	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}

	if addr.IsPrivate() {
		return false
	}
	if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return false
	}
	if addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}

	if v4 := addr; v4.Is4() {
		if cgnat.Contains(v4) {
			return false
		}
	}

	if addr == netip.AddrFrom4([4]byte{169, 254, 169, 254}) {
		return false
	}

	if addr.Is4() && (addr.As4()[0] == 0 || addr.As4()[0] >= 240) {
		return false
	}
	return true
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

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

		return err
	}
	return checkAllPublic(ips, network)
}

func checkAllPublic(ips []netip.Addr, network string) error {
	if len(ips) == 0 {

		return fmt.Errorf("ssrf guard: no addresses to check for the %s connection", network)
	}
	for _, ip := range ips {
		if !IsPublicAddr(ip) {
			return &PrivateAddrError{Addr: ip, Network: network}
		}
	}
	return nil
}

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

func CheckTargetPublic(host string) error {
	if allowPrivateBaseURL.Load() {
		return nil
	}
	ips, err := resolveGuardAddrs(host)
	if err != nil {
		return err
	}
	return checkAllPublic(ips, "tcp")
}

func resolveGuardAddrs(host string) ([]netip.Addr, error) {

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
