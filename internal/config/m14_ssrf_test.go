package config

import (
	"net/netip"
	"strings"
	"testing"
)

// M14: any base_url was accepted, so a config could aim the gateway at an
// internal host. The upstream request carries the provider Authorization header
// and the full user prompt, so that turned the gateway into a credentialed HTTP
// client for whatever the config named.
//
// The address policy is deliberately tested as a TABLE over address space rather
// than as a handful of examples. The whole point of a default-deny rule is that
// it holds for the address nobody thought to enumerate, so the test enumerates.

// TestM14IsPublicAddrDenyByDefault is the core address-space policy.
func TestM14IsPublicAddrDenyByDefault(t *testing.T) {
	blocked := []struct {
		addr string
		why  string
	}{
		// Loopback. The classic SSRF target, and what httptest binds to.
		{"127.0.0.1", "IPv4 loopback"},
		{"127.255.255.254", "IPv4 loopback range edge"},
		{"::1", "IPv6 loopback"},
		// IPv4-mapped loopback. The trap case: without Unmap, this reads as a
		// public IPv6 address and sails through the guard.
		{"::ffff:127.0.0.1", "IPv4-mapped loopback"},
		{"::ffff:10.0.0.1", "IPv4-mapped RFC1918"},
		// RFC1918.
		{"10.0.0.1", "RFC1918 10/8"},
		{"10.255.255.255", "RFC1918 10/8 edge"},
		{"172.16.0.1", "RFC1918 172.16/12 low"},
		{"172.31.255.254", "RFC1918 172.16/12 high"},
		{"192.168.1.1", "RFC1918 192.168/16"},
		// Carrier-grade NAT: globally routable in form, not in fact.
		{"100.64.0.1", "CGNAT 100.64/10 low"},
		{"100.127.255.255", "CGNAT 100.64/10 high"},
		// Link-local, including the cloud metadata endpoint.
		{"169.254.1.1", "link-local"},
		{"169.254.169.254", "cloud metadata endpoint"},
		{"fe80::1", "IPv6 link-local"},
		// Unique-local, the IPv6 analogue of RFC1918.
		{"fc00::1", "IPv6 unique-local fc00::/7"},
		{"fd00::1", "IPv6 unique-local fd00::/8"},
		// Unspecified and "this network".
		{"0.0.0.0", "unspecified"},
		{"0.1.2.3", "0.0.0.0/8 this-network"},
		// Reserved.
		{"240.0.0.1", "240.0.0.0/4 reserved"},
		{"255.255.255.255", "broadcast"},
		// Multicast.
		{"224.0.0.1", "IPv4 multicast"},
		{"ff02::1", "IPv6 link-local multicast"},
	}
	for _, tc := range blocked {
		t.Run(tc.addr, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.addr)
			if IsPublicAddr(addr) {
				t.Fatalf("M14 NOT FIXED: %s (%s) was treated as public; a base_url resolving here "+
					"receives the provider Authorization header and the user prompt", tc.addr, tc.why)
			}
		})
	}

	allowed := []string{
		"1.1.1.1", "8.8.8.8", "93.184.216.34", "172.32.0.1", // just outside 172.16/12
		"100.63.255.255", // just below CGNAT
		"100.128.0.1",    // just above CGNAT
		"192.169.0.1",    // just outside 192.168/16
		"11.0.0.1",       // just outside 10/8
		"2606:4700:4700::1111",
	}
	for _, s := range allowed {
		t.Run("public/"+s, func(t *testing.T) {
			if !IsPublicAddr(netip.MustParseAddr(s)) {
				t.Fatalf("%s is a public address but the guard refused it; a legitimate public "+
					"provider would be unreachable", s)
			}
		})
	}

	if IsPublicAddr(netip.Addr{}) {
		t.Fatal("the zero Addr must not be treated as public; an unresolvable host must fail closed")
	}
}

