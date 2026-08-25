package discord

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/claude"
	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/verify"
)

// stubWebhook captures POSTed bodies so tests can assert on the JSON a
// Notifier method actually sent, without hitting real Discord.
func stubWebhook(t *testing.T) (url string, bodies func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var captured [][]byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, buf)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	return srv.URL, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]byte, len(captured))
		copy(out, captured)
		return out
	}
}

func waitForCount(t *testing.T, bodies func() [][]byte, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := bodies(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d posted notification(s), got %d", n, len(bodies()))
	return nil
}

// testRef is the run every test in this file reports on.
func testRef() RunRef {
	return RunRef{
		Repo: "acme/widgets", Issue: 42, Title: "Add retries",
		URL: "https://github.com/acme/widgets/issues/42", RunID: "run-1", Attempt: 1,
	}
}

func decodeEmbed(t *testing.T, body []byte) embed {
	t.Helper()
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(payload.Embeds))
	}
	return payload.Embeds[0]
}

func field(e embed, name string) (string, bool) {
	for _, f := range e.Fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

func TestRunClaimedPostsEmbed(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	n.RunClaimed(testRef(), 2)
	n.Close(2 * time.Second)

	e := decodeEmbed(t, waitForCount(t, bodies, 1)[0])
	if e.Title != "Run claimed: acme/widgets#42" {
		t.Errorf("unexpected title: %q", e.Title)
	}
	if e.Timestamp == "" {
		t.Error("timestamp should be set")
	}
	// The issue title and link say what the work is, not only which number.
	if !strings.Contains(e.Description, "Add retries") || !strings.Contains(e.Description, "issues/42") {
		t.Errorf("description should link the issue, got %q", e.Description)
	}
	if v, ok := field(e, "Run ID"); !ok || v != "run-1" {
		t.Errorf("expected a Run ID field, got %+v", e.Fields)
	}
	if v, ok := field(e, "Previous failures"); !ok || v != "2" {
		t.Errorf("a retry should say how many attempts preceded it, got %+v", e.Fields)
	}
}

// The session ID is what ties a notification back to the sessions table.
func TestClaudeFinishedReportsSessionAndSpend(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	n.ClaudeFinished(testRef(), &claude.Result{
		SessionID: "sess-abc", NumTurns: 7, TotalCostUSD: 0.25,
		Usage:      claude.Usage{InputTokens: 100, OutputTokens: 40},
		ModelUsage: map[string]claude.ModelUsage{"claude-opus-5-20260101": {CanonicalModel: "claude-opus-5", OutputTokens: 40}},
	}, 90*time.Second)
	n.Close(2 * time.Second)

	e := decodeEmbed(t, waitForCount(t, bodies, 1)[0])
	for name, want := range map[string]string{
		"Session":  "sess-abc",
		"Model":    "claude-opus-5",
		"Turns":    "7",
		"Cost":     "$0.2500",
		"Duration": "1m30s",
	} {
		if got, ok := field(e, name); !ok || got != want {
			t.Errorf("field %q = %q (present=%v), want %q", name, got, ok, want)
		}
	}
}

// A nil result means the CLI never got far enough to report anything.
func TestClaudeFinishedIgnoresANilResult(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	n.ClaudeFinished(testRef(), nil, time.Second)
	n.Close(200 * time.Millisecond)

	if got := bodies(); len(got) != 0 {
		t.Fatalf("nothing to report should post nothing, got %d", len(got))
	}
}

// Having to open the PR to find out what broke defeats the point of the alert.
func TestVerifyFailureCarriesTheOutput(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	n.VerifyResult(testRef(), verify.Result{
		Status: store.VerifyFailed, Command: "go test ./...", Output: "--- FAIL: TestThing",
	})
	n.Close(2 * time.Second)

	e := decodeEmbed(t, waitForCount(t, bodies, 1)[0])
	if e.Title != "Verify failed: acme/widgets#42" {
		t.Errorf("unexpected title %q", e.Title)
	}
	out, ok := field(e, "Output (tail)")
	if !ok || !strings.Contains(out, "FAIL: TestThing") {
		t.Errorf("test output should be included, got %+v", e.Fields)
	}
}

// Retries are unbounded, so the useful number is when — not how many are left.
func TestRunFailedStatesTheNextAttempt(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	next := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	n.RunFailed(testRef(), "claude run failed", next)
	n.RunFailed(testRef(), "claude run failed", time.Time{})
	n.Close(2 * time.Second)

	// Posts are fire-and-forget, so they can land in either order.
	seen := map[string]bool{}
	for _, body := range waitForCount(t, bodies, 2) {
		v, _ := field(decodeEmbed(t, body), "Next attempt")
		seen[v] = true
	}
	if !seen["2026-03-01T12:00:00Z"] {
		t.Errorf("a scheduled retry should state its time, got %v", seen)
	}
	if !seen["next pass"] {
		t.Errorf("an unscheduled retry should say so, got %v", seen)
	}
}

// Every run-scoped notification must carry a clickable link to the issue —
// a linked title plus an "Issue" field — so a future method that forgets it
// fails this test.
func TestEveryRunScopedNotificationLinksTheIssue(t *testing.T) {
	const wantIssueURL = "https://github.com/acme/widgets/issues/42"

	res := &claude.Result{SessionID: "sess-abc", NumTurns: 1, TotalCostUSD: 0.1}
	vres := verify.Result{Status: store.VerifyPassed}

	cases := map[string]func(n *Notifier){
		"RunClaimed":     func(n *Notifier) { n.RunClaimed(testRef(), 0) },
		"ClaudeFinished": func(n *Notifier) { n.ClaudeFinished(testRef(), res, time.Second) },
		"VerifyResult":   func(n *Notifier) { n.VerifyResult(testRef(), vres) },
		"PROpened": func(n *Notifier) {
			n.PROpened(testRef(), "https://github.com/acme/widgets/pull/7", res, vres, "+1 -1", time.Second)
		},
		"PlanPosted":   func(n *Notifier) { n.PlanPosted(testRef(), res, time.Second) },
		"RunFailed":    func(n *Notifier) { n.RunFailed(testRef(), "boom", time.Time{}) },
		"RunAbandoned": func(n *Notifier) { n.RunAbandoned(testRef(), "issue closed", time.Time{}) },
		"RunDeferred":  func(n *Notifier) { n.RunDeferred(testRef(), "usage limit") },
		"LabelUpdateFailed": func(n *Notifier) {
			n.LabelUpdateFailed(testRef(), []string{"a"}, []string{"b"}, errors.New("HTTP 404"))
		},
	}

	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			url, bodies := stubWebhook(t)
			n := New(true, url, nil)

			call(n)
			n.Close(2 * time.Second)

			e := decodeEmbed(t, waitForCount(t, bodies, 1)[0])
			if e.URL == "" {
				t.Errorf("%s: expected a non-empty embed URL", name)
			}
			issue, ok := field(e, "Issue")
			if !ok || !strings.Contains(issue, wantIssueURL) {
				t.Errorf("%s: expected an Issue field linking %s, got %+v", name, wantIssueURL, e.Fields)
			}
		})
	}
}

