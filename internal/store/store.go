// Package store persists request metadata, raw bodies, errors, rollups, and an
// append-only audit log in SQLite (WAL mode). It implements tiered retention:
// age-based delete per table plus rollup-then-delete for indefinite rollups.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
	_ "modernc.org/sqlite" // pure-Go driver: CGO_ENABLED=0 static binary.
)

// Store wraps the SQLite connection and retention state.
type Store struct {
	db  *sql.DB
	mu  sync.Mutex // serializes writes and maintenance.
	cfg StoreConfig

	queue  chan writeOp // buffered request/audit write queue drained by the worker.
	enqMu  sync.Mutex   // guards closed + channel send against Close.
	closed bool
	wg     sync.WaitGroup // tracks the background writer goroutine.
}

// writeOp is one deferred database write produced by the request path. The
// public record methods enqueue an op and return immediately; a single worker
// goroutine performs the actual Exec under s.mu.
type writeOp struct {
	kind string

	req      model.RequestMeta // RecordRequest
	id       string            // RecordRaw / RecordError
	ts       time.Time         // RecordRaw
	reqBody  []byte            // RecordRaw
	respBody []byte            // RecordRaw

	// RecordError
	provider  string
	key       string
	model     string
	requestID string
	message   string

	// Audit
	action string
	entity string
	detail string

	flushDone chan struct{} // RecordRequest/Raw/Error/Audit leave nil; flushForTest sets it.
}

const (
	opRequest = "request"
	opRaw     = "raw"
	opError   = "error"
	opAudit   = "audit"
	opFlush   = "flush"
)

// StoreConfig carries retention and capture knobs.
type StoreConfig struct {
	Path          string
	RawBodies     bool
	RawBodiesDays int
	MetadataDays  int
	ErrorsDays    int
	RollupEvery   time.Duration
	VacuumEvery   time.Duration
}

const schema = `
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
CREATE INDEX IF NOT EXISTS idx_requests_started ON requests(started_at);
CREATE INDEX IF NOT EXISTS idx_requests_pk ON requests(provider, key);

CREATE TABLE IF NOT EXISTS raw_bodies (
	id           TEXT PRIMARY KEY,
	started_at   INTEGER NOT NULL,
	req_body     TEXT,
	resp_body    TEXT
);
CREATE INDEX IF NOT EXISTS idx_raw_started ON raw_bodies(started_at);

CREATE TABLE IF NOT EXISTS errors (
	id          TEXT PRIMARY KEY,
	started_at  INTEGER NOT NULL,
	provider    TEXT NOT NULL DEFAULT '',
	key         TEXT NOT NULL DEFAULT '',
	model       TEXT NOT NULL DEFAULT '',
	message     TEXT NOT NULL DEFAULT '',
	request_id  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_errors_started ON errors(started_at);

CREATE TABLE IF NOT EXISTS rollups (
	bucket            TEXT NOT NULL,   -- 'day' | 'week'
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

CREATE TABLE IF NOT EXISTS audit_log (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	ts     INTEGER NOT NULL,
	action TEXT NOT NULL,
	entity TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS meta (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);
`

const watermarkKey = "rollup_watermark"

// Open opens (creating if needed) the database and applies the schema in WAL
// mode.
func Open(cfg StoreConfig) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + cfg.Path + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; WAL readers still allowed via separate conns if needed.
	// WAL mode.
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: enable WAL: %w", err)
	}
	if _, err := db.Exec("PRAGMA synchronous=NORMAL;"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	s := &Store{
		db:    db,
		cfg:   cfg,
		queue: make(chan writeOp, 1024),
	}
	s.wg.Add(1)
	go s.worker()
	return s, nil
}

// Close flushes any pending queued writes, stops the writer goroutine, then
// closes the database. It is safe to call once; subsequent calls no-op.
func (s *Store) Close() error {
	s.enqMu.Lock()
	if s.closed {
		s.enqMu.Unlock()
		return nil
	}
	s.closed = true
	close(s.queue)
	s.enqMu.Unlock()

	s.wg.Wait() // worker drains and exits before we touch the db.
	return s.db.Close()
}

