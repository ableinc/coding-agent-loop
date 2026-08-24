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
			name:      "a PR announcement after a plan means the work was delivered",
			comments:  []gh.Comment{planComment, prComment},
			wantPhase: phaseDone,
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

		// The PR announcement is the only record of delivered work that
		// survives the store being deleted, so it has to outrank everything
		// that led up to it.
		{
			name: "a delivered issue stays delivered",
			comments: []gh.Comment{
				planComment,
				comment("alice", "implement"),
				prComment,
			},
			wantPhase: phaseDone,
		},
		{
			name:      "a PR with no plan at all still counts as delivered",
			comments:  []gh.Comment{prComment},
			wantPhase: phaseDone,
		},
		{
			name: "the harness's own failure comment after a PR does not reopen it",
			comments: []gh.Comment{
				planComment, comment("alice", "implement"), prComment, failureComment,
			},
			wantPhase: phaseDone,
		},
		{
			name: "ordinary human chatter after a PR does not reopen it",
			comments: []gh.Comment{
				planComment, comment("alice", "implement"), prComment,
				comment("bob", "thanks, looks good"),
			},
			wantPhase: phaseDone,
		},
		{
			name: "saying implement again after a PR asks for another attempt",
			comments: []gh.Comment{
				planComment, comment("alice", "implement"), prComment,
				comment("alice", "implement"),
			},
			wantPhase: phaseImplement,
		},
		{
			name: "feedback after a PR, then implement, re-plans first",
			comments: []gh.Comment{
				planComment, comment("alice", "implement"), prComment,
				comment("alice", "implement"),
				comment("alice", "actually, do it differently"),
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

// The store is disposable and the issue is not, so anything planComment writes
// has to be readable back off the issue.
func TestExtractPlanRoundTrip(t *testing.T) {
	plans := map[string]string{
		"plain":                  "Do the thing, then the other thing.",
		"with a horizontal rule": "## Step one\n\nfirst\n\n---\n\n## Step two\n\nsecond",
		"with a fenced block":    "Run:\n\n```sh\ngo test ./...\n```\n\nThen ship it.",
		"with the marker text":   "Mention " + markerPlan + " inside the plan body.",
	}
	for name, plan := range plans {
		t.Run(name, func(t *testing.T) {
			got := extractPlan(planComment(plan, "run-1", "opus", 1.25))
			if got != plan {
				t.Fatalf("round trip lost the plan:\n got: %q\nwant: %q", got, plan)
			}
		})
	}
}

func TestExtractPlanRejectsWhatItCannotRecover(t *testing.T) {
	cases := map[string]string{
		"a human comment":             "implement",
		"the harness's PR comment":    markerPR + "\n\nOpened a PR",
		"a plan comment with no body": markerPlan + "\n\nnothing structured here",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := extractPlan(body); got != "" {
				t.Fatalf("extractPlan(%q) = %q, want %q", body, got, "")
			}
		})
	}

	// A plan that was cut to fit the comment limit is worse than no plan: it
	// reads as complete but stops mid-sentence.
	t.Run("a truncated plan", func(t *testing.T) {
		long := strings.Repeat("a very long plan. ", maxPlanCommentChars/10)
		if got := extractPlan(planComment(long, "run-1", "opus", 0)); got != "" {
			t.Fatalf("a truncated plan must not be recovered, got %d chars", len(got))
		}
	})
}

func TestLatestPlanBody(t *testing.T) {
	issue := gh.Issue{Comments: []gh.Comment{
		comment("alice", "please fix this"),
		comment("agent-bot", planComment("the first plan", "run-1", "opus", 0)),
		comment("alice", "needs more detail"),
		comment("agent-bot", planComment("the revised plan", "run-2", "opus", 0)),
		comment("alice", "implement"),
	}}
	if got := latestPlanBody(issue); got != "the revised plan" {
		t.Fatalf("latestPlanBody() = %q, want the most recent plan", got)
	}

	if got := latestPlanBody(gh.Issue{Comments: []gh.Comment{comment("alice", "hi")}}); got != "" {
		t.Fatalf("latestPlanBody() = %q, want %q when nothing was planned", got, "")
	}
}
