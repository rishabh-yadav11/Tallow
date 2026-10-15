package resilience

import (
	"regexp"
	"strings"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

type Action int

const (
	ActionStop Action = iota
	ActionStopAndCooldown
	ActionContinue
	ActionContinueAndCooldown
)

func (a Action) String() string {
	switch a {
	case ActionStop:
		return "stop"
	case ActionStopAndCooldown:
		return "stop-and-cooldown"
	case ActionContinue:
		return "continue"
	case ActionContinueAndCooldown:
		return "continue-and-cooldown"
	}
	return "unknown"
}

var RetryableStatuses = map[int]bool{
	403: true,
	408: true,
	429: true,
	500: true,
	502: true,
	503: true,
	504: true,
}

type RetryPolicy struct {
	RequestRetryDefault int

	MaxRetryCredentials int

	MaxRetryInterval time.Duration
}

func EffectiveRounds(keyReq, provReq *int, policyDefault int) int {
	for _, v := range []*int{keyReq, provReq} {
		if v != nil && *v >= 0 {
			return *v
		}
	}
	if policyDefault < 0 {
		return 0
	}
	return policyDefault
}

func ruleMatches(r model.ErrorRule, status int, body []byte) bool {
	if r.Status != 0 && r.Status == status {
		return true
	}
	if len(r.Match) > 0 || len(r.MatchRegexp) > 0 {
		text := string(body)
		for _, m := range r.Match {
			if strings.Contains(text, m) {
				return true
			}
		}
		for _, pat := range r.MatchRegexp {
			if re, err := regexp.Compile(pat); err == nil && re.MatchString(text) {
				return true
			}
		}
	}
	return false
}

func actionOf(action string) Action {
	switch action {
	case "stop-and-cooldown":
		return ActionStopAndCooldown
	case "continue":
		return ActionContinue
	case "continue-and-cooldown":
		return ActionContinueAndCooldown
	case "stop":
		return ActionStop
	}
	return ActionStop
}

func Decide(status int, err error, body []byte, rules []model.ErrorRule) Action {
	for _, r := range rules {
		if ruleMatches(r, status, body) {
			return actionOf(r.Action)
		}
	}
	if err != nil && status == 0 {
		return ActionContinueAndCooldown
	}
	if RetryableStatuses[status] {
		return ActionContinueAndCooldown
	}
	return ActionStop
}

type Manager struct {
	*cooldownManager
	Retry RetryPolicy
}

func NewManager(policy RetryPolicy, cooldownFile string, now func() time.Time) *Manager {
	return &Manager{
		cooldownManager: newCooldownManager(cooldownFile, now),
		Retry:           policy,
	}
}

func (m *Manager) IsCooled(provider, key string) bool {
	return m.cooldownManager.IsCooled(provider, key)
}

func (m *Manager) Cool(provider, key string, d time.Duration) {
	m.cooldownManager.Cool(provider, key, d)
}

func (m *Manager) SetCooldownDisabled(provider, key string, disabled bool) {
	m.cooldownManager.SetDisabled(provider, key, disabled)
}

func (m *Manager) Decide(provider, key string, status int, err error, body []byte, rules []model.ErrorRule) Action {
	action := Decide(status, err, body, rules)
	if action == ActionContinueAndCooldown || action == ActionStopAndCooldown {
		d := cooldownFor(status)
		if m.Retry.MaxRetryInterval > 0 && d > m.Retry.MaxRetryInterval {
			d = m.Retry.MaxRetryInterval
		}
		m.Cool(provider, key, d)
	}
	return action
}

func (m *Manager) Excludes() func(provider, key string) bool {
	return func(provider, key string) bool { return m.IsCooled(provider, key) }
}

func (m *Manager) Persist() error { return m.cooldownManager.Persist() }

func (m *Manager) Load() error { return m.cooldownManager.Load() }

func cooldownFor(status int) time.Duration {
	return time.Minute
}
