package admin

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestLimitParamParsingAndClamp pins the rule itself, on the function that owns
// it, rather than inferring it from what the endpoints happen to return.
//
// The socket-level suite in internal/app proves the bound is observable where a
// caller reaches it, but it cannot distinguish every parsing outcome. A
// mutation run showed why: a mutant that passes a negative limit straight
// through survives there, because the store's own `if limit <= 0` clamp
// catches it and happens to substitute the same default. That is an agreement
// between two layers rather than a property of either, and the agreement does
// not hold everywhere: RecentRollups maps 0 to 200 while RecentRequests maps it
// to 100, so the same mutant would change /rollups output. A test that cannot
// see that difference is not evidence the parsing is right.
//
// So the values are asserted here. Every case is a real HTTP request through
// httptest, so this exercises the actual query-string handling, including the
// URL-encoding rules, rather than a hand-built Request.
func TestLimitParamParsingAndClamp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		def   int
		want  int
	}{
		// Absent: the caller's default.
		{"absent", "", 100, 100},
		{"absent with a different default", "", 200, 200},

		// Ordinary values pass through untouched.
		{"one", "?limit=1", 100, 1},
		{"small", "?limit=37", 100, 37},
		{"just below the cap", "?limit=999", 100, 999},
		{"exactly the cap", "?limit=1000", 100, 1000},

		// Above the cap clamps down. The off-by-one cases matter most: a clamp
		// written as `> cap+1` returns cap+1 rows, which a test that only asks
		// for 1e8 would not notice.
		{"one above the cap", "?limit=1001", 100, 1000},
		{"well above the cap", "?limit=5000", 100, 1000},
		{"max int32", "?limit=2147483647", 100, 1000},
		{"max int64", "?limit=9223372036854775807", 100, 1000},

		// Anything unparseable or non-positive falls back to the default. None
		// of these may reach SQL, and none may return the cap: a caller must
		// not be able to turn a typo into a 1000-row response.
		{"zero", "?limit=0", 100, 100},
		{"negative", "?limit=-4", 100, 100},
		{"large negative", "?limit=-999999", 100, 100},
		{"not a number", "?limit=abc", 100, 100},
		{"empty value", "?limit=", 100, 100},
		{"float", "?limit=5.9", 100, 100},
		{"overflows int64", "?limit=99999999999999999999", 100, 100},
		{"whitespace", "?limit=%205", 100, 100},
		{"hex", "?limit=0x10", 100, 100},

		// strconv.Atoi accepts a leading plus, so ?limit=+5 is 5. This is
		// harmless: it is a number the caller chose, and the clamp still
		// applies to it.
		{"leading plus is a number", "?limit=%2B5", 100, 5},

		// A repeated parameter takes the first value, which is what
		// url.Values.Get returns. Any consistent choice is fine here; what
		// matters is that the value is a small bounded one.
		{"repeated takes the first", "?limit=3&limit=7", 100, 3},

		// A zero limit must not be "fixed up" by clamping: it is the default
		// path, and the default is what the caller of limitParam asked for.
		{"zero with the rollups default", "?limit=0", 200, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/logs"+tc.query, nil)
			if got := limitParam(r, tc.def); got != tc.want {
				t.Errorf("limitParam(%q, %d) = %d, want %d",
					tc.query, tc.def, got, tc.want)
			}
		})
	}
}

// TestLimitParamNeverExceedsTheCap is the invariant, checked across a range
// rather than a list, so it holds for inputs nobody thought to write down.
//
// The targets are built with url.Parse and then handed to httptest, rather than
// concatenated into a raw string. That distinction is not cosmetic: a literal
// space in a request target makes httptest panic outright, and a hand-built
// target would have let this test assert against a query string that no client
// can actually send. Encoding first means every case here is one a real caller
// could issue.
func TestLimitParamNeverExceedsTheCap(t *testing.T) {
	// The query values are encoded through url.Values rather than pasted into a
	// target string. That distinction is not cosmetic: a literal space in a
	// request target makes httptest panic outright, so a hand-built target here
	// would have tested a query string no client can actually send, or crashed.
	// url.Values.Encode guarantees a well-formed target for every value,
	// including the deliberately awkward ones below.
	rawValues := []string{
		"", "1", "0", "-1", "abc", "1000", "1001", "99999999", "-99999999",
		"9223372036854775807", "99999999999999999999999", "1e9", "+5",
		"0x10", "5.9", " ", "\t", "1000000000000000000000000",
	}
	targets := []string{"/logs?other=1", "/logs?LIMIT=500", "/logs"}
	for _, v := range rawValues {
		q := url.Values{}
		q.Set("limit", v)
		targets = append(targets, "/logs?"+q.Encode())
	}
	for _, d := range []int{0, 1, 100, 200, 1000, 100000, 1 << 40} {
		for _, target := range targets {
			r := httptest.NewRequest("GET", target, nil)
			got := limitParam(r, d)
			if got < 0 {
				t.Errorf("limitParam(%q, %d) = %d, must never be negative",
					target, d, got)
			}
			if got > maxLimitParam {
				t.Errorf("limitParam(%q, %d) = %d, exceeds the cap of %d",
					target, d, got, maxLimitParam)
			}
		}
	}
}
