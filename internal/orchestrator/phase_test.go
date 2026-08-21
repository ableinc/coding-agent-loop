package orchestrator

import (
	"strings"
	"testing"

	"github.com/ableinc/coding-agent-loop/internal/gh"
)

func comment(author, body string) gh.Comment {
	return gh.Comment{Author: gh.User{Login: author}, Body: body}
}

func TestDecidePhase(t *testing.T) {
	planComment := comment("agent-bot", markerPlan+"\n\n## Plan\ndo the thing")
	prComment := comment("agent-bot", markerPR+"\n\nOpened a PR")
	failureComment := comment("agent-bot", markerFailure+"\n\nfailed")

	tests := []struct {
		name      string
		comments  []gh.Comment
		wantPhase string
		wantHuman string // substring expected in reason, "" to skip check
	}{
		{
			name:      "no comments at all",
			comments:  nil,
			wantPhase: phasePlan,
		},
		{
			name:      "implement requested before any plan exists",
			comments:  []gh.Comment{comment("alice", "implement")},
			wantPhase: phasePlan,
		},
		{
			name:      "plan posted, nothing since",
			comments:  []gh.Comment{planComment},
			wantPhase: phaseWait,
		},
		{
			name:      "exact approval",
			comments:  []gh.Comment{planComment, comment("alice", "implement")},
			wantPhase: phaseImplement,
			wantHuman: "alice",
		},
		{
			name:      "approval is case-insensitive and trims whitespace",
			comments:  []gh.Comment{planComment, comment("alice", "  Implement  ")},
			wantPhase: phaseImplement,
		},
		{
			name:      "a sentence merely containing the word must not approve",
			comments:  []gh.Comment{planComment, comment("alice", "please implement this soon")},
			wantPhase: phasePlan,
		},
		{
			name:      "feedback after the plan sends it back to re-plan",
			comments:  []gh.Comment{planComment, comment("alice", "please also handle the edge case")},
			wantPhase: phasePlan,
		},
		{
			name:      "the harness's own PR comment after a plan is not human feedback",
			comments:  []gh.Comment{planComment, prComment},
			wantPhase: phaseWait,
		},
		{
			name:      "the harness's own failure comment after a plan is not human feedback",
			comments:  []gh.Comment{planComment, failureComment},
			wantPhase: phaseWait,
		},
		{
			name: "a second, revised plan resets the wait",
			comments: []gh.Comment{
				planComment,
				comment("alice", "needs more detail"),
				planComment,
			},
			wantPhase: phaseWait,
		},
		{
			name: "approval only counts against the most recent plan",
			comments: []gh.Comment{
				planComment,
				comment("alice", "implement"), // stale approval of an old plan
				planComment,
				comment("alice", "one more thing"),
			},
			wantPhase: phasePlan,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := gh.Issue{Comments: tt.comments}
			phase, reason := decidePhase(issue)
			if phase != tt.wantPhase {
				t.Fatalf("decidePhase() phase = %q, want %q (reason: %s)", phase, tt.wantPhase, reason)
			}
			if tt.wantHuman != "" && !strings.Contains(reason, tt.wantHuman) {
				t.Fatalf("reason %q should mention %q", reason, tt.wantHuman)
			}
		})
	}
}
