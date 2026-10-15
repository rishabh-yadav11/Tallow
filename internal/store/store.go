package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	cfg StoreConfig

	queue  chan writeOp
	enqMu  sync.Mutex
	closed bool
	wg     sync.WaitGroup

	writeFailures atomic.Uint64
	writeDrops    atomic.Uint64
}

func (s *Store) WriteStats() (failures, drops uint64) {
	return s.writeFailures.Load(), s.writeDrops.Load()
}

type writeOp struct {
	kind string

	req      model.RequestMeta
	id       string
	ts       time.Time
	reqBody  []byte
	respBody []byte

	provider  string
	key       string
	model     string
	requestID string
	message   string

	action string
	entity string
	detail string

	flushDone chan struct{}
}

const (
	opRequest = "request"
	opRaw     = "raw"
	opError   = "error"
	opAudit   = "audit"
	opFlush   = "flush"
)

type StoreConfig struct {
	Path          string
	RawBodies     bool
	RawBodiesDays int
	MetadataDays  int
	ErrorsDays    int
	RollupEvery   time.Duration
	VacuumEvery   time.Duration
}

const createRequestsTable = `CREATE TABLE IF NOT EXISTS requests (
	seq               INTEGER PRIMARY KEY AUTOINCREMENT,
	id                TEXT NOT NULL UNIQUE,
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
)`

