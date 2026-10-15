package resilience

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

func TestDecideRetryableStatusesDefaultToContinueAndCooldown(t *testing.T) {
	for _, st := range []int{403, 408, 429, 500, 502, 503, 504} {
		if got := Decide(st, nil, nil, nil); got != ActionContinueAndCooldown {
			t.Errorf("Decide(status=%d) = %v, want ContinueAndCooldown", st, got)
		}
	}
}

func TestDecideNonRetryableStatusStops(t *testing.T) {
	for _, st := range []int{400, 401, 404, 409, 422, 500 + 100} {
		if got := Decide(st, nil, nil, nil); got != ActionStop {
			t.Errorf("Decide(status=%d) = %v, want Stop", st, got)
		}
	}
}

func TestDecideTransportErrorRetriesAcrossKeys(t *testing.T) {

	if got := Decide(0, errors.New("dial tcp: connection refused"), nil, nil); got != ActionContinueAndCooldown {
		t.Fatalf("Decide(transport err) = %v, want ContinueAndCooldown", got)
	}
}

func TestDecideErrorRuleByStatus(t *testing.T) {
	rules := []model.ErrorRule{{Status: 503, Action: "stop"}}
	if got := Decide(503, nil, nil, rules); got != ActionStop {
		t.Fatalf("503 with a stop rule = %v, want Stop (a configured rule overrides the default)", got)
	}

	if got := Decide(500, nil, nil, rules); got != ActionContinueAndCooldown {
		t.Fatalf("500 with an unrelated rule = %v, want ContinueAndCooldown", got)
	}
}

func TestDecideErrorRuleBySubstring(t *testing.T) {
	rules := []model.ErrorRule{{Match: []string{"insufficient_quota"}, Action: "stop-and-cooldown"}}
	body := []byte(`{"error":{"message":"insufficient_quota for this account"}}`)
	if got := Decide(429, nil, body, rules); got != ActionStopAndCooldown {
		t.Fatalf("substring rule = %v, want StopAndCooldown", got)
	}
	if got := Decide(429, nil, []byte(`{"error":"rate limited"}`), rules); got != ActionContinueAndCooldown {
		t.Fatalf("non-matching body = %v, want ContinueAndCooldown", got)
	}
}

func TestDecideErrorRuleByRegexp(t *testing.T) {
	rules := []model.ErrorRule{{MatchRegexp: []string{`over\s+(?:daily|monthly)\s+limits`}, Action: "continue"}}
	for _, body := range [][]byte{
		[]byte(`over daily limits for org`),
		[]byte(`you hit over monthly limits`),
	} {
		if got := Decide(429, nil, body, rules); got != ActionContinue {
			t.Errorf("regexp rule body %q = %v, want Continue", body, got)
		}
	}
	if got := Decide(429, nil, []byte(`all good`), rules); got != ActionContinueAndCooldown {
		t.Fatalf("non-matching body = %v, want ContinueAndCooldown", got)
	}
}

func TestDecideFirstMatchingRuleWins(t *testing.T) {
	rules := []model.ErrorRule{
		{Status: 500, Action: "continue"},
		{Status: 500, Action: "stop"},
	}
	if got := Decide(500, nil, nil, rules); got != ActionContinue {
		t.Fatalf("first matching rule should win, got %v", got)
	}
}

func TestDecideUnknownActionCollapsesToStop(t *testing.T) {

	rules := []model.ErrorRule{{Status: 429, Action: "banana"}}
	if got := Decide(429, nil, nil, rules); got != ActionStop {
		t.Fatalf("unknown action = %v, want Stop", got)
	}
}

func TestEffectiveRoundsOverrideChain(t *testing.T) {

	k, p := 2, 5
	if got := EffectiveRounds(&k, &p, 1); got != 2 {
		t.Fatalf("key override: got %d, want 2", got)
	}

	if got := EffectiveRounds(nil, &p, 1); got != 5 {
		t.Fatalf("provider override: got %d, want 5", got)
	}

	if got := EffectiveRounds(nil, nil, 3); got != 3 {
		t.Fatalf("default: got %d, want 3", got)
	}

	zero := 0
	if got := EffectiveRounds(&zero, &p, 3); got != 0 {
		t.Fatalf("explicit 0: got %d, want 0", got)
	}

	neg := -1
	if got := EffectiveRounds(&neg, &k, 4); got != 2 {
		t.Fatalf("negative override should inherit next: got %d, want 2", got)
	}

	neg2 := -5
	if got := EffectiveRounds(&neg2, nil, 7); got != 7 {
		t.Fatalf("negative with no fallback: got %d, want 7", got)
	}
}