// TestM14DialControlRefusesPrivateAddresses checks the guard at the boundary it
// actually runs at - the dialer - rather than only the predicate it calls.
func TestM14DialControlRefusesPrivateAddresses(t *testing.T) {
	t.Cleanup(func() { SetAllowPrivateBaseURL(false) })
	SetAllowPrivateBaseURL(false)

	cases := []struct {
		network string
		address string
	}{
		{"tcp", "127.0.0.1:443"},
		{"tcp", "10.0.0.5:8080"},
		{"tcp", "192.168.1.1:80"},
		{"tcp", "169.254.169.254:80"},
		{"tcp", "[::1]:443"},
		{"tcp", "[::ffff:127.0.0.1]:443"},
		{"tcp", "0.0.0.0:80"},
	}
	for _, tc := range cases {
		t.Run(tc.address, func(t *testing.T) {
			err := DialControl(tc.network, tc.address, nil)
			if err == nil {
				t.Fatalf("M14 NOT FIXED: DialControl permitted %s", tc.address)
			}
			var pae *PrivateAddrError
			if !asPrivateAddrError(err, &pae) {
				t.Fatalf("expected a *PrivateAddrError so callers can recognise the policy refusal, got %T: %v", err, err)
			}
			// The message is the operator's only clue, so it must say what to do.
			for _, want := range []string{"base_url", "SetAllowPrivateBaseURL"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal message does not mention %q, so the operator cannot act on it: %v", want, err)
				}
			}
		})
	}
}

// TestM14OptOutAllowsPrivateUpstreams is the other half: a private upstream is a
// legitimate deployment, and the guard must be switchable.
func TestM14OptOutAllowsPrivateUpstreams(t *testing.T) {
	t.Cleanup(func() { SetAllowPrivateBaseURL(false) })

	SetAllowPrivateBaseURL(true)
	for _, addr := range []string{"127.0.0.1:443", "10.0.0.5:8080", "[::1]:443"} {
		if err := DialControl("tcp", addr, nil); err != nil {
			t.Errorf("with the opt-in set, DialControl refused %s: %v", addr, err)
		}
	}

	SetAllowPrivateBaseURL(false)
	if err := DialControl("tcp", "127.0.0.1:443", nil); err == nil {
		t.Error("the default posture must refuse loopback; the opt-in is not sticky")
	}
}

// TestM14DialControlRejectsUnparseableAddress: a guard that silently permits
// when it cannot parse is not a guard.
func TestM14DialControlRejectsUnparseableAddress(t *testing.T) {
	t.Cleanup(func() { SetAllowPrivateBaseURL(false) })
	SetAllowPrivateBaseURL(false)

	for _, addr := range []string{"", "not-an-address", "127.0.0.1", ":::"} {
		if err := DialControl("tcp", addr, nil); err == nil {
			t.Errorf("DialControl permitted the unparseable address %q; it must fail closed", addr)
		}
	}
}

// TestM14DialControlAllowsPublicLiterals is the non-regression half: a real
// public provider must still be reachable, or the guard is a denial of service.
func TestM14DialControlAllowsPublicLiterals(t *testing.T) {
	t.Cleanup(func() { SetAllowPrivateBaseURL(false) })
	SetAllowPrivateBaseURL(false)

	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:53", "[2606:4700:4700::1111]:443"} {
		if err := DialControl("tcp", addr, nil); err != nil {
			t.Errorf("DialControl refused the public address %s: %v", addr, err)
		}
	}
}

