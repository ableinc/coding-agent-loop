package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/gh"
	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/verify"
)

func TestBranchName(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"Add retry logic", "agent/issue-42-add-retry-logic"},
		{"Fix: crash on empty input!", "agent/issue-42-fix-crash-on-empty-input"},
		{"   ", "agent/issue-42"},
		{"", "agent/issue-42"},
		{"...", "agent/issue-42"},
		{"UPPER Case Title", "agent/issue-42-upper-case-title"},
		{"emoji 🚀 in title", "agent/issue-42-emoji-in-title"},
	}
	for _, tc := range tests {
		got := branchName("agent/issue-", 42, tc.title)
		if got != tc.want {
			t.Errorf("branchName(%q) = %q, want %q", tc.title, got, tc.want)
		}
	}
}

func TestBranchNameIsBounded(t *testing.T) {
	got := branchName("agent/issue-", 7, strings.Repeat("very long title ", 20))
	if len(got) > 70 {
		t.Fatalf("branch name should stay short, got %d chars: %q", len(got), got)
	}
	if strings.HasSuffix(got, "-") {
		t.Fatalf("branch name must not end in a separator: %q", got)
	}
}

func TestPRTitleAndCommitSubject(t *testing.T) {
	if got := prTitle("Add retry logic", 42); got != "Add retry logic (#42)" {
		t.Fatalf("prTitle = %q", got)
	}
	if got := prTitle("", 42); !strings.Contains(got, "#42") {
		t.Fatalf("empty title should still reference the issue, got %q", got)
	}
	long := prTitle(strings.Repeat("x", 300), 42)
	if len(long) > 130 {
		t.Fatalf("prTitle should be bounded, got %d chars", len(long))
	}

	if got := commitSubject("Add retry logic\nwith details", 42); got != "Add retry logic" {
		t.Fatalf("commit subject should be the first line only, got %q", got)
	}
	if got := commitSubject(strings.Repeat("y", 200), 42); len(got) > 68 {
		t.Fatalf("commit subject should be bounded, got %d chars", len(got))
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("summary line\nmore detail"); got != "summary line" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine("   "); got != "(no summary)" {
		t.Fatalf("empty summary should be labelled, got %q", got)
	}
}

func TestPRBodyLinksIssueAndReportsVerification(t *testing.T) {
	body := prBody(prReport{
		Repo: "acme/widgets", Issue: 42, RunID: "run-1", ModelID: "claude-opus-5",
		CostUSD: 0.42, Summary: "Added a retry helper.", DiffStat: " main.go | 10 +++",
		Verify: verify.Result{Status: store.VerifyPassed, Command: "go test ./..."}, Attempt: 1,
	})
	for _, want := range []string{"Closes #42", "Added a retry helper.", "go test ./...", "main.go", "claude-opus-5", "run-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "been reviewed by a human") {
		t.Error("PR body should say the work is unreviewed")
	}
}

// A failing test suite must be visible to the reviewer, not quietly dropped.
func TestPRBodySurfacesTestFailure(t *testing.T) {
	body := prBody(prReport{
		Issue: 7, RunID: "run-2",
		Verify: verify.Result{Status: store.VerifyFailed, Command: "go test ./...", Output: "FAIL: TestThing"},
	})
	if !strings.Contains(body, "**Tests failed**") || !strings.Contains(body, "FAIL: TestThing") {
		t.Fatalf("failure not surfaced:\n%s", body)
	}
}

func TestPRBodyHandlesMissingSummary(t *testing.T) {
	body := prBody(prReport{Issue: 1, Verify: verify.Result{Status: store.VerifySkipped}})
	if !strings.Contains(body, "no closing summary") {
		t.Fatalf("missing summary should be stated explicitly:\n%s", body)
	}
	if !strings.Contains(body, "No test command") {
		t.Fatalf("skipped verification should be explained:\n%s", body)
	}
}

func TestFailureCommentExplainsWhenTheNextAttemptIsDue(t *testing.T) {
	next := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	c := failureComment("run-1", 3, "claude run failed", next, "agent-ready")
	if !strings.Contains(c, "attempt 3") || !strings.Contains(c, "try again after") {
		t.Fatalf("retry comment unclear:\n%s", c)
	}
	if !strings.Contains(c, "1 Mar 2026 12:00:00 UTC") {
		t.Fatalf("the next attempt time should be stated:\n%s", c)
	}
	// Retries are unbounded, so the comment has to say what actually stops them.
	if !strings.Contains(c, "Remove the `agent-ready` label") {
		t.Fatalf("the opt-out should be spelled out:\n%s", c)
	}
}

func TestRetryDelayBacksOffExponentiallyAndCaps(t *testing.T) {
	base, max := 15*time.Minute, 4*time.Hour
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 15 * time.Minute},
		{2, 30 * time.Minute},
		{3, time.Hour},
		{4, 2 * time.Hour},
		{5, 4 * time.Hour},
		{6, 4 * time.Hour},
		{99, 4 * time.Hour}, // never grows without bound, never overflows
	}
	for _, tc := range tests {
		if got := retryDelay(tc.failures, base, max); got != tc.want {
			t.Errorf("retryDelay(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
	if got := retryDelay(3, time.Hour, time.Minute); got != time.Hour {
		t.Errorf("a max below base should clamp to base, got %v", got)
	}
}

// The trigger label is the only thing that decides whether an issue is worked,
// so a long history of failures must delay a retry, never cancel it.
func TestNextAttemptAtOnlyDelays(t *testing.T) {
	o := New(Options{Config: config.Config{Run: config.RunConfig{
		RetryBackoff:    config.Duration(15 * time.Minute),
		RetryBackoffMax: config.Duration(24 * time.Hour),
	}}})

	failed := time.Now().Add(-time.Minute)
	next := o.nextAttemptAt(store.IssueState{Failures: 2, LastFailureAt: failed})
	if !next.Equal(failed.Add(30 * time.Minute)) {
		t.Fatalf("next attempt = %v, want %v", next, failed.Add(30*time.Minute))
	}

	stale := o.nextAttemptAt(store.IssueState{Failures: 50, LastFailureAt: time.Now().Add(-72 * time.Hour)})
	if stale.After(time.Now()) {
		t.Fatalf("an old failure must eventually become claimable again, got %v", stale)
	}

	if got := o.nextAttemptAt(store.IssueState{}); !got.IsZero() {
		t.Fatalf("an issue with no failures should be claimable now, got %v", got)
	}
}

func TestIssueComment(t *testing.T) {
	c := issueComment("https://github.com/acme/widgets/pull/9", "run-1",
		verify.Result{Status: store.VerifyPassed, Command: "make test"})
	if !strings.Contains(c, "pull/9") || !strings.Contains(c, "make test") {
		t.Fatalf("issue comment missing detail:\n%s", c)
	}
	// Without the marker, decidePhase would read the harness's own comment
	// back as human feedback and re-plan forever.
	if !isAgentComment(c) {
		t.Fatalf("issue comment must carry the agent marker:\n%s", c)
	}
}

func TestFailureCommentCarriesTheAgentMarker(t *testing.T) {
	c := failureComment("run-1", 1, "boom", time.Time{}, "agent-ready")
	if !isAgentComment(c) {
		t.Fatalf("failure comment must carry the agent marker:\n%s", c)
	}
}

func TestPlanComment(t *testing.T) {
	c := planComment("## Plan\n\ndo the thing", "run-1", "claude-opus-5", 0.42)
	if !isPlanComment(c) {
		t.Fatalf("plan comment must carry the plan marker:\n%s", c)
	}
	for _, want := range []string{"do the thing", "implement", "claude-opus-5", "0.4200"} {
		if !strings.Contains(c, want) {
			t.Errorf("plan comment missing %q:\n%s", want, c)
		}
	}
}

func TestSystemPromptStatesTheHarnessBoundary(t *testing.T) {
	p := systemPrompt("acme/widgets", "agent/issue-42", "/work/wt")
	for _, want := range []string{"acme/widgets", "agent/issue-42", "/work/wt", "git push", "Nobody is watching"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	// Instructing verification causes redundant re-checking on current models.
	for _, unwanted := range []string{"double-check", "verify your work", "re-verify"} {
		if strings.Contains(strings.ToLower(p), unwanted) {
			t.Errorf("system prompt should not instruct self-verification, found %q", unwanted)
		}
	}
}

func TestTaskPromptIncludesIssueContext(t *testing.T) {
	issue := gh.Issue{
		Number: 42, Title: "Add retry logic", Body: "We need retries on 5xx.",
		URL:    "https://github.com/acme/widgets/issues/42",
		Labels: []gh.Label{{Name: "agent-ready"}, {Name: "bug"}},
		Comments: []gh.Comment{
			{Author: gh.User{Login: "alice"}, Body: "Only for idempotent requests."},
		},
	}
	p := implementTaskPrompt("acme/widgets", issue, "")
	for _, want := range []string{"#42", "Add retry logic", "We need retries on 5xx.", "@alice", "Only for idempotent requests.", "agent-ready"} {
		if !strings.Contains(p, want) {
			t.Errorf("task prompt missing %q", want)
		}
	}
}

func TestTaskPromptHandlesEmptyBody(t *testing.T) {
	p := implementTaskPrompt("acme/widgets", gh.Issue{Number: 1, Title: "Do the thing"}, "")
	if !strings.Contains(p, "no description") {
		t.Fatalf("an empty body should be called out:\n%s", p)
	}
}

func TestTaskPromptTrimsLongDiscussions(t *testing.T) {
	issue := gh.Issue{Number: 1, Title: "t", Body: "b"}
	for range 40 {
		issue.Comments = append(issue.Comments, gh.Comment{Author: gh.User{Login: "u"}, Body: "comment"})
	}
	p := implementTaskPrompt("acme/widgets", issue, "")
	if !strings.Contains(p, "showing the last") {
		t.Fatalf("long discussions should be trimmed and say so:\n%s", p)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Fatalf("truncate should leave short input alone, got %q", got)
	}
	got := truncate(strings.Repeat("x", 200), 50)
	if !strings.Contains(got, "truncated") || len(got) > 100 {
		t.Fatalf("truncate = %q", got)
	}
}
