package compact

import "testing"

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

			name: "counts bytes not runes",
			messages: []any{
				map[string]any{"role": "tool", "content": "世界"},
				map[string]any{"role": "tool", "content": "👍"},
			},
			want: 10,
		},
		{

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

func TestEstimateSizeIsAdditive(t *testing.T) {
	var msgs []any
	total := 0
	for i := 0; i < 50; i++ {
		s := "0123456789"
		msgs = append(msgs, map[string]any{"role": "tool", "content": s})
		total += len(s)
	}
	if got := EstimateSize(msgs); got != total {
		t.Errorf("EstimateSize = %d, want %d", got, total)
	}
}
