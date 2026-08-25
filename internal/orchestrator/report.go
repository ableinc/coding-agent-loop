package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/gh"
	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/verify"
)

// prReport is everything the PR body needs.
type prReport struct {
	Repo      string
	Issue     int
	RunID     string
	SessionID string
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
	fmt.Fprintf(&b, "%s (run `%s`, attempt %d, model `%s`, cost $%.4f",
		gh.ProvenanceMarker, r.RunID, r.Attempt, r.ModelID, r.CostUSD)
	if r.SessionID != "" {
		fmt.Fprintf(&b, ", session `%s`", r.SessionID)
	}
	b.WriteString("). ")
	b.WriteString("Nothing here has been reviewed by a human yet.\n")

	return b.String()
}

// issueComment is the short note left on the issue when a PR is opened.
func issueComment(prURL, runID string, v verify.Result) string {
	var b strings.Builder
	b.WriteString(markerPR)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Opened a draft pull request for this issue: %s\n\n", prURL)
	switch v.Status {
	case store.VerifyPassed:
		fmt.Fprintf(&b, "Tests passed (`%s`).\n", v.Command)
	case store.VerifyFailed:
		fmt.Fprintf(&b, "Tests failed (`%s`) — see the PR for output.\n", v.Command)
	default:
		b.WriteString("No test command was detected for this repository.\n")
	}
	// The marker in this comment is what stops the issue being worked again, so
	// the way back out has to be written where a human will read it.
	b.WriteString("\nComment `implement` again if you want another attempt at this issue.\n")
	fmt.Fprintf(&b, "\n<sub>coding-agent-loop run `%s`</sub>\n", runID)
	return b.String()
}

// adoptedComment is left on the issue when a pull request that already covers
// it is found. It carries the same marker as issueComment, because it plays the
// same role for decidePhase — but it says plainly that the PR was found rather
// than opened by this run, so nobody reads it as a second delivery.
func adoptedComment(prURL, runID, state string) string {
	var b strings.Builder
	b.WriteString(markerPR)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "This issue is already covered by an existing pull request, so no new work was done: %s\n\n", prURL)
	if st := strings.TrimSpace(state); st != "" {
		fmt.Fprintf(&b, "That pull request is %s. ", strings.ToLower(st))
	}
	b.WriteString("Comment `implement` again if you want another attempt at this issue.\n")
	fmt.Fprintf(&b, "\n<sub>coding-agent-loop run `%s`</sub>\n", runID)
	return b.String()
}

// failureComment explains an unsuccessful attempt on the issue, and when the
// next one is due. Nothing is abandoned for failing too often, so the comment
// says what actually stops the retries: taking the trigger label off.
func failureComment(runID string, attempt int, reason string, nextAttempt time.Time, triggerLabel string) string {
	var b strings.Builder
	b.WriteString(markerFailure)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "The coding agent could not complete this issue (attempt %d).\n\n", attempt)
	b.WriteString("```\n")
	b.WriteString(strings.TrimSpace(reason))
	b.WriteString("\n```\n\n")
	if nextAttempt.IsZero() {
		b.WriteString("It will try again on a later pass.")
	} else {
		fmt.Fprintf(&b, "It will try again after %s (the wait doubles with each consecutive failure).",
			nextAttempt.UTC().Format(time.RFC1123))
	}
	fmt.Fprintf(&b, " Remove the `%s` label to stop it retrying.\n", triggerLabel)
	fmt.Fprintf(&b, "\n<sub>coding-agent-loop run `%s`</sub>\n", runID)
	return b.String()
}

// maxPlanCommentChars keeps a plan comment under GitHub's ~65536-character
// comment body limit, leaving room for the marker and footer.
const maxPlanCommentChars = 60000

// planComment is what gets posted after a planning run: the plan itself, and
// instructions for how a human moves the issue forward.
func planComment(plan, runID, modelID string, costUSD float64) string {
	var b strings.Builder
	b.WriteString(markerPlan)
	b.WriteString("\n\n## Plan\n\n")
	b.WriteString(truncate(strings.TrimSpace(plan), maxPlanCommentChars))
	b.WriteString("\n\n---\n\n")
	b.WriteString("Reply with exactly `implement` to approve this plan and start the change. ")
	b.WriteString("Reply with anything else and the plan will be revised to address it.\n\n")
	fmt.Fprintf(&b, "<sub>coding-agent-loop run `%s`, model `%s`, cost $%.4f</sub>\n", runID, modelID, costUSD)
	return b.String()
}
