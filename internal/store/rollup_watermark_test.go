package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

func dayRequests(t *testing.T, s *Store) int64 {
	t.Helper()
	rolls, err := s.RecentRollups(200)
	if err != nil {
		t.Fatalf("RecentRollups: %v", err)
	}
	var total int64
	for _, r := range rolls {
		if r.Bucket == "day" {
			total += r.Requests
		}
	}
	return total
}

func TestRollupCountsARecordedLateWithAnEarlierTimestamp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	s.RecordRequest(model.RequestMeta{ID: "first", StartedAt: now, Provider: "p1", Key: "k1", PromptTokens: 10})
	s.Flush()
	if err := s.Maintain(ctx); err != nil {
		t.Fatalf("first Maintain: %v", err)
	}

	s.RecordRequest(model.RequestMeta{ID: "late", StartedAt: now.Add(-time.Second), Provider: "p1", Key: "k1", PromptTokens: 999})
	s.Flush()
	if err := s.Maintain(ctx); err != nil {
		t.Fatalf("second Maintain: %v", err)
	}

	if got := dayRequests(t, s); got != 2 {
		t.Errorf("day rollup counted %d requests, want 2: a late-recorded row must not fall behind the watermark", got)
	}
}

func TestRollupAggregatesErrorsAndTokensAcrossRequests(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	s.RecordRequest(model.RequestMeta{ID: "ok1", StartedAt: now, Provider: "p1", Key: "k1", PromptTokens: 100, CompletionTokens: 200, CostMicros: 1000})
	s.RecordRequest(model.RequestMeta{ID: "ok2", StartedAt: now, Provider: "p1", Key: "k1", PromptTokens: 300, CompletionTokens: 500, CostMicros: 50})
	s.RecordRequest(model.RequestMeta{ID: "err1", StartedAt: now.Add(-time.Second), Provider: "p1", Key: "k1", Status: "error", PromptTokens: 10, CompletionTokens: 20, CostMicros: 7})
	s.Flush()
	if err := s.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	rolls, err := s.RecentRollups(200)
	if err != nil {
		t.Fatalf("RecentRollups: %v", err)
	}
	var day *Rollup
	for i := range rolls {
		if rolls[i].Bucket == "day" {
			day = &rolls[i]
		}
	}
	if day == nil {
		t.Fatal("Maintain produced no day rollup")
	}
	if day.Requests != 3 {
		t.Errorf("day requests = %d, want 3", day.Requests)
	}
	if day.Errors != 1 {
		t.Errorf("day errors = %d, want 1 (the err1 row)", day.Errors)
	}
	if day.Prompt != 100+300+10 {
		t.Errorf("day prompt_tokens = %d, want %d", day.Prompt, 100+300+10)
	}
	if day.Completion != 200+500+20 {
		t.Errorf("day completion_tokens = %d, want %d", day.Completion, 200+500+20)
	}
	if day.CostMicros != 1000+50+7 {
		t.Errorf("day cost_micros = %d, want %d", day.CostMicros, 1000+50+7)
	}
}

func TestRollupIsIdempotentAcrossRepeatedRuns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	for i := 0; i < 5; i++ {
		s.RecordRequest(model.RequestMeta{ID: "r" + string(rune('a'+i)), StartedAt: now, Provider: "p1", Key: "k1"})
	}
	s.Flush()

	for i := 0; i < 3; i++ {
		if err := s.Maintain(ctx); err != nil {
			t.Fatalf("Maintain %d: %v", i, err)
		}
	}

	if got := dayRequests(t, s); got != 5 {
		t.Errorf("day rollup counted %d requests after 3 runs, want 5: rollup must not double count", got)
	}
}

func TestRollupWatermarkUnaffectedByBackdatedTimestamps(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	base := now.Add(-365 * 24 * time.Hour)
	for i := 0; i < 10; i++ {
		id := "old-" + string(rune('a'+i))
		s.RecordRequest(model.RequestMeta{ID: id, StartedAt: base.Add(time.Duration(i) * time.Hour), Provider: "p1", Key: "k1"})
	}
	s.Flush()
	if err := s.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if got := dayRequests(t, s); got != 10 {
		t.Errorf("day rollup counted %d, want 10 for backdated rows", got)
	}

	s.RecordRequest(model.RequestMeta{ID: "older", StartedAt: base.Add(-30 * 24 * time.Hour), Provider: "p1", Key: "k1"})
	s.Flush()
	if err := s.Maintain(ctx); err != nil {
		t.Fatalf("second Maintain: %v", err)
	}
	if got := dayRequests(t, s); got != 11 {
		t.Errorf("day rollup counted %d, want 11 after a further-backdated row", got)
	}
}

