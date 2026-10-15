package config

import (
	"strings"
	"testing"
	"time"
)

func TestDurEmptyIsUnset(t *testing.T) {
	got, err := Dur("")
	if err != nil {
		t.Fatalf(`Dur("") = error %v, want (0, nil); an omitted field is unset, not malformed`, err)
	}
	if got != 0 {
		t.Errorf(`Dur("") = %v, want 0`, got)
	}
}

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

		{"1m60s", 2 * time.Minute},
		{"1h90m", 150 * time.Minute},
		{"500ms", 500 * time.Millisecond},
		{"0s", 0},
		{"-1s", -time.Second},

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

func TestDurRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"1d",
		"1w",
		"00",
		"1h30",
		"abc",
		"1 s",
		"1s ",
		" 1s",
		"1sec",
		"s",
		"1s2",
		"1.5.5s",
		"--1s",
		"NaN",
		"Infinity",
		"+-1s",
		"1m-1s",
	} {
		if got, err := Dur(in); err == nil {
			t.Errorf("Dur(%q) = %v, nil; want an error. A duration that silently "+
				"becomes 0 turns a typo into a disabled timeout or an unlimited limit", in, got)
		}
	}
}

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

func TestValidateDurationsRejectsMalformedBeforeItIsUsed(t *testing.T) {
	cfg := baseConfig()
	cfg.Server.ReadTimeout = "1d"

	err := cfg.Validate()
	if err == nil {
		t.Fatal(`Validate() accepted read_timeout = "1d"; a malformed duration must fail at load`)
	}
	if !strings.Contains(err.Error(), "read_timeout") {
		t.Errorf("Validate() error = %q, want it to name read_timeout", err)
	}
}
