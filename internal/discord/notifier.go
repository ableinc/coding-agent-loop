// Package discord posts one-way status updates to a Discord channel via an
// incoming webhook. It never reads Discord and never listens for anything —
// a webhook POST is structurally incapable of that — so this is strictly a
// status feed, never a control surface.
//
// Every notification is fire-and-forget: a slow or unreachable webhook must
// never block, delay, or fail the run it is reporting on.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/verify"
)

const postTimeout = 5 * time.Second

// Discord embed colors (decimal), by what they mean here.
const (
	colorBlurple = 3447003
	colorGray    = 9807270
	colorGreen   = 3066993
	colorOrange  = 15105570
	colorRed     = 15158332
	colorYellow  = 16776960
)

// Notifier posts embeds to a Discord incoming webhook. The zero value is not
// usable; construct with New. A disabled Notifier (New(false, ...) or an
// empty webhookURL) makes every method a no-op, so callers never need a nil
// check — Options.Discord can be threaded through exactly like Options.Gate.
type Notifier struct {
	enabled    bool
	webhookURL string
	client     *http.Client
	log        func(format string, args ...any)
	wg         sync.WaitGroup
}

// New returns a Notifier. If enabled is false or webhookURL is empty, the
// returned Notifier posts nothing.
func New(enabled bool, webhookURL string, log func(format string, args ...any)) *Notifier {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Notifier{
		enabled:    enabled && webhookURL != "",
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: postTimeout},
		log:        log,
	}
}

// Close waits up to drain for any in-flight notifications to land, so a
// shutdown notification posted right before process exit has a real chance
// to leave instead of being torn down mid-flight.
func (n *Notifier) Close(drain time.Duration) {
	if n == nil || !n.enabled {
		return
	}
	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drain):
		n.log("discord: shutdown notification may not have been delivered before drain timeout")
	}
}

type embed struct {
	Title       string       `json:"title"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color"`
	Fields      []embedField `json:"fields,omitempty"`
	Timestamp   string       `json:"timestamp"`
}

type embedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type webhookPayload struct {
	Username string  `json:"username,omitempty"`
	Embeds   []embed `json:"embeds"`
}