// A RunRef with no explicit URL still links the issue, derived from repo and
// issue number, because the store does not carry an issue-URL column.
func TestRunRefDerivesTheIssueURLWhenNotSet(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	ref := testRef()
	ref.URL = ""
	n.RunClaimed(ref, 0)
	n.Close(2 * time.Second)

	e := decodeEmbed(t, waitForCount(t, bodies, 1)[0])
	if e.URL != "https://github.com/acme/widgets/issues/42" {
		t.Errorf("expected a derived issue URL, got %q", e.URL)
	}
}

// PROpened's title links the PR, not the issue, but the issue is still
// reachable from the Issue field.
func TestPROpenedLinksThePRAndTheIssue(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	prURL := "https://github.com/acme/widgets/pull/7"
	n.PROpened(testRef(), prURL, &claude.Result{TotalCostUSD: 0.1}, verify.Result{Status: store.VerifyPassed}, "+1 -1", time.Second)
	n.Close(2 * time.Second)

	e := decodeEmbed(t, waitForCount(t, bodies, 1)[0])
	if e.URL != prURL {
		t.Errorf("expected the embed url to be the PR, got %q", e.URL)
	}
	if issue, ok := field(e, "Issue"); !ok || !strings.Contains(issue, "issues/42") {
		t.Errorf("expected an Issue field linking the issue, got %+v", e.Fields)
	}
}

