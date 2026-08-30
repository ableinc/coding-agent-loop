package orchestrator

import (
	"strings"
	"testing"

	"github.com/ableinc/coding-agent-loop/internal/gh"
)

func TestImplementTaskPromptCarriesTheApprovedPlanVerbatim(t *testing.T) {
	issue := gh.Issue{Number: 7, Title: "Add retry logic"}
	plan := "## Plan\n\nEdit internal/foo/bar.go to add a retry loop."

	p := implementTaskPrompt("acme/widgets", issue, plan)
	if !strings.Contains(p, plan) {
		t.Fatalf("implement prompt should carry the approved plan verbatim:\n%s", p)
	}
	if !strings.Contains(p, "Approved plan") {
		t.Fatalf("implement prompt should label the plan section:\n%s", p)
	}
}

func TestImplementTaskPromptWithoutAPlan(t *testing.T) {
	issue := gh.Issue{Number: 7, Title: "Add retry logic"}
	p := implementTaskPrompt("acme/widgets", issue, "")
	if strings.Contains(p, "Approved plan") {
		t.Fatalf("no plan section should appear when there is no plan:\n%s", p)
	}
}

func TestPlanTaskPromptWithoutAPreviousPlan(t *testing.T) {
	issue := gh.Issue{Number: 9, Title: "Support pagination"}
	p := planTaskPrompt("acme/widgets", issue, "")
	if !strings.Contains(p, "#9") || !strings.Contains(p, "Support pagination") {
		t.Fatalf("plan prompt should reference the issue:\n%s", p)
	}
	if strings.Contains(p, "Previous plan") {
		t.Fatalf("a fresh plan prompt should not mention a previous plan:\n%s", p)
	}
}

func TestPlanTaskPromptIncludesThePreviousPlanOnReplan(t *testing.T) {
	issue := gh.Issue{Number: 9, Title: "Support pagination"}
	previous := "## Plan\n\nAdd a `page` query param."

	p := planTaskPrompt("acme/widgets", issue, previous)
	if !strings.Contains(p, previous) {
		t.Fatalf("re-plan prompt should carry the previous plan verbatim:\n%s", p)
	}
	if !strings.Contains(p, "Previous plan") {
		t.Fatalf("re-plan prompt should label the previous plan:\n%s", p)
	}
}

// The harness sets the commit identity for the worktree before Claude runs;
// this stops a helpful model from "fixing" an unfamiliar author.
func TestSystemPromptForbidsChangingGitIdentity(t *testing.T) {
	p := systemPrompt("acme/widgets", "agent/issue-9", "/work/widgets/issue-9")
	for _, want := range []string{"user.name", "user.email", "--author", "already set the commit identity"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing %q:\n%s", want, p)
		}
	}
}

func TestPlanSystemPromptForbidsEditing(t *testing.T) {
	p := planSystemPrompt("acme/widgets", "/work/widgets/issue-9")
	for _, want := range []string{"Do NOT edit", "Do NOT", "plan"} {
		if !strings.Contains(p, want) {
			t.Errorf("plan system prompt missing %q:\n%s", want, p)
		}
	}
}

func TestPRCommentTaskPromptIncludesEveryCommentAndDiffHunk(t *testing.T) {
	pr := gh.PullRequest{Number: 12, Title: "Add retry logic", URL: "https://github.com/acme/widgets/pull/12"}
	comments := []gh.PRComment{
		{Kind: gh.CommentKindIssue, Author: "alice", Body: "please rename Foo to Bar"},
		{Kind: gh.CommentKindReview, Author: "bob", Body: "this is unsafe",
			Path: "main.go", Line: 42, DiffHunk: "@@ -1,3 +1,3 @@\n-old\n+new"},
	}
	p := prCommentTaskPrompt("acme/widgets", pr, comments, []string{"looks good overall"})

	for _, want := range []string{
		"#12", "Add retry logic", "please rename Foo to Bar", "@bob", "this is unsafe",
		"main.go", "42", "@@ -1,3 +1,3 @@", "looks good overall",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("pr comment task prompt missing %q:\n%s", want, p)
		}
	}
}

func TestPRCommentTaskPromptWithoutReviews(t *testing.T) {
	pr := gh.PullRequest{Number: 12, Title: "Add retry logic"}
	p := prCommentTaskPrompt("acme/widgets", pr, []gh.PRComment{{Author: "alice", Body: "fix this"}}, nil)
	if strings.Contains(p, "Review summaries") {
		t.Fatalf("no review section should appear when there are no reviews:\n%s", p)
	}
}

