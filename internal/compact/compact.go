package compact

import (
	"unicode/utf8"
)

type Config struct {
	Enabled            bool
	MaxToolOutputChars int
}

const shortTruncationMarker = "…[truncated]"

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
	if max <= 0 {

		return ""
	}

	if orig <= max {
		return s
	}

	marker := shortTruncationMarker
	kept := max - len(marker)
	if kept < 0 {

		marker = ""
		kept = max
	}

	if kept > orig {
		kept = orig
	}
	for kept > 0 && kept < len(s) && !utf8.RuneStart(s[kept]) {
		kept--
	}
	return s[:kept] + marker
}

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
