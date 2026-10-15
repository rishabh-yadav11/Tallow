package observ

import (
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

func TestNoProviderFailureIsCountedButNotAttributed(t *testing.T) {
	c := New(true)

	c.Record(meta("openrouter", "ok", 100, 50, false, 700), 120)
	c.Record(meta("", "error", 0, 0, false, 0), 3)

	s := c.Snapshot()

	if s.Total != 2 {
		t.Errorf("Total = %d, want 2; a gateway-level failure must still be counted", s.Total)
	}
	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1; a gateway-level failure is a gateway-level error", s.Errors)
	}

	if _, ok := s.ByProvider[""]; ok {
		t.Errorf("ByProvider contains an empty-name bucket %+v; "+
			"a request no provider served must not be attributed to one", s.ByProvider[""])
	}
	if len(s.ByProvider) != 1 {
		t.Errorf("len(ByProvider) = %d (%v), want 1; only the real provider belongs here",
			len(s.ByProvider), s.ByProvider)
	}

	p := s.ByProvider["openrouter"]
	if p.Requests != 1 || p.Errors != 0 {
		t.Errorf("ByProvider[openrouter] = %+v, want {Requests:1 Errors:0}; "+
			"a gateway failure must not contaminate a healthy provider's error rate", p)
	}
}

func TestCacheHitIsCountedButNotAttributed(t *testing.T) {
	c := New(true)
	c.Record(meta("openrouter", "ok", 100, 50, false, 700), 120)
	c.Record(meta("", "cached", 0, 0, true, 0), 1)

	s := c.Snapshot()
	if s.Total != 2 {
		t.Errorf("Total = %d, want 2; a cache hit is a served request", s.Total)
	}
	if s.Cached != 1 {
		t.Errorf("Cached = %d, want 1; cache effectiveness is the counter that must survive", s.Cached)
	}

	if s.PromptTokens != 100 || s.CompletionTokens != 50 {
		t.Errorf("tokens = %d/%d, want 100/50; a cache hit consumed none",
			s.PromptTokens, s.CompletionTokens)
	}
	if s.CostMicros != 700 {
		t.Errorf("CostMicros = %d, want 700; a cache hit is not charged", s.CostMicros)
	}
	if _, ok := s.ByProvider[""]; ok {
		t.Errorf("ByProvider contains an empty-name bucket %+v from a cache hit", s.ByProvider[""])
	}
	if _, ok := s.ByProvider["cache"]; ok {
		t.Errorf("ByProvider contains a synthetic %q bucket %+v; "+
			"no operator configured a provider called \"cache\"", "cache", s.ByProvider["cache"])
	}
	if len(s.ByProvider) != 1 {
		t.Errorf("len(ByProvider) = %d (%v), want 1; a cache hit belongs to no provider",
			len(s.ByProvider), s.ByProvider)
	}

	p := s.ByProvider["openrouter"]
	if p.Requests != 1 {
		t.Errorf("ByProvider[openrouter].Requests = %d, want 1; "+
			"a cache hit must not add a request to the provider that made it", p.Requests)
	}
	if p.CostMicros != 700 {
		t.Errorf("ByProvider[openrouter].CostMicros = %d, want 700; "+
			"a cache hit must not add cost to the provider", p.CostMicros)
	}
}

func TestGatewayFailuresDoNotAccumulateIntoAnEmptyBucket(t *testing.T) {
	c := New(true)
	for i := 0; i < 500; i++ {
		c.Record(meta("", "error", 0, 0, false, 0), 2)
	}

	s := c.Snapshot()
	if s.Total != 500 || s.Errors != 500 {
		t.Errorf("Total/Errors = %d/%d, want 500/500", s.Total, s.Errors)
	}
	if len(s.ByProvider) != 0 {
		t.Errorf("len(ByProvider) = %d (%v), want 0 after 500 unattributable failures",
			len(s.ByProvider), s.ByProvider)
	}
}

