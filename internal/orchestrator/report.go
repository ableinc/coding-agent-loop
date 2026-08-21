package orchestrator

import (
	"fmt"
	"strings"

	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/verify"
)

// prReport is everything the PR body needs.
type prReport struct {
	Repo      string
	Issue     int
	RunID     string
	ModelID   string
	CostUSD   float64
	Summary   string
	DiffStat  string
	Verify    verify.Result
	Attempt   int
	Truncated bool
}

// prBody renders the draft PR description. It leads with what a reviewer needs
// (what changed, whether tests pass) and puts provenance at the end.
func prBody(r prReport) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Closes #%d\n\n", r.Issue)

	b.WriteString("## What the agent did\n\n")
	summary := strings.TrimSpace(r.Summary)
	if summary == "" {
		summary = "_The agent produced no closing summary._"
	}
	b.WriteString(summary)
	b.WriteString("\n\n")

	b.WriteString("## Verification\n\n")
	switch r.Verify.Status {
	case store.VerifyPassed:
		fmt.Fprintf(&b, "Tests passed: `%s`\n\n", r.Verify.Command)
	case store.VerifyFailed:
		fmt.Fprintf(&b, "**Tests failed** (`%s`). This PR is a draft — the failure is reported "+
			"rather than hidden, so you can judge whether the change is salvageable.\n\n", r.Verify.Command)
		if out := strings.TrimSpace(r.Verify.Output); out != "" {
			b.WriteString("<details><summary>Test output (tail)</summary>\n\n```\n")
			b.WriteString(out)
			b.WriteString("\n```\n\n</details>\n\n")
		}
	default:
		b.WriteString("No test command was configured or detected for this repository, so nothing was run.\n\n")
	}

	if stat := strings.TrimSpace(r.DiffStat); stat != "" {
		b.WriteString("## Changes\n\n```\n")
		b.WriteString(stat)
		b.WriteString("\n```\n\n")
	}

	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "Opened automatically by coding-agent-loop (run `%s`, attempt %d, model `%s`, cost $%.4f). ",
		r.RunID, r.Attempt, r.ModelID, r.CostUSD)
	b.WriteString("Nothing here has been reviewed by a human yet.\n")

	return b.String()
}

// issueComment is the short note left on the issue when a PR is opened.
func issueComment(prURL, runID string, v verify.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Opened a draft pull request for this issue: %s\n\n", prURL)
	switch v.Status {
	case store.VerifyPassed:
		fmt.Fprintf(&b, "Tests passed (`%s`).\n", v.Command)
	case store.VerifyFailed:
		fmt.Fprintf(&b, "Tests failed (`%s`) — see the PR for output.\n", v.Command)
	default:
		b.WriteString("No test command was detected for this repository.\n")
	}
	fmt.Fprintf(&b, "\n<sub>coding-agent-loop run `%s`</sub>\n", runID)
	return b.String()
}

// failureComment explains an unsuccessful attempt on the issue.
func failureComment(runID string, attempt, maxAttempts int, reason string, willRetry bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The coding agent could not complete this issue (attempt %d of %d).\n\n", attempt, maxAttempts)
	b.WriteString("```\n")
	b.WriteString(strings.TrimSpace(reason))
	b.WriteString("\n```\n\n")
	if willRetry {
		b.WriteString("It will try again on a later pass.\n")
	} else {
		b.WriteString("No further attempts will be made automatically. Remove and re-add the trigger label to retry.\n")
	}
	fmt.Fprintf(&b, "\n<sub>coding-agent-loop run `%s`</sub>\n", runID)
	return b.String()
}
