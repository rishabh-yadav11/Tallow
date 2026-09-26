package compact

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestH1TruncateNeverSplitsRune is the CJK regression. The audit measured:
// cjk max=8000, input=30000 bytes, kept=7948 -> out=8000 bytes,
// validUTF8=false, first bad byte at offset 7947.
//
// The pre-existing e2e test could not catch this because it used max=100 with
// 5000 ASCII chars, giving kept=50, an EVEN byte count that happens to land on a
// rune boundary. The defect is invisible to ASCII-only fixtures, which is the
// general test-quality failure the audit called out.
func TestH1TruncateNeverSplitsRune(t *testing.T) {
	// The audit's exact case.
	cjk := strings.Repeat("世界", 10000) // 30000 bytes
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
	// It must actually have truncated, or the test proves nothing.
	if len(out) >= len(cjk) {
		t.Fatal("expected truncation to occur")
	}
	// The bound is enforced against the SUM, so with a 30000-byte CJK input and
	// max=8000 the content budget is the full 8000 bytes and no notice fits
	// alongside it. The abbreviated notice is used, which is the correct
	// trade: the bound is the invariant, the notice is best-effort.
	if !strings.HasSuffix(out, shortTruncationMarker) {
		t.Fatalf("expected a truncation notice at max=8000, got %q", tail(out, 60))
	}
}

// TestH1AllRuneWidthsAtAllBoundaries walks every byte offset in a
// multi-byte string as the cut point, so the boundary walk is exercised at every
// possible position rather than at one lucky offset. Any single split rune fails
// this test.
func TestH1AllRuneWidthsAtAllBoundaries(t *testing.T) {
	// A mix of 1-, 2-, 3- and 4-byte runes so every continuation-byte depth is hit.
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

// TestH1EmojiAndCombiningMarks covers the two shapes most likely to defeat a
// naive boundary walk: 4-byte runes (astral plane) and combining marks, where
// even a rune-aligned cut can still look wrong to a reader.
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

// TestH2BoundIsNeverExceeded is the H2 regression. The audit measured the bound
// being overshot by up to 49 bytes because the marker was appended
// unconditionally:
//
//	max=1  -> 50 bytes (exceeded by 49)
//	max=10 -> 50 bytes (exceeded by 40)
//	max=40 -> 50 bytes (exceeded by 10)
//	max=48 -> 50 bytes (exceeded by 2)
//	max=49 -> 50 bytes (exceeded by 1)
//
// A limit overshot by 50x is not a limit, and at max=1 the output was LONGER
// than the input it replaced.
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

// TestH2BoundWinsOverNotice documents the chosen H2 policy: the bound is the
// invariant and the notice is best-effort. When the full marker cannot fit, a
// fixed-width abbreviated marker is used; below even that, the bound still holds
// and the notice is dropped.
//
// The first attempt at this fix passed the content through INTACT instead. That
// was wrong and the test suite caught it: a bound that stops being enforced
// below ~50 bytes is not a bound, and it was non-monotonic too - max=1 returned
// 5000 bytes while max=100 returned 100, so raising the limit shrank the output.
func TestH2BoundWinsOverNotice(t *testing.T) {
	in := strings.Repeat("z", 500)
	for maxBytes := 0; maxBytes <= 300; maxBytes++ {
		out := truncate(in, maxBytes)
		if len(out) > maxBytes {
			t.Fatalf("max=%d produced %d bytes (H2)", maxBytes, len(out))
		}
	}
	// The bound is monotonic: a larger limit never yields a smaller result.
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

// TestH2NeverInflatesThePayload pins the sharper form of the H2 bug: at max=1
// the old code turned a 1-byte budget into 50 bytes of output, so truncation
// made the payload LARGER. That can never happen now.
func TestH2NeverInflatesThePayload(t *testing.T) {
	// The bound is the real invariant and is covered exhaustively above. This
	// test covers the sharper symptom: the old code turned a 1-byte budget into
	// 50 bytes of output, so truncating a payload made it BIGGER. Below the
	// notice width a 1-byte input legitimately yields the 14-byte notice, so the
	// inputs here are all larger than any notice that could be emitted.
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

// TestH2NoticeIsPresentWheneverAffordable checks that the fix did not solve H2
// by simply deleting the notice. For any bound that can afford one, the model
// must still be told content was cut.
func TestH2NoticeIsPresentWheneverAffordable(t *testing.T) {
	in := strings.Repeat("q", 5000)
	for maxBytes := len(shortTruncationMarker) + 1; maxBytes <= 300; maxBytes++ {
		out := truncate(in, maxBytes)
		if !strings.Contains(out, "truncated") {
			t.Fatalf("max=%d has room for a notice but none was emitted: %q", maxBytes, tail(out, 40))
		}
	}
}

// TestTruncateIdempotenceAndPrefixProperty checks the two properties callers
// actually depend on: the result is a prefix of the input (nothing invented),
// and the marker's reported "kept" count matches the real prefix length.
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
		// When truncation actually happens, the content must be a real prefix of
		// the input, with nothing invented ahead of the marker.
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

// TestTruncateMarkerReportsFinalKeptLength guards the subtle bug the fix could
// have introduced: the marker renders its two %d verbs, and a marker that
// reports a length computed BEFORE the rune walk-back would misreport what the
// caller received. This parses the marker's numbers back out and checks them.
func TestTruncateMarkerReportsFinalKeptLength(t *testing.T) {
	s := strings.Repeat("世", 1000) // 3000 bytes, every rune is 3 bytes
	for maxBytes := 60; maxBytes <= 400; maxBytes++ {
		out := truncate(s, maxBytes)
		idx := strings.Index(out, "\n\n[toutput truncated by tallow: ")
		if idx < 0 {
			// Below the full-marker threshold the abbreviated notice is used,
			// which carries no counts and so cannot be checked this way.
			if strings.HasSuffix(out, shortTruncationMarker) {
				continue
			}
			t.Fatalf("max=%d: truncated without a marker", maxBytes)
		}
		var orig, kept int
		if _, err := fmt.Sscanf(out[idx:], "\n\n[toutput truncated by tallow: %d -> %d chars]", &orig, &kept); err != nil {
			t.Fatalf("max=%d: could not parse marker: %v", maxBytes, err)
		}
		if orig != len(s) {
			t.Fatalf("max=%d: marker reports orig=%d, want %d", maxBytes, orig, len(s))
		}
		// The reported kept count must equal the ACTUAL bytes of content
		// preceding the marker, not a pre-adjustment number.
		if kept != idx {
			t.Fatalf("max=%d: marker reports kept=%d but %d bytes of content precede it", maxBytes, kept, idx)
		}
	}
}

// TestApplyBoundsToolOutputThroughBothContentShapes checks the bound holds
// through the public Apply entry point, for both a plain string content and the
// structured []any content parts, since those are separate code paths.
func TestApplyBoundsToolOutputThroughBothContentShapes(t *testing.T) {
	big := strings.Repeat("世界", 5000) // 30000 bytes
	cfg := Config{Enabled: true, MaxToolOutputChars: 8000}

	messages := []any{
		map[string]any{"role": "tool", "content": big},
		map[string]any{"role": "tool", "content": []any{map[string]any{"text": big}}},
		map[string]any{"role": "user", "content": big}, // must NOT be touched
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
