package routing

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

// ---------- Cooldown ----------

// Cooldown takes a credential ("provider/key") out of rotation for a period
// after a transient upstream error (408/5xx/520-526). It bridges health and
// budget: the breaker only opens after the configured consecutive-failure
// threshold, which is often much larger than one, while a single 429/5xx from a
// misbehaving key should stop sending it traffic for the cooldown window.
//
// It is request-scoped and cheap: entries live only as long as the wait
// period (or until the size cap reclaims them), and Allow is O(1).
type Cooldown struct {
	mu      sync.Mutex
	enabled bool
	offset  time.Duration
	now     func() time.Time
	until   map[string]time.Time // "provider/key" -> time at which it may rotate again.
}

// Add schedules a credential to stay out of rotation for off (bounded by
// cooldownEdge if non-zero), regardless of the global enabled flag so callers
// (e.g. an explicit error-rule action) can force a cooldown. It is a no-op
// when off <= 0.
func (c *Cooldown) Add(id string, off time.Duration) {
	if c == nil || off <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	until := c.now().Add(off)
	if prev, ok := c.until[id]; ok && prev.After(until) {
		return // a longer cooldown is already in place.
	}
	c.until[id] = until
}

// Schedule places id in cooldown for the configured offset. It honours the
// enabled flag: a disabled cooldown does nothing.
func (c *Cooldown) Schedule(id string) {
	if c == nil || !c.enabled {
		return
	}
	c.Add(id, c.offset)
}

// Allow reports whether a credential may be routed to now.
func (c *Cooldown) Allow(id string, now time.Time) bool {
	if c == nil || !c.enabled {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[id]
	if !ok {
		return true
	}
	if !now.Before(until) {
		// The window has elapsed; reclaim the entry rather than keeping it.
		delete(c.until, id)
		return true
	}
	return false
}

// Hot returns a copy of the currently-cooling credentials, for tests and the
// admin view.
func (c *Cooldown) Hot() map[string]time.Time {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]time.Time, len(c.until))
	for k, v := range c.until {
		now := c.now()
		if now.Before(v) {
			out[k] = v
		} else {
			delete(c.until, k)
		}
	}
	return out
}

// coolingBypassed reports whether cooldown scheduling must be skipped for a
// credential that set DisableCooling on its key (or provider). The provider
// form is not currently surfaced on model.Provider, so this reads the key.
func (r *Router) cooldownDisabled(provider, key string) bool {
	p, ok := r.reg.Provider(provider)
	if !ok {
		return false
	}
	for i := range p.Keys {
		if p.Keys[i].ID == key && p.Keys[i].DisableCooling != nil && *p.Keys[i].DisableCooling {
			return true
		}
	}
	return false
}

// ---------- Retry policy ----------

// retryInherit marks "no override" so a zero value can still mean "initial
// round only" per the CLIProxyAPI contract. The config layer uses -1 for this;
// the runtime keeps the same sentinel so a hand-built registry behaves like a
// config that omitted the knob.
const retryInherit = -1

// RetryRounds reports how many UPSTREAM attempts a credential may make,
// resolving the global Routing.RequestRetry default against the provider and
// key overrides:
//
//	key override set (incl. explicit 0) -> key value
//	else provider override set -> provider value
//	else global value (explicit 0 -> initial only)
//	else inherit default (2)
//
// A value of N means N total attempts (the initial round plus N-1 retries), so
// RequestRetry=0 is the initial attempt only, matching the config contract.
func (r *Router) RetryRounds(provider, key string) int {
	// Key override wins.
	if p, ok := r.reg.Provider(provider); ok {
		for i := range p.Keys {
			if p.Keys[i].ID == key && p.Keys[i].RequestRetry != nil {
				return normaliseRounds(*p.Keys[i].RequestRetry)
			}
		}
		// Provider override.
		if p.RequestRetry != nil {
			return normaliseRounds(*p.RequestRetry)
		}
	}
	// Global.
	if r.globalRetry != retryInherit {
		return normaliseRounds(r.globalRetry)
	}
	return 2
}

func normaliseRounds(n int) int {
	if n < 0 {
		return 2
	}
	return n
}

