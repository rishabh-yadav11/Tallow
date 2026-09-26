package observ

import (
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

// The audit's Test Quality section said it precisely: "Observ tests never feed
// the two RequestMeta shapes the proxy actually emits ("" and "cache") -
// precisely why M7 and M9 shipped green."
//
// That is the systemic root cause #5 again, in a new costume. Every fixture in
// this package names a real provider, so the collector was only ever exercised
// on its happy path. Both defects live entirely in the shapes where a provider
// did NOT serve the request, which is exactly the region the fixtures never
// reached. A package at 100% statement coverage can still be blind to this.
//
// So these tests are written from the proxy's actual emissions rather than from
// the collector's API. Each names the RequestMeta the proxy really produces and
// asserts what an operator would read in /metrics.

// TestNoProviderFailureIsCountedButNotAttributed is the M9 shape. A request
// that never selected a provider - an unroutable alias, a routing failure
// before a target was chosen - reaches Record with an empty Provider and
// status "error".
//
// The gateway-level failure must be visible in the totals it belongs to, and
// invisible in the per-provider view. Bucketting it would invent an eleventh
// provider that no operator configured, and that bucket's error rate would sit
// beside the real ones, diluting exactly the signal an operator reads when
// routing is broken.
func TestNoProviderFailureIsCountedButNotAttributed(t *testing.T) {
	c := New(true)
	// One real provider serving normally, one gateway-level failure.
	c.Record(meta("openrouter", "ok", 100, 50, false, 700), 120)
	c.Record(meta("", "error", 0, 0, false, 0), 3)

	s := c.Snapshot()
	// The failure is counted, not swallowed.
	if s.Total != 2 {
		t.Errorf("Total = %d, want 2; a gateway-level failure must still be counted", s.Total)
	}
	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1; a gateway-level failure is a gateway-level error", s.Errors)
	}
	// And it is attributed to nobody.
	if _, ok := s.ByProvider[""]; ok {
		t.Errorf("ByProvider contains an empty-name bucket %+v; "+
			"a request no provider served must not be attributed to one", s.ByProvider[""])
	}
	if len(s.ByProvider) != 1 {
		t.Errorf("len(ByProvider) = %d (%v), want 1; only the real provider belongs here",
			len(s.ByProvider), s.ByProvider)
	}
	// The real provider's numbers are untouched by the gateway failure, which
	// is the point: a routing outage must not look like provider errors.
	p := s.ByProvider["openrouter"]
	if p.Requests != 1 || p.Errors != 0 {
		t.Errorf("ByProvider[openrouter] = %+v, want {Requests:1 Errors:0}; "+
			"a gateway failure must not contaminate a healthy provider's error rate", p)
	}
}

// TestCacheHitIsCountedButNotAttributed is the M7 shape. After the fix, a cache
// hit arrives with an empty Provider, status "cached", and Cached set - it did
// not come from a provider, so crediting one would report spend and calls that
// never happened.
//
// The cache-effectiveness signal must survive that: Cached counts them, and the
// global total includes them. Only the provider attribution is withheld.
func TestCacheHitIsCountedButNotAttributed(t *testing.T) {
	c := New(true)
	c.Record(meta("openrouter", "ok", 100, 50, false, 700), 120) // real call
	c.Record(meta("", "cached", 0, 0, true, 0), 1)               // served from cache

	s := c.Snapshot()
	if s.Total != 2 {
		t.Errorf("Total = %d, want 2; a cache hit is a served request", s.Total)
	}
	if s.Cached != 1 {
		t.Errorf("Cached = %d, want 1; cache effectiveness is the counter that must survive", s.Cached)
	}
	// A cache hit consumed no provider tokens and no provider money, so the
	// aggregates must not grow. If they did, the totals would report spend that
	// never happened and grow as the cache warmed.
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
	// The provider that originally served the answer is not re-credited for it.
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

// TestGatewayFailuresDoNotAccumulateIntoAnEmptyBucket is the M9 mutation guard
// across repeats. The defect was a PERMANENT phantom bucket that grew for the
// life of the process, so a single occurrence is not the interesting case. Many
// must still leave the map empty while the totals keep climbing.
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

// TestMixedShapesKeepProviderErrorRatesHonest composes the shapes the way
// traffic actually arrives, and asserts the thing an operator relies on: a
// provider's error rate reflects that provider's failures and nothing else.
//
// This is the assertion that was impossible to write before, because the
// fixtures only ever produced one shape.
func TestMixedShapesKeepProviderErrorRatesHonest(t *testing.T) {
	c := New(true)
	// p1: 3 requests, 1 of them a real failure. p2: 2 requests, 0 failures.
	c.Record(meta("p1", "ok", 10, 5, false, 10), 10)
	c.Record(meta("p1", "ok", 10, 5, false, 10), 10)
	c.Record(meta("p1", "error", 10, 5, false, 0), 10)
	c.Record(meta("p2", "ok", 10, 5, false, 10), 10)
	c.Record(meta("p2", "ok", 10, 5, false, 10), 10)
	// 4 gateway-level failures, 2 cache hits. Neither belongs to a provider.
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
	if s.Errors != 5 { // 1 real + 4 gateway
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
	// Tokens and cost sum only what providers actually consumed: 5 real calls
	// at 10/5 tokens, 20 micro-USD each. Cache hits and gateway failures add
	// nothing to either.
	if s.PromptTokens != 50 || s.CompletionTokens != 25 {
		t.Errorf("tokens = %d/%d, want 50/25", s.PromptTokens, s.CompletionTokens)
	}
	if s.CostMicros != 40 {
		t.Errorf("CostMicros = %d, want 40", s.CostMicros)
	}
}

// TestUnattributedShapesDoNotDistortProviderCost is the cost half of the same
// guarantee, stated on its own because C3 was a unit-of-measure defect in this
// exact aggregation: an unbanked or over-banked micro-USD total here is what
// makes a cost budget either never fire or fire early.
func TestUnattributedShapesDoNotDistortProviderCost(t *testing.T) {
	c := New(true)
	// A realistic sub-cent call: 1 in / 1 out at $1/M is 1 micro-USD in and out.
	c.Record(meta("p1", "ok", 1, 1, false, 2), 5)
	// A thousand cache hits and gateway failures must not move the total at all.
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

// TestEmptyProviderNameIsTheOnlyUnattributedShape documents the boundary. Any
// non-empty name IS a provider bucket, including one nobody configured - that
// is the collector's job, to report what it was told. The distinction is
// meaningful: the collector does not second-guess provider names, it only
// refuses to invent one. This guards against a future "fix" that starts
// filtering unknown provider names and silently drops real traffic.
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

// TestCoalescedFollowerShape covers the third proxy-only shape. H8 made a
// coalesced follower a first-class request: it has no provider attribution
// because it issued no upstream call, and it reports no tokens or cost, while
// still being counted as a served request.
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
	// The follower issued no upstream call, so it must not re-report the
	// leader's spend - otherwise the store's rollups equal the money actually
	// spent times the number of callers that shared it.
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
