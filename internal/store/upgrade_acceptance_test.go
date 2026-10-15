package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

func legacyStore(t *testing.T, watermark time.Time, rows int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(oldSchemaNoSeq); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO meta (k, v) VALUES ('cost_units', 'micros')`); err != nil {
		t.Fatalf("seed cost unit stamp: %v", err)
	}

	legacyWM := watermark.UnixMilli()
	if _, err := db.Exec(`INSERT INTO meta (k, v) VALUES ('rollup_watermark', ?)`, legacyWM); err != nil {
		t.Fatalf("seed legacy watermark: %v", err)
	}

	for i := 0; i < rows; i++ {
		if _, err := db.Exec(`INSERT INTO requests (id, started_at, dur_ms, provider, key, prompt_tokens)
			VALUES (?, ?, 5, 'p1', 'k1', 100)`, "legacy-"+strconv.Itoa(i), legacyWM-int64(i)); err != nil {
			t.Fatalf("seed request %d: %v", i, err)
		}
	}

	if rows > 0 {
		if _, err := db.Exec(`INSERT INTO rollups (bucket, bucket_start, provider, key, requests, errors, prompt_tokens, completion_tokens, cost_micros)
			VALUES ('day', ?, 'p1', 'k1', ?, 0, ?, 0, 0)`, dayStartMs(legacyWM), rows, rows*100); err != nil {
			t.Fatalf("seed rollup: %v", err)
		}
	}
	return path
}

func dayTotal(t *testing.T, s *Store) int64 {
	t.Helper()
	rollups, err := s.RecentRollups(200)
	if err != nil {
		t.Fatalf("RecentRollups: %v", err)
	}
	var n int64
	for _, r := range rollups {
		if r.Bucket == "day" {
			n += r.Requests
		}
	}
	return n
}

func weekTotal(t *testing.T, s *Store) int64 {
	t.Helper()
	rollups, err := s.RecentRollups(200)
	if err != nil {
		t.Fatalf("RecentRollups: %v", err)
	}
	var n int64
	for _, r := range rollups {
		if r.Bucket == "week" {
			n += r.Requests
		}
	}
	return n
}

func TestUpgradeFoldsNewRequestsInBothPeriods(t *testing.T) {
	const legacyRows = 5
	path := legacyStore(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), legacyRows)

	s, err := Open(StoreConfig{Path: path, MetadataDays: 0, ErrorsDays: 0})
	if err != nil {
		t.Fatalf("Open on a legacy database: %v", err)
	}
	defer s.Close()

	if err := s.RecordRequest(model.RequestMeta{
		ID: "post-upgrade", StartedAt: time.Now(), DurMillis: 42,
		Provider: "p1", Key: "k1", Model: "m", Status: "ok",
		PromptTokens: 11, CompletionTokens: 22, CostMicros: 3333,
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	s.Flush()
	if err := s.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	if got, want := dayTotal(t, s), int64(legacyRows+1); got != want {
		t.Errorf("day rollup counted %d requests, want %d: after the upgrade the store "+
			"%s", got, want, map[bool]string{
			true:  "stopped folding new requests in",
			false: "replayed rows the legacy scheme had already counted",
		}[got == legacyRows])
	}

	if got, want := weekTotal(t, s), int64(1); got != want {
		t.Errorf("week rollup counted %d requests, want %d", got, want)
	}

	rollups, err := s.RecentRollups(200)
	if err != nil {
		t.Fatal(err)
	}
	var cost int64
	for _, r := range rollups {
		if r.Bucket == "day" {
			cost += r.CostMicros
		}
	}
	if cost != 3333 {
		t.Errorf("day rollup cost is %d micro-USD, want 3333: the upgrade lost or "+
			"rescaled recorded spend", cost)
	}
}

func TestUpgradeThenRestartStaysACompleteCount(t *testing.T) {
	const legacyRows = 3
	path := legacyStore(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), legacyRows)

	for i := 0; i < 5; i++ {
		s, err := Open(StoreConfig{Path: path, MetadataDays: 0, ErrorsDays: 0})
		if err != nil {
			t.Fatalf("Open (lifetime %d): %v", i, err)
		}
		if err := s.RecordRequest(model.RequestMeta{
			ID: "life-" + strconv.Itoa(i), StartedAt: time.Now(),
			Provider: "p1", Key: "k1", Status: "ok", CostMicros: 100,
		}); err != nil {
			t.Fatalf("RecordRequest: %v", err)
		}
		s.Flush()
		if err := s.Maintain(context.Background()); err != nil {
			t.Fatalf("Maintain: %v", err)
		}

		if got, want := dayTotal(t, s), int64(legacyRows+i+1); got != want {
			t.Fatalf("after lifetime %d the day rollup counted %d, want %d", i, got, want)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func TestUpgradePreservesRequestRows(t *testing.T) {
	const legacyRows = 5
	path := legacyStore(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), legacyRows)

	s, err := Open(StoreConfig{Path: path, MetadataDays: 0, ErrorsDays: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.RecentRequests(200)
	if err != nil {
		t.Fatalf("RecentRequests: %v", err)
	}
	if len(got) != legacyRows {
		t.Fatalf("after the upgrade there are %d request rows, want %d: the table "+
			"rebuild lost data", len(got), legacyRows)
	}
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	for i := 0; i < legacyRows; i++ {
		if id := "legacy-" + strconv.Itoa(i); !ids[id] {
			t.Errorf("request %q was lost in the upgrade", id)
		}
	}

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, seq FROM requests ORDER BY seq`)
	if err != nil {
		t.Fatalf("the seq column is missing after the upgrade: %v", err)
	}
	defer rows.Close()
	var lastSeq, n int64
	for rows.Next() {
		var id string
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			t.Fatal(err)
		}
		if seq <= lastSeq {
			t.Errorf("seq %d for %q does not exceed the previous %d: the backfill "+
				"did not preserve insertion order, so the watermark is not monotonic", seq, id, lastSeq)
		}
		lastSeq = seq
		n++
	}
	if n != legacyRows {
		t.Errorf("read %d rows from the migrated table, want %d", n, legacyRows)
	}
}