// RetryPolicy is the resolved per-credential retry contract handed to the
// proxy's retry loop: how many attempts a credential allows and how long it
// must wait between rounds (the cooldown offset). Wait is derived from the
// routing cooldown offset and bounded by MaxRetryInterval.
type RetryPolicy struct {
	Provider string
	Key      string
	// Attempts is the total number of upstream attempts permitted.
	Attempts int
	// Wait is the cooldown to sleep between failed rounds before the next
	// attempt; 0 means "retry immediately".
	Wait time.Duration
}

// RetryPolicyFor resolves the retry policy for a single credential. Attempts is
// RetryRounds(); Wait is the cooldown offset unless DisableCooling is set.
func (r *Router) RetryPolicyFor(provider, key string) RetryPolicy {
	rounds := r.RetryRounds(provider, key)
	wait := time.Duration(0)
	if r.cd != nil && r.cd.enabled && !r.cooldownDisabled(provider, key) {
		wait = r.cd.offset
		if r.maxRetryInterval > 0 && wait > r.maxRetryInterval {
			wait = r.maxRetryInterval
		}
	}
	return RetryPolicy{Provider: provider, Key: key, Attempts: rounds, Wait: wait}
}

// ---------- Request-scoped error rules ----------

// RuleAction is the effective action the retry loop must take when an upstream
// error response matches a rule. It is the resolved combination of the matched
// rule's declared action and whether cooldown scheduling is enabled.
type RuleAction string

const (
	ActionStop                RuleAction = "stop"
	ActionStopAndCooldown     RuleAction = "stop-and-cooldown"
	ActionContinue            RuleAction = "continue"
	ActionContinueAndCooldown RuleAction = "continue-and-cooldown"
)

// EvalError applies the request-scoped error rules for a credential (key rules
// override provider rules) to an upstream error response. It returns the action
// the retry loop must take, and whether a cooldown was scheduled.
//
// When no rule matches, the response falls back to the generic retryable-status
// decision: continue is automatic (the loop decides by retryable), and cooldown
// scheduling for 408/5xx happens via CooldownSchedule.
func (r *Router) EvalError(provider, key string, status int, body []byte) (RuleAction, bool) {
	rules := r.errorRulesFor(provider, key)
	for _, rule := range rules {
		if !errorRuleMatches(rule, status, body) {
			continue
		}
		switch rule.Action {
		case "continue":
			return ActionContinue, false
		case "continue-and-cooldown":
			r.scheduleCooldown(provider, key)
			return ActionContinue, true
		case "stop-and-cooldown":
			r.scheduleCooldown(provider, key)
			return ActionStop, true
		default: // stop
			return ActionStop, false
		}
	}
	return "", false
}

func (r *Router) errorRulesFor(provider, key string) []model.ErrorRule {
	p, ok := r.reg.Provider(provider)
	if !ok {
		return nil
	}
	for i := range p.Keys {
		if p.Keys[i].ID == key {
			if len(p.Keys[i].ErrorRules) > 0 {
				return p.Keys[i].ErrorRules
			}
			return p.ErrorRules
		}
	}
	return p.ErrorRules
}

func errorRuleMatches(rule model.ErrorRule, status int, body []byte) bool {
	if rule.Status != 0 && rule.Status != status {
		return false
	}
	if rule.Status == 0 {
		// Rule has no status so it must match on content alone; require both a
		// status on config is not enforced for content-only rules, but one of the
		// content matchers must be present or the rule can never fire.
		if len(rule.Match) == 0 && len(rule.MatchRegexp) == 0 {
			return false
		}
	}
	if len(rule.Match) > 0 {
		text := string(body)
		any := false
		for _, m := range rule.Match {
			if strings.Contains(text, m) {
				any = true
				break
			}
		}
		if !any {
			return false
		}
	}
	if len(rule.MatchRegexp) > 0 {
		re, err := compileRegexps(rule.MatchRegexp)
		if err != nil {
			return false
		}
		if !re.Match(body) {
			return false
		}
	}
	// Status and every supplied matcher line up.
	return true
}

var regexpCache sync.Map // string -> *regexp.Regexp

func compileRegexps(patterns []string) (*regexp.Regexp, error) {
	joined := "(?:" + patterns[0]
	for _, p := range patterns[1:] {
		joined += ")|(?:" + p
	}
	joined += ")"
	if re, ok := regexpCache.Load(joined); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(joined)
	if err != nil {
		return nil, err
	}
	regexpCache.Store(joined, re)
	return re, nil
}

