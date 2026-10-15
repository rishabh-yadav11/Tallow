package proxy

import (
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestWriteRetryAfterRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want string
	}{

		{"exactly one second", time.Second, "1"},
		{"exactly a minute", 60 * time.Second, "60"},

		{"one millisecond over a second", time.Second + time.Millisecond, "2"},
		{"half a second over a minute", 60*time.Second + 500*time.Millisecond, "61"},

		{"56.5s, the aged provider RPM window", 56500 * time.Millisecond, "57"},
		{"999ms is one second, not zero", 999 * time.Millisecond, "1"},

		{"zero emits nothing", 0, ""},
		{"negative emits nothing", -5 * time.Second, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeRetryAfter(rec, tc.in)
			if got := rec.Header().Get("Retry-After"); got != tc.want {
				t.Errorf("writeRetryAfter(%v) set Retry-After=%q, want %q",
					tc.in, got, tc.want)
			}

			if got := rec.Header().Get("Retry-After"); got == "0" {
				t.Errorf("writeRetryAfter(%v) emitted 0, which means retry "+
					"immediately and is exactly wrong for a rate limit", tc.in)
			}
		})
	}
}

func TestWriteRetryAfterNeverUnderReportsTheWindow(t *testing.T) {
	for ms := 1; ms <= 60_000; ms += 37 {
		d := time.Duration(ms) * time.Millisecond
		rec := httptest.NewRecorder()
		writeRetryAfter(rec, d)
		raw := rec.Header().Get("Retry-After")
		if raw == "" {
			t.Fatalf("no Retry-After emitted for a %v wait", d)
		}
		secs, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("Retry-After %q: %v", raw, err)
		}
		if time.Duration(secs)*time.Second < d {
			t.Fatalf("wait %v was reported as %ds, which expires before the "+
				"window does: a client obeying this returns and is limited again", d, secs)
		}
	}
}
