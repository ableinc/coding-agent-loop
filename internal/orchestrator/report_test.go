package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/gh"
	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/verify"
)

func TestPRCommentCommentCarriesMarkerAndHandledURLs(t *testing.T) {
	handled := []gh.PRComment{
		{ID: 1, URL: "https://github.com/acme/widgets/pull/9#issuecomment-1"},
		{ID: 2, URL: "https://github.com/acme/widgets/pull/9#discussion_r2"},
	}
	body := prCommentComment(handled, "Renamed Foo to Bar as requested.", "run-1",
		verify.Result{Status: store.VerifyPassed, Command: "go test ./..."}, true)

	if !strings.Contains(body, markerPRComment) {
		t.Fatalf("reply must carry the pr-comment marker so it is never read back as a mention:\n%s", body)
	}
	for _, c := range handled {
		if !strings.Contains(body, c.URL) {
			t.Errorf("reply missing handled comment URL %q:\n%s", c.URL, body)
		}
	}
	if !strings.Contains(body, "Renamed Foo to Bar") {
		t.Fatalf("reply should carry the agent's summary:\n%s", body)
	}
	if !strings.Contains(body, "run-1") {
		t.Fatalf("reply should carry the run id:\n%s", body)
	}
	if !strings.Contains(body, "Tests passed") {
		t.Fatalf("reply should report verification when code was pushed:\n%s", body)
	}
}

func TestPRCommentCommentWithoutAPush(t *testing.T) {
	handled := []gh.PRComment{{ID: 1, URL: "u1"}}
	body := prCommentComment(handled, "It already works as-is.", "run-1", verify.Result{}, false)
	if !strings.Contains(body, "No code changes were needed") {
		t.Fatalf("a question answered in prose should say no code changed:\n%s", body)
	}
}

func TestPRCommentFailureCommentCarriesMarkerAndReason(t *testing.T) {
	next := time.Now().Add(30 * time.Minute)
	body := prCommentFailureComment("run-2", "claude run failed: boom", next)
	if !strings.Contains(body, markerPRComment) {
		t.Fatalf("failure reply must carry the pr-comment marker:\n%s", body)
	}
	if !strings.Contains(body, "boom") {
		t.Fatalf("failure reply should carry the reason:\n%s", body)
	}
	if !strings.Contains(body, "run-2") {
		t.Fatalf("failure reply should carry the run id:\n%s", body)
	}
}
