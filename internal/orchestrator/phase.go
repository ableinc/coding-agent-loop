package orchestrator

import (
	"strings"

	"github.com/ableinc/coding-agent-loop/internal/gh"
)

// Phases an issue can be in. Plan and implement are the two phases that ever
// run Claude; wait means the issue is sitting on a human, and done means the
// work has already been delivered.
const (
	phasePlan      = "plan"
	phaseImplement = "implement"
	phaseWait      = "wait"
	phaseDone      = "done"
)

// approvalKeyword is the exact (trimmed, case-insensitive) comment body that
// approves a posted plan. Anything else posted after a plan is read as
// feedback and sends the issue back through another plan pass.
const approvalKeyword = "implement"

// markerPrefix tags every comment the harness itself writes, so decidePhase
// can tell harness narration apart from human input. Without it, the
// harness's own plan/PR/failure comments would be read back as human
// feedback and the issue would re-plan forever.
const markerPrefix = "<!-- coding-agent-loop:"

const (
	markerPlan      = markerPrefix + "plan -->"
	markerPR        = markerPrefix + "pr -->"
	markerFailure   = markerPrefix + "failure -->"
	markerPRComment = markerPrefix + "pr-comment -->"
)

func isAgentComment(body string) bool {
	return strings.Contains(body, markerPrefix)
}

func isPlanComment(body string) bool {
	return strings.Contains(body, markerPlan)
}

func isPRComment(body string) bool {
	return strings.Contains(body, markerPR)
}

// isApproval reports whether a comment is the exact approval keyword.
func isApproval(body string) bool {
	return strings.EqualFold(strings.TrimSpace(body), approvalKeyword)
}

// decidePhase reads an issue's comment history and reports what should happen
// next. It is pure: given the same comments it always returns the same
// answer, so it can run before anything is claimed.
func decidePhase(issue gh.Issue) (phase string, reason string) {
	comments := issue.Comments

	// A PR marker is the durable record that this issue was delivered, and it
	// lives on the issue rather than in the store — so it still holds after the
	// database has been thrown away. Without this check a wiped database would
	// read the original `implement` approval as though the work were still
	// outstanding and open a second pull request for it.
	//
	// The escape hatch is the same keyword that approved the plan: saying
	// `implement` again, after the PR was announced, asks for another attempt.
	prIdx := -1
	for i, c := range comments {
		if isPRComment(c.Body) {
			prIdx = i
		}
	}
	if prIdx >= 0 {
		reopened := false
		for i := prIdx + 1; i < len(comments); i++ {
			if !isAgentComment(comments[i].Body) && isApproval(comments[i].Body) {
				reopened = true
			}
		}
		if !reopened {
			return phaseDone, "a pull request has already been delivered for this issue"
		}
	}

	planIdx := -1
	for i, c := range comments {
		if isPlanComment(c.Body) {
			planIdx = i
		}
	}
	if planIdx == -1 {
		return phasePlan, "no plan has been posted yet"
	}

	lastHumanIdx := -1
	for i := planIdx + 1; i < len(comments); i++ {
		if !isAgentComment(comments[i].Body) {
			lastHumanIdx = i
		}
	}
	if lastHumanIdx == -1 {
		return phaseWait, "plan posted, awaiting human approval"
	}

	c := comments[lastHumanIdx]
	if isApproval(c.Body) {
		author := c.Author.Login
		if author == "" {
			author = "unknown"
		}
		return phaseImplement, "approved by @" + author
	}
	return phasePlan, "human feedback received after the plan, re-planning"
}

// --- plan recovery ----------------------------------------------------------

// planHeader and planFooter bracket the plan body inside a plan comment. They
// have to stay in step with planComment in report.go, which is what writes them.
const (
	planHeader = "## Plan\n\n"
	planFooter = "\n\n---\n\n"
)

// extractPlan is the inverse of planComment: it recovers the plan body from a
// comment the harness posted. The store is the normal place a plan is read
// from, but the store is disposable and the issue is not, so a plan the human
// already approved can always be read back off GitHub.
//
// A truncated plan is deliberately not recovered. Feeding a plan that stops
// mid-sentence to an implement run is worse than implementing from the issue
// alone, because it reads as complete.
func extractPlan(body string) string {
	if !isPlanComment(body) {
		return ""
	}
	i := strings.Index(body, planHeader)
	if i < 0 {
		return ""
	}
	plan := body[i+len(planHeader):]

	// The footer is matched from the end: a plan may well contain its own
	// horizontal rule, and only the last one is the comment's own.
	j := strings.LastIndex(plan, planFooter)
	if j < 0 {
		return ""
	}
	plan = strings.TrimSpace(plan[:j])
	if strings.HasSuffix(plan, truncationSuffix) {
		return ""
	}
	return plan
}

// latestPlanBody returns the plan from the most recent plan comment on an
// issue, or "" when there is none to recover.
func latestPlanBody(issue gh.Issue) string {
	for i := len(issue.Comments) - 1; i >= 0; i-- {
		if plan := extractPlan(issue.Comments[i].Body); plan != "" {
			return plan
		}
	}
	return ""
}
