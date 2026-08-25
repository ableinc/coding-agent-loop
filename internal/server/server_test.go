package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	pollOK     bool
	polled     int
}

func (f *fakeController) Cancel(runID string) bool {
	f.cancelled = append(f.cancelled, runID)
	return f.cancelOK
}

func (f *fakeController) ActiveRepos() []string { return f.activeRepo }

func (f *fakeController) Poll() bool {
	f.polled++
	return f.pollOK
}

func testServer(t *testing.T) (*Server, *store.Store, *fakeController) {
	t.Helper()
	return testServerWithConfig(t, config.Default())
}

func testServerWithConfig(t *testing.T, cfg config.Config) (*Server, *store.Store, *fakeController) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg.Claude.CredentialsPath = filepath.Join(t.TempDir(), "absent.json")
	g := gate.New(st, cfg.Claude, nil)
	ctrl := &fakeController{cancelOK: true, pollOK: true, activeRepo: []string{"acme/widgets"}}
	s := New(Options{
		Addr: "127.0.0.1:0", Store: st, Gate: g, Ctrl: ctrl,
		Discord: discord.New(false, "", nil), Config: cfg,
	})
	return s, st, ctrl
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

// A run the controller cancels may not have a row in the store (e.g. it
// hasn't been recorded yet); the notification lookup must not affect the
// HTTP result or panic.
func TestCancelRunWithNoStoreRowStillSucceeds(t *testing.T) {
	s, _, ctrl := testServer(t)
	ctrl.cancelOK = true

	code, body := do(t, s, http.MethodPost, "/runs/unknown-run/cancel", nil)
	if code != http.StatusOK {
		t.Fatalf("cancel = %d", code)
	}
	if body["cancelled"] != true {
		t.Fatalf("unexpected body: %v", body)
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

func TestUIServedAndRootRedirects(t *testing.T) {
	s, _, _ := testServer(t) // config.Default() has server.ui = true

	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	resp, err := s.app.Test(req, fiberTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/ = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET /ui/ content-type = %q", ct)
	}
	if !strings.Contains(string(raw), "app.js") {
		t.Fatalf("GET /ui/ body does not reference app.js: %s", raw)
	}

	req = httptest.NewRequest(http.MethodGet, "/ui/app.js", nil)
	resp, err = s.app.Test(req, fiberTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/app.js = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("GET /ui/app.js content-type = %q", ct)
	}

	if code, _ := do(t, s, http.MethodGet, "/ui/nope.js", nil); code != http.StatusNotFound {
		t.Fatalf("GET /ui/nope.js = %d, want 404", code)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	resp, err = s.app.Test(req, fiberTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET / = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("GET / Location = %q, want /ui/", loc)
	}

	// The static mount must not shadow the JSON API.
	code, body := do(t, s, http.MethodGet, "/status", nil)
	if code != http.StatusOK || body["claiming_work"] != true {
		t.Fatalf("GET /status after UI mount = %d %v", code, body)
	}
}

func TestUIDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Server.UI = false
	s, _, _ := testServerWithConfig(t, cfg)

	if code, _ := do(t, s, http.MethodGet, "/ui/", nil); code != http.StatusNotFound {
		t.Fatalf("GET /ui/ with server.ui=false = %d, want 404", code)
	}
	code, body := do(t, s, http.MethodGet, "/status", nil)
	if code != http.StatusOK || body["claiming_work"] != true {
		t.Fatalf("GET /status with server.ui=false = %d %v", code, body)
	}
}

func TestSameOriginGuard(t *testing.T) {
	s, _, _ := testServer(t)

	post := func(origin string) int {
		req := httptest.NewRequest(http.MethodPost, "/pause", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := s.app.Test(req, fiberTimeout)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := post("https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d, want 403", code)
	}
	if code := post(""); code != http.StatusOK {
		t.Fatalf("no-Origin POST (curl-style) = %d, want 200", code)
	}
	// Resume so the next assertion isn't affected by the pause above.
	do(t, s, http.MethodPost, "/resume", nil)
	if code := post("http://127.0.0.1:8787"); code != http.StatusOK {
		t.Fatalf("loopback-origin POST = %d, want 200", code)
	}
}

func TestGetConfigRedactsWebhook(t *testing.T) {
	cfg := config.Default()
	cfg.Discord.Enabled = true
	cfg.Discord.WebhookURL = "https://discord.com/api/webhooks/super-secret"
	s, _, _ := testServerWithConfig(t, cfg)

	code, body := do(t, s, http.MethodGet, "/config", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /config = %d", code)
	}
	if body["discord_webhook_set"] != true {
		t.Fatalf("discord_webhook_set = %v, want true", body["discord_webhook_set"])
	}
	cfgBody, _ := body["config"].(map[string]any)
	discord, _ := cfgBody["discord"].(map[string]any)
	if discord["webhook_url"] != "" {
		t.Fatalf("webhook_url leaked: %v", discord["webhook_url"])
	}
}

func TestPollNow(t *testing.T) {
	s, _, ctrl := testServer(t)

	code, body := do(t, s, http.MethodPost, "/poll", nil)
	if code != http.StatusOK || body["queued"] != true {
		t.Fatalf("poll = %d %v", code, body)
	}
	if ctrl.polled != 1 {
		t.Fatalf("controller.Poll() calls = %d, want 1", ctrl.polled)
	}

	ctrl.pollOK = false
	if code, _ := do(t, s, http.MethodPost, "/poll", nil); code != http.StatusConflict {
		t.Fatalf("poll when already queued = %d, want 409", code)
	}
}