// scheduleCooldown puts a credential in cooldown unless its key disabled
// cooling. It is a helper so both the error rules and the standard cooldown
// path agree on honouring DisableCooling.
func (r *Router) scheduleCooldown(provider, key string) {
	if r.cooldownDisabled(provider, key) {
		return
	}
	r.cooldownFor().Schedule(provider + "/" + key)
}

// transientStatus reports whether an upstream status warrants the standard
// transient-error cooldown (408, 5xx, and the Cloudflare 520-526 range).
func transientStatus(status int) bool {
	if status == http.StatusRequestTimeout || status >= 500 {
		return true
	}
	return status >= 520 && status <= 526
}

// DecideError is the proxy-facing seam for a failed upstream attempt. It
// applies the request-scoped error rules and the standard transient cooldown,
// and reports whether the caller must STOP (a stop/stop-and-cooldown rule
// matched). When it returns false the caller continues to the next credential
// per its own retryable logic; the credential may have been put in cooldown.
func (r *Router) DecideError(provider, key string, status int, body []byte) (stop bool) {
	action, _ := r.EvalError(provider, key, status, body)
	switch action {
	case ActionStop: // stop, and stop-and-cooldown (already scheduled).
		return true
	}
	if transientStatus(status) {
		r.scheduleCooldown(provider, key)
	}
	return false
}

// ---------- Model wildcard matching ----------

// matchModel reports whether a model string matches a wildcard pattern.
//
// The syntax is deliberately simple and orthogonal to glob:
//   - A leading `*` matches any prefix (a `*flash*` requires the token appear);
//   - A trailing `*` matches any suffix (a `gemini-*` matches any prefix);
//   - `*` alone matches everything;
//   - Wrapping `*` (a `*-preview` or `*flash*`) matches any substring.
//
// A pattern with no `*` is an exact match. `/` is not treated specially, so a
// provider-qualified model like "org/gpt" is matched as a plain string. This is
// used to test a specific upstream model against both the provider and alias
// ExcludedModels lists, which a naive strings.Contains (or a glob expecting
// path separators) would not do correctly.
func matchModel(pattern, model string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == model
	}
	// Split on '*' and require each segment to appear in order. This gives
	// prefix (*x), suffix (x*), substring (*x*), and mixed (a*b) semantics.
	parts := strings.Split(pattern, "*")
	rest := model
	for i, part := range parts {
		if part == "" {
			if i == 0 || i == len(parts)-1 {
				continue // leading or trailing wildcard.
			}
			// "**" collapses to a single wildcard.
			continue
		}
		idx := strings.Index(rest, part)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(part):]
	}
	return true
}

// modelExcluded reports whether model is hidden by any pattern in exclusions.
func modelExcluded(model string, exclusions []string) bool {
	if len(exclusions) == 0 {
		return false
	}
	for _, p := range exclusions {
		if matchModel(p, model) {
			return true
		}
	}
	return false
}

// aliasModelExcluded folds both the provider and alias exclusion lists for a
// target: a model is not routable through a provider when either list hides it.
func (r *Router) aliasModelExcluded(provider, model string, aliasExcluded []string) bool {
	if modelExcluded(model, aliasExcluded) {
		return true
	}
	if p, ok := r.reg.Provider(provider); ok && modelExcluded(model, p.ExcludedModels) {
		return true
	}
	return false
}

// affinityTTL returns the session-affinity TTL when enabled, else 0.
func (r *Router) affinityTTL() time.Duration {
	if !r.affinity.enabled {
		return 0
	}
	return r.affinity.ttl
}

// AffinityForChild propagates a parent session's pin to a child session when
// session-affinity-subagents is enabled (used by the app to relay a subagent's
// request onto the parent's credential for prompt-cache reuse). It is a no-op
// otherwise.
func (r *Router) AffinityForChild(parent, child string) {
	if !r.affinity.subagents {
		return
	}
	r.sticky.Inherit(parent, child)
}

// ---------- Session affinity ----------

// sessionAffinity carries the §3 session-affinity knobs layered over the
// existing bare-sticky store. Bare-sticky pins a session to the first
// credential that served it (for prompt-cache reuse). Full session affinity
// additionally bounds the pin's lifetime with an explicit TTL, carries pins
// across subagent requests, and controls when a session may rebind.
type sessionAffinity struct {
	enabled   bool
	ttl       time.Duration
	subagents bool
	rebindOn  string // "any" (default) or "quota".
}
