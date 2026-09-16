package cache

import (
	"encoding/json"
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func TestNormalizeStripsVolatile(t *testing.T) {
	a := mustParse(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"timestamp":"2020-01-01T00:00:00Z","request_id":"abc-123","session_id":"s1","stream":true}`)
	b := mustParse(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"timestamp":"2024-06-06T00:00:00Z","request_id":"xyz-999","session_id":"s2","stream":false}`)

	na, err := Normalize(a)
	if err != nil {
		t.Fatal(err)
	}
	nb, err := Normalize(b)
	if err != nil {
		t.Fatal(err)
	}
	if Key(na) != Key(nb) {
		t.Fatal("volatile fields should not affect the cache key")
	}
}

func TestNormalizeKeyOrderInsensitive(t *testing.T) {
	a := mustParse(t, `{"temperature":0.3,"messages":[{"role":"user","content":"x"}],"model":"m"}`)
	b := mustParse(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":0.3}`)
	ka, _ := Normalize(a)
	kb, _ := Normalize(b)
	if Key(ka) != Key(kb) {
		t.Fatal("JSON key order must not affect the cache key")
	}
}

func TestNormalizeDistinguishesSemanticFields(t *testing.T) {
	a := mustParse(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":0.1,"tools":[{"type":"function","function":{"name":"f"}}]}`)
	b := mustParse(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":0.9,"tools":[{"type":"function","function":{"name":"f"}}]}`)
	ka, _ := Normalize(a)
	kb, _ := Normalize(b)
	if Key(ka) == Key(kb) {
		t.Fatal("temperature must be part of the cache key")
	}
}

func TestLRUAndTTL(t *testing.T) {
	c := New(2, 10*time.Millisecond, 0) // 0 = unbounded bytes; only count bound.
	c.Set(Entry{Key: "a", Body: []byte("1")})
	c.Set(Entry{Key: "b", Body: []byte("2")})
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should be cached")
	}
	// Insert a third; "b" (LRU) evicted.
	c.Set(Entry{Key: "c", Body: []byte("3")})
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should be evicted (LRU)")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should survive as most-recent")
	}
	// TTL expiry.
	time.Sleep(15 * time.Millisecond)
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should be expired by TTL")
	}
}

func TestByteBudgetEvictsLRU(t *testing.T) {
	// Each entry costs len(Body) + 256. Make the budget small enough that only
	// a couple of entries fit.
	c := New(100, time.Minute, 600) // unbounded count, small byte budget.
	// a: 100-byte body -> 356 bytes. b: 200-byte body -> 456 bytes. Sum 812 > 600.
	c.Set(Entry{Key: "a", Body: make([]byte, 100)})
	c.Set(Entry{Key: "b", Body: make([]byte, 200)})

	// b is most-recent, so a (older/LRU) must be evicted to stay under budget.
	if _, ok := c.Get("a"); ok {
		t.Fatal("a (oldest) should be evicted by byte budget")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b (most recent) should survive")
	}
	if c.bytes > c.maxBytes {
		t.Fatalf("bytes %d exceeded budget %d", c.bytes, c.maxBytes)
	}

	// Insert an entry that alone exceeds the budget; it cannot fit and evicts
	// everything (including itself) to keep total bytes within budget.
	c.Set(Entry{Key: "big", Body: make([]byte, 1000)}) // 1256 > 600
	if c.bytes != 0 {
		t.Fatalf("expected empty cache after oversized insert, bytes=%d", c.bytes)
	}
	if c.order.Len() != 0 {
		t.Fatal("oversized entry should be evicted to respect the byte budget")
	}
}

func TestByteBudgetCountBoundBothApply(t *testing.T) {
	// Both count and byte bounds can hold simultaneously: max count 1 and a
	// byte budget that holds ~2 entries.
	c := New(1, time.Minute, 5000)
	c.Set(Entry{Key: "a", Body: make([]byte, 50)})
	c.Set(Entry{Key: "b", Body: make([]byte, 50)})
	if _, ok := c.Get("a"); ok {
		t.Fatal("count bound should evict oldest a")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should survive")
	}
}

func TestRefreshUpdatesBytes(t *testing.T) {
	c := New(100, time.Minute, 400)
	c.Set(Entry{Key: "a", Body: make([]byte, 50)}) // 306 bytes
	// Refresh the same key with a larger body, pushing it over budget.
	c.Set(Entry{Key: "a", Body: make([]byte, 200)}) // 456 > 400 -> evicted
	if _, ok := c.Get("a"); ok {
		t.Fatal("refreshed oversized entry should be evicted under byte budget")
	}
}

func TestRefreshKeepsEntryLive(t *testing.T) {
	// Regression: refreshing an existing entry must reset its expiry; a set
	// twice key used to be immediately treated as expired (dead) on Get.
	c := New(10, time.Minute, 0)
	c.Set(Entry{Key: "a", Body: []byte("v1")})
	c.Set(Entry{Key: "a", Body: []byte("v2")})
	e, ok := c.Get("a")
	if !ok {
		t.Fatal("refreshed entry should still be retrievable")
	}
	if string(e.Body) != "v2" {
		t.Fatalf("expected refreshed body v2, got %q", e.Body)
	}
}
