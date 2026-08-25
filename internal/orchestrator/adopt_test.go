package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/claude"
	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/discord"
	"github.com/ableinc/coding-agent-loop/internal/gate"
	"github.com/ableinc/coding-agent-loop/internal/gh"
	gitpkg "github.com/ableinc/coding-agent-loop/internal/git"
	"github.com/ableinc/coding-agent-loop/internal/models"
	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/verify"
)

// stubGH stands in for the gh CLI, dispatching on the subcommand and appending
// every invocation to a log the test can assert on. Anything it is not told
// about returns empty JSON, so an unexpected call shows up as a missing result
// rather than a hang.
func stubGH(t *testing.T, callLog string, responses map[string]string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gh-stub.sh")

	var cases strings.Builder
	for key, body := range responses {
		fmt.Fprintf(&cases, "  %q) cat <<'STUBEOF'\n%s\nSTUBEOF\n  ;;\n", key, body)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + callLog + "\n" +
		"case \"$1 $2\" in\n" + cases.String() +
		"  *) echo '[]' ;;\nesac\nexit 0\n"

	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func ghCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

// testOrchestrator wires the real orchestrator against a stubbed gh. Claude is
// pointed at a binary that does not exist: every test here must reach its
// conclusion without running the agent, and a run that tries to would fail
// loudly rather than quietly costing money.
func testOrchestrator(t *testing.T, ghBin string, st *store.Store) *Orchestrator {
	t.Helper()
	cfg := config.Default()
	cfg.GitHub.Binary = ghBin
	cfg.GitHub.Owners = []string{"acme"}
	cfg.Claude.Binary = filepath.Join(t.TempDir(), "claude-must-not-run")
	cfg.Workspace.Root = t.TempDir()
	cfg.Workspace.ReposRoot = t.TempDir()
	cfg.Workspace.LogsRoot = t.TempDir()

	logf := func(string, ...any) {}
	return New(Options{
		Config:   cfg,
		Store:    st,
		Registry: testRegistry(t),
		GH:       gh.New(ghBin, false),
		Git:      &gitpkg.Manager{ReposRoot: cfg.Workspace.ReposRoot, WorkRoot: cfg.Workspace.Root},
		Runner:   &claude.Runner{},
		Gate:     gate.New(st, cfg.Claude, logf),
		Verify:   &verify.Runner{Cfg: cfg.Verify},
		Discord:  discord.New(false, "", logf),
		WorkerID: "test",
	})
}

// testRegistry loads the ladder the binary ships with, so these tests exercise
// the same model selection the daemon does.
func testRegistry(t *testing.T) *models.Registry {
	t.Helper()
	reg, err := models.Load(filepath.Join(t.TempDir(), "models.json"), true)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// The scenario this whole change exists for: the store has been thrown away,
// but the issue was planned, approved, implemented and delivered. Nothing may
// be re-done, and the issue must be brought back into agreement with the PR.
func TestAdoptsAnExistingPRAfterTheStoreIsLost(t *testing.T) {
	ctx := context.Background()
	callLog := filepath.Join(t.TempDir(), "calls.txt")

	issue := `{"number":5,"title":"Change your commit name","body":"please","url":"https://github.com/acme/widgets/issues/5",
	  "state":"OPEN","labels":[{"name":"agent-ready"}],
	  "comments":[
	    {"author":{"login":"agent-bot"},"body":"<!-- coding-agent-loop:plan -->\n\n## Plan\n\ndo the thing\n\n---\n\nfooter"},
	    {"author":{"login":"alice"},"body":"implement"}
	  ]}`
	ghBin := stubGH(t, callLog, map[string]string{
		"search issues": `[{"number":5,"title":"Change your commit name","url":"u",
		  "repository":{"name":"widgets","nameWithOwner":"acme/widgets"},
		  "labels":[{"name":"agent-ready"}],"isPullRequest":false,"state":"open"}]`,
		"issue view": issue,
		"pr list": `[{"number":12,"url":"https://github.com/acme/widgets/pull/12",
		  "body":"Closes #5\n\nthe work","headRefName":"agent/issue-5-change-your-commit-name",
		  "state":"MERGED","mergedAt":"2026-01-02T03:04:05Z"}]`,
	})

	st := openTestStore(t)
	if err := testOrchestrator(t, ghBin, st).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// The store now records the delivery it had forgotten.
	hist, err := st.IssueHistory(ctx, "acme/widgets", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !hist.Succeeded {
		t.Fatalf("the issue must be recorded as delivered, got %+v", hist)
	}
	if hist.LastPRURL != "https://github.com/acme/widgets/pull/12" {
		t.Fatalf("the adopted PR url should be recorded, got %q", hist.LastPRURL)
	}

	calls := ghCalls(t, callLog)
	// The agent never ran, and no second PR was opened.
	if strings.Contains(calls, "pr create") {
		t.Fatalf("a second pull request must never be opened:\n%s", calls)
	}
	// The issue is told about the PR, and stops being a candidate.
	if !strings.Contains(calls, "issue comment") {
		t.Fatalf("the adopted PR should be announced on the issue:\n%s", calls)
	}
	if !strings.Contains(calls, "--add-label agent-done") ||
		!strings.Contains(calls, "--remove-label agent-ready") {
		t.Fatalf("the issue should be labelled done and lose its trigger label:\n%s", calls)
	}
	// The PR already says "Closes #5", so nothing needs rewriting.
	if strings.Contains(calls, "pr edit") {
		t.Fatalf("an already-linked PR must not be edited:\n%s", calls)
	}
}

// A PR the harness opened but never linked back — the run died between
// creating it and announcing it — has to be associated with its issue.
func TestAdoptionLinksAnUnassociatedPR(t *testing.T) {
	ctx := context.Background()
	callLog := filepath.Join(t.TempDir(), "calls.txt")

	ghBin := stubGH(t, callLog, map[string]string{
		"search issues": `[{"number":5,"title":"Change your commit name","url":"u",
		  "repository":{"name":"widgets","nameWithOwner":"acme/widgets"},
		  "labels":[{"name":"agent-ready"}],"isPullRequest":false,"state":"open"}]`,
		"issue view": `{"number":5,"title":"Change your commit name","body":"please","url":"u",
		  "state":"OPEN","labels":[{"name":"agent-ready"}],
		  "comments":[
		    {"author":{"login":"agent-bot"},"body":"<!-- coding-agent-loop:plan -->\n\n## Plan\n\ndo the thing\n\n---\n\nfooter"},
		    {"author":{"login":"alice"},"body":"implement"}
		  ]}`,
		// No closing keyword, and a branch whose slug no longer matches the
		// issue title: only the "<prefix><number>-" match can find this.
		"pr list": `[{"number":12,"url":"https://github.com/acme/widgets/pull/12",
		  "body":"some work","headRefName":"agent/issue-5-an-older-title","state":"OPEN"}]`,
	})

	st := openTestStore(t)
	if err := testOrchestrator(t, ghBin, st).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	calls := ghCalls(t, callLog)
	if !strings.Contains(calls, "pr edit 12") {
		t.Fatalf("an unlinked PR should be edited to close its issue:\n%s", calls)
	}
	if strings.Contains(calls, "pr create") {
		t.Fatalf("a second pull request must never be opened:\n%s", calls)
	}
	hist, err := st.IssueHistory(ctx, "acme/widgets", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !hist.Succeeded {
		t.Fatalf("adopting a PR should mark the issue delivered, got %+v", hist)
	}
}

// Adoption has to settle: a second pass over an issue that has already been
// reconciled must not re-comment or re-label it.
func TestADeliveredIssueIsLeftAloneOnTheNextPass(t *testing.T) {
	ctx := context.Background()
	callLog := filepath.Join(t.TempDir(), "calls.txt")

	ghBin := stubGH(t, callLog, map[string]string{
		"search issues": `[{"number":5,"title":"Change your commit name","url":"u",
		  "repository":{"name":"widgets","nameWithOwner":"acme/widgets"},
		  "labels":[{"name":"agent-ready"}],"isPullRequest":false,"state":"open"}]`,
		// The issue already carries the harness's PR announcement.
		"issue view": `{"number":5,"title":"Change your commit name","body":"please","url":"u",
		  "state":"OPEN","labels":[{"name":"agent-ready"}],
		  "comments":[
		    {"author":{"login":"agent-bot"},"body":"<!-- coding-agent-loop:plan -->\n\n## Plan\n\ndo the thing\n\n---\n\nfooter"},
		    {"author":{"login":"alice"},"body":"implement"},
		    {"author":{"login":"agent-bot"},"body":"<!-- coding-agent-loop:pr -->\n\nOpened https://github.com/acme/widgets/pull/12"}
		  ]}`,
	})

	st := openTestStore(t)
	if err := testOrchestrator(t, ghBin, st).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	calls := ghCalls(t, callLog)
	// Reconciling the stale trigger label is expected and asserted separately;
	// what must never happen again is the work itself, or a second announcement
	// of it.
	for _, forbidden := range []string{"issue comment", "pr create", "pr edit"} {
		if strings.Contains(calls, forbidden) {
			t.Fatalf("a delivered issue must not be worked again, saw %q:\n%s", forbidden, calls)
		}
	}
	// It is not even claimed: the phase decision alone rules it out.
	runs, err := st.ListRuns(ctx, "acme/widgets", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("a delivered issue should never be claimed, got %d run(s)", len(runs))
	}
}

// The other half of a lost store: the plan the human approved exists only as a
// comment. It has to be read back rather than re-planned or ignored, because
// re-planning discards an approval that was already given.
func TestApprovedPlanIsRecoveredFromTheIssue(t *testing.T) {
	ctx := context.Background()
	const plan = "1. Add the config field.\n2. Thread it through.\n\n---\n\n3. Test it."
	issue := gh.Issue{Number: 5, Comments: []gh.Comment{
		comment("agent-bot", planComment(plan, "old-run", "opus", 0.5)),
		comment("alice", "implement"),
	}}
	cand := candidate{repo: "acme/widgets", number: 5}

	st := openTestStore(t)
	o := testOrchestrator(t, stubGH(t, filepath.Join(t.TempDir(), "calls.txt"), nil), st)

	// The store knows nothing, exactly as it would after being deleted.
	if got := o.approvedPlan(ctx, o.log, cand, "run-1", issue); got != plan {
		t.Fatalf("the approved plan should be recovered from the issue:\n got: %q\nwant: %q", got, plan)
	}

	// And it is written back, so the next run does not have to parse a comment.
	stored, err := st.LatestPlan(ctx, "acme/widgets", 5)
	if err != nil {
		t.Fatal(err)
	}
	if stored != plan {
		t.Fatalf("the recovered plan should be saved, got %q", stored)
	}
	events, err := st.ListEvents(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "plan_recovered" {
		t.Fatalf("recovery should be recorded on the run, got %+v", events)
	}
}

// The store stays authoritative when it does have an answer: recovery is a
// fallback, not a second opinion.
func TestStoredPlanWinsOverTheIssue(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if err := st.SavePlan(ctx, "acme/widgets", 5, "run-0", "the stored plan"); err != nil {
		t.Fatal(err)
	}
	o := testOrchestrator(t, stubGH(t, filepath.Join(t.TempDir(), "calls.txt"), nil), st)

	issue := gh.Issue{Number: 5, Comments: []gh.Comment{
		comment("agent-bot", planComment("a stale plan from a comment", "old-run", "opus", 0)),
	}}
	got := o.approvedPlan(ctx, o.log, candidate{repo: "acme/widgets", number: 5}, "run-1", issue)
	if got != "the stored plan" {
		t.Fatalf("approvedPlan() = %q, want the stored plan", got)
	}
}

// A delivered issue that still carries the trigger label — because a label edit
// failed, or because a human re-added it — would otherwise be rediscovered and
// silently rejected on every poll, forever. Taking the label off is what ends
// that, and it is the last time the issue is ever seen.
func TestADeliveredIssueLosesItsTriggerLabel(t *testing.T) {
	ctx := context.Background()
	callLog := filepath.Join(t.TempDir(), "calls.txt")

	ghBin := stubGH(t, callLog, map[string]string{
		"search issues": `[{"number":5,"title":"Change your commit name","url":"u",
		  "repository":{"name":"widgets","nameWithOwner":"acme/widgets"},
		  "labels":[{"name":"agent-ready"}],"isPullRequest":false,"state":"open"}]`,
		// Delivered, but the trigger label is somehow still on it.
		"issue view": `{"number":5,"title":"Change your commit name","body":"please","url":"u",
		  "state":"OPEN","labels":[{"name":"agent-ready"},{"name":"agent-done"}],
		  "comments":[
		    {"author":{"login":"agent-bot"},"body":"<!-- coding-agent-loop:pr -->\n\nOpened https://github.com/acme/widgets/pull/12"}
		  ]}`,
	})

	st := openTestStore(t)
	if err := testOrchestrator(t, ghBin, st).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	calls := ghCalls(t, callLog)
	if !strings.Contains(calls, "--remove-label agent-ready") {
		t.Fatalf("a delivered issue should lose its trigger label:\n%s", calls)
	}
	// Still no work, and no second PR.
	for _, forbidden := range []string{"pr create", "issue comment"} {
		if strings.Contains(calls, forbidden) {
			t.Fatalf("a delivered issue must not be worked, saw %q:\n%s", forbidden, calls)
		}
	}
	runs, err := st.ListRuns(ctx, "acme/widgets", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("a delivered issue should never be claimed, got %d run(s)", len(runs))
	}
}

// Stopping the daemon mid-run used to be recorded as the issue's failure: a
// public "the coding agent could not complete this issue" comment, a doubled
// back-off, and a sidelined model — every time an operator pressed Ctrl+C.
func TestShutdownMidRunIsNotTheIssuesFailure(t *testing.T) {
	ctx := context.Background()
	callLog := filepath.Join(t.TempDir(), "calls.txt")
	st := openTestStore(t)
	o := testOrchestrator(t, stubGH(t, callLog, nil), st)

	cand := candidate{repo: "acme/widgets", number: 5, title: "Change your commit name"}
	const runID = "run-1"
	if err := st.CreateRun(ctx, store.Run{
		ID: runID, Repo: cand.repo, Issue: cand.number, Attempt: 1,
		Status: store.StatusWorking, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Exactly what execute returns when the run's context is cancelled.
	o.handleFailure(ctx, o.log, cand, runID, 1,
		errCanceled{fmt.Errorf("claude run failed: %w", claude.ErrCanceled)})

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.StatusCanceled {
		t.Errorf("status = %q, want %q", run.Status, store.StatusCanceled)
	}

	hist, err := st.IssueHistory(ctx, cand.repo, cand.number)
	if err != nil {
		t.Fatal(err)
	}
	if hist.Failures != 0 {
		t.Errorf("a shutdown must not count as a failure, got %d", hist.Failures)
	}
	if hist.Attempts != 0 {
		t.Errorf("a shutdown must not consume an attempt, got %d", hist.Attempts)
	}
	if !o.nextAttemptAt(hist).IsZero() {
		t.Error("a shutdown must not back the issue off")
	}

	calls := ghCalls(t, callLog)
	if strings.Contains(calls, "issue comment") {
		t.Fatalf("a shutdown must not leave a failure comment on the issue:\n%s", calls)
	}
	if strings.Contains(calls, "agent-failed") {
		t.Fatalf("a shutdown must not label the issue as failed:\n%s", calls)
	}
}

// The contrast: a genuine failure still reports itself fully.
func TestARealFailureStillReportsItself(t *testing.T) {
	ctx := context.Background()
	callLog := filepath.Join(t.TempDir(), "calls.txt")
	st := openTestStore(t)
	o := testOrchestrator(t, stubGH(t, callLog, nil), st)

	cand := candidate{repo: "acme/widgets", number: 5, title: "Change your commit name"}
	const runID = "run-1"
	if err := st.CreateRun(ctx, store.Run{
		ID: runID, Repo: cand.repo, Issue: cand.number, Attempt: 1,
		Status: store.StatusWorking, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	o.handleFailure(ctx, o.log, cand, runID, 1, fmt.Errorf("claude run failed: something broke"))

	hist, err := st.IssueHistory(ctx, cand.repo, cand.number)
	if err != nil {
		t.Fatal(err)
	}
	if hist.Failures != 1 {
		t.Errorf("a real failure should count, got %d", hist.Failures)
	}
	calls := ghCalls(t, callLog)
	if !strings.Contains(calls, "issue comment") {
		t.Fatalf("a real failure should be reported on the issue:\n%s", calls)
	}
}

// A dry run that quietly spends subscription usage and rewrites a worktree is
// not a dry run. This pins the property that makes the flag worth having.
func TestDryRunSpendsNothingAndTouchesNothing(t *testing.T) {
	ctx := context.Background()
	callLog := filepath.Join(t.TempDir(), "calls.txt")

	ghBin := stubGH(t, callLog, map[string]string{
		"search issues": `[{"number":5,"title":"Change your commit name","url":"u",
		  "repository":{"name":"widgets","nameWithOwner":"acme/widgets"},
		  "labels":[{"name":"agent-ready"}],"isPullRequest":false,"state":"open"}]`,
		"issue view": `{"number":5,"title":"Change your commit name","body":"please","url":"u",
		  "state":"OPEN","labels":[{"name":"agent-ready"}],
		  "comments":[
		    {"author":{"login":"agent-bot"},"body":"<!-- coding-agent-loop:plan -->\n\n## Plan\n\ndo it\n\n---\n\nfooter"},
		    {"author":{"login":"alice"},"body":"implement"}
		  ]}`,
		"pr list": `[]`,
	})

	st := openTestStore(t)
	o := testOrchestrator(t, ghBin, st)
	o.opts.Rehearse = true
	// If anything reached for git or Claude, these paths would have to exist.
	o.opts.Config.Workspace.ReposRoot = "/nonexistent/repos"
	o.opts.Config.Workspace.Root = "/nonexistent/work"

	if err := o.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Nothing was claimed, no run was recorded, no plan was written back.
	runs, err := st.ListRuns(ctx, "acme/widgets", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("a dry run must not record a run, got %d", len(runs))
	}
	if claims, err := st.ActiveClaims(ctx); err != nil {
		t.Fatal(err)
	} else if len(claims) != 0 {
		t.Errorf("a dry run must not claim the issue, got %d claim(s)", len(claims))
	}
	if plan, err := st.LatestPlan(ctx, "acme/widgets", 5); err != nil {
		t.Fatal(err)
	} else if plan != "" {
		t.Error("a dry run must not write to the store")
	}

	// Only read-only gh calls were made.
	calls := ghCalls(t, callLog)
	for _, forbidden := range []string{"issue comment", "issue edit", "pr create", "pr edit", "label create"} {
		if strings.Contains(calls, forbidden) {
			t.Errorf("a dry run must not mutate anything, saw %q:\n%s", forbidden, calls)
		}
	}
	// And it still did the useful part: reporting the decision.
	if !strings.Contains(calls, "issue view") || !strings.Contains(calls, "pr list") {
		t.Errorf("a dry run should still evaluate the issue:\n%s", calls)
	}
}
