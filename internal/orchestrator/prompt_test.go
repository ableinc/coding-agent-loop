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
