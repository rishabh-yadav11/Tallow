package admin

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestLimitParamParsingAndClamp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		def   int
		want  int
	}{

		{"absent", "", 100, 100},
		{"absent with a different default", "", 200, 200},

		{"one", "?limit=1", 100, 1},
		{"small", "?limit=37", 100, 37},
		{"just below the cap", "?limit=999", 100, 999},
		{"exactly the cap", "?limit=1000", 100, 1000},

		{"one above the cap", "?limit=1001", 100, 1000},
		{"well above the cap", "?limit=5000", 100, 1000},
		{"max int32", "?limit=2147483647", 100, 1000},
		{"max int64", "?limit=9223372036854775807", 100, 1000},

		{"zero", "?limit=0", 100, 100},
		{"negative", "?limit=-4", 100, 100},
		{"large negative", "?limit=-999999", 100, 100},
		{"not a number", "?limit=abc", 100, 100},
		{"empty value", "?limit=", 100, 100},
		{"float", "?limit=5.9", 100, 100},
		{"overflows int64", "?limit=99999999999999999999", 100, 100},
		{"whitespace", "?limit=%205", 100, 100},
		{"hex", "?limit=0x10", 100, 100},

		{"leading plus is a number", "?limit=%2B5", 100, 5},

		{"repeated takes the first", "?limit=3&limit=7", 100, 3},

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

func TestLimitParamNeverExceedsTheCap(t *testing.T) {

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
