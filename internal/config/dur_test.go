package config

import (
	"strings"
	"testing"
	"time"
)

// Dur is the single duration parser for the whole config. Every caller either
// surfaces its error or has already run validateDurations, so its two
// behaviours are load-bearing:
//
//   - empty means "unset" and yields 0, not a parse error. applyDefaults fills
//     the real default in first, so an omitted field is never silently zero.
//   - anything malformed returns an error rather than 0. A duration that
//     degrades to 0 instead of failing is H9's exact failure mode: a
//     "0 means unlimited" convention turns a typo into an unlimited budget,
//     a disabled timeout, or a cache that never expires.
//
// These are unit tests for the parser itself; the config-level consequences
// (H9) are covered in h9_validate_test.go.

// TestDurEmptyIsUnset covers the deliberate zero-for-empty branch. A bare ""
// is the one input that must not be an error, because it means "not specified"
// and applyDefaults has already substituted the real value.
func TestDurEmptyIsUnset(t *testing.T) {
	got, err := Dur("")
	if err != nil {
		t.Fatalf(`Dur("") = error %v, want (0, nil); an omitted field is unset, not malformed`, err)
	}
	if got != 0 {
		t.Errorf(`Dur("") = %v, want 0`, got)
	}
}

// TestDurParsesValidDurations pins the parser against time.ParseDuration's
// units, because a config field is written by a human in a TOML file.
//
// The valid set is exactly what time.ParseDuration accepts - no "d" for days,
// which is the one most operators reach for. The error message naming an
// unknown unit is the operator's only clue, so that path matters more than the
// happy one.
func TestDurParsesValidDurations(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"1s", time.Second},
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
		{"2h", 2 * time.Hour},
		{"1h30m", 90 * time.Minute},
		// Out-of-range components are normalized, not rejected. Worth pinning
		// because it is surprising, and because silently accepting it is the
		// right behaviour here - "1m60s" plainly means two minutes, and an
		// operator is not better served by a startup failure.
		{"1m60s", 2 * time.Minute},
		{"1h90m", 150 * time.Minute},
		{"500ms", 500 * time.Millisecond},
		{"0s", 0},
		{"-1s", -time.Second}, // parses; validateDurations is what rejects negatives
		// ParseDuration special-cases a bare "0" with no unit, but NOT "00".
		// That asymmetry is real behaviour worth pinning, not a wish: it is why
		// "0" and "00" belong in opposite tables below.
		{"0", 0},
		{"+0", 0},
		{"-0", 0},
	}
	for _, c := range cases {
		got, err := Dur(c.in)
		if err != nil {
			t.Errorf("Dur(%q) = error %v, want %v", c.in, err, c.want)
			continue
		}
		if got != c.want {
			t.Errorf("Dur(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestDurRejectsMalformed is the half that matters. Every one of these must
// return an error, because the alternative is the value silently becoming 0.
//
// The first entry is the important one: "1d" is not a Go duration. An operator
// writing days in a config file gets an error naming the unknown unit, which is
// the behaviour we want - silently reading it as 0 would mean "unlimited".
func TestDurRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"1d",       // days are not a Go duration unit
		"1w",       // nor weeks
		"00",       // a bare zero is special-cased, "00" is not
		"1h30",     // trailing component with no unit
		"abc",      // not a number
		"1 s",      // internal space
		"1s ",      // trailing space
		" 1s",      // leading space
		"1sec",     // wrong unit suffix
		"s",        // unit with no number
		"1s2",      // junk after a valid duration
		"1.5.5s",   // malformed number
		"--1s",     // double sign
		"NaN",      // not a number
		"Infinity", // not a number
		"+-1s",     // mixed signs
		"1m-1s",    // negative component inside a positive duration
	} {
		if got, err := Dur(in); err == nil {
			t.Errorf("Dur(%q) = %v, nil; want an error. A duration that silently "+
				"becomes 0 turns a typo into a disabled timeout or an unlimited limit", in, got)
		}
	}
}

// TestDurErrorNamesTheField makes the failure actionable. validateDurations
// wraps this error with the config key, so the parser's own message is the last
// thing a reader sees; it must not be empty, and for an unknown unit it should
// say which unit, because "1d" failing is otherwise baffling.
func TestDurErrorNamesTheField(t *testing.T) {
	_, err := Dur("not-a-duration")
	if err == nil {
		t.Fatal("expected an error")
	}
	if msg := err.Error(); msg == "" {
		t.Error("Dur returned an empty error message; the operator cannot tell which value failed")
	}

	_, err = Dur("1d")
	if err == nil {
		t.Fatal(`Dur("1d") must fail: days are not a Go duration unit`)
	}
	if msg := err.Error(); !strings.Contains(msg, "d") {
		t.Errorf(`Dur("1d") error = %q, want it to name the unknown unit "d"; `+
			"otherwise an operator cannot tell why their days value was rejected", msg)
	}
}

// TestValidateDurationsRejectsMalformedBeforeItIsUsed ties the parser to its
// only remaining contract: a malformed duration must be rejected at Validate,
// not discovered later at the point of use, where a typo has already disabled
// whatever the field governed.
//
// "1d" is the case: it is a plausible thing for an operator to write, it parses
// nowhere else in the config, and silently reading it as 0 would set the server
// read timeout to "no timeout".
func TestValidateDurationsRejectsMalformedBeforeItIsUsed(t *testing.T) {
	cfg := baseConfig()
	cfg.Server.ReadTimeout = "1d" // days: plausible, and not a Go duration

	err := cfg.Validate()
	if err == nil {
		t.Fatal(`Validate() accepted read_timeout = "1d"; a malformed duration must fail at load`)
	}
	if !strings.Contains(err.Error(), "read_timeout") {
		t.Errorf("Validate() error = %q, want it to name read_timeout", err)
	}
}