func TestMixedShapesKeepProviderErrorRatesHonest(t *testing.T) {
	c := New(true)

	c.Record(meta("p1", "ok", 10, 5, false, 10), 10)
	c.Record(meta("p1", "ok", 10, 5, false, 10), 10)
	c.Record(meta("p1", "error", 10, 5, false, 0), 10)
	c.Record(meta("p2", "ok", 10, 5, false, 10), 10)
	c.Record(meta("p2", "ok", 10, 5, false, 10), 10)

	for i := 0; i < 4; i++ {
		c.Record(meta("", "error", 0, 0, false, 0), 1)
	}
	for i := 0; i < 2; i++ {
		c.Record(meta("", "cached", 0, 0, true, 0), 1)
	}

	s := c.Snapshot()
	if s.Total != 11 {
		t.Errorf("Total = %d, want 11", s.Total)
	}
	if s.Errors != 5 {
		t.Errorf("Errors = %d, want 5", s.Errors)
	}
	if s.Cached != 2 {
		t.Errorf("Cached = %d, want 2", s.Cached)
	}
	want := map[string]ProviderStat{
		"p1": {Requests: 3, Errors: 1, CostMicros: 20},
		"p2": {Requests: 2, CostMicros: 20},
	}
	if len(s.ByProvider) != len(want) {
		t.Fatalf("len(ByProvider) = %d (%v), want %d", len(s.ByProvider), s.ByProvider, len(want))
	}
	for name, w := range want {
		if got := s.ByProvider[name]; got != w {
			t.Errorf("ByProvider[%q] = %+v, want %+v", name, got, w)
		}
	}

	if s.PromptTokens != 50 || s.CompletionTokens != 25 {
		t.Errorf("tokens = %d/%d, want 50/25", s.PromptTokens, s.CompletionTokens)
	}
	if s.CostMicros != 40 {
		t.Errorf("CostMicros = %d, want 40", s.CostMicros)
	}
}

func TestUnattributedShapesDoNotDistortProviderCost(t *testing.T) {
	c := New(true)

	c.Record(meta("p1", "ok", 1, 1, false, 2), 5)

	for i := 0; i < 1000; i++ {
		c.Record(meta("", "cached", 0, 0, true, 0), 1)
		c.Record(meta("", "error", 0, 0, false, 0), 1)
	}

	s := c.Snapshot()
	if s.CostMicros != 2 {
		t.Errorf("CostMicros = %d, want 2; unattributable traffic must not move the money", s.CostMicros)
	}
	if got := s.ByProvider["p1"].CostMicros; got != 2 {
		t.Errorf("ByProvider[p1].CostMicros = %d, want 2", got)
	}
}

func TestEmptyProviderNameIsTheOnlyUnattributedShape(t *testing.T) {
	c := New(true)
	c.Record(meta("", "ok", 1, 1, false, 1), 5)
	c.Record(meta("some-provider-nobody-configured", "ok", 1, 1, false, 1), 5)

	s := c.Snapshot()
	if _, ok := s.ByProvider[""]; ok {
		t.Error("empty provider name must not be bucketed")
	}
	if _, ok := s.ByProvider["some-provider-nobody-configured"]; !ok {
		t.Error("a named provider must be reported even if the name is unrecognised; " +
			"the collector reports what it is told and must not filter real traffic")
	}
}

func TestCoalescedFollowerShape(t *testing.T) {
	c := New(true)
	leader := model.RequestMeta{
		Provider: "p1", Status: "ok", PromptTokens: 100, CompletionTokens: 50, CostMicros: 700,
	}
	follower := model.RequestMeta{
		Provider: "", Status: "ok", Coalesced: true, RouteReason: "coalesced_provider:p1/key:k1",
	}
	c.Record(leader, 120)
	c.Record(follower, 115)

	s := c.Snapshot()
	if s.Total != 2 {
		t.Errorf("Total = %d, want 2; a coalesced follower is a served request", s.Total)
	}

	if s.PromptTokens != 100 || s.CompletionTokens != 50 {
		t.Errorf("tokens = %d/%d, want 100/50; a follower must not re-count the leader's tokens",
			s.PromptTokens, s.CompletionTokens)
	}
	if s.CostMicros != 700 {
		t.Errorf("CostMicros = %d, want 700; a follower must not re-charge the leader's cost", s.CostMicros)
	}
	if p := s.ByProvider["p1"]; p.Requests != 1 {
		t.Errorf("ByProvider[p1].Requests = %d, want 1; a follower is not a provider call", p.Requests)
	}
	if len(s.ByProvider) != 1 {
		t.Errorf("len(ByProvider) = %d (%v), want 1", len(s.ByProvider), s.ByProvider)
	}
}
