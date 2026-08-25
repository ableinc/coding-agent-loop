package orchestrator

import (
	"encoding/json"
	"log/slog"
	"time"
)

// progressInterval is how often a long-running Claude run reports that it is
// still alive. Runs routinely go many minutes between the session starting and
// the result arriving, and silence for that long is indistinguishable from a
// hang — which is what made every eventual failure feel like it came out of
// nowhere.
const progressInterval = 60 * time.Second

// progress turns the CLI's event stream into an occasional heartbeat: how long
// the run has been going, how many turns it has taken, and what it last
// reached for.
//
// It is driven from the runner's OnEvent callback, which the runner calls
// serially from the goroutine reading the stream, so no locking is needed.
type progress struct {
	log      *slog.Logger
	started  time.Time
	lastLog  time.Time
	turns    int
	lastTool string
}

func newProgress(log *slog.Logger, started time.Time) *progress {
	return &progress{log: log, started: started, lastLog: started}
}

// observe records one stream event and logs a heartbeat when one is due.
func (p *progress) observe(kind string, raw json.RawMessage) {
	if p == nil {
		return
	}
	if kind == "assistant" {
		p.turns++
		if tool := toolName(raw); tool != "" {
			p.lastTool = tool
		}
	}

	now := time.Now()
	if now.Sub(p.lastLog) < progressInterval {
		return
	}
	p.lastLog = now
	p.log.Info("claude still working",
		"elapsed", now.Sub(p.started).Round(time.Second).String(),
		"turns", p.turns,
		"last_tool", orNone(p.lastTool))
}

// toolName pulls the name of the last tool an assistant message reached for.
// Anything unexpected simply yields "", because a heartbeat is never worth
// failing a run over.
func toolName(raw json.RawMessage) string {
	var msg struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	name := ""
	for _, c := range msg.Message.Content {
		if c.Type == "tool_use" && c.Name != "" {
			name = c.Name
		}
	}
	return name
}
