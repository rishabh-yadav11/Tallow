package model

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"
)

// requestMetaWireKeys is the admin API wire contract: the exact JSON keys a
// RequestMeta marshals to, in struct declaration order. Renaming or dropping
// any `json` tag changes this set and fails the tests below.
var requestMetaWireKeys = []string{
	"id",
	"started_at",
	"dur_ms",
	"provider",
	"key",
	"model",
	"upstream_model",
	"stream",
	"cached",
	"status",
	"route_reason",
	"prompt_tokens",
	"completion_tokens",
	"cost_cents",
	"err",
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func filledRequestMeta() RequestMeta {
	return RequestMeta{
		ID:               "req-123",
		StartedAt:        time.Date(2026, 9, 23, 10, 30, 0, 123456789, time.UTC),
		DurMillis:        1234,
		Provider:         "openai",
		Key:              "primary",
		Model:            "gpt-4o",
		UpstreamModel:    "gpt-4o-2024-11-20",
		Stream:           true,
		Cached:           true,
		Status:           "ok",
		RouteReason:      "alias",
		PromptTokens:     1500,
		CompletionTokens: 250,
		CostCents:        7,
	}
}

// TestRequestMetaWireKeys defends the admin API wire contract: the exact set
// of JSON keys RequestMeta produces. A renamed tag shows up as one missing and
// one unexpected key; a dropped tag (no `json:"..."`) leaks the Go field name.
func TestRequestMetaWireKeys(t *testing.T) {
	b, err := json.Marshal(filledRequestMeta())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}
	want := append([]string(nil), requestMetaWireKeys...)
	sort.Strings(want)
	got := sortedKeys(obj)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wire key set mismatch:\n got: %v\nwant: %v", got, want)
	}
}

// TestRequestMetaMarshalValues pins the encoded values and their JSON types:
// durations and costs as numbers, stream/cached as booleans, timestamps as
// RFC3339 strings, everything else as strings.
func TestRequestMetaMarshalValues(t *testing.T) {
	cases := []struct {
		name string
		meta RequestMeta
		want map[string]any
	}{
		{
			name: "success path",
			meta: filledRequestMeta(),
			want: map[string]any{
				"id":                "req-123",
				"started_at":        "2026-09-23T10:30:00.123456789Z",
				"dur_ms":            float64(1234),
				"provider":          "openai",
				"key":               "primary",
				"model":             "gpt-4o",
				"upstream_model":    "gpt-4o-2024-11-20",
				"stream":            true,
				"cached":            true,
				"status":            "ok",
				"route_reason":      "alias",
				"prompt_tokens":     float64(1500),
				"completion_tokens": float64(250),
				"cost_cents":        float64(7),
				"err":               "",
			},
		},
		{
			name: "error path",
			meta: RequestMeta{
				ID:            "req-err",
				StartedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				DurMillis:     42,
				Provider:      "anthropic",
				Key:           "backup",
				Model:         "claude-sonnet",
				UpstreamModel: "claude-sonnet-4",
				Status:        "error",
				RouteReason:   "failover",
				Err:           "upstream 503",
			},
			want: map[string]any{
				"id":                "req-err",
				"started_at":        "2026-01-02T03:04:05Z",
				"dur_ms":            float64(42),
				"provider":          "anthropic",
				"key":               "backup",
				"model":             "claude-sonnet",
				"upstream_model":    "claude-sonnet-4",
				"stream":            false,
				"cached":            false,
				"status":            "error",
				"route_reason":      "failover",
				"prompt_tokens":     float64(0),
				"completion_tokens": float64(0),
				"cost_cents":        float64(0),
				"err":               "upstream 503",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.meta)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal into map: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("wire payload mismatch:\n got:  %#v\n want: %#v", got, tc.want)
			}
		})
	}
}

// TestRequestMetaRoundTrip checks marshal→unmarshal fidelity. StartedAt is
// built with time.Date in UTC so the value carries no monotonic clock reading
// and no zone offset — DeepEqual on the decoded time is exact.
func TestRequestMetaRoundTrip(t *testing.T) {
	in := filledRequestMeta()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RequestMeta
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch:\n in:  %#v\n out: %#v", in, out)
	}
}

// TestRequestMetaZeroValueJSON pins the zero-value encoding: every wire key is
// present (no `omitempty` anywhere) and zeros encode as 0/false/""/zero-time.
func TestRequestMetaZeroValueJSON(t *testing.T) {
	b, err := json.Marshal(RequestMeta{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"id":"","started_at":"0001-01-01T00:00:00Z","dur_ms":0,"provider":"","key":"","model":"","upstream_model":"","stream":false,"cached":false,"status":"","route_reason":"","prompt_tokens":0,"completion_tokens":0,"cost_cents":0,"err":""}`
	if string(b) != want {
		t.Errorf("zero RequestMeta JSON mismatch:\n got:  %s\n want: %s", b, want)
	}
}

// TestKeyBudgetZeroSemantics: the Key doc contract says zero budgets mean
// unlimited. The observable contract is that zero values round-trip and stay
// distinct from set values.
func TestKeyBudgetZeroSemantics(t *testing.T) {
	t.Run("zero key round trips unchanged", func(t *testing.T) {
		var in Key
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out Key
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !reflect.DeepEqual(in, out) {
			t.Errorf("zero Key round trip mismatch:\n in:  %#v\n out: %#v", in, out)
		}
	})

	zeroJSON, err := json.Marshal(Key{})
	if err != nil {
		t.Fatalf("marshal zero key: %v", err)
	}
	for _, tc := range []struct {
		name string
		key  Key
	}{
		{"rpm", Key{RPM: 60}},
		{"max_requests", Key{MaxRequests: 100}},
		{"window", Key{Window: time.Minute}},
		{"max_concurrent", Key{MaxConcurrent: 4}},
		{"cost_limit_cents", Key{CostLimitCents: 500}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.key)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if bytes.Equal(b, zeroJSON) {
				t.Fatalf("set budget marshals identically to the zero (unlimited) key: %s", b)
			}
			var out Key
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(tc.key, out) {
				t.Errorf("round trip mismatch:\n in:  %#v\n out: %#v", tc.key, out)
			}
		})
	}
}

// TestTargetPricingZeroIsUnknown: the Target doc contract says pricing 0 means
// unknown, reported as $0. Zero pricing round-trips and is distinct from set
// pricing.
func TestTargetPricingZeroIsUnknown(t *testing.T) {
	t.Run("zero pricing round trips unchanged", func(t *testing.T) {
		var in Target
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out Target
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !reflect.DeepEqual(in, out) {
			t.Errorf("zero Target round trip mismatch:\n in:  %#v\n out: %#v", in, out)
		}
	})

	t.Run("set pricing round trips and differs from zero", func(t *testing.T) {
		zero := Target{}
		set := Target{
			Provider:        "openai",
			Model:           "gpt-4o",
			ContextWindow:   128000,
			SupportsTools:   true,
			SupportsVision:  true,
			SupportsStream:  true,
			PriceInputPerM:  2.5,
			PriceOutputPerM: 10,
		}
		zeroJSON, err := json.Marshal(zero)
		if err != nil {
			t.Fatalf("marshal zero: %v", err)
		}
		b, err := json.Marshal(set)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Equal(zeroJSON, b) {
			t.Fatalf("set pricing marshals identically to zero (unknown) pricing")
		}
		var out Target
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !reflect.DeepEqual(set, out) {
			t.Errorf("round trip mismatch:\n in:  %#v\n out: %#v", set, out)
		}
	})
}
