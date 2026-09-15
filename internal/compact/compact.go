// Package compact implements the tool-output compaction pipeline stage. It is
// a distinct stage from response caching: it truncates tool results before
// they reach the upstream model's context.
package compact

import (
	"fmt"
)

// Config controls compaction.
type Config struct {
	Enabled            bool
	MaxToolOutputChars int
}

// truncationMarker is appended after truncation so the model can see that
// content was elided.
const truncationMarker = "\n\n[toutput truncated by tallow: %d -> %d chars]"

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

func truncate(s string, max int) string {
	orig := len(s)
	head := s[:max]
	return fmt.Sprintf("%s%s", head, fmt.Sprintf(truncationMarker, orig, max))
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
