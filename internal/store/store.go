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
	StatusFailed    = "failed"    // terminal, retried after a back-off
	StatusAbandoned = "abandoned" // terminal, skipped: the issue is not workable as it stands
	StatusCanceled  = "canceled"  // terminal, operator cancelled
	StatusDeferred  = "deferred"  // terminal, stopped by the usage gate; not the issue's fault
	StatusPlanned   = "planned"   // terminal, a plan was posted and awaits human approval
	StatusAddressed = "addressed" // terminal, success: a PR review comment was acted on
)

// Run kinds distinguish an issue-driven run from one triggered by a PR review
// comment. Both share the runs table and the claim/lease machinery — GitHub
// numbers PRs in the same sequence as issues, so a PR number is just another
// "issue" as far as claims are concerned.
const (
	RunKindIssue     = "issue"
	RunKindPRComment = "pr_comment"
)

// PR comment task statuses.
const (
	PRCommentAcked  = "acked"
	PRCommentDone   = "done"
	PRCommentFailed = "failed"
)

// Verify outcomes recorded on a run.
const (
	VerifySkipped = "skipped"
	VerifyPassed  = "passed"
	VerifyFailed  = "failed"
	// VerifyUnavailable means the command could not be run at all — the
	// toolchain it needs is not on the daemon's PATH, or the environment
	// forbade it. It is deliberately not VerifyFailed: the tests did not fail,
	// they never ran, and telling a reviewer otherwise blames the agent's code
	// for the operator's environment.
	VerifyUnavailable = "unavailable"
)

// Gate kinds. Model cooldowns use the prefix GateModelPrefix + model ID.
const (
	GateUsageLimit  = "usage_limit"
	GatePause       = "pause"
	GateAuthExpired = "auth_expired"
	GateModelPrefix = "model:"
)

// IsTerminal reports whether a run status is final.
func IsTerminal(status string) bool {
	switch status {
	case StatusPROpen, StatusFailed, StatusAbandoned, StatusCanceled, StatusDeferred, StatusPlanned, StatusAddressed:
		return true
	}
	return false
}

// Run is one attempt at one issue, or at one PR-comment task.
type Run struct {
	ID        string
	Repo      string
	Issue     int
	CreatedAt time.Time
	Attempt   int
	ModelID   string
	Branch    string
	PRURL     string
	Status    string
	// Kind is RunKindIssue or RunKindPRComment.
	Kind      string
	StartedAt time.Time
	EndedAt   time.Time
	CostUSD   float64
	TokensIn  int64
	TokensOut int64
	// TokensCacheRead and TokensCacheWrite are the split-out subset of
	// TokensIn that was cache traffic rather than fresh input. See issue #18.
	TokensCacheRead  int64
	TokensCacheWrite int64
	NumTurns         int
	SessionID        string
	VerifyStatus     string
	Error            string
	LogPath          string
}

