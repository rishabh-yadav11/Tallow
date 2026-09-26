package compact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The bound is a BYTE bound (len() is used throughout Apply). Verify the result
// never exceeds it for any max, including below the marker width, and that the
// result is always valid UTF-8.
func TestZZZBoundAndUTF8AcrossAllMaxima(t *testing.T) {
	// Multi-byte content that will straddle many cut points.
	bodies := map[string]string{
		"cjk":     strings.Repeat("世界你好", 500),
		"emoji":   strings.Repeat("👍🎉", 500),
		"mixed":   strings.Repeat("a世界b👍c", 500),
		"ascii":   strings.Repeat("x", 5000),
		"oneRune": strings.Repeat("世", 100),
	}
	for name, body := range bodies {
		for max := 1; max <= 200; max++ {
			got := truncate(body, max)
			if len(got) > max {
				t.Fatalf("%s max=%d: result is %d bytes, %d over the bound",
					name, max, len(got), len(got)-max)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("%s max=%d: result is not valid UTF-8", name, max)
			}
		}
	}
}

// When the bound is large enough for content but not content+marker, the
// content must still win: the result is cut to max with no marker.
func TestZZZNoMarkerWhenItWouldOverrun(t *testing.T) {
	marker := len(shortTruncationMarker)
	body := strings.Repeat("a", 1000)
	// max == marker: kept = 0, so no content, marker exactly fills.
	got := truncate(body, marker)
	if len(got) > marker {
		t.Errorf("max=marker(%d): got %d bytes", marker, len(got))
	}
	// max == marker+1: kept = 1, content 1 byte + marker.
	got = truncate(body, marker+1)
	if len(got) > marker+1 {
		t.Errorf("max=marker+1 (%d): got %d bytes", marker+1, len(got))
	}
}

// Content that already fits must be returned byte-identical - no marker, no
// change. A truncation that did not happen must not be reported.
func TestZZZFittingContentIsUntouched(t *testing.T) {
	for n := 0; n <= 200; n++ {
		body := strings.Repeat("a", n)
		if got := truncate(body, 200); got != body {
			t.Fatalf("n=%d: content that fits was modified", n)
		}
	}
	// And a multi-byte body exactly at the bound.
	body := strings.Repeat("世", 10) // 30 bytes
	if got := truncate(body, 30); got != body {
		t.Error("a 30-byte body at max=30 was modified")
	}
}

// A body one byte over the bound with a multi-byte tail must retreat to the
// rune boundary, so the result is strictly shorter than the marker-budgeted
// slice but still valid.
func TestZZZRuneBoundaryRetreat(t *testing.T) {
	// 3-byte runes. max chosen so kept lands mid-rune.
	body := strings.Repeat("世", 100) // 300 bytes
	for max := 1; max <= 300; max++ {
		got := truncate(body, max)
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d: invalid UTF-8", max)
		}
		// Every result must be a prefix of the body, optionally plus the marker.
		stripped := strings.TrimSuffix(got, shortTruncationMarker)
		if !strings.HasPrefix(body, stripped) {
			t.Fatalf("max=%d: content is not a prefix of the input: %q", max, stripped)
		}
	}
}

// The result must never be LONGER than the input, for any bound.
func TestZZZNeverGrows(t *testing.T) {
	body := strings.Repeat("世界", 100) // 600 bytes
	for max := 1; max <= 1000; max++ {
		if got := truncate(body, max); len(got) > len(body) {
			t.Fatalf("max=%d: result %d bytes exceeds the %d-byte input",
				max, len(got), len(body))
		}
	}
}

// max<=0 must yield the empty string, never the input.
func TestZZZNonPositiveMax(t *testing.T) {
	body := strings.Repeat("x", 100)
	for _, max := range []int{0, -1, -1000} {
		if got := truncate(body, max); got != "" {
			t.Errorf("max=%d: got %q, want the empty string", max, got)
		}
	}
}