// TestM14ValidateBaseURL covers the structural half, which runs at load time and
// is what turns an operator typo into a message rather than a 502.
func TestM14ValidateBaseURL(t *testing.T) {
	bad := []struct {
		url    string
		why    string
		wantIn string
	}{
		{"", "empty", "required"},
		{"   https://api.example.com", "leading whitespace", "whitespace"},
		{"https://api.example.com  ", "trailing whitespace", "whitespace"},
		{"api.example.com", "no scheme", "scheme"},
		{"://api.example.com", "unparseable", "base_url"},
		{"file:///etc/passwd", "file scheme", "http or https"},
		{"gopher://example.com", "gopher scheme", "http or https"},
		{"ftp://example.com", "ftp scheme", "http or https"},
		{"https://", "no host", "no host"},
		{"https://user:pass@api.example.com", "embedded credentials", "credentials"},
		{"https://api.example.com?key=secret", "query string", "query"},
		{"https://api.example.com#frag", "fragment", "query"},
	}
	for _, tc := range bad {
		t.Run(tc.why, func(t *testing.T) {
			err := ValidateBaseURL("p1", tc.url)
			if err == nil {
				t.Fatalf("M14 NOT FIXED: ValidateBaseURL accepted %q (%s)", tc.url, tc.why)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error does not mention %q: %v", tc.wantIn, err)
			}
		})
	}

	good := []string{
		"https://api.openai.com/v1",
		"http://localhost:8080/v1",
		"https://api.example.com",
		"https://api.example.com/",
		"https://api.example.com:8443/openai/v1",
		"http://10.0.0.5:8000/v1",
		"https://[2606:4700:4700::1111]:443/v1",
		"https://127.0.0.1:1234",
	}
	for _, u := range good {
		t.Run("ok/"+u, func(t *testing.T) {
			if err := ValidateBaseURL("p1", u); err != nil {
				t.Fatalf("ValidateBaseURL rejected a legitimate base_url %q: %v", u, err)
			}
		})
	}
}

// TestM14ValidateRejectsBadBaseURLThroughConfig is the wiring test: the
// structural check must actually run from Validate, not merely exist.
func TestM14ValidateRejectsBadBaseURLThroughConfig(t *testing.T) {
	for _, u := range []string{"api.example.com", "file:///etc/passwd", "https://user:pw@api.example.com"} {
		c := baseConfig()
		c.Provider[0].BaseURL = u
		if err := c.Validate(); err == nil {
			t.Errorf("M14 NOT FIXED: Config.Validate accepted base_url %q", u)
		}
	}
}

// TestM14EveryResolvedAddressMustBePublic covers the branch an attacker
// actually controls: what their domain resolves to.
//
// Judging only the first resolved address would let a name resolving to one
// public and one private address pass validation and then connect to the private
// one. This is the DNS-rebinding shape of M14, and it is why the guard checks
// every address rather than picking a winner.
func TestM14EveryResolvedAddressMustBePublic(t *testing.T) {
	pub := netip.MustParseAddr("1.1.1.1")
	pub2 := netip.MustParseAddr("8.8.8.8")
	priv := netip.MustParseAddr("127.0.0.1")
	rfc1918 := netip.MustParseAddr("10.0.0.5")
	meta := netip.MustParseAddr("169.254.169.254")

	t.Run("all public passes", func(t *testing.T) {
		if err := checkAllPublic([]netip.Addr{pub, pub2}, "tcp"); err != nil {
			t.Fatalf("a name resolving only to public addresses was refused: %v", err)
		}
	})

	// The private address is FIRST in one case and LAST in the other, so a
	// first-address-only check fails one and the last-address-only fails the
	// other. A guard that inspects only one position cannot pass both.
	for _, tc := range []struct {
		name string
		ips  []netip.Addr
		want string
	}{
		{"private first", []netip.Addr{priv, pub}, "127.0.0.1"},
		{"private last", []netip.Addr{pub, priv}, "127.0.0.1"},
		{"rfc1918 first", []netip.Addr{rfc1918, pub, pub2}, "10.0.0.5"},
		{"rfc1918 last", []netip.Addr{pub, pub2, rfc1918}, "10.0.0.5"},
		{"metadata in the middle", []netip.Addr{pub, meta, pub2}, "169.254.169.254"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAllPublic(tc.ips, "tcp")
			if err == nil {
				t.Fatalf("M14 NOT FIXED: a name resolving to %v (including %s) was permitted; "+
					"the guard must judge EVERY resolved address", tc.ips, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal names the wrong address; want %s, got %v", tc.want, err)
			}
		})
	}

	t.Run("empty set fails closed", func(t *testing.T) {
		if err := checkAllPublic(nil, "tcp"); err == nil {
			t.Fatal("M14 NOT FIXED: an empty address set was permitted; the guard must fail closed")
		}
	})
}

// asPrivateAddrError is errors.As specialised, so the test reads cleanly and
// the type assertion stays honest.
func asPrivateAddrError(err error, target **PrivateAddrError) bool {
	for err != nil {
		if e, ok := err.(*PrivateAddrError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
