package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	ok, err := st.TryClaim(ctx, "o/r", 1, "run-a", "worker-a", time.Hour)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	ok, err = st.TryClaim(ctx, "o/r", 1, "run-b", "worker-b", time.Hour)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ok {
		t.Fatal("a live claim must not be stealable")
	}
}

// A crashed worker never releases its claim; the lease expiring is the entire
// recovery mechanism, so this is the important one.
func TestExpiredClaimIsStealable(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if ok, err := st.TryClaim(ctx, "o/r", 7, "dead-run", "dead-worker", -time.Minute); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	ok, err := st.TryClaim(ctx, "o/r", 7, "new-run", "live-worker", time.Hour)
	if err != nil {
		t.Fatalf("steal: %v", err)
	}
	if !ok {
		t.Fatal("an expired lease must be reclaimable")
	}

	claims, err := st.ActiveClaims(ctx)
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	if len(claims) != 1 || claims[0].RunID != "new-run" {
		t.Fatalf("expected the new run to hold the claim, got %+v", claims)
	}
}

func TestConcurrentClaimsElectOneWinner(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	const workers = 12
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ok, err := st.TryClaim(ctx, "o/r", 42, "run", "worker", time.Hour)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("exactly one worker must win the claim, got %d", wins)
	}
}

func TestReleaseOnlyByHolder(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if _, err := st.TryClaim(ctx, "o/r", 3, "run-a", "w", time.Hour); err != nil {
		t.Fatal(err)
	}
	// A run that overran and lost its claim must not release the new holder's.
	if err := st.ReleaseClaim(ctx, "o/r", 3, "some-other-run"); err != nil {
		t.Fatal(err)
	}
	claims, _ := st.ActiveClaims(ctx)
	if len(claims) != 1 {
		t.Fatalf("claim should survive a release by a non-holder, got %+v", claims)
	}
	if err := st.ReleaseClaim(ctx, "o/r", 3, "run-a"); err != nil {
		t.Fatal(err)
	}
	claims, _ = st.ActiveClaims(ctx)
	if len(claims) != 0 {
		t.Fatalf("holder release should drop the claim, got %+v", claims)
	}
}

func TestRepoBusy(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	busy, err := st.RepoBusy(ctx, "o/r")
	if err != nil || busy {
		t.Fatalf("fresh repo should be idle: busy=%v err=%v", busy, err)
	}
	if _, err := st.TryClaim(ctx, "o/r", 1, "run", "w", time.Hour); err != nil {
		t.Fatal(err)
	}
	if busy, _ := st.RepoBusy(ctx, "o/r"); !busy {
		t.Fatal("repo with a live claim must read as busy")
	}
	if busy, _ := st.RepoBusy(ctx, "o/other"); busy {
		t.Fatal("a different repo must stay claimable in parallel")
	}
}

