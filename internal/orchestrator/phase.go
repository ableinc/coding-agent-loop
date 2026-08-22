package orchestrator

import (
	"strings"

	"github.com/ableinc/coding-agent-loop/internal/gh"
)

// Phases an issue can be in. Plan and implement are the two phases that ever
// run Claude; wait means the issue is sitting on a human.
const (
	phasePlan      = "plan"
	phaseImplement = "implement"
	phaseWait      = "wait"
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

// decidePhase reads an issue's comment history and reports what should happen
// next. It is pure: given the same comments it always returns the same
// answer, so it can run before anything is claimed.
func decidePhase(issue gh.Issue) (phase string, reason string) {
	comments := issue.Comments

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
	if strings.EqualFold(strings.TrimSpace(c.Body), approvalKeyword) {
		author := c.Author.Login
		if author == "" {
			author = "unknown"
		}
		return phaseImplement, "approved by @" + author
	}
	return phasePlan, "human feedback received after the plan, re-planning"
}
