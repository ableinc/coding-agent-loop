// Package store is the daemon's durable state: which issues are claimed, what
// each run did, and whether the usage gate is currently closed.
//
// Claims are lease-based. That is the whole crash-recovery story: a worker
// that dies never releases its claim explicitly, and the lease simply expires
// so another worker can take the issue. Nothing needs to detect the crash.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

// Run statuses. The terminal ones are those a run can end on.
const (
	StatusClaimed   = "claimed"
	StatusWorking   = "working"
	StatusVerifying = "verifying"
	StatusPushed    = "pushed"
	StatusPROpen    = "pr_open"   // terminal, success
	StatusFailed    = "failed"    // terminal, may be retried as a new run
	StatusAbandoned = "abandoned" // terminal, attempts exhausted
	StatusCanceled  = "canceled"  // terminal, operator cancelled
)

// Verify outcomes recorded on a run.
const (
	VerifySkipped = "skipped"
	VerifyPassed  = "passed"
	VerifyFailed  = "failed"
)

// Gate kinds. Model cooldowns use the prefix GateModelPrefix + model ID.
const (
	GateUsageLimit  = "usage_limit"
	GatePause       = "pause"
	GateModelPrefix = "model:"
)

// IsTerminal reports whether a run status is final.
func IsTerminal(status string) bool {
	switch status {
	case StatusPROpen, StatusFailed, StatusAbandoned, StatusCanceled:
		return true
	}
	return false
}

// Run is one attempt at one issue.
type Run struct {
	ID           string
	Repo         string
	Issue        int
	Attempt      int
	ModelID      string
	Branch       string
	PRURL        string
	Status       string
	StartedAt    time.Time
	EndedAt      time.Time
	CostUSD      float64
	TokensIn     int64
	TokensOut    int64
	NumTurns     int
	SessionID    string
	VerifyStatus string
	Error        string
	LogPath      string
}

// Gate is a reason the loop is not claiming new work.
type Gate struct {
	Kind         string
	BlockedUntil time.Time
	Reason       string
	UpdatedAt    time.Time
}

// Event is one entry in a run's audit trail.
type Event struct {
	ID     int64
	RunID  string
	At     time.Time
	Kind   string
	Detail string
}

// Claim is a lease on an issue.
type Claim struct {
	Repo        string
	Issue       int
	RunID       string
	Worker      string
	LeasedUntil time.Time
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open creates the database file if needed, applies migrations, and returns a
// ready Store.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create state dir %s: %w", dir, err)
		}
	}
	// WAL keeps the HTTP read path from blocking the workers' writes;
	// busy_timeout absorbs the remaining short write contention.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state db %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open state db %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests.