func TestRunLifecycleAndHistory(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	run := Run{ID: "r1", Repo: "o/r", Issue: 5, Attempt: 1, Status: StatusClaimed, StartedAt: time.Now(), LogPath: "/tmp/r1.jsonl"}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := st.RecordUsage(ctx, "r1", "claude-opus-5", "sess", 0.25, 1200, 300, 4); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := st.SetVerifyStatus(ctx, "r1", VerifyPassed); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPRURL(ctx, "r1", "https://github.com/o/r/pull/9"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(ctx, "r1", StatusPROpen); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetRun(ctx, "r1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.CostUSD != 0.25 || got.TokensIn != 1200 || got.ModelID != "claude-opus-5" {
		t.Fatalf("usage not persisted: %+v", got)
	}
	if got.Status != StatusPROpen || got.EndedAt.IsZero() {
		t.Fatalf("terminal status should stamp ended_at: %+v", got)
	}

	hist, err := st.IssueHistory(ctx, "o/r", 5)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if hist.Attempts != 1 || !hist.Succeeded || hist.Abandoned {
		t.Fatalf("unexpected history: %+v", hist)
	}

	// An untouched issue must read as clean rather than erroring.
	fresh, err := st.IssueHistory(ctx, "o/r", 999)
	if err != nil {
		t.Fatalf("history for unseen issue: %v", err)
	}
	if fresh.Attempts != 0 || fresh.Succeeded || fresh.Abandoned {
		t.Fatalf("unseen issue should be clean: %+v", fresh)
	}
}

func TestInFlightRuns(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	for id, status := range map[string]string{
		"a": StatusWorking, "b": StatusPROpen, "c": StatusVerifying, "d": StatusAbandoned,
	} {
		if err := st.CreateRun(ctx, Run{ID: id, Repo: "o/r", Issue: 1, Status: status, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := st.InFlightRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 in-flight runs, got %d", len(runs))
	}
}

func TestGates(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if err := st.SetGate(ctx, GateUsageLimit, time.Now().Add(time.Hour), "limit reached"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetGate(ctx, GateModelPrefix+"claude-opus-5", time.Now().Add(time.Hour), "cooldown"); err != nil {
		t.Fatal(err)
	}
	// An already-expired gate must not show up.
	if err := st.SetGate(ctx, GatePause, time.Now().Add(-time.Hour), "old"); err != nil {
		t.Fatal(err)
	}

	gates, err := st.ActiveGates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 2 {
		t.Fatalf("expected 2 active gates, got %d: %+v", len(gates), gates)
	}

	cooled, err := st.CooledDownModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cooled["claude-opus-5"] || len(cooled) != 1 {
		t.Fatalf("unexpected cooldown set: %+v", cooled)
	}

	if err := st.ClearGate(ctx, GateUsageLimit); err != nil {
		t.Fatal(err)
	}
	gates, _ = st.ActiveGates(ctx)
	if len(gates) != 1 {
		t.Fatalf("clearing should leave 1 gate, got %+v", gates)
	}
}

func TestEvents(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if err := st.CreateRun(ctx, Run{ID: "r1", Repo: "o/r", Issue: 1, Status: StatusClaimed, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"claimed", "model", "pr_open"} {
		if err := st.AppendEvent(ctx, "r1", k, "detail-"+k); err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.ListEvents(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Kind != "claimed" || events[2].Kind != "pr_open" {
		t.Fatalf("events out of order or missing: %+v", events)
	}
}

func TestGetRunNotFound(t *testing.T) {
	if _, err := testStore(t).GetRun(context.Background(), "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	for i := 0; i < 3; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		st.Close()
	}
}

func TestRowsRecordWhenTheyWereCreated(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if err := st.CreateRun(ctx, Run{ID: "r1", Repo: "o/r", Issue: 1, Status: StatusClaimed, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	run, err := st.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if run.CreatedAt.IsZero() {
		t.Error("run should record when its row was created")
	}

	if _, err := st.TryClaim(ctx, "o/r", 1, "r1", "w", time.Hour); err != nil {
		t.Fatal(err)
	}
	claims, err := st.ActiveClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].CreatedAt.IsZero() {
		t.Errorf("claim should record when it was taken: %+v", claims)
	}

	if err := st.SetGate(ctx, GatePause, time.Now().Add(time.Hour), "operator"); err != nil {
		t.Fatal(err)
	}
	gates, err := st.ActiveGates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].CreatedAt.IsZero() {
		t.Errorf("gate should record when it was first closed: %+v", gates)
	}
}

// A gate that keeps being extended must stay distinguishable from a fresh one,
// so re-closing it moves updated_at but never created_at.
func TestReclosingAGateKeepsItsCreatedAt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if err := st.SetGate(ctx, GateUsageLimit, time.Now().Add(time.Hour), "first"); err != nil {
		t.Fatal(err)
	}
	first, _ := st.ActiveGates(ctx)

	time.Sleep(1100 * time.Millisecond) // second resolution on disk
	if err := st.SetGate(ctx, GateUsageLimit, time.Now().Add(2*time.Hour), "again"); err != nil {
		t.Fatal(err)
	}
	second, _ := st.ActiveGates(ctx)

	if !second[0].CreatedAt.Equal(first[0].CreatedAt) {
		t.Errorf("created_at moved on re-close: %v -> %v", first[0].CreatedAt, second[0].CreatedAt)
	}
	if !second[0].UpdatedAt.After(first[0].UpdatedAt) {
		t.Errorf("updated_at should move on re-close: %v -> %v", first[0].UpdatedAt, second[0].UpdatedAt)
	}
}

// Databases written by an earlier version have no created_at column at all;
// the migration has to add it and backfill something meaningful.
func TestCreatedAtIsBackfilledOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, migrations[0]); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	started := time.Now().Add(-time.Hour)
	if _, err := old.ExecContext(ctx,
		`INSERT INTO runs (id, repo, issue, attempt, status, started_at) VALUES ('old-run', 'o/r', 1, 1, 'pr_open', ?)`,
		started.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer st.Close()

	run, err := st.GetRun(ctx, "old-run")
	if err != nil {
		t.Fatal(err)
	}
	if run.CreatedAt.Unix() != started.Unix() {
		t.Fatalf("created_at should be backfilled from started_at, got %v want %v", run.CreatedAt, started)
	}
}

// The back-off is driven off IssueHistory, so what it counts matters: a run
// the usage gate stopped is neither an attempt nor a failure.
func TestIssueHistoryCountsFailuresButNotDeferrals(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	seed := func(id, status string, ended time.Time) {
		if err := st.CreateRun(ctx, Run{ID: id, Repo: "o/r", Issue: 5, Status: StatusClaimed, StartedAt: ended}); err != nil {
			t.Fatal(err)
		}
		if err := st.FailRun(ctx, id, status, "because"); err != nil {
			t.Fatal(err)
		}
	}
	seed("a", StatusFailed, time.Now())
	seed("b", StatusDeferred, time.Now())
	seed("c", StatusAbandoned, time.Now())

	hist, err := st.IssueHistory(ctx, "o/r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if hist.Attempts != 2 {
		t.Errorf("a deferred run should not consume an attempt, got %d attempts", hist.Attempts)
	}
	if hist.Failures != 2 {
		t.Errorf("failures = %d, want 2", hist.Failures)
	}
	if hist.LastFailureAt.IsZero() {
		t.Error("the back-off has nothing to measure from without LastFailureAt")
	}
	if hist.Succeeded {
		t.Error("no run delivered a PR")
	}
}
