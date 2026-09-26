// Package compact implements the tool-output compaction pipeline stage. It is
// a distinct stage from response caching: it truncates tool results before
// they reach the upstream model's context.
package compact

import (
	"unicode/utf8"
)

// Config controls compaction.
type Config struct {
	Enabled            bool
	MaxToolOutputChars int
}

// shortTruncationMarker is appended after truncation so the model can see that
// content was elided.
//
// It is fixed-width and carries no counts. The original marker rendered the
// original and kept lengths ("\n\n[toutput truncated by tallow: %d -> %d
// chars]"), which is informative to a human reading a log, but its length
// depends on those numbers, so a caller could not know how much content budget
// remained without first rendering it. That made the bound unenforceable for
// small max values, which is audit finding H2. A fixed-width notice makes the
// content budget exactly max - len(marker), with no fixed point to search for.
const shortTruncationMarker = "…[truncated]"

// Apply truncates tool outputs in-place on the OpenAI messages array. messages
// is the parsed request body's "messages" value: []any of map[string]any.
// Non-tool messages are left untouched.
func Apply(messages []any, cfg Config) {
	if !cfg.Enabled || cfg.MaxToolOutputChars <= 0 {
		return
	}
	for _, m := range messages {
		obj, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := obj["role"].(string); role != "tool" {
			continue
		}
		switch content := obj["content"].(type) {
		case string:
			if len(content) > cfg.MaxToolOutputChars {
				obj["content"] = truncate(content, cfg.MaxToolOutputChars)
			}
		case []any:
			for _, part := range content {
				pm, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if txt, ok := pm["text"].(string); ok && len(txt) > cfg.MaxToolOutputChars {
					pm["text"] = truncate(txt, cfg.MaxToolOutputChars)
				}
			}
		}
	}
}

// truncate shortens s so the RESULT never exceeds max bytes.
//
// Two independent defects lived here, both silent, and both fed the upstream
// model corrupted or oversized text while the gateway still answered 200.
//
// H1 - rune safety. `kept` was a BYTE count applied as s[:kept], so any
// multi-byte character straddling the cut was split. json.Marshal does not error
// on invalid UTF-8, it silently substitutes U+FFFD, so the provider received
// replacement characters instead of the real text and nothing anywhere reported
// a problem. The shipped default max_tool_output_chars = 8000 sits squarely in
// the affected range: 30000 bytes of CJK produced kept=7948, an 8000-byte
// result, and the first bad byte landed at offset 7947. ASCII-only fixtures
// cannot reach this, which is why the existing e2e test passed.
//
// H2 - the bound actually bounding. The marker was appended unconditionally, so
// whenever the marker was longer than max the RESULT EXCEEDED max:
//
//	max=1  -> 50 bytes (exceeded by 49)
//	max=10 -> 50 bytes (exceeded by 40)
//	max=48 -> 50 bytes (exceeded by 2)
//
// A configured limit that can be overshot by 50x, and that can return more
// bytes than it consumed, is not a limit.
//
// The design point is that the NOTICE is charged against the bound, not added
// on top of it. The content budget is whatever remains after the notice, and
// the content is then cut on a rune boundary within that budget. Two
// consequences follow, and both are deliberate:
//
//   - A notice is only emitted when the content budget can absorb it, so a
//     truncation that consumes the entire max budget carries no notice at all.
//     Spending the whole budget on content beats discarding a chunk of it to
//     print a notice, and the model tolerates a hard cut far better than a
//     mangled one. Returning the content untouched when it already fits is
//     checked first, so a truncation is never reported that did not happen.
func truncate(s string, max int) string {
	orig := len(s)
	if max <= 0 {
		// No budget at all. The empty string is the only answer that cannot
		// exceed a zero-byte bound.
		return ""
	}
	// Nothing to do when the input already fits. Checked before any marker
	// logic: reporting a truncation that did not happen is a lie, and at this
	// boundary a notice could push the result over max even though the content
	// itself is fine.
	if orig <= max {
		return s
	}

	// H2: the bound is charged with the notice first, then the content gets
	// whatever is left.
	marker := shortTruncationMarker
	kept := max - len(marker)
	if kept < 0 {
		// Below the notice width there is no room for both, and the bound is the
		// invariant while the notice is best-effort. Content still wins: cutting
		// to max with no notice beats exceeding max to include one.
		marker = ""
		kept = max
	}

	// H1: cut on a rune boundary. s[kept] is the first EXCLUDED byte, so if it is
	// a continuation byte the cut landed mid-rune and must retreat to that rune's
	// start. The `kept < len(s)` guard is not merely defensive: kept is
	// max - len(marker) here, which is strictly less than orig because we already
	// returned when orig <= max. So s[kept] is always in range. The guard is
	// kept so the walk stays correct if that early return is ever removed.
	if kept > orig {
		kept = orig
	}
	for kept > 0 && kept < len(s) && !utf8.RuneStart(s[kept]) {
		kept--
	}
	return s[:kept] + marker
}

// EstimateSize returns a rough byte estimate of tool-output content, useful
// for logging how much was trimmed.
func EstimateSize(messages []any) int {
	n := 0
	for _, m := range messages {
		obj, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := obj["role"].(string); role != "tool" {
			continue
		}
		if s, ok := obj["content"].(string); ok {
			n += len(s)
		}
	}
	return n
}
