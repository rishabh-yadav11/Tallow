package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

func TestUpgradeResetsALegacyTimestampWatermark(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy-wm.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := db.Exec(oldSchemaNoSeq); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	legacyWM := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := db.Exec(`INSERT INTO requests (id, started_at, dur_ms, provider, key, prompt_tokens)
		VALUES ('already-rolled', ?, 5, 'p1', 'k1', 100)`, legacyWM); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO rollups (bucket, bucket_start, provider, key, requests, errors, prompt_tokens, completion_tokens, cost_micros)
		VALUES ('day', ?, 'p1', 'k1', 1, 0, 100, 0, 0)`, dayStartMs(legacyWM)); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO meta (k, v) VALUES ('rollup_watermark', ?)`, legacyWM); err != nil {
		t.Fatalf("seed legacy watermark: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	s, err := Open(StoreConfig{Path: path, MetadataDays: 0, ErrorsDays: 0})
	if err != nil {
		t.Fatalf("Open on a database with a legacy watermark: %v", err)
	}
	defer s.Close()

	s.RecordRequest(model.RequestMeta{ID: "post-upgrade", StartedAt: time.Now(), Provider: "p1", Key: "k1", PromptTokens: 7})
	s.Flush()

	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	if got := dayRequests(t, s); got != 2 {
		t.Errorf("day rollup counted %d requests, want 2: an upgraded store with a legacy "+
			"timestamp watermark stopped rolling up entirely", got)
	}
}

func TestLegacyWatermarkDoesNotDoubleCountHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy-dup.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := db.Exec(oldSchemaNoSeq); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	legacyWM := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	for _, id := range []string{"h1", "h2"} {
		if _, err := db.Exec(`INSERT INTO requests (id, started_at, dur_ms, provider, key, prompt_tokens)
			VALUES (?, ?, 5, 'p1', 'k1', 100)`, id, legacyWM); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO rollups (bucket, bucket_start, provider, key, requests, errors, prompt_tokens, completion_tokens, cost_micros)
		VALUES ('day', ?, 'p1', 'k1', 2, 0, 200, 0, 0)`, dayStartMs(legacyWM)); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO meta (k, v) VALUES ('rollup_watermark', ?)`, legacyWM); err != nil {
		t.Fatalf("seed legacy watermark: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	s, err := Open(StoreConfig{Path: path, MetadataDays: 0, ErrorsDays: 0})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var wm int64
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, watermarkKey).Scan(&wm); err != nil {
		t.Fatalf("read re-based watermark: %v", err)
	}
	if wm != 2 {
		t.Errorf("re-based watermark = %d, want 2 (the highest backfilled sequence)", wm)
	}

	s.RecordRequest(model.RequestMeta{ID: "post", StartedAt: time.Now(), Provider: "p1", Key: "k1", PromptTokens: 3})
	s.Flush()

	for i := 0; i < 3; i++ {
		if err := s.Maintain(context.Background()); err != nil {
			t.Fatalf("Maintain %d: %v", i, err)
		}
	}

	if got := dayRequests(t, s); got != 3 {
		t.Errorf("day rollup counted %d requests, want 3: the upgrade re-counted history "+
			"that the legacy rollup had already folded in", got)
	}
}