// Gate is a reason the loop is not claiming new work.
type Gate struct {
	Kind         string
	BlockedUntil time.Time
	Reason       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Event is one entry in a run's audit trail. At is when the event happened and
// CreatedAt is when its row was written; for events appended live the two are
// the same, and they differ only for rows backfilled by a migration.
type Event struct {
	ID        int64
	RunID     string
	At        time.Time
	Kind      string
	Detail    string
	CreatedAt time.Time
}

// Session is one Claude Code session the daemon has driven. Sessions are kept
// per repo and issue so a later run on a related task can be pointed at what
// was already done, and so a transcript can be tied back to the CLI's own
// session identifier long after the run row has scrolled out of view.
type Session struct {
	SessionID string
	RunID     string
	Repo      string
	Issue     int
	ModelID   string
	CreatedAt time.Time
}

// Claim is a lease on an issue.
type Claim struct {
	Repo        string
	Issue       int
	RunID       string
	Worker      string
	CreatedAt   time.Time
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

// migrations is the schema, one statement block per version. The daemon is not
// deployed anywhere that has to be upgraded in place, so this is kept as a
// single authoritative definition of the current schema rather than a history
// of how it got here: a database written by an older build is deleted and
// recreated, not migrated. Everything that matters across a wipe is recoverable
// from GitHub, which is what makes that safe.
//
// Add a new entry rather than editing this one only if in-place upgrades ever
// start to matter; editing it is otherwise the right way to change the schema.
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
		log_path      TEXT NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS runs_repo_issue ON runs(repo, issue);
	CREATE INDEX IF NOT EXISTS runs_started_at ON runs(started_at DESC);
	CREATE INDEX IF NOT EXISTS runs_created_at ON runs(created_at DESC);

	CREATE TABLE IF NOT EXISTS claims (
		repo         TEXT NOT NULL,
		issue        INTEGER NOT NULL,
		run_id       TEXT NOT NULL,
		worker       TEXT NOT NULL,
		leased_until INTEGER NOT NULL,
		created_at   INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (repo, issue)
	);

	CREATE TABLE IF NOT EXISTS gate (
		kind          TEXT PRIMARY KEY,
		blocked_until INTEGER NOT NULL,
		reason        TEXT NOT NULL DEFAULT '',
		updated_at    INTEGER NOT NULL DEFAULT 0,
		created_at    INTEGER NOT NULL DEFAULT 0
	);

	-- events.at is when the event happened and created_at is when the row was
	-- written. For a live append the two are the same; they are kept apart so
	-- events line up with every other table.
	CREATE TABLE IF NOT EXISTS events (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id     TEXT NOT NULL,
		at         INTEGER NOT NULL,
		kind       TEXT NOT NULL,
		detail     TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS events_run ON events(run_id, id);

	-- Claude Code session IDs, indexed by what you would look them up by: the
	-- issue they were spent on. runs.session_id holds the same value, but only
	-- for as long as you know which run to ask about.
	CREATE TABLE IF NOT EXISTS sessions (
		session_id TEXT NOT NULL,
		run_id     TEXT NOT NULL,
		repo       TEXT NOT NULL,
		issue      INTEGER NOT NULL,
		model_id   TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		PRIMARY KEY (session_id, run_id)
	);
	CREATE INDEX IF NOT EXISTS sessions_repo_issue ON sessions(repo, issue, created_at DESC);
	CREATE INDEX IF NOT EXISTS sessions_run ON sessions(run_id);

	-- One plan per issue: the latest plan posted. The issue comment it came
	-- from is the durable copy; this is the one the implement prompt carries
	-- verbatim without having to be parsed back out of a comment.
	CREATE TABLE IF NOT EXISTS plans (
		repo       TEXT NOT NULL,
		issue      INTEGER NOT NULL,
		run_id     TEXT NOT NULL,
		body       TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (repo, issue)
	);`,

	// Distinguishes an issue-driven run from one triggered by a PR review
	// comment, and tracks each triggering comment through ack -> done/failed
	// so a restart between the 👀 and the run retries it instead of stranding
	// it, and so a failed comment gets its own back-off.
	`ALTER TABLE runs ADD COLUMN kind TEXT NOT NULL DEFAULT 'issue';

	CREATE TABLE IF NOT EXISTS pr_comment_tasks (
		repo            TEXT    NOT NULL,
		pr              INTEGER NOT NULL,
		comment_kind    TEXT    NOT NULL,
		comment_id      INTEGER NOT NULL,
		status          TEXT    NOT NULL,
		attempts        INTEGER NOT NULL DEFAULT 0,
		last_attempt_at INTEGER NOT NULL DEFAULT 0,
		run_id          TEXT    NOT NULL DEFAULT '',
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL,
		PRIMARY KEY (comment_kind, comment_id)
	);
	CREATE INDEX IF NOT EXISTS pr_comment_tasks_pr ON pr_comment_tasks(repo, pr);`,

	// Splits the cache-heavy portion out of tokens_in so the dashboard can show
	// how much of a run's reported usage was fresh input versus cache traffic,
	// which is billed at a fraction and is not what drives a rate limit.
	`ALTER TABLE runs ADD COLUMN tokens_cache_read  INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE runs ADD COLUMN tokens_cache_write INTEGER NOT NULL DEFAULT 0;`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	// A database written by a build with a different schema history cannot be
	// upgraded in place, because the schema is kept as one definition rather
	// than a chain of alterations. Say so plainly: the alternative is a
	// confusing "no such column" from the first query that touches it.
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this build understands (%d); "+
			"delete the database and let it be recreated", version, len(migrations))
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
		INSERT INTO claims (repo, issue, run_id, worker, created_at, leased_until)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo, issue) DO UPDATE SET
			run_id       = excluded.run_id,
			worker       = excluded.worker,
			created_at   = excluded.created_at,
			leased_until = excluded.leased_until
		WHERE claims.leased_until <= ?`,
		repo, issue, runID, worker, now.Unix(), now.Add(lease).Unix(), now.Unix())
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
		`SELECT repo, issue, run_id, worker, created_at, leased_until FROM claims
		 WHERE leased_until > ? ORDER BY repo, issue`,
		time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}
	defer rows.Close()

	var out []Claim
	for rows.Next() {
		var c Claim
		var created, until int64
		if err := rows.Scan(&c.Repo, &c.Issue, &c.RunID, &c.Worker, &created, &until); err != nil {
			return nil, fmt.Errorf("scan claim: %w", err)
		}
		c.CreatedAt = time.Unix(created, 0)
		c.LeasedUntil = time.Unix(until, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- runs -------------------------------------------------------------------

// CreateRun inserts a new run row. CreatedAt defaults to now, and Kind
// defaults to RunKindIssue.
func (s *Store) CreateRun(ctx context.Context, r Run) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	if r.Kind == "" {
		r.Kind = RunKindIssue
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, repo, issue, attempt, model_id, branch, status, kind, created_at, started_at, log_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Repo, r.Issue, r.Attempt, r.ModelID, r.Branch, r.Status, r.Kind,
		r.CreatedAt.Unix(), r.StartedAt.Unix(), r.LogPath)
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

// RunUsage is what one Claude invocation cost, passed to RecordUsage.
type RunUsage struct {
	ModelID    string
	SessionID  string
	CostUSD    float64
	TokensIn   int64
	TokensOut  int64
	CacheRead  int64
	CacheWrite int64
	Turns      int
}

// RecordUsage stores what one Claude invocation cost.
func (s *Store) RecordUsage(ctx context.Context, runID string, u RunUsage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET
			model_id   = CASE WHEN ? <> '' THEN ? ELSE model_id END,
			session_id = CASE WHEN ? <> '' THEN ? ELSE session_id END,
			cost_usd = ?, tokens_in = ?, tokens_out = ?,
			tokens_cache_read = ?, tokens_cache_write = ?, num_turns = ?
		WHERE id = ?`,
		u.ModelID, u.ModelID, u.SessionID, u.SessionID, u.CostUSD, u.TokensIn, u.TokensOut,
		u.CacheRead, u.CacheWrite, u.Turns, runID)
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

// SetSessionID records the Claude session a run is using. It is called as soon
// as the CLI announces one, so a run that is killed before it produces a result
// still leaves its session behind.
func (s *Store) SetSessionID(ctx context.Context, runID, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET session_id = ? WHERE id = ?`, sessionID, runID); err != nil {
		return fmt.Errorf("set session id for run %s: %w", runID, err)
	}
	return nil
}

// RecordSession stores a session for later reference. Re-recording the same
// session fills in the model once it is known, and never clears it.
func (s *Store) RecordSession(ctx context.Context, sess Session) error {
	if sess.SessionID == "" {
		return nil
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (session_id, run_id, repo, issue, model_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, run_id) DO UPDATE SET
			model_id = CASE WHEN excluded.model_id <> '' THEN excluded.model_id ELSE sessions.model_id END`,
		sess.SessionID, sess.RunID, sess.Repo, sess.Issue, sess.ModelID, sess.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("record session %s: %w", sess.SessionID, err)
	}
	return nil
}

// ListSessions returns recorded sessions newest first. repo, and issue when
// positive, narrow the result to one repository or one issue.
func (s *Store) ListSessions(ctx context.Context, repo string, issue, limit int) ([]Session, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	query := `SELECT session_id, run_id, repo, issue, model_id, created_at FROM sessions`
	args := []any{}
	switch {
	case repo != "" && issue > 0:
		query += ` WHERE repo = ? AND issue = ?`
		args = append(args, repo, issue)
	case repo != "":
		query += ` WHERE repo = ?`
		args = append(args, repo)
	}
	query += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var sess Session
		var created int64
		if err := rows.Scan(&sess.SessionID, &sess.RunID, &sess.Repo, &sess.Issue, &sess.ModelID, &created); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sess.CreatedAt = time.Unix(created, 0)
		out = append(out, sess)
	}
	return out, rows.Err()
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

const runColumns = `id, repo, issue, attempt, model_id, branch, pr_url, status, kind, created_at, started_at, ended_at,
	cost_usd, tokens_in, tokens_out, tokens_cache_read, tokens_cache_write, num_turns, session_id, verify_status, error, log_path`

func scanRun(sc interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var created, started, ended int64
	err := sc.Scan(&r.ID, &r.Repo, &r.Issue, &r.Attempt, &r.ModelID, &r.Branch, &r.PRURL, &r.Status, &r.Kind,
		&created, &started, &ended, &r.CostUSD, &r.TokensIn, &r.TokensOut, &r.TokensCacheRead, &r.TokensCacheWrite,
		&r.NumTurns, &r.SessionID, &r.VerifyStatus, &r.Error, &r.LogPath)
	if err != nil {
		return r, err
	}
	r.CreatedAt = time.Unix(created, 0)
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
// whether to pick it up again, and how long to wait first.
type IssueState struct {
	// Attempts counts runs that actually got a chance at the issue. Runs the
	// usage gate stopped are excluded, and so are runs stopped from outside:
	// neither being rate-limited nor being cancelled is an attempt.
	Attempts int
	// Failures counts runs that ended without delivering a PR through some
	// fault of the issue or the agent working it. It is the exponent of the
	// retry back-off, so runs stopped from outside — cancelled by an operator
	// or by the daemon shutting down — are deliberately excluded: punishing an
	// issue for a restart would back it off for hours over nothing.
	Failures  int
	Succeeded bool
	Abandoned bool
	// LastFailureAt is when the most recent failure ended, i.e. what the
	// back-off is measured from.
	LastFailureAt time.Time
	LastPRURL     string
	LastStatus    string
}

// IssueHistory reports what previous runs did with an issue.
func (s *Store) IssueHistory(ctx context.Context, repo string, issue int) (IssueState, error) {
	var st IssueState
	var succeeded, abandoned, lastFailure int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(CASE WHEN status NOT IN (?, ?) THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN status IN (?, ?) THEN 1 ELSE 0 END), 0),
		        COALESCE(MAX(CASE WHEN status IN (?, ?) THEN ended_at ELSE 0 END), 0)
		 FROM runs WHERE repo = ? AND issue = ? AND kind = ?`,
		StatusDeferred, StatusCanceled, StatusPROpen, StatusAbandoned,
		StatusFailed, StatusAbandoned,
		StatusFailed, StatusAbandoned,
		repo, issue, RunKindIssue).
		Scan(&st.Attempts, &succeeded, &abandoned, &st.Failures, &lastFailure)
	if err != nil {
		return st, fmt.Errorf("issue history %s#%d: %w", repo, issue, err)
	}
	st.Succeeded = succeeded > 0
	st.Abandoned = abandoned > 0
	if lastFailure > 0 {
		st.LastFailureAt = time.Unix(lastFailure, 0)
	}

	row := s.db.QueryRowContext(ctx,
		`SELECT status, pr_url FROM runs WHERE repo = ? AND issue = ? AND kind = ? ORDER BY started_at DESC, rowid DESC LIMIT 1`,
		repo, issue, RunKindIssue)
	if err := row.Scan(&st.LastStatus, &st.LastPRURL); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("issue history %s#%d: %w", repo, issue, err)
	}
	return st, nil
}

// --- plans ------------------------------------------------------------------

// SavePlan records the latest plan posted for an issue, overwriting any
// previous one: only the most recent plan is ever relevant to an implement run
// or a re-plan.
func (s *Store) SavePlan(ctx context.Context, repo string, issue int, runID, body string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO plans (repo, issue, run_id, body, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo, issue) DO UPDATE SET
			run_id     = excluded.run_id,
			body       = excluded.body,
			created_at = excluded.created_at`,
		repo, issue, runID, body, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("save plan %s#%d: %w", repo, issue, err)
	}
	return nil
}

// LatestPlan returns the most recent plan body for an issue, or "" when none
// has been recorded.
func (s *Store) LatestPlan(ctx context.Context, repo string, issue int) (string, error) {
	var body string
	err := s.db.QueryRowContext(ctx,
		`SELECT body FROM plans WHERE repo = ? AND issue = ?`, repo, issue).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("latest plan %s#%d: %w", repo, issue, err)
	}
	return body, nil
}

// --- pr comment tasks ---------------------------------------------------

// PRCommentTask tracks one triggering PR comment through ack -> done/failed.
// It is the only thing that survives a crash between reacting 👀 and running
// Claude, and it gives each comment its own attempt count and back-off,
// independent of who else may have reacted to it.
type PRCommentTask struct {
	Repo          string
	PR            int
	CommentKind   string
	CommentID     int64
	Status        string
	Attempts      int
	LastAttemptAt time.Time
	RunID         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// MarkPRCommentAcked records that a comment has been seen and 👀'd. It is
// safe to call again for a comment that failed a previous attempt: the
// PRIMARY KEY upsert takes it back to "acked" and preserves its attempt count.
func (s *Store) MarkPRCommentAcked(ctx context.Context, repo string, pr int, kind string, commentID int64, runID string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pr_comment_tasks (repo, pr, comment_kind, comment_id, status, run_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(comment_kind, comment_id) DO UPDATE SET
			status     = excluded.status,
			run_id     = excluded.run_id,
			updated_at = excluded.updated_at`,
		repo, pr, kind, commentID, PRCommentAcked, runID, now, now)
	if err != nil {
		return fmt.Errorf("ack pr comment %s/%d: %w", kind, commentID, err)
	}
	return nil
}