const oldSchemaNoSeq = `
CREATE TABLE IF NOT EXISTS requests (
	id                TEXT PRIMARY KEY,
	started_at        INTEGER NOT NULL,
	dur_ms            INTEGER NOT NULL,
	provider          TEXT NOT NULL DEFAULT '',
	key               TEXT NOT NULL DEFAULT '',
	model             TEXT NOT NULL DEFAULT '',
	upstream_model    TEXT NOT NULL DEFAULT '',
	stream            INTEGER NOT NULL DEFAULT 0,
	cached            INTEGER NOT NULL DEFAULT 0,
	coalesced         INTEGER NOT NULL DEFAULT 0,
	status            TEXT NOT NULL DEFAULT 'ok',
	route_reason      TEXT NOT NULL DEFAULT '',
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost_micros        INTEGER NOT NULL DEFAULT 0,
	err               TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS rollups (
	bucket            TEXT NOT NULL,
	bucket_start      INTEGER NOT NULL,
	provider          TEXT NOT NULL,
	key               TEXT NOT NULL,
	requests          INTEGER NOT NULL DEFAULT 0,
	errors            INTEGER NOT NULL DEFAULT 0,
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost_micros        INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (bucket, bucket_start, provider, key)
);
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_requests_started ON requests(started_at);
CREATE INDEX IF NOT EXISTS idx_requests_pk ON requests(provider, key);
`

