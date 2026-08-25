package orchestrator

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func assistantEvent(t *testing.T, tool string) json.RawMessage {
	t.Helper()
	body := `{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"}]}}`
	if tool != "" {
		body = `{"type":"assistant","message":{"content":[{"type":"text","text":"ok"},{"type":"tool_use","name":"` + tool + `"}]}}`
	}
	return json.RawMessage(body)
}

func TestProgressHeartbeatIsRateLimited(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	started := time.Now().Add(-5 * time.Minute)
	p := newProgress(log, started)
	// lastLog is set to started, which is already well past the interval, so
	// the first event reports and the ones straight after it do not.
	p.observe("assistant", assistantEvent(t, "Edit"))
	first := strings.Count(buf.String(), "claude still working")
	if first != 1 {
		t.Fatalf("the first event past the interval should report, got %d lines:\n%s", first, buf.String())
	}
	for i := 0; i < 50; i++ {
		p.observe("assistant", assistantEvent(t, "Bash"))
	}
	if got := strings.Count(buf.String(), "claude still working"); got != 1 {
		t.Fatalf("a busy run must not flood the log, got %d heartbeats", got)
	}

	out := buf.String()
	if !strings.Contains(out, "turns=1") {
		t.Errorf("the heartbeat should carry the turn count:\n%s", out)
	}
	if !strings.Contains(out, "last_tool=Edit") {
		t.Errorf("the heartbeat should name the last tool:\n%s", out)
	}
	if !strings.Contains(out, "elapsed=5m0s") {
		t.Errorf("the heartbeat should say how long the run has been going:\n%s", out)
	}
}

func TestProgressCountsOnlyAssistantTurns(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(slog.New(slog.NewTextHandler(&buf, nil)), time.Now())

	p.observe("system", json.RawMessage(`{"type":"system","subtype":"init"}`))
	p.observe("user", json.RawMessage(`{"type":"user"}`))
	p.observe("assistant", assistantEvent(t, "Read"))
	if p.turns != 1 {
		t.Fatalf("turns = %d, want 1 (only assistant messages are turns)", p.turns)
	}
	if p.lastTool != "Read" {
		t.Fatalf("lastTool = %q, want %q", p.lastTool, "Read")
	}
}

// A malformed or unexpected event must never take a run down.
func TestProgressSurvivesGarbage(t *testing.T) {
	p := newProgress(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), time.Now())
	p.observe("assistant", json.RawMessage(`not json at all`))
	p.observe("assistant", json.RawMessage(`{"message":{"content":"a string, not a list"}}`))
	p.observe("assistant", json.RawMessage(`null`))
	if p.turns != 3 {
		t.Fatalf("turns = %d, want 3: unparseable content is still a turn", p.turns)
	}
	if p.lastTool != "" {
		t.Fatalf("lastTool = %q, want empty", p.lastTool)
	}
	var nilProgress *progress
	nilProgress.observe("assistant", nil) // must not panic
}
