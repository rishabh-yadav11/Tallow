package compact

import "testing"

func TestTruncateRespectsMaxBound(t *testing.T) {

	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'a'
	}
	const max = 1024
	out := truncate(string(long), max)
	if len(out) > max {
		t.Fatalf("truncate result len %d exceeds max %d", len(out), max)
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty truncated output")
	}
}

func TestApplySkipsNonToolMessages(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "hello"},
	}
	Apply(msgs, Config{Enabled: true, MaxToolOutputChars: 10})
	if msgs[0].(map[string]any)["content"] != "hello" {
		t.Fatal("user message must be left untouched")
	}
}
