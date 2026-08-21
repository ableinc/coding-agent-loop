package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/discord"
	"github.com/ableinc/coding-agent-loop/internal/gate"
	"github.com/ableinc/coding-agent-loop/internal/store"
)

type fakeController struct {
	cancelled  []string
	cancelOK   bool
	activeRepo []string
}

func (f *fakeController) Cancel(runID string) bool {
	f.cancelled = append(f.cancelled, runID)
	return f.cancelOK
}

func (f *fakeController) ActiveRepos() []string { return f.activeRepo }

func testServer(t *testing.T) (*Server, *store.Store, *fakeController) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.Claude.CredentialsPath = filepath.Join(t.TempDir(), "absent.json")
	g := gate.New(st, cfg.Claude, nil)
	ctrl := &fakeController{cancelOK: true, activeRepo: []string{"acme/widgets"}}
	return New("127.0.0.1:0", st, g, ctrl, nil, discord.New(false, "", nil)), st, ctrl
}

func do(t *testing.T, s *Server, method, target string, body io.Reader) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.app.Test(req, fiberTimeout)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode %s: %v (%s)", target, err, raw)
		}
	}
	return resp.StatusCode, decoded
}

func TestHealthz(t *testing.T) {
	s, _, _ := testServer(t)
	code, body := do(t, s, http.MethodGet, "/healthz", nil)
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz = %d %v", code, body)
	}
}

