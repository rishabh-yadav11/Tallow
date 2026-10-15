// Package catalog implements the opt-in per-provider model catalog fetcher.
//
// A manager holds the fetched model lists for the providers that have opted in
// (Catalog.Enabled). Refreshing is pull-based and driven by the composition
// root's ticker loop; by default nothing is fetched and no network call is made
// until a provider opts in with `catalog` in its config.
package catalog

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

// Manager caches the fetched catalog per provider. It is safe for concurrent
// use: Refresh writes under a lock, and Models reads under the same lock.
type Manager struct {
	client *http.Client
	// fetch overridable for tests.
	fetch func(ctx context.Context, p model.Provider, url string) ([]string, error)

	mu     sync.RWMutex
	cat    map[string][]string
	catHas map[string]bool // distinguishes "empty fetch" from "no fetch yet".
}

// NewManager builds a manager. fetch is wired to the real HTTP GET.
func NewManager(client *http.Client) *Manager {
	m := &Manager{client: client, cat: map[string][]string{}, catHas: map[string]bool{}}
	m.fetch = m.fetchDefault
	return m
}

// fetchDefault GETs the models endpoint and parses the OpenAI-compatible
// response: {"data":[{"id":"..."}]}.
func (m *Manager) fetchDefault(ctx context.Context, p model.Provider, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode}
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(body.Data))
	for _, d := range body.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	return out, nil
}

// HTTPError reports a non-200 catalog response.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string {
	return "catalog: unexpected status " + http.StatusText(e.Status)
}

// Refresh fetches the catalog for every enabled provider, keeping the last
// valid list on fetch failure. It never issues a remote call for a provider whose
// catalog is not enabled (or absent); with no opt-in this is a no-op.
func (m *Manager) Refresh(ctx context.Context, providers []model.Provider) {
	for i := range providers {
		p := providers[i]
		if p.Catalog == nil || !p.Catalog.Enabled {
			continue
		}
		url := p.Catalog.URL
		if url == "" {
			url = trimSlash(p.BaseURL) + "/models"
		}
		models, err := m.fetch(ctx, p, url)
		if err != nil {
			// Refresh failure keeps the last valid catalog; log and move on.
			slog.Warn("catalog refresh failed; keeping last valid catalog",
				"provider", p.Name, "err", err)
			continue
		}
		filtered := filterExcluded(models, p.ExcludedModels)
		m.mu.Lock()
		m.cat[p.Name] = filtered
		m.catHas[p.Name] = true
		m.mu.Unlock()
	}
}

// Models returns the cached catalog models for a provider, and whether any have
// been fetched. The bool distinguishes "provider with an empty catalog" (true)
// from "provider never fetched" (false).
func (m *Manager) Models(provider string) ([]string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.catHas[provider] {
		return nil, false
	}
	return m.cat[provider], true
}

// SetModels seeds the cache directly. It is intended for tests and the app's
// composition root to inject a cached catalog without a network call.
func (m *Manager) SetModels(provider string, models []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cat[provider] = models
	m.catHas[provider] = true
}

// filterExcluded drops models matching any excluded wildcard.
func filterExcluded(models, excluded []string) []string {
	if len(excluded) == 0 {
		return models
	}
	out := make([]string, 0, len(models))
	for _, name := range models {
		if !MatchModel(name, excluded) {
			out = append(out, name)
		}
	}
	return out
}

// MatchModel reports whether the model name matches any of the wildcard
// patterns. A pattern with no `*` is an exact match; otherwise the `*` acts as
// a wildcard: prefix (`gemini-*`), suffix (`*-preview`), or substring
// (`*flash*`) when both ends carry a `*`.
func MatchModel(name string, patterns []string) bool {
	for _, p := range patterns {
		if matchOne(name, p) {
			return true
		}
	}
	return false
}

func matchOne(name, pat string) bool {
	if pat == "" {
		return false
	}
	leadStar := strings.HasPrefix(pat, "*")
	trailStar := strings.HasSuffix(pat, "*") && len(pat) > 1 // bare "*" is not a suffix star
	switch {
	case leadStar && trailStar:
		inner := strings.Trim(pat, "*")
		return strings.Contains(name, inner)
	case leadStar:
		return strings.HasSuffix(name, pat[1:])
	case trailStar:
		return strings.HasPrefix(name, pat[:len(pat)-1])
	default:
		return name == pat
	}
}

func trimSlash(s string) string {
	return strings.TrimRight(s, "/")
}
