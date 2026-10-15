package compact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestH1TruncateNeverSplitsRune(t *testing.T) {

	cjk := strings.Repeat("世界", 10000)
	out := truncate(cjk, 8000)

	if !utf8.ValidString(out) {
		bad := 0
		for i := range out {
			if !utf8.ValidString(out[i:]) {
				bad = i
				break
			}
		}
		t.Fatalf("truncate produced invalid UTF-8 (H1); first bad byte at offset %d\ncontext: % x",
			bad, out[max(0, bad-8):min(len(out), bad+8)])
	}
	if len(out) > 8000 {
		t.Fatalf("result exceeds bound: %d > 8000", len(out))
	}

	if len(out) >= len(cjk) {
		t.Fatal("expected truncation to occur")
	}

	if !strings.HasSuffix(out, shortTruncationMarker) {
		t.Fatalf("expected a truncation notice at max=8000, got %q", tail(out, 60))
	}
}

func TestH1AllRuneWidthsAtAllBoundaries(t *testing.T) {

	mixed := strings.Repeat("a世€𝄞", 200)
	for maxBytes := 0; maxBytes <= 400; maxBytes++ {
		out := truncate(mixed, maxBytes)
		if !utf8.ValidString(out) {
			t.Fatalf("max=%d produced invalid UTF-8 (H1)", maxBytes)
		}
		if len(out) > maxBytes {
			t.Fatalf("max=%d produced %d bytes, over the bound (H2)", maxBytes, len(out))
		}
	}
}

func TestH1EmojiAndCombiningMarks(t *testing.T) {
	for name, s := range map[string]string{
		"astral":    strings.Repeat("𝄞", 500),
		"combining": strings.Repeat("é", 500),
		"mixed":     strings.Repeat("a𝄞é世", 300),
	} {
		for maxBytes := 0; maxBytes <= 300; maxBytes++ {
			out := truncate(s, maxBytes)
			if !utf8.ValidString(out) {
				t.Fatalf("%s: max=%d produced invalid UTF-8 (H1)", name, maxBytes)
			}
			if len(out) > maxBytes {
				t.Fatalf("%s: max=%d produced %d bytes, over the bound (H2)", name, maxBytes, len(out))
			}
		}
	}
}

func TestH2BoundIsNeverExceeded(t *testing.T) {
	body := strings.Repeat("x", 5000)
	for maxBytes := 0; maxBytes <= 200; maxBytes++ {
		out := truncate(body, maxBytes)
		if len(out) > maxBytes {
			t.Fatalf("max=%d produced %d bytes, exceeding the bound by %d (H2)",
				maxBytes, len(out), len(out)-maxBytes)
		}
	}
}

func TestH2BoundWinsOverNotice(t *testing.T) {
	in := strings.Repeat("z", 500)
	for maxBytes := 0; maxBytes <= 300; maxBytes++ {
		out := truncate(in, maxBytes)
		if len(out) > maxBytes {
			t.Fatalf("max=%d produced %d bytes (H2)", maxBytes, len(out))
		}
	}

	prev := -1
	for maxBytes := 0; maxBytes <= 300; maxBytes++ {
		cur := len(truncate(in, maxBytes))
		if cur < prev {
			t.Fatalf("non-monotonic: max=%d produced %d bytes, less than max=%d's %d",
				maxBytes, cur, maxBytes-1, prev)
		}
		prev = cur
	}
}

func TestH2NeverInflatesThePayload(t *testing.T) {

	for _, n := range []int{60, 100, 500} {
		in := strings.Repeat("y", n)
		for maxBytes := 1; maxBytes <= 120; maxBytes++ {
			if got := len(truncate(in, maxBytes)); got > n {
				t.Fatalf("input %d bytes, max=%d produced %d bytes: truncation inflated the payload (H2)",
					n, maxBytes, got)
			}
		}
	}
}

func TestH2NoticeIsPresentWheneverAffordable(t *testing.T) {
	in := strings.Repeat("q", 5000)
	for maxBytes := len(shortTruncationMarker) + 1; maxBytes <= 300; maxBytes++ {
		out := truncate(in, maxBytes)
		if !strings.Contains(out, "truncated") {
			t.Fatalf("max=%d has room for a notice but none was emitted: %q", maxBytes, tail(out, 40))
		}
	}
}

func TestTruncateIdempotenceAndPrefixProperty(t *testing.T) {
	inputs := map[string]string{
		"ascii": strings.Repeat("a", 5000),
		"cjk":   strings.Repeat("世界", 2500),
		"mixed": strings.Repeat("a世𝄞", 1500),
		"short": "hello world",
		"empty": "",
		"one":   "x",
	}
	for name, in := range inputs {
		for maxBytes := 0; maxBytes <= 300; maxBytes++ {
			out := truncate(in, maxBytes)
			if len(out) > maxBytes {
				t.Fatalf("%s: max=%d produced %d bytes (H2)", name, maxBytes, len(out))
			}
			if !utf8.ValidString(out) {
				t.Fatalf("%s: max=%d produced invalid UTF-8 (H1)", name, maxBytes)
			}
		}

		out := truncate(in, 100)
		if len(out) < len(in) && len(out) > 0 {
			idx := strings.Index(out, "\n\n[toutput")
			if idx < 0 {
				idx = strings.Index(out, shortTruncationMarker)
			}
			if idx < 0 {
				t.Fatalf("%s: truncated to %d bytes with no marker at all", name, len(out))
			}
			if !strings.HasPrefix(in, out[:idx]) {
				t.Fatalf("%s: kept content is not a prefix of the input", name)
			}
			if idx != 0 && !utf8.ValidString(out[:idx]) {
				t.Fatalf("%s: kept content is not valid UTF-8 (H1)", name)
			}
		}
	}
}

func TestApplyBoundsToolOutputThroughBothContentShapes(t *testing.T) {
	big := strings.Repeat("世界", 5000)
	cfg := Config{Enabled: true, MaxToolOutputChars: 8000}

	messages := []any{
		map[string]any{"role": "tool", "content": big},
		map[string]any{"role": "tool", "content": []any{map[string]any{"text": big}}},
		map[string]any{"role": "user", "content": big},
	}
	Apply(messages, cfg)

	first, _ := messages[0].(map[string]any)["content"].(string)
	if len(first) > 8000 {
		t.Fatalf("string content: %d bytes, over the 8000 bound", len(first))
	}
	if !utf8.ValidString(first) {
		t.Fatal("string content: invalid UTF-8 after Apply (H1)")
	}

	parts, _ := messages[1].(map[string]any)["content"].([]any)
	part, _ := parts[0].(map[string]any)
	txt, _ := part["text"].(string)
	if len(txt) > 8000 {
		t.Fatalf("[]any content part: %d bytes, over the 8000 bound", len(txt))
	}
	if !utf8.ValidString(txt) {
		t.Fatal("[]any content part: invalid UTF-8 after Apply (H1)")
	}

	user, _ := messages[2].(map[string]any)["content"].(string)
	if user != big {
		t.Fatal("non-tool message content must not be modified")
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