func TestStatusReportsGates(t *testing.T) {
	s, st, _ := testServer(t)
	ctx := t.Context()

	code, body := do(t, s, http.MethodGet, "/status", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["claiming_work"] != true {
		t.Fatalf("a fresh daemon should be claiming work: %v", body)
	}

	// A usage limit must both flip the flag and explain itself.
	if err := st.SetGate(ctx, store.GateUsageLimit, time.Now().Add(time.Hour), "limit reached"); err != nil {
		t.Fatal(err)
	}
	// A model cooldown steers the ladder but must not stop the loop.
	if err := st.SetGate(ctx, store.GateModelPrefix+"claude-opus-5", time.Now().Add(time.Hour), "cooldown"); err != nil {
		t.Fatal(err)
	}

	code, body = do(t, s, http.MethodGet, "/status", nil)
	if code != http.StatusOK || body["claiming_work"] != false {
		t.Fatalf("usage limit should stop claiming: %v", body)
	}
	gates, _ := body["gates"].([]any)
	if len(gates) != 2 {
		t.Fatalf("both gates should be listed: %v", body["gates"])
	}
	cooldowns, _ := body["model_cooldowns"].(map[string]any)
	if _, ok := cooldowns["claude-opus-5"]; !ok {
		t.Fatalf("model cooldown should be reported separately: %v", body["model_cooldowns"])
	}
	if repos, _ := body["active_repos"].([]any); len(repos) != 1 {
		t.Fatalf("active repos should come from the controller: %v", body["active_repos"])
	}
}

func TestPauseAndResume(t *testing.T) {
	s, _, _ := testServer(t)

	code, body := do(t, s, http.MethodPost, "/pause", nil)
	if code != http.StatusOK || body["paused"] != true {
		t.Fatalf("pause = %d %v", code, body)
	}
	_, status := do(t, s, http.MethodGet, "/status", nil)
	if status["claiming_work"] != false {
		t.Fatal("pause should stop new claims")
	}

	if code, _ := do(t, s, http.MethodPost, "/resume", nil); code != http.StatusOK {
		t.Fatalf("resume = %d", code)
	}
	_, status = do(t, s, http.MethodGet, "/status", nil)
	if status["claiming_work"] != true {
		t.Fatal("resume should restore claiming")
	}
}

func TestRunsEndpoints(t *testing.T) {
	s, st, _ := testServer(t)
	ctx := t.Context()

	logPath := filepath.Join(t.TempDir(), "run-1.jsonl")
	if err := os.WriteFile(logPath, []byte(`{"type":"result"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(ctx, store.Run{
		ID: "run-1", Repo: "acme/widgets", Issue: 42, Attempt: 1,
		Status: store.StatusPROpen, StartedAt: time.Now(), LogPath: logPath,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, "run-1", "claimed", "attempt 1"); err != nil {
		t.Fatal(err)
	}

	code, body := do(t, s, http.MethodGet, "/runs", nil)
	if code != http.StatusOK || body["count"].(float64) != 1 {
		t.Fatalf("runs = %d %v", code, body)
	}

	// The repo filter should exclude non-matching runs.
	_, body = do(t, s, http.MethodGet, "/runs?repo=other/repo", nil)
	if body["count"].(float64) != 0 {
		t.Fatalf("repo filter ignored: %v", body)
	}

	code, body = do(t, s, http.MethodGet, "/runs/run-1", nil)
	if code != http.StatusOK {
		t.Fatalf("get run = %d", code)
	}
	if events, _ := body["events"].([]any); len(events) != 1 {
		t.Fatalf("events missing: %v", body["events"])
	}

	if code, _ := do(t, s, http.MethodGet, "/runs/nope", nil); code != http.StatusNotFound {
		t.Fatalf("unknown run should 404, got %d", code)
	}

	// The transcript path comes from the run row, never the request.
	req := httptest.NewRequest(http.MethodGet, "/runs/run-1/log", nil)
	resp, err := s.app.Test(req, fiberTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(raw) == 0 {
		t.Fatalf("log = %d %q", resp.StatusCode, raw)
	}
}

func TestRunLogMissingFile(t *testing.T) {
	s, st, _ := testServer(t)
	if err := st.CreateRun(t.Context(), store.Run{
		ID: "run-2", Repo: "acme/widgets", Issue: 1, Status: store.StatusFailed,
		StartedAt: time.Now(), LogPath: "/nonexistent/run-2.jsonl",
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, s, http.MethodGet, "/runs/run-2/log", nil); code != http.StatusNotFound {
		t.Fatalf("a missing transcript should 404, got %d", code)
	}
}

func TestCancelRun(t *testing.T) {
	s, _, ctrl := testServer(t)

	code, _ := do(t, s, http.MethodPost, "/runs/run-1/cancel", nil)
	if code != http.StatusOK {
		t.Fatalf("cancel = %d", code)
	}
	if len(ctrl.cancelled) != 1 || ctrl.cancelled[0] != "run-1" {
		t.Fatalf("controller not called correctly: %v", ctrl.cancelled)
	}

	ctrl.cancelOK = false
	if code, _ := do(t, s, http.MethodPost, "/runs/gone/cancel", nil); code != http.StatusNotFound {
		t.Fatalf("cancelling a run that is not in flight should 404, got %d", code)
	}
}

// fiberTimeout gives handlers room on a loaded machine; the default is 1s.
var fiberTimeout = fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true}

func TestListSessions(t *testing.T) {
	s, st, _ := testServer(t)
	ctx := t.Context()

	for _, sess := range []store.Session{
		{SessionID: "sess-1", RunID: "run-1", Repo: "acme/widgets", Issue: 42, ModelID: "claude-opus-5"},
		{SessionID: "sess-2", RunID: "run-2", Repo: "acme/widgets", Issue: 7},
		{SessionID: "sess-3", RunID: "run-3", Repo: "acme/other", Issue: 1},
	} {
		if err := st.RecordSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}

	code, body := do(t, s, http.MethodGet, "/sessions", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /sessions = %d", code)
	}
	if got := body["count"].(float64); got != 3 {
		t.Fatalf("count = %v, want 3", got)
	}

	_, body = do(t, s, http.MethodGet, "/sessions?repo=acme/widgets&issue=42", nil)
	sessions := body["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("filtering by issue should narrow the result, got %+v", sessions)
	}
	if id := sessions[0].(map[string]any)["SessionID"]; id != "sess-1" {
		t.Fatalf("unexpected session %v", id)
	}
}
