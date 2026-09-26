package proxy

import (
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestWriteRetryAfterRoundsUp pins the conversion from a wait to the whole
// seconds a Retry-After header carries.
//
// This exists as a direct unit test because the rounding is only observable when
// the wait falls on a fractional second, and the end-to-end test that exercises
// it has to sleep to produce that. Asserting an upper bound there cannot tell
// rounding up from rounding down: a fast run lands at 59.99s, which is 59 either
// way. So the arithmetic is checked here, exactly, and the end-to-end test only
// has to confirm that the header is wired to a live window at all.
//
// The direction of the rounding is the whole point. Rounding DOWN reports a wait
// that ends before the RPM window does, so a well-behaved client returns and is
// rate limited again immediately. That converts a rate limit into self-inflicted
// retry load, which is worse than no header at all.
func TestWriteRetryAfterRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want string
	}{
		// The exact-boundary cases: an integer number of seconds must pass
		// through unchanged, not gain a second.
		{"exactly one second", time.Second, "1"},
		{"exactly a minute", 60 * time.Second, "60"},

		// The fractional cases. Each is the case that fails under floor.
		{"one millisecond over a second", time.Second + time.Millisecond, "2"},
		{"half a second over a minute", 60*time.Second + 500*time.Millisecond, "61"},
		{"57.5s is the aged-window case", 57500 * time.Millisecond, "58"},
		{"999ms is one second, not zero", 999 * time.Millisecond, "1"},

		// Unknown waits are omitted rather than guessed. A wrong Retry-After is
		// worse than none, because a client will trust it.
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
			// A Retry-After of zero is invalid per RFC 9110 and reads as
			// "retry immediately", so it must never appear.
			if got := rec.Header().Get("Retry-After"); got == "0" {
				t.Errorf("writeRetryAfter(%v) emitted 0, which means retry "+
					"immediately and is exactly wrong for a rate limit", tc.in)
			}
		})
	}
}

// TestWriteRetryAfterNeverUnderReportsTheWindow is the property, stated
// directly rather than through the table above. For every wait, the reported
// number of seconds must be at least the wait itself, so a client that obeys
// the header cannot come back before the window has rolled.
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
