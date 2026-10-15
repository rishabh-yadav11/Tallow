package resilience

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type cooldownManager struct {
	mu      sync.Mutex
	until   map[string]time.Time
	disable map[string]bool
	file    string
	now     func() time.Time
}

func cid(provider, key string) string { return provider + "/" + key }

func newCooldownManager(file string, now func() time.Time) *cooldownManager {
	if now == nil {
		now = time.Now
	}
	return &cooldownManager{
		until:   map[string]time.Time{},
		disable: map[string]bool{},
		file:    file,
		now:     now,
	}
}

func (c *cooldownManager) Cool(provider, key string, d time.Duration) {
	id := cid(provider, key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disable[id] {
		return
	}
	if d <= 0 {
		delete(c.until, id)
		return
	}
	c.until[id] = c.now().Add(d)
}

func (c *cooldownManager) SetDisabled(provider, key string, disabled bool) {
	id := cid(provider, key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if disabled {
		c.disable[id] = true
	} else {
		delete(c.disable, id)
	}
}

func (c *cooldownManager) IsCooled(provider, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[cid(provider, key)]
	return ok && c.now().Before(until)
}

type snapshot struct {
	FileVersion int              `json:"file_version"`
	SavedAt     int64            `json:"saved_at"`
	Cooldowns   map[string]int64 `json:"cooldowns"`
	Disabled    map[string]bool  `json:"disabled"`
}

func (c *cooldownManager) Persist() error {
	if c.file == "" {
		return nil
	}
	c.mu.Lock()
	now := c.now()

	for id, until := range c.until {
		if !now.Before(until) {
			delete(c.until, id)
		}
	}
	snap := snapshot{
		FileVersion: 1,
		SavedAt:     now.UnixMilli(),
		Cooldowns:   map[string]int64{},
		Disabled:    map[string]bool{},
	}
	for id, until := range c.until {
		snap.Cooldowns[id] = until.UnixMilli()
	}
	for id, v := range c.disable {
		snap.Disabled[id] = v
	}

	b, err := json.Marshal(snap)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return atomicWriteFile(c.file, b, 0o600)
}

func (c *cooldownManager) Load() error {
	if c.file == "" {
		return nil
	}
	raw, err := os.ReadFile(c.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return err
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ms := range snap.Cooldowns {
		if time.UnixMilli(ms).After(now) {
			c.until[id] = time.UnixMilli(ms)
		}
	}
	for id, v := range snap.Disabled {
		c.disable[id] = v
	}
	return nil
}

func atomicWriteFile(path string, b []byte, mode os.FileMode) error {
	dir, _ := splitDir(path)
	tmp, err := os.CreateTemp(dir, ".cooldown-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func splitDir(path string) (string, string) {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i], path[i+1:]
		}
	}
	return "", path
}