const schema = `
` + createRequestsTable + `;
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
	cost_micros        INTEGER NOT NULL DEFAULT 0,
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

const costUnitsKey = "cost_units"

func Open(cfg StoreConfig) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, err
	}

	if fi, err := os.Stat(cfg.Path); os.IsNotExist(err) {
		if fh, err := os.OpenFile(cfg.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			return nil, err
		} else if err := fh.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if fi != nil {

		if mode := fi.Mode().Perm(); mode&0o077 != 0 {
			if err := os.Chmod(cfg.Path, mode&^0o077); err != nil {
				return nil, err
			}
		}
	}
	dsn := "file:" + cfg.Path + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

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
	if err := migrateCostUnits(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateCoalescedColumn(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateRequestSeq(db); err != nil {
		db.Close()
		return nil, err
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

func (s *Store) Close() error {
	s.enqMu.Lock()
	if s.closed {
		s.enqMu.Unlock()
		return nil
	}
	s.closed = true
	close(s.queue)
	s.enqMu.Unlock()

	s.wg.Wait()
	return s.db.Close()
}

func migrateCostUnits(db *sql.DB) error {
	var unit string
	err := db.QueryRow(`SELECT v FROM meta WHERE k=?`, costUnitsKey).Scan(&unit)
	if err == nil {
		if unit == "micros" {
			return nil
		}
		return fmt.Errorf("store: unknown cost unit %q in database", unit)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: read cost units: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin cost units: %w", err)
	}
	defer tx.Rollback()

	for _, table := range []string{"requests", "rollups"} {
		var n int

		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name='cost_cents'`, table).Scan(&n); err != nil {
			return fmt.Errorf("store: inspect %s: %w", table, err)
		}
		if n == 0 {
			continue
		}

		if _, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET cost_cents = cost_cents * 10000`, table)); err != nil {
			return fmt.Errorf("store: convert %s cost: %w", table, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN cost_cents TO cost_micros`, table)); err != nil {
			return fmt.Errorf("store: rename %s cost column: %w", table, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta (k, v) VALUES (?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		costUnitsKey, "micros"); err != nil {
		return fmt.Errorf("store: record cost units: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit cost units: %w", err)
	}
	return nil
}

func migrateCoalescedColumn(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests') WHERE name='coalesced'`).Scan(&n); err != nil {
		return fmt.Errorf("store: inspect requests: %w", err)
	}
	if n > 0 {
		return nil
	}

	if _, err := db.Exec(`ALTER TABLE requests ADD COLUMN coalesced INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("store: add requests.coalesced: %w", err)
	}
	return nil
}

func migrateRequestSeq(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests') WHERE name='seq'`).Scan(&n); err != nil {
		return fmt.Errorf("store: inspect requests.seq: %w", err)
	}
	if n > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin seq migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`ALTER TABLE requests RENAME TO requests_old`); err != nil {
		return fmt.Errorf("store: rename requests: %w", err)
	}

	if _, err := tx.Exec(createRequestsTable); err != nil {
		return fmt.Errorf("store: recreate requests: %w", err)
	}

	if _, err := tx.Exec(`INSERT INTO requests
		(seq, id, started_at, dur_ms, provider, key, model, upstream_model, stream, cached, coalesced, status, route_reason, prompt_tokens, completion_tokens, cost_micros, err)
		SELECT rowid, id, started_at, dur_ms, provider, key, model, upstream_model, stream, cached, coalesced, status, route_reason, prompt_tokens, completion_tokens, cost_micros, err
		FROM requests_old ORDER BY rowid`); err != nil {
		return fmt.Errorf("store: backfill requests.seq: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE requests_old`); err != nil {
		return fmt.Errorf("store: drop requests_old: %w", err)
	}

	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_requests_started ON requests(started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_pk ON requests(provider, key)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("store: recreate requests index: %w", err)
		}
	}

	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM requests`).Scan(&maxSeq); err != nil {
		return fmt.Errorf("store: read requests.seq high-water mark: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO meta (k, v) VALUES (?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		watermarkKey, strconv.FormatInt(maxSeq.Int64, 10)); err != nil {
		return fmt.Errorf("store: rebase rollup watermark: %w", err)
	}
	return tx.Commit()
}

func (s *Store) worker() {
	defer s.wg.Done()
	for op := range s.queue {
		s.mu.Lock()
		var err error
		switch op.kind {
		case opRequest:
			_, err = s.db.Exec(
				`INSERT INTO requests (id, started_at, dur_ms, provider, key, model, upstream_model, stream, cached, coalesced, status, route_reason, prompt_tokens, completion_tokens, cost_micros, err)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				op.req.ID, op.req.StartedAt.UnixMilli(), op.req.DurMillis, op.req.Provider, op.req.Key, op.req.Model, op.req.UpstreamModel,
				b2i(op.req.Stream), b2i(op.req.Cached), b2i(op.req.Coalesced), op.req.Status, op.req.RouteReason, op.req.PromptTokens, op.req.CompletionTokens, op.req.CostMicros, op.req.Err,
			)
		case opRaw:
			_, err = s.db.Exec(`INSERT INTO raw_bodies (id, started_at, req_body, resp_body) VALUES (?,?,?,?)`,
				op.id, op.ts.UnixMilli(), string(op.reqBody), string(op.respBody))
		case opError:
			_, err = s.db.Exec(`INSERT INTO errors (id, started_at, provider, key, model, message, request_id) VALUES (?,?,?,?,?,?,?)`,
				op.id, time.Now().UnixMilli(), op.provider, op.key, op.model, op.message, op.requestID)
		case opAudit:
			_, err = s.db.Exec(`INSERT INTO audit_log (ts, action, entity, detail) VALUES (?,?,?,?)`,
				time.Now().UnixMilli(), op.action, op.entity, op.detail)
		}
		if err != nil {

			s.writeFailures.Add(1)
			log.Printf("tallow-store: async %s write failed: %v", op.kind, err)
		}
		s.mu.Unlock()
		if op.flushDone != nil {
			close(op.flushDone)
		}
	}
}

func (s *Store) enqueue(op writeOp) error {
	s.enqMu.Lock()
	if s.closed {
		s.enqMu.Unlock()
		s.writeDrops.Add(1)
		return errStoreClosed
	}
	select {
	case s.queue <- op:
		s.enqMu.Unlock()
		return nil
	default:
		s.enqMu.Unlock()
		s.writeDrops.Add(1)
		return errQueueFull
	}
}

func (s *Store) Flush() {
	s.flushForTest()
}

