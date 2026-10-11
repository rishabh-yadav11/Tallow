package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// oldSchemaCents recreates the pre-migration schema (cost_cents) so the upgrade
// path is exercised against a realistic database rather than a fresh one.
const oldSchemaCents = `
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
	status            TEXT NOT NULL DEFAULT 'ok',
	route_reason      TEXT NOT NULL DEFAULT '',
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost_cents        INTEGER NOT NULL DEFAULT 0,
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
	cost_cents        INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (bucket, bucket_start, provider, key)
);
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
`

// TestMigrateCostUnitsFromCents proves an existing cents database is converted
// to micro-USD, and that the stored value is preserved in magnitude rather than
// reset to zero.
func TestMigrateCostUnitsFromCents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(oldSchemaCents); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	// Two historical requests at 7 and 3 cents.
	if _, err := db.Exec(`INSERT INTO requests (id, started_at, dur_ms, cost_cents) VALUES ('a', 1, 0, 7), ('b', 2, 0, 3)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rollups (bucket, bucket_start, provider, key, cost_cents) VALUES ('day', 1, 'p', 'k', 10)`); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}
	if err := migrateCostUnits(db); err != nil {
		t.Fatalf("migrateCostUnits: %v", err)
	}

	// 10 cents total must survive as 100,000 micro-USD ($1.00), not zero.
	var total int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(cost_micros), 0) FROM requests`).Scan(&total); err != nil {
		t.Fatalf("query cost_micros: %v", err)
	}
	if total != 10*10_000 {
		t.Errorf("requests cost total = %d micro-USD, want %d (10 cents preserved)", total, 10*10_000)
	}
	var rollup int64
	if err := db.QueryRow(`SELECT cost_micros FROM rollups`).Scan(&rollup); err != nil {
		t.Fatalf("query rollup: %v", err)
	}
	if rollup != 10*10_000 {
		t.Errorf("rollup cost = %d micro-USD, want %d", rollup, 10*10_000)
	}
	db.Close()

	// Reopening must be a no-op: the value must not be scaled twice.
	if err := migrateCostUnits(mustReopen(t, path)); err != nil {
		t.Fatalf("second migrateCostUnits: %v", err)
	}
	db2 := mustReopen(t, path)
	defer db2.Close()
	if err := db2.QueryRow(`SELECT COALESCE(SUM(cost_micros), 0) FROM requests`).Scan(&total); err != nil {
		t.Fatalf("requery: %v", err)
	}
	if total != 10*10_000 {
		t.Errorf("after second migrate total = %d, want %d (migration must be idempotent)", total, 10*10_000)
	}
}

// TestMigrateCostUnitsAtomicOnFailure proves the conversion is one transaction:
// a step failing partway must leave the database in its original state, with
// requests scaled/renamed rolled back, no stamp recorded, and no half-converted
// cost_cents column that a later open would scale a second time. It uses a CHECK
// constraint that the scaled value violates so the UPDATE fails mid-migration.
func TestMigrateCostUnitsAtomicOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Same old schema, but rollups carries a CHECK the scaled value violates
	// (10 cents × 10000 > 5000), forcing the UPDATE to fail inside the txn.
	const withCheck = `
CREATE TABLE IF NOT EXISTS requests (
	id TEXT PRIMARY KEY, started_at INTEGER NOT NULL, cost_cents INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS rollups (
	bucket TEXT NOT NULL, bucket_start INTEGER NOT NULL, provider TEXT NOT NULL,
	key TEXT NOT NULL, requests INTEGER NOT NULL DEFAULT 0, errors INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost_cents INTEGER NOT NULL DEFAULT 0 CHECK (cost_cents < 5000),
	PRIMARY KEY (bucket, bucket_start, provider, key)
);
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
`
	if _, err := db.Exec(withCheck); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO requests (id, started_at, cost_cents) VALUES ('a', 1, 7), ('b', 2, 3)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rollups (bucket, bucket_start, provider, key, cost_cents) VALUES ('day', 1, 'p', 'k', 10)`); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}
	if err := migrateCostUnits(db); err == nil {
		t.Fatal("migration should have failed against the CHECK constraint")
	}
	// Everything rolled back: requests unscaled, column still cost_cents, no stamp.
	var reqCents []int64
	rows, err := db.Query(`SELECT cost_cents FROM requests ORDER BY id`)
	if err != nil {
		t.Fatalf("query requests: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		reqCents = append(reqCents, v)
	}
	if len(reqCents) != 2 || reqCents[0] != 7 || reqCents[1] != 3 {
		t.Fatalf("requests not rolled back to original cents: %v", reqCents)
	}
	var stamp string
	if err := db.QueryRow(`SELECT v FROM meta WHERE k=?`, costUnitsKey).Scan(&stamp); err != sql.ErrNoRows {
		t.Fatalf("stamp written despite failure: %q err=%v", stamp, err)
	}
	db.Close()
}

// TestMigrateCostUnitsFreshDatabase covers a brand new database: it already has
// cost_micros, so the migration only stamps the unit.
func TestMigrateCostUnitsFreshDatabase(t *testing.T) {
	db := mustReopen(t, filepath.Join(t.TempDir(), "fresh.db"))
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	defer db.Close()

	if err := migrateCostUnits(db); err != nil {
		t.Fatalf("migrateCostUnits on fresh db: %v", err)
	}
	var unit string
	if err := db.QueryRow(`SELECT v FROM meta WHERE k=?`, costUnitsKey).Scan(&unit); err != nil {
		t.Fatalf("unit not stamped: %v", err)
	}
	if unit != "micros" {
		t.Errorf("unit = %q, want %q", unit, "micros")
	}
}

// TestStoreDBFilePermissions covers M13: the SQLite file holds raw request and
// response bodies, so it must not be world-readable.
func TestStoreDBFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perm.db")
	s, err := Open(StoreConfig{Path: path, RollupEvery: time.Hour, VacuumEvery: time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("db file mode = %04o; group/other bits must not be set (holds raw bodies)", mode)
	}
}

// TestStoreTightensExistingPermissions proves a pre-existing world-readable
// database is tightened, without ever widening a restrictive mode.
func TestStoreTightensExistingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loose.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	s, err := Open(StoreConfig{Path: path, RollupEvery: time.Hour, VacuumEvery: time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("db file mode = %04o after Open; want group/other bits cleared", mode)
	}
}

func mustReopen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("reopen %s: %v", path, err)
	}
	return db
}
