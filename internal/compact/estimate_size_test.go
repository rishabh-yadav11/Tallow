package compact

import "testing"

// EstimateSize is currently unreferenced by production code: nothing in the
// tree calls it, and the "trimmed N bytes" log line its doc comment describes
// was never wired up. It is covered here anyway, because the semantics are
// subtle enough (role filter, string filter, byte length) that the first
// caller should not have to rediscover them, and because dead code that is
// wrong is a trap for whoever does wire it up.

// TestEstimateSizeCountsOnlyToolContent pins the two filters: role must be
// "tool" and content must be a string. Anything else contributes zero.
func TestEstimateSizeCountsOnlyToolContent(t *testing.T) {
	cases := []struct {
		name     string
		messages []any
		want     int
	}{
		{
			name:     "no messages",
			messages: nil,
			want:     0,
		},
		{
			name:     "empty slice",
			messages: []any{},
			want:     0,
		},
		{
			name: "counts tool string content",
			messages: []any{
				map[string]any{"role": "tool", "content": "12345"},
				map[string]any{"role": "tool", "content": "678"},
			},
			want: 8,
		},
		{
			// User, assistant and system content is not tool output, so it must
			// not be counted. A bug that ignored the role filter would report
			// the whole conversation as trimmed tool output.
			name: "ignores non-tool roles",
			messages: []any{
				map[string]any{"role": "user", "content": "aaaaaaaaaaaaaaaa"},
				map[string]any{"role": "assistant", "content": "bbbbbbbbbbbbbbbb"},
				map[string]any{"role": "system", "content": "cccccccc"},
				map[string]any{"role": "tool", "content": "dd"},
			},
			want: 2,
		},
		{
			// Tool content in a non-string form (a content block array, a
			// number, nil) is skipped rather than counted or panicking.
			name: "skips non-string content",
			messages: []any{
				map[string]any{"role": "tool", "content": nil},
				map[string]any{"role": "tool", "content": 42},
				map[string]any{"role": "tool", "content": []any{map[string]any{"text": "ffff"}}},
				map[string]any{"role": "tool", "content": "ok"},
			},
			want: 2,
		},
		{
			name: "skips messages that are not objects",
			messages: []any{
				"a bare string",
				123,
				nil,
				map[string]any{"role": "tool", "content": "xyz"},
			},
			want: 3,
		},
		{
			// A role of the right type but the wrong value, and a missing role,
			// are both excluded. An empty role is not the tool role.
			name: "excludes wrong or missing role",
			messages: []any{
				map[string]any{"role": "tools", "content": "aaaa"},
				map[string]any{"role": "", "content": "bbbb"},
				map[string]any{"role": 7, "content": "cccc"},
				map[string]any{"content": "dddd"},
			},
			want: 0,
		},
		{
			// Byte length, not rune count: callers use this to reason about
			// byte-bounded trimming, so multi-byte content must count bytes.
			name: "counts bytes not runes",
			messages: []any{
				map[string]any{"role": "tool", "content": "世界"}, // 6 bytes, 2 runes
				map[string]any{"role": "tool", "content": "👍"},  // 4 bytes, 1 rune
			},
			want: 10,
		},
		{
			// A tool message with an empty string contributes zero, which must
			// not be confused with "not counted" by any caller arithmetic.
			name:     "empty tool content is zero",
			messages: []any{map[string]any{"role": "tool", "content": ""}},
			want:     0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateSize(tc.messages); got != tc.want {
				t.Errorf("EstimateSize = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEstimateSizeIsAdditive checks the accumulation is a plain sum, not, for
// example, clamped at a single message's size.
func TestEstimateSizeIsAdditive(t *testing.T) {
	var msgs []any
	total := 0
	for i := 0; i < 50; i++ {
		s := "0123456789" // 10 bytes
		msgs = append(msgs, map[string]any{"role": "tool", "content": s})
		total += len(s)
	}
	if got := EstimateSize(msgs); got != total {
		t.Errorf("EstimateSize = %d, want %d", got, total)
	}
}
