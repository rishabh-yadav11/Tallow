package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

var volatileFields = map[string]bool{
	"timestamp":           true,
	"request_id":          true,
	"session_id":          true,
	"metadata":            true,
	"user":                true,
	"stream":              true,
	"n":                   false,
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

func Key(canonical []byte) string {
	h := sha256.Sum256(canonical)
	return hex.EncodeToString(h[:])
}

type Entry struct {
	Key              string
	Body             []byte
	ContentType      string
	Status           int
	Model            string
	PromptTokens     int
	CompletionTokens int

	Provider  string
	CreatedAt time.Time
	expires   time.Time
}

type Cache struct {
	mu       sync.Mutex
	max      int
	ttl      time.Duration
	maxBytes int64
	bytes    int64
	entries  map[string]*list.Element
	order    *list.List
	hit      int64
	miss     int64
	evict    int64
}

const overheadBytes = 256

func entrySize(e Entry) int64 {
	return int64(len(e.Body)) + overheadBytes
}

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

func (c *Cache) ExpiresAt(key string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return time.Time{}
	}
	return el.Value.(*item).e.expires
}

func (c *Cache) Set(e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepExpiredLocked()
	e.CreatedAt = time.Now()
	e.expires = e.CreatedAt.Add(c.ttl)
	if el, ok := c.entries[e.Key]; ok {
		old := el.Value.(*item).e
		c.bytes += entrySize(e) - entrySize(old)
		el.Value.(*item).e = e
		c.order.MoveToFront(el)
		c.evictBytesOverLocked()
		return
	}
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

func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.removeLocked(el)
	}
}

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

func (c *Cache) Stats() (hit, miss, evict, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hit, c.miss, c.evict, int64(c.order.Len())
}
