package compact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateHonorsByteBoundAndUTF8AcrossAllMaxima(t *testing.T) {

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

func TestTruncateDropsMarkerRatherThanOverrunBound(t *testing.T) {
	marker := len(shortTruncationMarker)
	body := strings.Repeat("a", 1000)

	got := truncate(body, marker)
	if len(got) > marker {
		t.Errorf("max=marker(%d): got %d bytes", marker, len(got))
	}

	got = truncate(body, marker+1)
	if len(got) > marker+1 {
		t.Errorf("max=marker+1 (%d): got %d bytes", marker+1, len(got))
	}
}

func TestTruncateLeavesFittingContentByteIdentical(t *testing.T) {
	for n := 0; n <= 200; n++ {
		body := strings.Repeat("a", n)
		if got := truncate(body, 200); got != body {
			t.Fatalf("n=%d: content that fits was modified", n)
		}
	}

	body := strings.Repeat("世", 10)
	if got := truncate(body, 30); got != body {
		t.Error("a 30-byte body at max=30 was modified")
	}
}

func TestTruncateRetreatsToRuneBoundary(t *testing.T) {

	body := strings.Repeat("世", 100)
	for max := 1; max <= 300; max++ {
		got := truncate(body, max)
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d: invalid UTF-8", max)
		}

		stripped := strings.TrimSuffix(got, shortTruncationMarker)
		if !strings.HasPrefix(body, stripped) {
			t.Fatalf("max=%d: content is not a prefix of the input: %q", max, stripped)
		}
	}
}

func TestTruncateNeverGrows(t *testing.T) {
	body := strings.Repeat("世界", 100)
	for max := 1; max <= 1000; max++ {
		if got := truncate(body, max); len(got) > len(body) {
			t.Fatalf("max=%d: result %d bytes exceeds the %d-byte input",
				max, len(got), len(body))
		}
	}
}

func TestTruncateNonPositiveMaxYieldsEmpty(t *testing.T) {
	body := strings.Repeat("x", 100)
	for _, max := range []int{0, -1, -1000} {
		if got := truncate(body, max); got != "" {
			t.Errorf("max=%d: got %q, want the empty string", max, got)
		}
	}
}
