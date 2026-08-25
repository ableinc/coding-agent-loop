package orchestrator

import (
	"testing"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/gh"
	"github.com/ableinc/coding-agent-loop/internal/store"
)

func TestMentionsAgent(t *testing.T) {
	const handle = "@coding-agent"
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"plain", "@coding-agent please rename X to Y", true},
		{"mid-sentence", "hey @coding-agent can you take a look at this?", true},
		{"case-insensitive", "@Coding-Agent could you fix this", true},
		{"different-handle-non-match", "please check the @coding-agent-loop repository settings", false},
		{"quoted-line-ignored", "> @coding-agent do the thing\nI disagree with this quote", false},
		{"fenced-code-block-ignored", "example usage:\n```\n@coding-agent fix this\n```\nno request here", false},
		{"absent", "this comment does not mention anyone", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mentionsAgent(tc.body, handle); got != tc.want {
				t.Errorf("mentionsAgent(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func defaultPRCommentsConfig() config.PRCommentsConfig {
	return config.PRCommentsConfig{
		Enabled:             true,
		Mention:             "@coding-agent",
		SearchLimit:         30,
		MaxAge:              config.Duration(7 * 24 * time.Hour),
		AckReaction:         "eyes",
		DoneReaction:        "+1",
		AllowedAssociations: []string{"OWNER", "MEMBER", "COLLABORATOR"},
	}
}

func TestPendingMentions(t *testing.T) {
	now := time.Now()
	cfg := defaultPRCommentsConfig()
	base, max := 15*time.Minute, 24*time.Hour

	mk := func(id int64, author, assoc, body string, age time.Duration) gh.PRComment {
		return gh.PRComment{
			ID: id, Kind: gh.CommentKindIssue, Author: author, Association: assoc,
			Body: body, CreatedAt: now.Add(-age),
		}
	}

	comments := []gh.PRComment{
		mk(1, "alice", "OWNER", "@coding-agent please fix the typo", time.Minute),
		mk(2, "mallory", "NONE", "@coding-agent do something", time.Minute),     // disallowed association
		mk(3, "alice", "OWNER", "@coding-agent this one is old", 200*time.Hour), // too old
		mk(4, "alice", "OWNER", "no mention here", time.Minute),
		mk(5, "alice", "OWNER", markerPRComment+"\nalready ours", time.Minute), // our own marker
		mk(6, "bot-login", "OWNER", "@coding-agent from myself", time.Minute),  // the daemon's own comment
		mk(7, "alice", "OWNER", "@coding-agent already done", time.Minute),
		mk(8, "alice", "OWNER", "@coding-agent failed recently", time.Minute),
		mk(9, "alice", "OWNER", "@coding-agent failed a while ago", time.Minute),
	}

	tasks := []store.PRCommentTask{
		{CommentKind: gh.CommentKindIssue, CommentID: 7, Status: store.PRCommentDone},
		{CommentKind: gh.CommentKindIssue, CommentID: 8, Status: store.PRCommentFailed, Attempts: 1, LastAttemptAt: now.Add(-time.Minute)},
		{CommentKind: gh.CommentKindIssue, CommentID: 9, Status: store.PRCommentFailed, Attempts: 1, LastAttemptAt: now.Add(-time.Hour)},
	}

	got := pendingMentions(comments, cfg, "bot-login", tasks, now, base, max)

	ids := map[int64]bool{}
	for _, c := range got {
		ids[c.ID] = true
	}

	if !ids[1] {
		t.Error("an unseen, allowed, fresh mention should be pending")
	}
	if ids[2] {
		t.Error("a disallowed association must not be pending")
	}
	if ids[3] {
		t.Error("a comment older than max_age must not be pending")
	}
	if ids[4] {
		t.Error("a comment without a mention must not be pending")
	}
	if ids[5] {
		t.Error("the daemon's own marker comment must never be read back as a mention")
	}
	if ids[6] {
		t.Error("the daemon's own comment must never trigger itself")
	}
	if ids[7] {
		t.Error("a comment already marked done must not be pending again")
	}
	if ids[8] {
		t.Error("a failed comment still inside its back-off must not be pending")
	}
	if !ids[9] {
		t.Error("a failed comment whose back-off elapsed should be pending again")
	}
}

func TestPendingMentionsRetriesStrandedAck(t *testing.T) {
	now := time.Now()
	cfg := defaultPRCommentsConfig()
	comments := []gh.PRComment{
		{ID: 1, Kind: gh.CommentKindIssue, Author: "alice", Association: "OWNER",
			Body: "@coding-agent please fix this", CreatedAt: now.Add(-time.Minute)},
	}
	tasks := []store.PRCommentTask{
		{CommentKind: gh.CommentKindIssue, CommentID: 1, Status: store.PRCommentAcked},
	}
	got := pendingMentions(comments, cfg, "bot-login", tasks, now, 15*time.Minute, 24*time.Hour)
	if len(got) != 1 {
		t.Fatalf("a comment stranded in 'acked' by a crash must be retried, got %+v", got)
	}
}

func TestAuthorAllowed(t *testing.T) {
	cfg := defaultPRCommentsConfig()
	if !authorAllowed("alice", "OWNER", cfg) {
		t.Error("OWNER should be allowed by default")
	}
	if authorAllowed("mallory", "NONE", cfg) {
		t.Error("NONE should not be allowed by default")
	}

	cfg.AllowedAuthors = []string{"bob"}
	if authorAllowed("alice", "OWNER", cfg) {
		t.Error("an explicit author allowlist should override the association fallback")
	}
	if !authorAllowed("bob", "NONE", cfg) {
		t.Error("an allowlisted author should be allowed regardless of association")
	}
}