func (s *Store) DB() *sql.DB { return s.db }

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS runs (
		id            TEXT PRIMARY KEY,
		repo          TEXT NOT NULL,
		issue         INTEGER NOT NULL,
		attempt       INTEGER NOT NULL DEFAULT 1,
		model_id      TEXT NOT NULL DEFAULT '',
		branch        TEXT NOT NULL DEFAULT '',
		pr_url        TEXT NOT NULL DEFAULT '',
		status        TEXT NOT NULL,
		started_at    INTEGER NOT NULL,
		ended_at      INTEGER NOT NULL DEFAULT 0,
		cost_usd      REAL NOT NULL DEFAULT 0,
		tokens_in     INTEGER NOT NULL DEFAULT 0,
		tokens_out    INTEGER NOT NULL DEFAULT 0,
		num_turns     INTEGER NOT NULL DEFAULT 0,
		session_id    TEXT NOT NULL DEFAULT '',
		verify_status TEXT NOT NULL DEFAULT '',
		error         TEXT NOT NULL DEFAULT '',
		log_path      TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS runs_repo_issue ON runs(repo, issue);
	CREATE INDEX IF NOT EXISTS runs_started_at ON runs(started_at DESC);

	CREATE TABLE IF NOT EXISTS claims (
		repo         TEXT NOT NULL,
		issue        INTEGER NOT NULL,
		run_id       TEXT NOT NULL,
		worker       TEXT NOT NULL,
		leased_until INTEGER NOT NULL,
		PRIMARY KEY (repo, issue)
	);

	CREATE TABLE IF NOT EXISTS gate (
		kind          TEXT PRIMARY KEY,
		blocked_until INTEGER NOT NULL,
		reason        TEXT NOT NULL DEFAULT '',
		updated_at    INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS events (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		at     INTEGER NOT NULL,
		kind   TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS events_run ON events(run_id, id);`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		if _, err := s.db.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
		// PRAGMA does not accept bound parameters.
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			return fmt.Errorf("record schema version %d: %w", i+1, err)
		}
	}
	return nil
}

// --- claims -----------------------------------------------------------------

// TryClaim takes the lease on an issue, either because nobody holds it or
// because the previous holder's lease expired. It returns false when the issue
// is actively claimed by someone else.
func (s *Store) TryClaim(ctx context.Context, repo string, issue int, runID, worker string, lease time.Duration) (bool, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO claims (repo, issue, run_id, worker, leased_until)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo, issue) DO UPDATE SET
			run_id       = excluded.run_id,
			worker       = excluded.worker,
			leased_until = excluded.leased_until
		WHERE claims.leased_until <= ?`,
		repo, issue, runID, worker, now.Add(lease).Unix(), now.Unix())
	if err != nil {
		return false, fmt.Errorf("claim %s#%d: %w", repo, issue, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim %s#%d: %w", repo, issue, err)
	}
	return n > 0, nil
}

// RenewClaim extends a lease held by runID. Long runs call this so a slow run
// never has its claim stolen mid-flight.
func (s *Store) RenewClaim(ctx context.Context, repo string, issue int, runID string, lease time.Duration) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE claims SET leased_until = ? WHERE repo = ? AND issue = ? AND run_id = ?`,
		time.Now().Add(lease).Unix(), repo, issue, runID)
	if err != nil {
		return fmt.Errorf("renew claim %s#%d: %w", repo, issue, err)
	}
	return nil
}

// ReleaseClaim drops the lease, but only if runID still holds it — so a run
// that overran and lost its claim cannot release the new holder's.
func (s *Store) ReleaseClaim(ctx context.Context, repo string, issue int, runID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM claims WHERE repo = ? AND issue = ? AND run_id = ?`, repo, issue, runID)
	if err != nil {
		return fmt.Errorf("release claim %s#%d: %w", repo, issue, err)
	}
	return nil
}

