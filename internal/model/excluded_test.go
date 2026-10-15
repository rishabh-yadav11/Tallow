package model

import "testing"

func TestExcludedModelWildcards(t *testing.T) {
	tests := []struct {
		name     string
		match    string
		patterns []string
		want     bool
	}{

		{"substring hit", "gemini-flash-sonnet", []string{"*flash*"}, true},
		{"substring miss", "chromium", []string{"*flash*"}, false},
		{"prefix hit", "gemini-2.0-flash", []string{"gemini-*"}, true},
		{"prefix miss", "claude-3", []string{"gemini-*"}, false},
		{"suffix hit", "gpt-4.1-preview", []string{"*-preview"}, true},
		{"suffix miss", "gpt-4.1", []string{"*-preview"}, false},

		{"any of several", "claude-3-opus", []string{"gemini-*", "claude-3-*"}, true},
		{"none of several", "o3-mini", []string{"gemini-*", "claude-3-*"}, false},

		{"no patterns", "anything", nil, false},
		{"empty pattern ignored", "flash", []string{""}, false},

		{"exact target model", "flash-v4", []string{"*flash-v4*"}, true},
		{"exact without wildcard", "flash-v4", []string{"flash-v4"}, true},
		{"exact wrong", "flash-v5", []string{"flash-v4"}, false},
		{"lone star matches all", "anything", []string{"*"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExcludedModel(tt.match, tt.patterns); got != tt.want {
				t.Fatalf("ExcludedModel(%q, %v) = %v, want %v", tt.match, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestExcludedModelEmptyMatch(t *testing.T) {

	if ExcludedModel("", []string{"*"}) {
		t.Fatal("empty match must not be excluded by a lone star")
	}
}