func TestPRCommentSystemPromptForbidsCreatingPRs(t *testing.T) {
	p := prCommentSystemPrompt("acme/widgets", "agent/issue-9", "/work/widgets/issue-9")
	for _, want := range []string{"Do NOT create branches, tags, pull requests", "already exists", "final summary"} {
		if !strings.Contains(p, want) {
			t.Errorf("pr comment system prompt missing %q:\n%s", want, p)
		}
	}
}

// PR comments beyond maxPRCommentsInclu must be dropped, keeping the newest.
func TestPRCommentTaskPromptCapsCommentCount(t *testing.T) {
	pr := gh.PullRequest{Number: 12, Title: "Add retry logic"}
	var comments []gh.PRComment
	for i := 0; i < maxPRCommentsInclu+5; i++ {
		comments = append(comments, gh.PRComment{Author: "alice", Body: strings.Repeat("x", 10) + " " + string(rune('a'+i))})
	}
	p := prCommentTaskPrompt("acme/widgets", pr, comments, nil)
	if !strings.Contains(p, "showing the last") {
		t.Fatalf("prompt should say how many comments were dropped:\n%s", p)
	}
	// The oldest comment's marker must be gone; the newest must survive.
	oldest := string(rune('a' + 0))
	newest := string(rune('a' + len(comments) - 1))
	if strings.Contains(p, "x "+oldest) {
		t.Fatalf("oldest comment should have been dropped:\n%s", p)
	}
	if !strings.Contains(p, "x "+newest) {
		t.Fatalf("newest comment should be kept:\n%s", p)
	}
}

// The harness's own comments (plan, PR announcement, failure notes) and bare
// approvals carry nothing the "Approved plan" section doesn't already state,
// and on a retry they are pure noise fed back to the model. See issue #18.
func TestIssueContextFiltersOutAgentCommentsAndApprovals(t *testing.T) {
	issue := gh.Issue{Number: 7, Title: "Add retry logic", Comments: []gh.Comment{
		{Author: gh.User{Login: "the-bot"}, Body: markerPlan + "\n\n## Plan\n\nDo the thing."},
		{Author: gh.User{Login: "alice"}, Body: "please also handle timeouts"},
		{Author: gh.User{Login: "alice"}, Body: "implement"},
		{Author: gh.User{Login: "the-bot"}, Body: markerFailure + "\n\nrun failed: boom"},
	}}
	p := issueContext("acme/widgets", issue)
	if strings.Contains(p, "boom") || strings.Contains(p, "Do the thing") {
		t.Fatalf("harness-authored comments must be filtered out:\n%s", p)
	}
	if strings.Contains(p, "implement") {
		t.Fatalf("a bare approval must be filtered out:\n%s", p)
	}
	if !strings.Contains(p, "please also handle timeouts") {
		t.Fatalf("genuine human feedback must survive the filter:\n%s", p)
	}
}

// When filtering leaves no comments at all, the Discussion header must not
// appear — an empty section is worse than none.
func TestIssueContextOmitsEmptyDiscussion(t *testing.T) {
	issue := gh.Issue{Number: 7, Title: "Add retry logic", Comments: []gh.Comment{
		{Author: gh.User{Login: "the-bot"}, Body: markerPlan + "\n\n## Plan\n\nDo the thing."},
		{Author: gh.User{Login: "alice"}, Body: "implement"},
	}}
	p := issueContext("acme/widgets", issue)
	if strings.Contains(p, "### Discussion") {
		t.Fatalf("an all-filtered discussion should omit the header entirely:\n%s", p)
	}
}

// A real-shaped implement prompt must stay well under the old worst case
// (~36K chars for the issue alone, plus an untruncated plan on top). This is
// a regression guard against the constants drifting back up.
func TestImplementTaskPromptStaysBounded(t *testing.T) {
	issue := gh.Issue{
		Number: 42,
		Title:  "A moderately complex feature request",
		Body:   strings.Repeat("This is the issue body. ", 1000), // ~24K chars
	}
	for i := 0; i < 20; i++ {
		body := strings.Repeat("Some discussion text. ", 200) // ~4.6K chars
		if i%2 == 0 {
			body = markerFailure + "\n\n" + body
		}
		issue.Comments = append(issue.Comments, gh.Comment{Author: gh.User{Login: "someone"}, Body: body})
	}
	plan := strings.Repeat("Plan detail line.\n", 3000) // ~57K chars

	p := implementTaskPrompt("acme/widgets", issue, plan)
	const ceiling = 40000
	if len(p) > ceiling {
		t.Fatalf("implement prompt is %d chars, want under %d", len(p), ceiling)
	}
}