func TestMigrateRequestSeqPreservesEveryRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(oldSchemaNoSeq); err != nil {
		t.Fatalf("create old schema: %v", err)
	}

	seeds := []struct {
		id string
		ts int64
	}{
		{"a", 300}, {"b", 100}, {"c", 400}, {"d", 200}, {"e", 500},
	}
	for _, sd := range seeds {
		if _, err := db.Exec(`INSERT INTO requests (id, started_at, dur_ms, provider, key, prompt_tokens, coalesced)
			VALUES (?,?,?,?,?,?,?)`, sd.id, sd.ts, 5, "p1", "k1", 10, 0); err != nil {
			t.Fatalf("seed %s: %v", sd.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	s, err := Open(StoreConfig{Path: path, MetadataDays: 7, ErrorsDays: 7})
	if err != nil {
		t.Fatalf("Open on a pre-seq database: %v", err)
	}
	defer s.Close()

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(seeds) {
		t.Errorf("row count = %d, want %d: the migration lost rows", n, len(seeds))
	}
	for _, sd := range seeds {
		var got int64
		if err := s.db.QueryRow(`SELECT started_at FROM requests WHERE id=?`, sd.id).Scan(&got); err != nil {
			t.Fatalf("row %s missing after migration: %v", sd.id, err)
		}
		if got != sd.ts {
			t.Errorf("row %s started_at = %d, want %d", sd.id, got, sd.ts)
		}
	}

	rows, err := s.db.Query(`SELECT id FROM requests ORDER BY seq`)
	if err != nil {
		t.Fatalf("order by seq: %v", err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		order = append(order, id)
	}
	want := []string{"a", "b", "c", "d", "e"}
	if len(order) != len(want) {
		t.Fatalf("seq order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("seq order = %v, want insertion order %v", order, want)
			break
		}
	}

	for _, idx := range []string{"idx_requests_started", "idx_requests_pk"} {
		var c int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&c); err != nil {
			t.Fatalf("index lookup %s: %v", idx, err)
		}
		if c != 1 {
			t.Errorf("index %s is missing after the rebuild", idx)
		}
	}

	var leftover int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='requests_old'`).Scan(&leftover); err != nil {
		t.Fatalf("leftover table lookup: %v", err)
	}
	if leftover != 0 {
		t.Error("the migration left requests_old behind: a full unpruned copy of the history")
	}
}

func TestMigrateRequestSeqIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twice.db")
	s, err := Open(StoreConfig{Path: path, MetadataDays: 7})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s.RecordRequest(model.RequestMeta{ID: "x", StartedAt: time.Now(), Provider: "p1", Key: "k1"})
	s.Flush()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for i := 0; i < 3; i++ {
		s, err := Open(StoreConfig{Path: path, MetadataDays: 7})
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("after %d reopens row count = %d, want 1", i+1, n)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("database file missing: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

func TestRetentionDeletesAgedRows(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		Path:          filepath.Join(dir, "ret.db"),
		RawBodies:     true,
		MetadataDays:  7,
		ErrorsDays:    7,
		RawBodiesDays: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	now := time.Now()
	fresh := model.RequestMeta{ID: "fresh", StartedAt: now.Add(-time.Hour), Provider: "p1", Key: "k1"}
	aged := model.RequestMeta{ID: "aged", StartedAt: now.Add(-30 * 24 * time.Hour), Provider: "p1", Key: "k1"}
	s.RecordRequest(fresh)
	s.RecordRequest(aged)
	s.RecordRaw("fresh", fresh.StartedAt, []byte("r"), []byte("s"))
	s.RecordRaw("aged", aged.StartedAt, []byte("r"), []byte("s"))
	s.Flush()

	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE id='fresh'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("a request inside the retention window was deleted")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE id='aged'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a request past the retention window survived")
	}

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_bodies WHERE id='fresh'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("a raw body inside its 1-day window was deleted")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_bodies WHERE id='aged'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a raw body past its 1-day window survived")
	}

	mid := model.RequestMeta{ID: "mid", StartedAt: now.Add(-3 * 24 * time.Hour), Provider: "p1", Key: "k1"}
	s.RecordRequest(mid)
	s.RecordRaw("mid", mid.StartedAt, []byte("r"), []byte("s"))
	s.Flush()
	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_bodies WHERE id='mid'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a raw body past its 1-day window survived because retention used the " +
			"7-day metadata window instead of raw_bodies_days")
	}

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE id='mid'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("a request inside the 7-day metadata window was deleted alongside its raw body")
	}
}

func TestRetentionOfZeroKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		Path:          filepath.Join(dir, "keep.db"),
		RawBodies:     true,
		MetadataDays:  0,
		ErrorsDays:    0,
		RawBodiesDays: 0,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ancient := time.Now().Add(-10 * 365 * 24 * time.Hour)
	s.RecordRequest(model.RequestMeta{ID: "ancient", StartedAt: ancient, Provider: "p1", Key: "k1"})
	s.RecordRaw("ancient", ancient, []byte("r"), []byte("s"))
	s.Flush()

	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	for _, table := range []string{"requests", "raw_bodies"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s: %d rows remain, want 1: a zero retention window must keep everything", table, n)
		}
	}
}

func TestRunRetentionStopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		Path:         filepath.Join(dir, "loop.db"),
		MetadataDays: 7,
		RollupEvery:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.RunRetention(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("RunRetention returned nil, want context.Canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunRetention did not return after the context was cancelled")
	}
}

func TestRunRetentionSurvivesAMaintenanceError(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		Path:         filepath.Join(dir, "err.db"),
		MetadataDays: 7,
		RollupEvery:  5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.RunRetention(ctx) }()

	broken, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open broken handle: %v", err)
	}
	if err := broken.Close(); err != nil {
		t.Fatalf("close broken handle: %v", err)
	}
	s.mu.Lock()
	real := s.db
	s.db = broken
	s.mu.Unlock()

	time.Sleep(50 * time.Millisecond)

	s.mu.Lock()
	s.db = real
	s.mu.Unlock()
	s.RecordRequest(model.RequestMeta{ID: "after-repair", StartedAt: time.Now(), Provider: "p1", Key: "k1"})
	s.Flush()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := dayRequests(t, s); got == 1 {
			break
		} else if time.Now().After(deadline) {
			cancel()
			t.Fatalf("day rollup counted %d requests after repairing the database, want 1: "+
				"RunRetention stopped looping after a maintenance error", got)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("RunRetention returned nil, want context.Canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunRetention did not return after the context was cancelled")
	}
}
