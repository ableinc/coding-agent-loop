package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/gh"
)

// Sanity check against the live repository, using only read-only gh calls.
// Skipped unless SANITY_REPO names one, so it never runs in CI.
func TestAgainstRealRepository(t *testing.T) {
	repo := os.Getenv("SANITY_REPO")
	if repo == "" {
		t.Skip("set SANITY_REPO=owner/name to run against a live repository")
	}
	ctx := context.Background()
	c := gh.New("gh", true) // dry-run: no mutation is possible from here
	prefix := config.Default().Workspace.BranchPrefix

	numbers, err := recentIssues(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range numbers {
		issue, err := c.ViewIssue(ctx, repo, number)
		if err != nil {
			t.Logf("issue #%d: %v", number, err)
			continue
		}
		phase, reason := decidePhase(issue)
		plan := latestPlanBody(issue)
		branch := branchName(prefix, issue.Number, issue.Title)

		pr, found, err := c.FindPRForIssue(ctx, repo, number, branch, prefix)
		if err != nil {
			t.Fatal(err)
		}
		match := "none"
		if found {
			match = pr.URL + "  state=" + pr.State + "  head=" + pr.HeadRefName +
				"  linked=" + boolStr(gh.PRLinksIssue(pr, number))
		}
		t.Logf("issue #%d %q [%s]\n    phase:  %s (%s)\n    branch: %s\n    PR:     %s\n    plan:   %d chars recoverable",
			issue.Number, issue.Title, issue.State, phase, reason, branch, match, len(plan))
	}
}

// recentIssues lists the repository's issues, newest first, so the check is not
// pinned to numbers that only exist in one repository.
func recentIssues(ctx context.Context, repo string) ([]int, error) {
	out, err := exec.CommandContext(ctx, "gh", "issue", "list",
		"--repo", repo, "--state", "all", "--limit", "10", "--json", "number").Output()
	if err != nil {
		return nil, err
	}
	var issues []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, err
	}
	numbers := make([]int, 0, len(issues))
	for _, i := range issues {
		numbers = append(numbers, i.Number)
	}
	return numbers, nil
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