// MarkPRCommentDone records that a comment has been addressed.
func (s *Store) MarkPRCommentDone(ctx context.Context, kind string, commentID int64, runID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE pr_comment_tasks SET status = ?, run_id = ?, updated_at = ?
		WHERE comment_kind = ? AND comment_id = ?`,
		PRCommentDone, runID, time.Now().Unix(), kind, commentID)
	if err != nil {
		return fmt.Errorf("mark pr comment done %s/%d: %w", kind, commentID, err)
	}
	return nil
}

// MarkPRCommentFailed records a failed attempt at a comment, bumping its
// attempt count and stamping when the attempt happened, which is what the
// per-comment back-off is measured from.
func (s *Store) MarkPRCommentFailed(ctx context.Context, kind string, commentID int64, runID string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE pr_comment_tasks SET status = ?, run_id = ?, attempts = attempts + 1,
			last_attempt_at = ?, updated_at = ?
		WHERE comment_kind = ? AND comment_id = ?`,
		PRCommentFailed, runID, now, now, kind, commentID)
	if err != nil {
		return fmt.Errorf("mark pr comment failed %s/%d: %w", kind, commentID, err)
	}
	return nil
}

// PRCommentTasks returns every task recorded for one pull request.
func (s *Store) PRCommentTasks(ctx context.Context, repo string, pr int) ([]PRCommentTask, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repo, pr, comment_kind, comment_id, status, attempts, last_attempt_at, run_id, created_at, updated_at
		FROM pr_comment_tasks WHERE repo = ? AND pr = ?`, repo, pr)
	if err != nil {
		return nil, fmt.Errorf("pr comment tasks %s#%d: %w", repo, pr, err)
	}
	defer rows.Close()

	var out []PRCommentTask
	for rows.Next() {
		var t PRCommentTask
		var lastAttempt, created, updated int64
		if err := rows.Scan(&t.Repo, &t.PR, &t.CommentKind, &t.CommentID, &t.Status, &t.Attempts,
			&lastAttempt, &t.RunID, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan pr comment task: %w", err)
		}
		if lastAttempt > 0 {
			t.LastAttemptAt = time.Unix(lastAttempt, 0)
		}
		t.CreatedAt = time.Unix(created, 0)
		t.UpdatedAt = time.Unix(updated, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- gate -------------------------------------------------------------------

// SetGate closes a gate until the given time.
func (s *Store) SetGate(ctx context.Context, kind string, until time.Time, reason string) error {
	// created_at is deliberately not touched on conflict: it records when this
	// gate was first closed, which is what makes a gate that keeps being
	// re-extended distinguishable from a fresh one.
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO gate (kind, blocked_until, reason, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(kind) DO UPDATE SET blocked_until = excluded.blocked_until,
			reason = excluded.reason, updated_at = excluded.updated_at`,
		kind, until.Unix(), reason, now, now)
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
		`SELECT kind, blocked_until, reason, created_at, updated_at FROM gate
		 WHERE blocked_until > ? ORDER BY kind`,
		time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list gates: %w", err)
	}
	defer rows.Close()

	var out []Gate
	for rows.Next() {
		var g Gate
		var until, created, updated int64
		if err := rows.Scan(&g.Kind, &until, &g.Reason, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan gate: %w", err)
		}
		g.BlockedUntil = time.Unix(until, 0)
		g.CreatedAt = time.Unix(created, 0)
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
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (run_id, at, kind, detail, created_at) VALUES (?, ?, ?, ?, ?)`,
		runID, now, kind, detail, now)
	if err != nil {
		return fmt.Errorf("append event for run %s: %w", runID, err)
	}
	return nil
}

// ListEvents returns a run's audit trail in order.
func (s *Store) ListEvents(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, at, kind, detail, created_at FROM events WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list events for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var at, createdAt int64
		if err := rows.Scan(&e.ID, &e.RunID, &at, &e.Kind, &e.Detail, &createdAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.At = time.Unix(at, 0)
		e.CreatedAt = time.Unix(createdAt, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}
