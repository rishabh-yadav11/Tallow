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
	c := New(2, 10*time.Millisecond, 0)
	c.Set(Entry{Key: "a", Body: []byte("1")})
	c.Set(Entry{Key: "b", Body: []byte("2")})
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should be cached")
	}

	c.Set(Entry{Key: "c", Body: []byte("3")})
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should be evicted (LRU)")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should survive as most-recent")
	}

	time.Sleep(15 * time.Millisecond)
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should be expired by TTL")
	}
}

func TestByteBudgetEvictsLRU(t *testing.T) {

	c := New(100, time.Minute, 600)

	c.Set(Entry{Key: "a", Body: make([]byte, 100)})
	c.Set(Entry{Key: "b", Body: make([]byte, 200)})

	if _, ok := c.Get("a"); ok {
		t.Fatal("a (oldest) should be evicted by byte budget")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b (most recent) should survive")
	}
	if c.bytes > c.maxBytes {
		t.Fatalf("bytes %d exceeded budget %d", c.bytes, c.maxBytes)
	}

	c.Set(Entry{Key: "big", Body: make([]byte, 1000)})
	if c.bytes != 0 {
		t.Fatalf("expected empty cache after oversized insert, bytes=%d", c.bytes)
	}
	if c.order.Len() != 0 {
		t.Fatal("oversized entry should be evicted to respect the byte budget")
	}
}

func TestByteBudgetCountBoundBothApply(t *testing.T) {

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
	c.Set(Entry{Key: "a", Body: make([]byte, 50)})

	c.Set(Entry{Key: "a", Body: make([]byte, 200)})
	if _, ok := c.Get("a"); ok {
		t.Fatal("refreshed oversized entry should be evicted under byte budget")
	}
}

func TestRefreshKeepsEntryLive(t *testing.T) {

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

func TestZeroEntriesSelfEvictsImmediately(t *testing.T) {
	c := New(0, time.Minute, 0)
	c.Set(Entry{Key: "a", Body: []byte("1")})
	if c.order.Len() != 0 {
		t.Fatalf("a zero-entry cache retained %d entries: maxEntries=0 must mean "+
			"nothing can be cached", c.order.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("a zero-entry cache served an entry it must have evicted")
	}
	hit, miss, _, size := c.Stats()
	if hit != 0 || size != 0 {
		t.Fatalf("zero-entry cache: hit=%d miss=%d size=%d, want a cache that can "+
			"never hold or serve anything", hit, miss, size)
	}
}

func TestNonPositiveTTLMakesEverySetExpired(t *testing.T) {

	c := New(64, -1*time.Second, 0)
	c.Set(Entry{Key: "a", Body: []byte("1")})

	c.Get("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("a non-positive TTL must make Set produce an already-expired entry")
	}
	hit, miss, _, _ := c.Stats()
	if hit != 0 {
		t.Fatalf("non-positive TTL: hit=%d, want an entry that can never hit", hit)
	}
	if miss == 0 {
		t.Fatalf("non-positive TTL: miss=%d after the second Get, want the "+
			"dead-on-arrival entry to register as a miss", miss)
	}
}