func TestUpgradeIsIdempotent(t *testing.T) {
	path := legacyStore(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), 3)

	for i := 0; i < 4; i++ {
		s, err := Open(StoreConfig{Path: path, MetadataDays: 0, ErrorsDays: 0})
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if err := s.RecordRequest(model.RequestMeta{
			ID: "run-" + strconv.Itoa(i), StartedAt: time.Now(),
			Provider: "p1", Key: "k1", Status: "ok",
		}); err != nil {
			t.Fatal(err)
		}
		s.Flush()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Open(StoreConfig{Path: path, MetadataDays: 0, ErrorsDays: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.RecentRequests(200)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 7 {
		t.Errorf("after 5 opens there are %d rows, want 7: the migration re-ran on an "+
			"already-upgraded database and disturbed the backfill", len(got))
	}

	var wm int64
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, watermarkKey).Scan(&wm); err != nil {
		t.Fatal(err)
	}
	var maxSeq int64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM requests`).Scan(&maxSeq); err != nil {
		t.Fatal(err)
	}
	if wm > maxSeq {
		t.Errorf("watermark %d is ahead of the highest sequence %d: rows between them "+
			"would be skipped forever", wm, maxSeq)
	}
}

func TestUpgradeWithNoRequestsKeepsFirstRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	s, err := Open(StoreConfig{Path: path, MetadataDays: 0, ErrorsDays: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRequest(model.RequestMeta{
		ID: "first-ever", StartedAt: time.Now(), Provider: "p1", Key: "k1",
		Status: "ok", PromptTokens: 5, CompletionTokens: 7, CostMicros: 100,
	}); err != nil {
		t.Fatal(err)
	}
	s.Flush()
	if err := s.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := dayTotal(t, s), int64(1); got != want {
		t.Errorf("the very first request on a fresh database counted %d, want %d: "+
			"it was skipped before ever being rolled up", got, want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(StoreConfig{Path: path, MetadataDays: 0, ErrorsDays: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := dayTotal(t, s2), int64(1); got != want {
		t.Errorf("after a restart the first request counts as %d, want %d", got, want)
	}
}

func TestStoreFileIsNotWorldReadable(t *testing.T) {
	path := legacyStore(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), 1)

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(StoreConfig{Path: path, MetadataDays: 0, ErrorsDays: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("store file mode is %04o after Open, want the group and other bits "+
			"cleared: the database holds raw request and response bodies", mode)
	}
}