// RepoBusy reports whether any issue in repo is actively claimed. This is how
// "serial within a repo" is enforced across restarts.
func (s *Store) RepoBusy(ctx context.Context, repo string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM claims WHERE repo = ? AND leased_until > ?`, repo, time.Now().Unix()).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check repo busy %s: %w", repo, err)
	}
	return n > 0, nil
}

// ActiveClaims lists every unexpired claim.
func (s *Store) ActiveClaims(ctx context.Context) ([]Claim, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo, issue, run_id, worker, leased_until FROM claims WHERE leased_until > ? ORDER BY repo, issue`,
		time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}
	defer rows.Close()

	var out []Claim
	for rows.Next() {
		var c Claim
		var until int64
		if err := rows.Scan(&c.Repo, &c.Issue, &c.RunID, &c.Worker, &until); err != nil {
			return nil, fmt.Errorf("scan claim: %w", err)
		}
		c.LeasedUntil = time.Unix(until, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- runs -------------------------------------------------------------------

// CreateRun inserts a new run row.
func (s *Store) CreateRun(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, repo, issue, attempt, model_id, branch, status, started_at, log_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Repo, r.Issue, r.Attempt, r.ModelID, r.Branch, r.Status, r.StartedAt.Unix(), r.LogPath)
	if err != nil {
		return fmt.Errorf("create run %s: %w", r.ID, err)
	}
	return nil
}

// SetRunStatus moves a run to a new status, stamping ended_at on terminal ones.
func (s *Store) SetRunStatus(ctx context.Context, runID, status string) error {
	var ended int64
	if IsTerminal(status) {
		ended = time.Now().Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, ended_at = CASE WHEN ? > 0 THEN ? ELSE ended_at END WHERE id = ?`,
		status, ended, ended, runID)
	if err != nil {
		return fmt.Errorf("set run %s status %s: %w", runID, status, err)
	}
	return nil
}

// RecordUsage stores what one Claude invocation cost.
func (s *Store) RecordUsage(ctx context.Context, runID, modelID, sessionID string, cost float64, in, out int64, turns int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET
			model_id   = CASE WHEN ? <> '' THEN ? ELSE model_id END,
			session_id = ?, cost_usd = ?, tokens_in = ?, tokens_out = ?, num_turns = ?
		WHERE id = ?`,
		modelID, modelID, sessionID, cost, in, out, turns, runID)
	if err != nil {
		return fmt.Errorf("record usage for run %s: %w", runID, err)
	}
	return nil
}

// SetVerifyStatus records the outcome of the repo's test command.
func (s *Store) SetVerifyStatus(ctx context.Context, runID, status string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET verify_status = ? WHERE id = ?`, status, runID); err != nil {
		return fmt.Errorf("set verify status for run %s: %w", runID, err)
	}
	return nil
}

// SetBranch records the branch a run is working on.
func (s *Store) SetBranch(ctx context.Context, runID, branch string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET branch = ? WHERE id = ?`, branch, runID); err != nil {
		return fmt.Errorf("set branch for run %s: %w", runID, err)
	}
	return nil
}

// SetPRURL records the opened pull request.
func (s *Store) SetPRURL(ctx context.Context, runID, url string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET pr_url = ? WHERE id = ?`, url, runID); err != nil {
		return fmt.Errorf("set pr url for run %s: %w", runID, err)
	}
	return nil
}

// FailRun marks a run terminal with an error message.
func (s *Store) FailRun(ctx context.Context, runID, status, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, error = ?, ended_at = ? WHERE id = ?`,
		status, msg, time.Now().Unix(), runID)
	if err != nil {
		return fmt.Errorf("fail run %s: %w", runID, err)
	}
	return nil
}

var errNoRows = errors.New("not found")

// ErrNotFound is returned when a lookup finds nothing.
var ErrNotFound = errNoRows

const runColumns = `id, repo, issue, attempt, model_id, branch, pr_url, status, started_at, ended_at,
	cost_usd, tokens_in, tokens_out, num_turns, session_id, verify_status, error, log_path`

func scanRun(sc interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var started, ended int64
	err := sc.Scan(&r.ID, &r.Repo, &r.Issue, &r.Attempt, &r.ModelID, &r.Branch, &r.PRURL, &r.Status,
		&started, &ended, &r.CostUSD, &r.TokensIn, &r.TokensOut, &r.NumTurns, &r.SessionID,
		&r.VerifyStatus, &r.Error, &r.LogPath)
	if err != nil {
		return r, err
	}
	r.StartedAt = time.Unix(started, 0)
	if ended > 0 {
		r.EndedAt = time.Unix(ended, 0)
	}
	return r, nil
}

// GetRun returns one run by ID.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("get run %s: %w", id, err)
	}
	return r, nil
}