func (s *Store) flushForTest() {
	done := make(chan struct{})
	if err := s.enqueue(writeOp{kind: opFlush, flushDone: done}); err != nil {
		return
	}
	<-done
}

var (
	errStoreClosed = fmt.Errorf("store: closed")
	errQueueFull   = fmt.Errorf("store: write queue full")
)

func (s *Store) RecordRequest(m model.RequestMeta) error {
	return s.enqueue(writeOp{kind: opRequest, req: m})
}

func (s *Store) RecordRaw(id string, startedAt time.Time, reqBody, respBody []byte) error {
	if !s.cfg.RawBodies {
		return nil
	}
	return s.enqueue(writeOp{kind: opRaw, id: id, ts: startedAt, reqBody: reqBody, respBody: respBody})
}

func (s *Store) RecordError(id, provider, key, model, requestID, message string) error {
	return s.enqueue(writeOp{kind: opError, id: id, provider: provider, key: key, model: model, requestID: requestID, message: message})
}

func (s *Store) Audit(action, entity, detail string) error {
	return s.enqueue(writeOp{kind: opAudit, action: action, entity: entity, detail: detail})
}

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

				fmt.Fprintf(os.Stderr, "tallow: retention maintenance failed: %v\n", err)
			}
		}
	}
}

type rollRow struct {
	seq                int64
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
		`SELECT seq, started_at, provider, key, prompt_tokens, completion_tokens, cost_micros, status
		 FROM requests WHERE seq > ? ORDER BY seq`, wm)
	if err != nil {
		return err
	}
	var batch []rollRow
	for rows.Next() {
		var r rollRow
		var status string
		if err := rows.Scan(&r.seq, &r.ts, &r.provider, &r.key, &r.prompt, &r.completion, &r.cost, &status); err != nil {
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
	var maxSeq int64 = wm
	for _, r := range batch {
		day := dayStartMs(r.ts)
		week := weekStartMs(r.ts)
		for _, bk := range []bucketKey{{"day", day, r.provider, r.key}, {"week", week, r.provider, r.key}} {
			a := agg[bk]
			if a == nil {
				a = &[5]int64{}
				agg[bk] = a
			}
			a[0]++
			if r.err {
				a[1]++
			}
			a[2] += int64(r.prompt)
			a[3] += int64(r.completion)
			a[4] += r.cost
		}
		if r.seq > maxSeq {
			maxSeq = r.seq
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for bk, a := range agg {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rollups (bucket, bucket_start, provider, key, requests, errors, prompt_tokens, completion_tokens, cost_micros)
			 VALUES (?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(bucket, bucket_start, provider, key) DO UPDATE SET
			   requests=requests+excluded.requests,
			   errors=errors+excluded.errors,
			   prompt_tokens=prompt_tokens+excluded.prompt_tokens,
			   completion_tokens=completion_tokens+excluded.completion_tokens,
			   cost_micros=cost_micros+excluded.cost_micros`,
			bk.bucket, bk.start, bk.prov, bk.key, a[0], a[1], a[2], a[3], a[4]); err != nil {
			return err
		}
	}

	if maxSeq > wm {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta (k, v) VALUES (?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
			watermarkKey, fmt.Sprintf("%d", maxSeq)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) deleteAgedLocked(now time.Time) error {

	cut := func(days int) int64 {
		return now.AddDate(0, 0, -days).UnixMilli()
	}
	if s.cfg.RawBodiesDays > 0 {
		if _, err := s.db.Exec(`DELETE FROM raw_bodies WHERE started_at < ?`, cut(s.cfg.RawBodiesDays)); err != nil {
			return err
		}
	}
	if s.cfg.MetadataDays > 0 {
		if _, err := s.db.Exec(`DELETE FROM requests WHERE started_at < ?`, cut(s.cfg.MetadataDays)); err != nil {
			return err
		}
	}
	if s.cfg.ErrorsDays > 0 {
		if _, err := s.db.Exec(`DELETE FROM errors WHERE started_at < ?`, cut(s.cfg.ErrorsDays)); err != nil {
			return err
		}
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

func (s *Store) RecentRequests(limit int) ([]model.RequestMeta, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, started_at, dur_ms, provider, key, model, upstream_model, stream, cached, coalesced, status, route_reason, prompt_tokens, completion_tokens, cost_micros, err
		FROM requests ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RequestMeta
	for rows.Next() {
		var m model.RequestMeta
		var started, stream, cached, coalesced int64
		if err := rows.Scan(&m.ID, &started, &m.DurMillis, &m.Provider, &m.Key, &m.Model, &m.UpstreamModel, &stream, &cached, &coalesced, &m.Status, &m.RouteReason, &m.PromptTokens, &m.CompletionTokens, &m.CostMicros, &m.Err); err != nil {
			return nil, err
		}
		m.StartedAt = time.UnixMilli(started)
		m.Stream = stream != 0
		m.Cached = cached != 0
		m.Coalesced = coalesced != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

var ErrRawNotCaptured = errors.New("store: raw bodies not captured for this request")

func (s *Store) RequestByID(id string) (model.RequestMeta, []byte, []byte, error) {
	var m model.RequestMeta
	var started, stream, cached, coalesced int64
	err := s.db.QueryRow(`SELECT id, started_at, dur_ms, provider, key, model, upstream_model, stream, cached, coalesced, status, route_reason, prompt_tokens, completion_tokens, cost_micros, err
		FROM requests WHERE id=?`, id).Scan(
		&m.ID, &started, &m.DurMillis, &m.Provider, &m.Key, &m.Model, &m.UpstreamModel,
		&stream, &cached, &coalesced, &m.Status, &m.RouteReason, &m.PromptTokens, &m.CompletionTokens, &m.CostMicros, &m.Err)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.RequestMeta{}, nil, nil, fmt.Errorf("store: unknown request %q", id)
		}
		return model.RequestMeta{}, nil, nil, err
	}
	m.StartedAt = time.UnixMilli(started)
	m.Stream = stream != 0
	m.Cached = cached != 0
	m.Coalesced = coalesced != 0

	var reqBody, respBody *string
	err = s.db.QueryRow(`SELECT req_body, resp_body FROM raw_bodies WHERE id=?`, id).Scan(&reqBody, &respBody)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return m, nil, nil, ErrRawNotCaptured
		}
		return m, nil, nil, err
	}
	mbr, mbr2 := []byte{}, []byte{}
	if reqBody != nil {
		mbr = []byte(*reqBody)
	}
	if respBody != nil {
		mbr2 = []byte(*respBody)
	}
	return m, mbr, mbr2, nil
}

type Rollup struct {
	Bucket      string    `json:"bucket"`
	BucketStart time.Time `json:"bucket_start"`
	Provider    string    `json:"provider"`
	Key         string    `json:"key"`
	Requests    int64     `json:"requests"`
	Errors      int64     `json:"errors"`
	Prompt      int64     `json:"prompt_tokens"`
	Completion  int64     `json:"completion_tokens"`
	CostMicros  int64     `json:"cost_micros"`
}

func (s *Store) RecentRollups(limit int) ([]Rollup, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT bucket, bucket_start, provider, key, requests, errors, prompt_tokens, completion_tokens, cost_micros
		FROM rollups ORDER BY bucket_start DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rollup
	for rows.Next() {
		var r Rollup
		var start int64
		if err := rows.Scan(&r.Bucket, &start, &r.Provider, &r.Key, &r.Requests, &r.Errors, &r.Prompt, &r.Completion, &r.CostMicros); err != nil {
			return nil, err
		}
		r.BucketStart = time.UnixMilli(start)
		out = append(out, r)
	}
	return out, rows.Err()
}

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

type AuditEntry struct {
	TS     time.Time `json:"ts"`
	Action string    `json:"action"`
	Entity string    `json:"entity"`
	Detail string    `json:"detail"`
}