func TestManagerCooldownCycle(t *testing.T) {
	now := time.Now()
	m := NewManager(RetryPolicy{}, "", func() time.Time { return now })
	m.Cool("p1", "k1", time.Minute)
	if !m.IsCooled("p1", "k1") {
		t.Fatal("p1/k1 should be on cooldown after Cool")
	}

	now = now.Add(61 * time.Second)
	if m.IsCooled("p1", "k1") {
		t.Fatal("p1/k1 cooldown should have expired after the interval")
	}

	if m.IsCooled("p1", "k2") {
		t.Fatal("p1/k2 must not be cooled by p1/k1's cooldown")
	}
}

func TestManagerExcludes(t *testing.T) {
	m := NewManager(RetryPolicy{}, "", nil)
	m.Cool("p1", "k1", time.Minute)
	ex := m.Excludes()
	if !ex("p1", "k1") {
		t.Fatal("Excludes must report a cooled credential so the router skips it")
	}
	if ex("p1", "k2") {
		t.Fatal("Excludes must leave an uncooled credential routable")
	}
}

func TestManagerDecideAppliesCooldown(t *testing.T) {
	m := NewManager(RetryPolicy{}, "", nil)

	if got := m.Decide("p1", "k1", 429, nil, nil, nil); got != ActionContinueAndCooldown {
		t.Fatalf("Manager.Decide(429) = %v, want ContinueAndCooldown", got)
	}
	if !m.IsCooled("p1", "k1") {
		t.Fatal("Manager.Decide must cooldown the failing credential")
	}

	if got := m.Decide("p2", "k2", 400, nil, nil, nil); got != ActionStop {
		t.Fatalf("Manager.Decide(400) = %v, want Stop", got)
	}
	if m.IsCooled("p2", "k2") {
		t.Fatal("a plain Stop must not cooldown the credential")
	}
}

func TestManagerDisableCoolingOverride(t *testing.T) {
	now := time.Now()
	m := NewManager(RetryPolicy{}, "", func() time.Time { return now })
	m.SetCooldownDisabled("p1", "k1", true)
	m.Cool("p1", "k1", time.Hour)
	if m.IsCooled("p1", "k1") {
		t.Fatal("Cool must be a no-op when disable_cooling is set for the credential")
	}

	m.SetCooldownDisabled("p1", "k1", false)
	m.Cool("p1", "k1", time.Minute)
	if !m.IsCooled("p1", "k1") {
		t.Fatal("cooling should apply once re-enabled")
	}

	if m.IsCooled("p1", "k2") {
		t.Fatal("p1/k2 must not inherit p1/k1's disable override")
	}
}

func TestManagerPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/cooldowns.cds"
	now := time.Now()
	m := NewManager(RetryPolicy{}, file, func() time.Time { return now })
	m.Cool("p1", "k1", time.Minute)
	m.Cool("p1", "k2", time.Millisecond)
	m.SetCooldownDisabled("p2", "k9", true)
	if err := m.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	now = now.Add(2 * time.Millisecond)
	m2 := NewManager(RetryPolicy{}, file, func() time.Time { return now })
	if err := m2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !m2.IsCooled("p1", "k1") {
		t.Fatal("k1 cooldown must survive Persist/Load")
	}
	if m2.IsCooled("p1", "k2") {
		t.Fatal("expired cooldown must be dropped on Load")
	}

	m2.Cool("p2", "k9", time.Hour)
	if m2.IsCooled("p2", "k9") {
		t.Fatal("disabled credential must stay un-cooled after reload")
	}
}

func TestManagerPersistIsNoOpWithoutFile(t *testing.T) {
	m := NewManager(RetryPolicy{}, "", nil)
	if err := m.Persist(); err != nil {
		t.Fatalf("Persist with no file should be a no-op, got %v", err)
	}
	if err := m.Load(); err != nil {
		t.Fatalf("Load with no file should be a no-op, got %v", err)
	}
}

func TestManagerPersistenceFileMode(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/cooldowns.cds"
	m := NewManager(RetryPolicy{}, file, nil)
	m.Cool("p1", "k1", time.Minute)
	if err := m.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cooldown file mode = %o, want 600", got)
	}
	if !strings.Contains(file, ".cds") {
		t.Fatal("test sanity: file should carry .cds suffix")
	}
}