// worker is the single background goroutine that performs all deferred writes.
func (s *Store) worker() {
	defer s.wg.Done()
	for op := range s.queue {
		s.mu.Lock()
		switch op.kind {
		case opRequest:
			_, err := s.db.Exec(
				`INSERT INTO requests (id, started_at, dur_ms, provider, key, model, upstream_model, stream, cached, status, route_reason, prompt_tokens, completion_tokens, cost_cents, err)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				op.req.ID, op.req.StartedAt.UnixMilli(), op.req.DurMillis, op.req.Provider, op.req.Key, op.req.Model, op.req.UpstreamModel,
				b2i(op.req.Stream), b2i(op.req.Cached), op.req.Status, op.req.RouteReason, op.req.PromptTokens, op.req.CompletionTokens, op.req.CostCents, op.req.Err,
			)
			_ = err
		case opRaw:
			_, err := s.db.Exec(`INSERT INTO raw_bodies (id, started_at, req_body, resp_body) VALUES (?,?,?,?)`,
				op.id, op.ts.UnixMilli(), string(op.reqBody), string(op.respBody))
			_ = err
		case opError:
			_, err := s.db.Exec(`INSERT INTO errors (id, started_at, provider, key, model, message, request_id) VALUES (?,?,?,?,?,?,?)`,
				op.id, time.Now().UnixMilli(), op.provider, op.key, op.model, op.message, op.requestID)
			_ = err
		case opAudit:
			_, err := s.db.Exec(`INSERT INTO audit_log (ts, action, entity, detail) VALUES (?,?,?,?)`,
				time.Now().UnixMilli(), op.action, op.entity, op.detail)
			_ = err
		}
		s.mu.Unlock()
		if op.flushDone != nil {
			close(op.flushDone)
		}
	}
}

// enqueue adds an op to the worker queue without blocking the caller. If the
// store is closed or the queue is full the op is dropped (best-effort), keeping
// request latency low.
func (s *Store) enqueue(op writeOp) error {
	s.enqMu.Lock()
	if s.closed {
		s.enqMu.Unlock()
		return errStoreClosed
	}
	select {
	case s.queue <- op:
		s.enqMu.Unlock()
		return nil
	default:
		s.enqMu.Unlock()
		return errQueueFull
	}
}

// flushForTest blocks until every op enqueued so far has been written, by
// enqueueing a sentinel after the pending queue. Test-only helper.
func (s *Store) flushForTest() {
	done := make(chan struct{})
	if err := s.enqueue(writeOp{kind: opFlush, flushDone: done}); err != nil {
		return // store closed or queue dropped; nothing to flush reliably.
	}
	<-done
}

var (
	errStoreClosed = fmt.Errorf("store: closed")
	errQueueFull   = fmt.Errorf("store: write queue full")
)

// RecordRequest enqueues a per-request metadata row; the write is performed
// asynchronously by the background worker.
func (s *Store) RecordRequest(m model.RequestMeta) error {
	return s.enqueue(writeOp{kind: opRequest, req: m})
}

// RecordRaw queues request/response bodies (if raw capture is enabled).
func (s *Store) RecordRaw(id string, startedAt time.Time, reqBody, respBody []byte) error {
	if !s.cfg.RawBodies {
		return nil
	}
	return s.enqueue(writeOp{kind: opRaw, id: id, ts: startedAt, reqBody: reqBody, respBody: respBody})
}

// RecordError queues an error row.
func (s *Store) RecordError(id, provider, key, model, requestID, message string) error {
	return s.enqueue(writeOp{kind: opError, id: id, provider: provider, key: key, model: model, requestID: requestID, message: message})
}

// Audit queues an immutable config/key-change entry.
func (s *Store) Audit(action, entity, detail string) error {
	return s.enqueue(writeOp{kind: opAudit, action: action, entity: entity, detail: detail})
}

// ---------- Retention ----------

// Maintain rolls up recent per-request rows into daily/weekly rollups, then
// applies age-based deletes, and occasionally VACUUMs. It is idempotent and
// safe to call concurrently from a single goroutine.
func (s *Store) Maintain(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	if err := s.rollupLocked(ctx, now); err != nil {
		return err
	}
	if err := s.deleteAgedLocked(now); err != nil {
		return err
	}
	return s.maybeVacuumLocked(now)
}

// RunRetention is a blocking loop invoking Maintain on rollupEvery, plus
// vacuum on vacuumEvery.
func (s *Store) RunRetention(ctx context.Context) error {
	tick := s.cfg.RollupEvery
	if tick <= 0 {
		tick = time.Hour
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := s.Maintain(ctx); err != nil {
				// A transient maintenance error must not permanently kill future
				// rollup/delete/vacuum runs; log and continue.
				fmt.Fprintf(os.Stderr, "tallow: retention maintenance failed: %v\n", err)
			}
		}
	}
}

type rollRow struct {
	ts                 int64
	provider, key      string
	prompt, completion int
	cost               int64
	err                bool
}

func (s *Store) rollupLocked(ctx context.Context, now time.Time) error {
	var wm int64
	err := s.db.QueryRowContext(ctx, `SELECT v FROM meta WHERE k=?`, watermarkKey).Scan(&wm)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT started_at, provider, key, prompt_tokens, completion_tokens, cost_cents, status
		 FROM requests WHERE started_at > ? ORDER BY started_at`, wm)
	if err != nil {
		return err
	}
	var batch []rollRow
	for rows.Next() {
		var r rollRow
		var status string
		if err := rows.Scan(&r.ts, &r.provider, &r.key, &r.prompt, &r.completion, &r.cost, &status); err != nil {
			rows.Close()
			return err
		}
		r.err = status == "error"
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	type bucketKey struct {
		bucket string
		start  int64
		prov   string
		key    string
	}
	agg := map[bucketKey]*[5]int64{}
	var maxTS int64 = wm
	for _, r := range batch {
		day := dayStartMs(r.ts)
		week := weekStartMs(r.ts)
		for _, bk := range []bucketKey{{"day", day, r.provider, r.key}, {"week", week, r.provider, r.key}} {
			a := agg[bk]
			if a == nil {
				a = &[5]int64{}
				agg[bk] = a
			}
			a[0]++ // requests
			if r.err {
				a[1]++ // errors
			}
			a[2] += int64(r.prompt)
			a[3] += int64(r.completion)
			a[4] += r.cost
		}
		if r.ts > maxTS {
			maxTS = r.ts
		}
	}

	// Perform the rollup INSERTs and the watermark advance in one transaction so
	// a crash between them cannot re-merge the same range (double counting) on
	// the next run.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for bk, a := range agg {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rollups (bucket, bucket_start, provider, key, requests, errors, prompt_tokens, completion_tokens, cost_cents)
			 VALUES (?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(bucket, bucket_start, provider, key) DO UPDATE SET
			   requests=requests+excluded.requests,
			   errors=errors+excluded.errors,
			   prompt_tokens=prompt_tokens+excluded.prompt_tokens,
			   completion_tokens=completion_tokens+excluded.completion_tokens,
			   cost_cents=cost_cents+excluded.cost_cents`,
			bk.bucket, bk.start, bk.prov, bk.key, a[0], a[1], a[2], a[3], a[4]); err != nil {
			return err
		}
	}

	if maxTS > wm {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta (k, v) VALUES (?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
			watermarkKey, fmt.Sprintf("%d", maxTS)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) deleteAgedLocked(now time.Time) error {
	cut := func(days int) int64 {
		return now.AddDate(0, 0, -days).UnixMilli()
	}
	if _, err := s.db.Exec(`DELETE FROM raw_bodies WHERE started_at < ?`, cut(s.cfg.RawBodiesDays)); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM requests WHERE started_at < ?`, cut(s.cfg.MetadataDays)); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM errors WHERE started_at < ?`, cut(s.cfg.ErrorsDays)); err != nil {
		return err
	}
	return nil
}

func (s *Store) maybeVacuumLocked(now time.Time) error {
	if s.cfg.VacuumEvery <= 0 {
		return nil
	}
	var last int64
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k='last_vacuum'`).Scan(&last)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if now.UnixMilli()-last < s.cfg.VacuumEvery.Milliseconds() {
		return nil
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO meta (k,v) VALUES ('last_vacuum', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, fmt.Sprintf("%d", now.UnixMilli()))
	return err
}

func dayStartMs(ms int64) int64 {
	t := time.UnixMilli(ms).UTC()
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).UnixMilli()
}

func weekStartMs(ms int64) int64 {
	t := time.UnixMilli(ms).UTC()
	// Monday-based ISO week start.
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	start := t.AddDate(0, 0, -(wd - 1))
	y, m, d := start.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).UnixMilli()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- Read helpers (admin API / TUI) ----------

// RecentRequests returns the latest N request-metadata rows (newest first).
func (s *Store) RecentRequests(limit int) ([]model.RequestMeta, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, started_at, dur_ms, provider, key, model, upstream_model, stream, cached, status, route_reason, prompt_tokens, completion_tokens, cost_cents, err
		FROM requests ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RequestMeta
	for rows.Next() {
		var m model.RequestMeta
		var started, stream, cached int64
		if err := rows.Scan(&m.ID, &started, &m.DurMillis, &m.Provider, &m.Key, &m.Model, &m.UpstreamModel, &stream, &cached, &m.Status, &m.RouteReason, &m.PromptTokens, &m.CompletionTokens, &m.CostCents, &m.Err); err != nil {
			return nil, err
		}
		m.StartedAt = time.UnixMilli(started)
		m.Stream = stream != 0
		m.Cached = cached != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// Rollup is one aggregated bucket row for display.
type Rollup struct {
	Bucket      string    `json:"bucket"`
	BucketStart time.Time `json:"bucket_start"`
	Provider    string    `json:"provider"`
	Key         string    `json:"key"`
	Requests    int64     `json:"requests"`
	Errors      int64     `json:"errors"`
	Prompt      int64     `json:"prompt_tokens"`
	Completion  int64     `json:"completion_tokens"`
	CostCents   int64     `json:"cost_cents"`
}

// RecentRollups returns the latest rollup rows.
func (s *Store) RecentRollups(limit int) ([]Rollup, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT bucket, bucket_start, provider, key, requests, errors, prompt_tokens, completion_tokens, cost_cents
		FROM rollups ORDER BY bucket_start DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rollup
	for rows.Next() {
		var r Rollup
		var start int64
		if err := rows.Scan(&r.Bucket, &start, &r.Provider, &r.Key, &r.Requests, &r.Errors, &r.Prompt, &r.Completion, &r.CostCents); err != nil {
			return nil, err
		}
		r.BucketStart = time.UnixMilli(start)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentAudit returns the latest audit-log rows.
func (s *Store) RecentAudit(limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT ts, action, entity, detail FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		if err := rows.Scan(&ts, &e.Action, &e.Entity, &e.Detail); err != nil {
			return nil, err
		}
		e.TS = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AuditEntry is an audit-log row.
type AuditEntry struct {
	TS     time.Time `json:"ts"`
	Action string    `json:"action"`
	Entity string    `json:"entity"`
	Detail string    `json:"detail"`
}
