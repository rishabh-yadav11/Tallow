// Package cache implements the normalized exact-match response cache. The
// cache key is the SHA-256 of the request with volatile fields stripped and
// JSON object keys canonicalized (sorted). Only non-streaming completions are
// cached; streaming repeats are served by provider-native prompt caching via
// sticky routing instead.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// volatileFields are top-level request fields that do not affect the response
// and are stripped before hashing.
var volatileFields = map[string]bool{
	"timestamp":           true,
	"request_id":          true,
	"session_id":          true,
	"metadata":            true,
	"user":                true,
	"stream":              true,  // only non-stream requests reach the cache.
	"n":                   false, // keep: affects response content.
	"temperature":         false,
	"tools":               false,
	"messages":            false,
	"model":               false,
	"max_tokens":          false,
	"stop":                false,
	"top_p":               false,
	"frequency_penalty":   false,
	"presence_penalty":    false,
	"functions":           false,
	"function_call":       false,
	"response_format":     false,
	"seed":                false,
	"tool_choice":         false,
	"parallel_tool_calls": false,
}

// Normalize returns canonical JSON bytes: volatile fields removed, map keys
// sorted (encoding/json sorts map keys deterministically at every level).
func Normalize(req map[string]any) ([]byte, error) {
	m := make(map[string]any, len(req))
	for k, v := range req {
		if volatileFields[k] {
			continue
		}
		m[k] = v
	}
	return json.Marshal(m)
}

// Key hashes canonical bytes to a cache key string.
func Key(canonical []byte) string {
	h := sha256.Sum256(canonical)
	return hex.EncodeToString(h[:])
}

// Entry is a cached response.
type Entry struct {
	Key              string
	Body             []byte // raw JSON response body (non-stream).
	ContentType      string
	Status           int
	Model            string
	PromptTokens     int
	CompletionTokens int
	CreatedAt        time.Time
	expires          time.Time
}

// Cache is a bounded LRU with per-entry TTL. In-process only; no external
// cache service by design. It respects two independent bounds: a maximum entry
// count and (optionally) a total approximate byte budget.
type Cache struct {
	mu       sync.Mutex
	max      int
	ttl      time.Duration
	maxBytes int64 // <= 0 means unbounded.
	bytes    int64 // approximate total bytes currently consumed.
	entries  map[string]*list.Element
	order    *list.List // front = most recently used.
	hit      int64
	miss     int64
	evict    int64
}

// sizeOf approximates the memory an entry consumes in the cache.
const overheadBytes = 256

func entrySize(e Entry) int64 {
	return int64(len(e.Body)) + overheadBytes
}

// New builds an LRU cache with a max entry count, a TTL, and an optional total
// byte budget. A maxBytes <= 0 means the byte budget is unbounded (only the
// entry-count bound applies).
func New(maxEntries int, ttl time.Duration, maxBytes int64) *Cache {
	return &Cache{
		max:      maxEntries,
		ttl:      ttl,
		maxBytes: maxBytes,
		entries:  make(map[string]*list.Element, maxEntries),
		order:    list.New(),
	}
}

type item struct {
	key string
	e   Entry
}

// Get returns a live (unexpired) entry and marks it most-recently-used.
func (c *Cache) Get(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		c.miss++
		return Entry{}, false
	}
	it := el.Value.(*item)
	if time.Now().After(it.e.expires) {
		c.removeLocked(el)
		return Entry{}, false
	}
	c.hit++
	c.order.MoveToFront(el)
	return it.e, true
}

// Set inserts or refreshes an entry, evicting expired/least-recently-used as
// needed to respect both the entry-count and byte budgets.
func (c *Cache) Set(e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepExpiredLocked()
	if el, ok := c.entries[e.Key]; ok {
		old := el.Value.(*item).e
		c.bytes += entrySize(e) - entrySize(old)
		el.Value.(*item).e = e
		c.order.MoveToFront(el)
		c.evictBytesOverLocked()
		return
	}
	e.CreatedAt = time.Now()
	e.expires = e.CreatedAt.Add(c.ttl)
	el := c.order.PushFront(&item{key: e.Key, e: e})
	c.entries[e.Key] = el
	c.bytes += entrySize(e)
	for c.order.Len() > c.max {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.removeLocked(back)
	}
	c.evictBytesOverLocked()
}

// evictBytesOverLocked evicts least-recently-used entries until total bytes are
// within the byte budget (no-op when unbounded).
func (c *Cache) evictBytesOverLocked() {
	if c.maxBytes <= 0 || c.bytes <= c.maxBytes {
		return
	}
	for c.bytes > c.maxBytes {
		back := c.order.Back()
		if back == nil {
			return
		}
		c.removeLocked(back)
	}
}

// Delete removes a key.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.removeLocked(el)
	}
}

// SweepExpired removes expired entries; cheap enough to call periodically.
func (c *Cache) SweepExpired() {
	c.mu.Lock()
	c.sweepExpiredLocked()
	c.mu.Unlock()
}

func (c *Cache) sweepExpiredLocked() {
	now := time.Now()
	for el := c.order.Back(); el != nil; {
		prev := el.Prev()
		if now.After(el.Value.(*item).e.expires) {
			c.removeLocked(el)
		}
		el = prev
	}
}

func (c *Cache) removeLocked(el *list.Element) {
	it := el.Value.(*item)
	c.bytes -= entrySize(it.e)
	if c.bytes < 0 {
		c.bytes = 0
	}
	delete(c.entries, it.key)
	c.order.Remove(el)
	c.evict++
}

// Stats reports hit/miss/evict counters for observability.
func (c *Cache) Stats() (hit, miss, evict, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hit, c.miss, c.evict, int64(c.order.Len())
}