// post sends e to the webhook in a tracked goroutine with its own bounded
// timeout, deliberately not derived from any caller context (which may
// already be cancelled by the time a run finishes reporting on itself).
// Errors are logged, never returned — every exported method is fire-and-forget
// by construction.
func (n *Notifier) post(e embed) {
	if n == nil || !n.enabled {
		return
	}
	e.Timestamp = time.Now().UTC().Format(time.RFC3339)
	body, err := json.Marshal(webhookPayload{Username: "coding-agent-loop", Embeds: []embed{e}})
	if err != nil {
		n.log("discord: encode notification: %v", err)
		return
	}

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), postTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(body))
		if err != nil {
			n.log("discord: build request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := n.client.Do(req)
		if err != nil {
			n.log("discord: post notification: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			n.log("discord: webhook returned %d", resp.StatusCode)
		}
	}()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func money(usd float64) string {
	return fmt.Sprintf("$%.4f", usd)
}

// RunClaimed reports that an issue was claimed and work is starting.
func (n *Notifier) RunClaimed(repo string, issue int, runID string, attempt int) {
	n.post(embed{
		Title: fmt.Sprintf("Run claimed: %s#%d", repo, issue),
		Color: colorBlurple,
		Fields: []embedField{
			{Name: "Run ID", Value: runID, Inline: true},
			{Name: "Attempt", Value: fmt.Sprintf("%d", attempt), Inline: true},
		},
	})
}

// ClaudeFinished reports that the Claude Code invocation for a run completed.
func (n *Notifier) ClaudeFinished(repo string, issue int, runID, model string, turns int, costUSD float64) {
	n.post(embed{
		Title: fmt.Sprintf("Claude finished: %s#%d", repo, issue),
		Color: colorGray,
		Fields: []embedField{
			{Name: "Model", Value: model, Inline: true},
			{Name: "Turns", Value: fmt.Sprintf("%d", turns), Inline: true},
			{Name: "Cost", Value: money(costUSD), Inline: true},
			{Name: "Run ID", Value: runID, Inline: true},
		},
	})
}

// VerifyResult reports the outcome of running the repository's test suite.
func (n *Notifier) VerifyResult(repo string, issue int, runID string, v verify.Result) {
	title := fmt.Sprintf("Verify passed: %s#%d", repo, issue)
	color := colorGreen
	if v.Status == store.VerifyFailed {
		title = fmt.Sprintf("Verify failed: %s#%d", repo, issue)
		color = colorOrange
	}
	fields := []embedField{{Name: "Run ID", Value: runID, Inline: true}}
	if v.Command != "" {
		fields = append(fields, embedField{Name: "Command", Value: truncate(v.Command, 1000), Inline: false})
	}
	n.post(embed{Title: title, Color: color, Fields: fields})
}

// PROpened reports that a draft pull request was opened.
func (n *Notifier) PROpened(repo string, issue int, runID, model, prURL string, costUSD float64, diffstat string) {
	n.post(embed{
		Title:       fmt.Sprintf("Draft PR opened: %s#%d", repo, issue),
		Description: prURL,
		Color:       colorGreen,
		Fields: []embedField{
			{Name: "Model", Value: model, Inline: true},
			{Name: "Cost", Value: money(costUSD), Inline: true},
			{Name: "Run ID", Value: runID, Inline: true},
			{Name: "Diffstat", Value: truncate(orNone(diffstat), 1000), Inline: false},
		},
	})
}

// RunFailed reports a run failure that will be retried.
func (n *Notifier) RunFailed(repo string, issue int, runID string, attempt, maxAttempts int, cause string, willRetry bool) {
	n.post(embed{
		Title:       fmt.Sprintf("Run failed: %s#%d", repo, issue),
		Description: truncate(cause, 500),
		Color:       colorOrange,
		Fields: []embedField{
			{Name: "Attempt", Value: fmt.Sprintf("%d/%d", attempt, maxAttempts), Inline: true},
			{Name: "Will retry", Value: fmt.Sprintf("%v", willRetry), Inline: true},
			{Name: "Run ID", Value: runID, Inline: true},
		},
	})
}

// RunAbandoned reports a run that will not be retried (skipped, or retries
// exhausted).
func (n *Notifier) RunAbandoned(repo string, issue int, runID, reason string) {
	n.post(embed{
		Title:       fmt.Sprintf("Run abandoned: %s#%d", repo, issue),
		Description: truncate(reason, 500),
		Color:       colorRed,
		Fields: []embedField{
			{Name: "Run ID", Value: runID, Inline: true},
		},
	})
}

// GateClosed reports that the usage-limit gate closed and new claims will
// stop until until.
func (n *Notifier) GateClosed(reason string, until time.Time) {
	n.post(embed{
		Title:       "Usage gate closed",
		Description: reason,
		Color:       colorRed,
		Fields: []embedField{
			{Name: "Until", Value: until.Format(time.RFC3339), Inline: true},
		},
	})
}

// GateCleared reports that the usage-limit gate is open again.
func (n *Notifier) GateCleared() {
	n.post(embed{Title: "Usage gate cleared", Color: colorGreen})
}

// Paused reports that an operator paused new claims via the control API.
func (n *Notifier) Paused(reason string) {
	desc := reason
	if desc == "" {
		desc = "paused by operator"
	}
	n.post(embed{Title: "Daemon paused", Description: desc, Color: colorYellow})
}

// Resumed reports that an operator resumed claiming via the control API.
func (n *Notifier) Resumed() {
	n.post(embed{Title: "Daemon resumed", Color: colorGreen})
}

// DaemonStarted reports that the daemon process started.
func (n *Notifier) DaemonStarted(workerID string) {
	n.post(embed{
		Title: "coding-agent-loop started",
		Color: colorBlurple,
		Fields: []embedField{
			{Name: "Worker", Value: workerID, Inline: true},
		},
	})
}

// DaemonStopped reports that the daemon process stopped, with reason such as
// "graceful shutdown" or "crash: <err>".
func (n *Notifier) DaemonStopped(reason string) {
	color := colorGray
	if strings.HasPrefix(reason, "crash") {
		color = colorRed
	}
	n.post(embed{Title: "coding-agent-loop stopped", Description: reason, Color: color})
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
