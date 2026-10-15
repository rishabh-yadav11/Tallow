package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const maxModelsBody = 4 << 20

type Fetcher struct {
	name    string
	baseURL string
	client  *http.Client
	enabled bool

	mu      sync.RWMutex
	models  []string
	lastErr error
}

func NewFetcher(name, baseURL string, client *http.Client, enabled bool) *Fetcher {
	if client == nil {
		client = http.DefaultClient
	}
	return &Fetcher{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  client,
		enabled: enabled,
	}
}

func (f *Fetcher) Refresh() error {
	if !f.enabled {
		return nil
	}
	models, err := f.fetch()
	if err != nil {
		f.mu.Lock()
		f.lastErr = err
		f.mu.Unlock()
		return err
	}
	f.mu.Lock()
	f.models = models
	f.lastErr = nil
	f.mu.Unlock()
	return nil
}

func (f *Fetcher) Models() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if len(f.models) == 0 {
		return nil
	}
	out := make([]string, len(f.models))
	copy(out, f.models)
	return out
}

func (f *Fetcher) LastError() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastErr
}

func (f *Fetcher) Enabled() bool { return f.enabled }

func (f *Fetcher) fetch() ([]string, error) {
	url := f.baseURL + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: build request: %w", f.name, err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: fetch %s: %w", f.name, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
	if err != nil {
		return nil, fmt.Errorf("catalog %s: read body: %w", f.name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog %s: %s returned status %d", f.name, url, resp.StatusCode)
	}
	models, err := parseModels(body)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: %w", f.name, err)
	}
	return models, nil
}

func parseModels(body []byte) ([]string, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("empty /models response")
	}
	switch trimmed[0] {
	case '{':
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, fmt.Errorf("invalid object /models payload: %w", err)
		}
		return cleanIDs(list.Data), nil
	case '[':
		var strs []string
		if err := json.Unmarshal(trimmed, &strs); err != nil {
			return nil, fmt.Errorf("invalid array /models payload: %w", err)
		}
		return cleanStrings(strs), nil
	default:
		return nil, fmt.Errorf("unexpected /models payload starting with %q", rune(trimmed[0]))
	}
}

func cleanIDs(items []struct {
	ID string `json:"id"`
}) []string {
	models := make([]string, 0, len(items))
	for _, m := range items {
		if id := strings.TrimSpace(m.ID); id != "" {
			models = append(models, id)
		}
	}
	return models
}

func cleanStrings(items []string) []string {
	models := make([]string, 0, len(items))
	for _, s := range items {
		if v := strings.TrimSpace(s); v != "" {
			models = append(models, v)
		}
	}
	return models
}
