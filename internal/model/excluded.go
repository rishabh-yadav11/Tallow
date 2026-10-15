package model

func ExcludedModel(match string, patterns []string) bool {
	if match == "" {
		return false
	}
	for _, p := range patterns {
		if p != "" && matchGlob(match, p) {
			return true
		}
	}
	return false
}

func matchGlob(s, pat string) bool {
	if pat == "" {
		return false
	}
	var si, pi int
	star, mark := -1, 0
	for si < len(s) {
		if pi < len(pat) && pat[pi] == s[si] {
			pi++
			si++
			continue
		}
		if pi < len(pat) && pat[pi] == '*' {
			star, mark = pi, si
			pi++
			continue
		}
		if star >= 0 {
			pi = star + 1
			mark++
			si = mark
			continue
		}
		return false
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}
