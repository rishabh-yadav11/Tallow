package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

const legacyRequestsSchema = `
CREATE TABLE requests (
	id                TEXT PRIMARY KEY,
	started_at        INTEGER NOT NULL,
	dur_ms            INTEGER NOT NULL,
	provider          TEXT NOT NULL DEFAULT '',
	key               TEXT NOT NULL DEFAULT '',
	model             TEXT NOT NULL DEFAULT '',
	upstream_model    TEXT NOT NULL DEFAULT '',
	stream            INTEGER NOT NULL DEFAULT 0,
	cached            INTEGER NOT NULL DEFAULT 0,
	status            TEXT NOT NULL DEFAULT 'ok',
	route_reason      TEXT NOT NULL DEFAULT '',
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost_micros       INTEGER NOT NULL DEFAULT 0,
	err               TEXT NOT NULL DEFAULT ''
);
`

func TestMigrateCoalescedColumnOnLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := raw.Exec(legacyRequestsSchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	oldID := "old-request"
	if _, err := raw.Exec(
		`INSERT INTO requests (id, started_at, dur_ms, provider, key, model, status, cost_micros)
		 VALUES (?,?,?,?,?,?,?,?)`,
		oldID, time.Now().UnixMilli(), 10, "p1", "k1", "gpt-4o", "ok", 42,
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(StoreConfig{Path: path, RollupEvery: time.Hour, VacuumEvery: time.Hour})
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer s.Close()

	rows, err := s.RecentRequests(10)
	if err != nil {
		t.Fatalf("recent requests: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the historical row did not survive the migration: got %d rows", len(rows))
	}
	if rows[0].ID != oldID {
		t.Fatalf("ID = %q, want %q", rows[0].ID, oldID)
	}
	if rows[0].Coalesced {
		t.Fatal("a pre-coalescing row must backfill to coalesced=false")
	}
	if rows[0].CostMicros != 42 {
		t.Fatalf("CostMicros = %d, want 42 (the migration must not disturb spend)", rows[0].CostMicros)
	}

	newID := "new-request"
	if err := s.RecordRequest(model.RequestMeta{
		ID: newID, StartedAt: time.Now(), Provider: "p1", Key: "k1",
		Model: "gpt-4o", Status: "ok", Coalesced: true, CostMicros: 7,
	}); err != nil {
		t.Fatalf("record after migration: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, err = s.RecentRequests(10)
		if err != nil {
			t.Fatalf("recent requests: %v", err)
		}
		if len(rows) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the post-migration row was never written (rows=%d); the coalesced column is not usable",
				len(rows))
		}
		time.Sleep(10 * time.Millisecond)
	}
	var found bool
	for _, r := range rows {
		if r.ID == newID {
			found = true
			if !r.Coalesced {
				t.Fatal("Coalesced did not round-trip through the database")
			}
			if r.CostMicros != 7 {
				t.Fatalf("CostMicros = %d, want 7", r.CostMicros)
			}
		}
	}
	if !found {
		t.Fatal("the post-migration row is missing")
	}
}

func TestMigrateCoalescedColumnIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twice.db")
	for i := 0; i < 3; i++ {
		s, err := Open(StoreConfig{Path: path, RollupEvery: time.Hour, VacuumEvery: time.Hour})
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close #%d: %v", i+1, err)
		}
	}
}

func TestCoalescedSpendIsNotDoubleCountedInRollups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roll.db")
	s, err := Open(StoreConfig{Path: path, RollupEvery: time.Hour, VacuumEvery: time.Hour})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now()

	if err := s.RecordRequest(model.RequestMeta{
		ID: "leader", StartedAt: now, Provider: "p1", Key: "k1", Model: "m",
		Status: "ok", PromptTokens: 5, CompletionTokens: 3, CostMicros: 11,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.RecordRequest(model.RequestMeta{
			ID: fmt.Sprintf("follower-%d", i), StartedAt: now, Provider: "p1", Key: "k1", Model: "m",
			Status: "ok", Coalesced: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, err := s.RecentRequests(10)
		if err != nil {
			t.Fatalf("recent: %v", err)
		}
		if len(rows) == 4 {
			var spend, tokens int64
			for _, r := range rows {
				spend += r.CostMicros
				tokens += int64(r.PromptTokens)
			}
			if spend != 11 {
				t.Fatalf("H8: the four stored rows total %d micro-USD, want 11 (the three followers carried no spend)", spend)
			}
			if tokens != 5 {
				t.Fatalf("H8: the four stored rows total %d prompt tokens, want 5", tokens)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rows never landed: %d", len(rows))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
