package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		Path:          filepath.Join(dir, "test.db"),
		RawBodies:     true,
		MetadataDays:  7,
		ErrorsDays:    7,
		RawBodiesDays: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAsyncRecordRequestRoundTrip(t *testing.T) {
	s := newTestStore(t)
	meta := model.RequestMeta{
		ID:               "req-1",
		StartedAt:        time.Now().Add(-time.Minute),
		DurMillis:        42,
		Provider:         "openai",
		Key:              "k1",
		Model:            "gpt-4o",
		UpstreamModel:    "gpt-4o",
		Stream:           false,
		Cached:           true,
		Status:           "ok",
		RouteReason:      "primary",
		PromptTokens:     10,
		CompletionTokens: 20,
		CostCents:        5,
		Err:              "",
	}

	if err := s.RecordRequest(meta); err != nil {
		t.Fatalf("RecordRequest returned error: %v", err)
	}
	// Records are async; flush to make the write deterministic.
	s.flushForTest()

	got, err := s.RecentRequests(10)
	if err != nil {
		t.Fatalf("RecentRequests: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 request, got %d", len(got))
	}
	g := got[0]
	if g.ID != meta.ID || g.Provider != meta.Provider || g.Model != meta.Model ||
		g.DurMillis != meta.DurMillis || g.PromptTokens != meta.PromptTokens ||
		g.CompletionTokens != meta.CompletionTokens || g.CostCents != meta.CostCents {
		t.Fatalf("round-trip mismatch: %+v", g)
	}
	if !g.Cached || g.Status != "ok" {
		t.Fatalf("bool/string fields not preserved: %+v", g)
	}
}

func TestAsyncRecordRawAndAudit(t *testing.T) {
	s := newTestStore(t)
	started := time.Now().Add(-2 * time.Minute)
	if err := s.RecordRaw("req-raw", started, []byte(`{"a":1}`), []byte(`{"b":2}`)); err != nil {
		t.Fatalf("RecordRaw: %v", err)
	}
	if err := s.Audit("reload", "config", "/etc/tallow.toml"); err != nil {
		t.Fatalf("Audit: %v", err)
	}
	s.flushForTest()

	aud, err := s.RecentAudit(10)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(aud) != 1 || aud[0].Action != "reload" || aud[0].Entity != "config" {
		t.Fatalf("unexpected audit rows: %+v", aud)
	}
}

func TestAsyncRecordError(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordError("err-1", "openai", "k1", "gpt-4o", "req-1", "boom"); err != nil {
		t.Fatalf("RecordError: %v", err)
	}
	s.flushForTest()

	// Errors are visible via RecentRequests? No: RecentRequests reads requests.
	// Verify the error count by exercising recent endpoints that touch errors.
	// We check indirectly that no write error surfaced by querying the table.
	rows, err := s.db.Query(`SELECT COUNT(*) FROM errors`)
	if err != nil {
		t.Fatalf("count errors: %v", err)
	}
	defer rows.Close()
	rows.Next()
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 error row, got %d", n)
	}
}

func TestRecordRawDisabled(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{Path: filepath.Join(dir, "d.db"), RawBodies: false})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.RecordRaw("x", time.Now(), []byte("req"), []byte("resp")); err != nil {
		t.Fatalf("RecordRaw with disabled capture should succeed: %v", err)
	}
	s.flushForTest()
	rows, err := s.db.Query(`SELECT COUNT(*) FROM raw_bodies`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	rows.Next()
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 raw rows when disabled, got %d", n)
	}
}

func TestCloseFlushesPendingWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flush.db")
	s, err := Open(StoreConfig{Path: path, RawBodies: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	meta := model.RequestMeta{ID: "req-flush", StartedAt: time.Now(), Provider: "p", Model: "m"}
	if err := s.RecordRequest(meta); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	// Do NOT flush; Close must drain the pending write before closing db.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and confirm the queued write landed.
	s2, err := Open(StoreConfig{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.RecentRequests(10)
	if err != nil {
		t.Fatalf("RecentRequests after reopen: %v", err)
	}
	if len(got) != 1 || got[0].ID != "req-flush" {
		t.Fatalf("Close did not flush pending write; got %+v", got)
	}
}

func TestRecordAfterCloseNoPanic(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Enqueueing after close must not panic and should report an error.
	if err := s.RecordRequest(model.RequestMeta{ID: "late"}); err == nil {
		t.Fatal("expected error enqueueing after close")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close should no-op: %v", err)
	}
}

// TestMaintainVacuum exercises the deferred VACUUM path (issue #2): retention
// maintenance must run off the request path and shrink the db without error.
func TestMaintainVacuum(t *testing.T) {
	dir := t.TempDir()
	cfg := StoreConfig{
		Path:          filepath.Join(dir, "tallow.db"),
		RawBodies:     true,
		RawBodiesDays: 5,
		MetadataDays:  30,
		ErrorsDays:    90,
		RollupEvery:   time.Hour,
		VacuumEvery:   0, // force a VACUUM on first Maintain
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Seed enough rows that the db file is non-trivial, then delete most of them
	// so a VACUUM actually reclaims space.
	for i := 0; i < 200; i++ {
		if err := s.RecordRequest(model.RequestMeta{ID: fmt.Sprintf("seed-%d", i), DurMillis: 5}); err != nil {
			t.Fatal(err)
		}
	}
	s.flushForTest()
	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	// Force a second VACUUM now that enough time (simulated) has passed.
	cfg.VacuumEvery = time.Nanosecond
	s.cfg = cfg
	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain vacuum: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDurableAfterReopen verifies async writes are persisted to the SQLite WAL
// and survive Close + reopen (issue #2): a crash-free shutdown must not lose
// request metadata that was enqueued before Close.
func TestDurableAfterReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tallow.db")
	cfg := StoreConfig{Path: dbPath, RawBodies: true, RawBodiesDays: 5, MetadataDays: 30, ErrorsDays: 90}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	for i := 0; i < n; i++ {
		if err := s.RecordRequest(model.RequestMeta{ID: fmt.Sprintf("durable-%d", i), DurMillis: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// Close flushes the queue (worker drains before db.Close).
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and confirm all rows persisted.
	s2, err := Open(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	rows, err := s2.RecentRequests(1000)
	if err != nil {
		t.Fatalf("RecentRequests: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("expected %d durable rows after reopen, got %d", n, len(rows))
	}
}

// TestRetentionZeroKeepsForever verifies that a retention window of 0 is
// interpreted as "keep forever": Maintain must not delete any rows (a natural
// but wrong reading of 0 as "delete everything" previously wiped all data).
func TestRetentionZeroKeepsForever(t *testing.T) {
	dir := t.TempDir()
	cfg := StoreConfig{Path: filepath.Join(dir, "tallow.db"), RawBodies: true, RawBodiesDays: 0, MetadataDays: 0, ErrorsDays: 0}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Old rows that retention would definitely delete if 0 were "keep nothing".
	old := time.Now().AddDate(0, -6, 0)
	if err := s.RecordRequest(model.RequestMeta{ID: "old-req", StartedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRaw("old-raw", old, []byte(`{}`), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordError("err-1", "p", "k", "m", "old-req", "boom"); err != nil {
		t.Fatal(err)
	}
	s.flushForTest()

	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain with zero retention: %v", err)
	}

	rows, err := s.RecentRequests(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("retention 0 must keep rows forever: got %d rows, want 1", len(rows))
	}
}

// TestRetentionPositiveDeletesAgedRows verifies positive windows still delete
// old rows, so the zero-keep-forever change did not break real retention.
func TestRetentionPositiveDeletesAgedRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tallow.db")
	cfg := StoreConfig{Path: dbPath, RawBodies: true, RawBodiesDays: 1, MetadataDays: 1, ErrorsDays: 1}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	old := time.Now().AddDate(0, 0, -30)
	recent := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		id string
		ts time.Time
	}{{"aged-1", old}, {"fresh-1", recent}} {
		if err := s.RecordRequest(model.RequestMeta{ID: tc.id, StartedAt: tc.ts}); err != nil {
			t.Fatal(err)
		}
	}
	s.flushForTest()

	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	rows, err := s.RecentRequests(100)
	if err != nil {
		t.Fatalf("RecentRequests: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "fresh-1" {
		t.Fatalf("positive retention must delete aged rows only; got %d rows: %+v", len(rows), rows)
	}
}

// TestWriteStatsCountsDrops verifies the drop counter records ops lost to a
// closed store, making silent telemetry loss observable (audit H1).
func TestWriteStatsCountsDrops(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.RecordRequest(model.RequestMeta{ID: "after-close"}); err == nil {
		t.Fatal("expected errStoreClosed from RecordRequest after Close")
	}
	fails, drops := s.WriteStats()
	if fails != 0 || drops != 1 {
		t.Fatalf("WriteStats after closed-store write: failures=%d drops=%d, want 0/1", fails, drops)
	}
}
