package store

import (
	"context"
	"errors"
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
		CostMicros:       5,
		Err:              "",
	}

	if err := s.RecordRequest(meta); err != nil {
		t.Fatalf("RecordRequest returned error: %v", err)
	}

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
		g.CompletionTokens != meta.CompletionTokens || g.CostMicros != meta.CostMicros {
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

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

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

	if err := s.RecordRequest(model.RequestMeta{ID: "late"}); err == nil {
		t.Fatal("expected error enqueueing after close")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close should no-op: %v", err)
	}
}

func TestMaintainVacuum(t *testing.T) {
	dir := t.TempDir()
	cfg := StoreConfig{
		Path:          filepath.Join(dir, "tallow.db"),
		RawBodies:     true,
		RawBodiesDays: 5,
		MetadataDays:  30,
		ErrorsDays:    90,
		RollupEvery:   time.Hour,
		VacuumEvery:   0,
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 200; i++ {
		if err := s.RecordRequest(model.RequestMeta{ID: fmt.Sprintf("seed-%d", i), DurMillis: 5}); err != nil {
			t.Fatal(err)
		}
	}
	s.flushForTest()
	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	cfg.VacuumEvery = time.Nanosecond
	s.cfg = cfg
	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain vacuum: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

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

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

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

func TestRetentionZeroKeepsForever(t *testing.T) {
	dir := t.TempDir()
	cfg := StoreConfig{Path: filepath.Join(dir, "tallow.db"), RawBodies: true, RawBodiesDays: 0, MetadataDays: 0, ErrorsDays: 0}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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

	var errCount int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM errors`).Scan(&errCount); err != nil {
		t.Fatalf("query error rows: %v", err)
	}
	if errCount != 1 {
		t.Fatalf("retention 0 must keep error rows forever: got %d error rows, want 1", errCount)
	}
}

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

func TestRecentRequestsDefaultsZeroLimit(t *testing.T) {
	s := newTestStore(t)
	for i := range 120 {
		if err := s.RecordRequest(model.RequestMeta{ID: fmt.Sprintf("r-%d", i), DurMillis: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	s.flushForTest()

	got, err := s.RecentRequests(0)
	if err != nil {
		t.Fatalf("RecentRequests(0): %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("RecentRequests(0) default = %d rows, want 100", len(got))
	}
	got, err = s.RecentRequests(120)
	if err != nil {
		t.Fatalf("RecentRequests(120): %v", err)
	}
	if len(got) != 120 {
		t.Fatalf("RecentRequests(120) = %d rows, want 120", len(got))
	}
}

func TestAuditAndRollupDefaultZeroLimit(t *testing.T) {
	s := newTestStore(t)
	for i := range 5 {
		if err := s.Audit("set", "cfg", fmt.Sprintf("detail-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	s.flushForTest()
	audits, err := s.RecentAudit(0)
	if err != nil {
		t.Fatalf("RecentAudit(0): %v", err)
	}
	if len(audits) != 5 {
		t.Fatalf("RecentAudit(0) = %d rows, want 5", len(audits))
	}
	if audits[0].Detail != "detail-4" {
		t.Errorf("RecentAudit(0) newest-first ordering broke: first detail = %q", audits[0].Detail)
	}

	for i := range 5 {
		if err := s.RecordRequest(model.RequestMeta{ID: fmt.Sprintf("req-%d", i), PromptTokens: 1}); err != nil {
			t.Fatal(err)
		}
	}
	s.flushForTest()
	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	rolls, err := s.RecentRollups(0)
	if err != nil {
		t.Fatalf("RecentRollups(0): %v", err)
	}
	if len(rolls) == 0 {
		t.Fatal("RecentRollups(0) returned no rollups after Maintain")
	}
	for _, r := range rolls {
		if r.Bucket == "day" && r.Requests == 5 {
			return
		}
	}
	t.Errorf("no day rollup with 5 requests found: %+v", rolls)
}

func TestQueueFullDrops(t *testing.T) {
	st := &Store{queue: make(chan writeOp, 1024)}
	for range 1024 {
		st.queue <- writeOp{}
	}
	err := st.enqueue(writeOp{kind: opRequest, req: model.RequestMeta{ID: "overflow"}})
	if err != errQueueFull {
		t.Fatalf("enqueue into full queue returned %v, want errQueueFull", err)
	}
	fails, drops := st.WriteStats()
	if fails != 0 || drops != 1 {
		t.Fatalf("WriteStats after queue-full drop: failures=%d drops=%d, want 0/1", fails, drops)
	}
}

func TestWorkerExecFailureCountsFailures(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.db.Exec(`DROP TABLE requests`); err != nil {
		t.Fatalf("drop requests table: %v", err)
	}
	if err := s.RecordRequest(model.RequestMeta{ID: "doomed"}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	s.flushForTest()
	fails, drops := s.WriteStats()
	if fails != 1 {
		t.Fatalf("WriteStats after failed worker insert: failures=%d, want 1", fails)
	}
	if drops != 0 {
		t.Fatalf("WriteStats after failed worker insert: drops=%d, want 0", drops)
	}
}

func TestRequestByID(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Truncate(time.Millisecond)
	if err := s.RecordRequest(model.RequestMeta{
		ID: "id1", StartedAt: now, DurMillis: 7, Provider: "p1", Key: "k1",
		Model: "m1", UpstreamModel: "m1", Stream: false, Status: "ok",
		PromptTokens: 5, CompletionTokens: 3, CostMicros: 300,
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := s.RecordRaw("id1", now, []byte(`{"model":"m1"}`), []byte(`{"content":"hi"}`)); err != nil {
		t.Fatalf("RecordRaw: %v", err)
	}

	if err := s.RecordRequest(model.RequestMeta{ID: "meta-only", StartedAt: now, DurMillis: 1}); err != nil {
		t.Fatalf("RecordRequest meta-only: %v", err)
	}
	s.flushForTest()

	meta, reqB, respB, err := s.RequestByID("id1")
	if err != nil {
		t.Fatalf("RequestByID(id1): %v", err)
	}
	if meta.ID != "id1" || meta.Provider != "p1" || meta.Model != "m1" ||
		!meta.StartedAt.Equal(now) || meta.CostMicros != 300 {
		t.Errorf("RequestByID metadata = %+v", meta)
	}
	if string(reqB) != `{"model":"m1"}` || string(respB) != `{"content":"hi"}` {
		t.Errorf("raw bodies = %q / %q; want the recorded bytes", reqB, respB)
	}

	if _, _, _, err := s.RequestByID("meta-only"); !errors.Is(err, ErrRawNotCaptured) {
		t.Fatalf("RequestByID(meta-only) err = %v, want ErrRawNotCaptured", err)
	}

	if _, _, _, err := s.RequestByID("nope"); err == nil {
		t.Fatal("RequestByID(unknown) = nil error; want error")
	} else if errors.Is(err, ErrRawNotCaptured) {
		t.Errorf("RequestByID(unknown) reported ErrRawNotCaptured; want unknown-id error")
	}
}