// RunFailed must not lose the issue link just because Description is
// overwritten with the cause.
func TestRunFailedDescriptionKeepsTheIssueLink(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	n.RunFailed(testRef(), "claude run failed", time.Time{})
	n.Close(2 * time.Second)

	e := decodeEmbed(t, waitForCount(t, bodies, 1)[0])
	if !strings.Contains(e.Description, "claude run failed") {
		t.Errorf("expected the cause in the description, got %q", e.Description)
	}
	if !strings.Contains(e.Description, "issues/42") {
		t.Errorf("expected the issue link in the description, got %q", e.Description)
	}
}

// A label edit that failed leaves GitHub disagreeing with the store, which is
// invisible unless it is reported.
func TestLabelUpdateFailedNamesTheLabels(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	n.LabelUpdateFailed(testRef(),
		[]string{"agent-failed"}, []string{"agent-working"}, errors.New("HTTP 404"))
	n.Close(2 * time.Second)

	e := decodeEmbed(t, waitForCount(t, bodies, 1)[0])
	if v, _ := field(e, "Add"); v != "agent-failed" {
		t.Errorf("Add = %q", v)
	}
	if v, _ := field(e, "Remove"); v != "agent-working" {
		t.Errorf("Remove = %q", v)
	}
	if !strings.Contains(e.Description, "HTTP 404") {
		t.Errorf("the failure should be described, got %q", e.Description)
	}
}

func TestDaemonStartedDescribesTheConfiguration(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	n.DaemonStarted(DaemonInfo{
		Worker: "host-1", Label: "agent-ready", Owners: []string{"ableinc"},
		PollInterval: 5 * time.Minute, MaxConcurrentRepos: 3,
		RetryBackoff: 15 * time.Minute, RetryBackoffMax: 24 * time.Hour,
	})
	n.Close(2 * time.Second)

	e := decodeEmbed(t, waitForCount(t, bodies, 1)[0])
	for name, want := range map[string]string{
		"Worker":         "host-1",
		"Trigger label":  "agent-ready",
		"Owners":         "ableinc",
		"Poll interval":  "5m0s",
		"Retry back-off": "15m0s → 24h0m0s",
	} {
		if got, ok := field(e, name); !ok || got != want {
			t.Errorf("field %q = %q (present=%v), want %q", name, got, ok, want)
		}
	}
}

func TestDisabledNotifierMakesNoRequests(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(false, url, nil)

	n.RunClaimed(testRef(), 0)
	n.Paused("testing")
	n.Close(200 * time.Millisecond)

	if got := bodies(); len(got) != 0 {
		t.Fatalf("disabled notifier made %d request(s)", len(got))
	}
}

func TestEmptyWebhookURLIsNoop(t *testing.T) {
	n := New(true, "", nil)
	// Must not panic and must not hang.
	done := make(chan struct{})
	go func() {
		n.RunClaimed(testRef(), 0)
		n.DaemonStarted(DaemonInfo{Worker: "worker-1"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("calls with an empty webhook URL should return immediately")
	}
}

func TestNilNotifierIsSafe(t *testing.T) {
	var n *Notifier
	// Every exported method must tolerate a nil receiver, since Options{}
	// literals in other packages' tests may not set Discord.
	n.RunClaimed(testRef(), 0)
	n.Paused("")
	n.Close(time.Millisecond)
}

func TestPostDoesNotBlockOnSlowWebhook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(true, srv.URL, nil)
	done := make(chan struct{})
	go func() {
		n.RunClaimed(testRef(), 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RunClaimed should return immediately regardless of webhook latency")
	}
}

func TestCloseDrainsInFlight(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	n.Resumed()
	n.Close(2 * time.Second)

	if got := bodies(); len(got) != 1 {
		t.Fatalf("expected the in-flight post to land before Close returned, got %d", len(got))
	}
}