// ListRuns returns recent runs, newest first, optionally filtered by repo.
func (s *Store) ListRuns(ctx context.Context, repo string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	query := `SELECT ` + runColumns + ` FROM runs`
	args := []any{}
	if repo != "" {
		query += ` WHERE repo = ?`
		args = append(args, repo)
	}
	query += ` ORDER BY started_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InFlightRuns returns runs that have not reached a terminal status.
func (s *Store) InFlightRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE status IN (?, ?, ?, ?) ORDER BY started_at`,
		StatusClaimed, StatusWorking, StatusVerifying, StatusPushed)
	if err != nil {
		return nil, fmt.Errorf("list in-flight runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IssueState summarises prior work on an issue so the poller can decide
// whether to pick it up again.
type IssueState struct {
	Attempts   int
	Succeeded  bool
	Abandoned  bool
	LastPRURL  string
	LastStatus string
}

// IssueHistory reports what previous runs did with an issue.
func (s *Store) IssueHistory(ctx context.Context, repo string, issue int) (IssueState, error) {
	var st IssueState
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        SUM(CASE WHEN status = ? THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status = ? THEN 1 ELSE 0 END)
		 FROM runs WHERE repo = ? AND issue = ?`,
		StatusPROpen, StatusAbandoned, repo, issue).
		Scan(&st.Attempts, &nullInt{&st.Succeeded}, &nullInt{&st.Abandoned})
	if err != nil {
		return st, fmt.Errorf("issue history %s#%d: %w", repo, issue, err)
	}

	row := s.db.QueryRowContext(ctx,
		`SELECT status, pr_url FROM runs WHERE repo = ? AND issue = ? ORDER BY started_at DESC, rowid DESC LIMIT 1`,
		repo, issue)
	if err := row.Scan(&st.LastStatus, &st.LastPRURL); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("issue history %s#%d: %w", repo, issue, err)
	}
	return st, nil
}

// nullInt scans a possibly-NULL SUM() into a bool ("more than zero").
type nullInt struct{ target *bool }

func (n *nullInt) Scan(v any) error {
	switch t := v.(type) {
	case nil:
		*n.target = false
	case int64:
		*n.target = t > 0
	case float64:
		*n.target = t > 0
	default:
		return fmt.Errorf("unexpected count type %T", v)
	}
	return nil
}

// --- gate -------------------------------------------------------------------

// SetGate closes a gate until the given time.
func (s *Store) SetGate(ctx context.Context, kind string, until time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO gate (kind, blocked_until, reason, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind) DO UPDATE SET blocked_until = excluded.blocked_until,
			reason = excluded.reason, updated_at = excluded.updated_at`,
		kind, until.Unix(), reason, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("set gate %s: %w", kind, err)
	}
	return nil
}

// ClearGate reopens a gate.
func (s *Store) ClearGate(ctx context.Context, kind string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gate WHERE kind = ?`, kind); err != nil {
		return fmt.Errorf("clear gate %s: %w", kind, err)
	}
	return nil
}

// ActiveGates returns every gate still in effect.
func (s *Store) ActiveGates(ctx context.Context) ([]Gate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, blocked_until, reason, updated_at FROM gate WHERE blocked_until > ? ORDER BY kind`,
		time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list gates: %w", err)
	}
	defer rows.Close()

	var out []Gate
	for rows.Next() {
		var g Gate
		var until, updated int64
		if err := rows.Scan(&g.Kind, &until, &g.Reason, &updated); err != nil {
			return nil, fmt.Errorf("scan gate: %w", err)
		}
		g.BlockedUntil = time.Unix(until, 0)
		g.UpdatedAt = time.Unix(updated, 0)
		out = append(out, g)
	}
	return out, rows.Err()
}

// CooledDownModels returns the set of model IDs currently under cooldown.
func (s *Store) CooledDownModels(ctx context.Context) (map[string]bool, error) {
	gates, err := s.ActiveGates(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, g := range gates {
		if len(g.Kind) > len(GateModelPrefix) && g.Kind[:len(GateModelPrefix)] == GateModelPrefix {
			out[g.Kind[len(GateModelPrefix):]] = true
		}
	}
	return out, nil
}

// --- events -----------------------------------------------------------------

// AppendEvent adds to a run's audit trail.
func (s *Store) AppendEvent(ctx context.Context, runID, kind, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (run_id, at, kind, detail) VALUES (?, ?, ?, ?)`,
		runID, time.Now().Unix(), kind, detail)
	if err != nil {
		return fmt.Errorf("append event for run %s: %w", runID, err)
	}
	return nil
}

// ListEvents returns a run's audit trail in order.
func (s *Store) ListEvents(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, at, kind, detail FROM events WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list events for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &e.RunID, &at, &e.Kind, &e.Detail); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}
