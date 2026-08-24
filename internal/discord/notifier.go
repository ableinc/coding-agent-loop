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

	"github.com/ableinc/coding-agent-loop/internal/claude"
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

// RunRef identifies the run a notification is about. Every run-scoped method
// takes one, so the fields shared by all of them do not have to be repeated as
// an ever-growing argument list.
type RunRef struct {
	Repo    string
	Issue   int
	Title   string
	URL     string
	RunID   string
	Attempt int
}

// title renders "<what happened>: owner/name#42".
func (r RunRef) title(what string) string {
	return fmt.Sprintf("%s: %s#%d", what, r.Repo, r.Issue)
}

// description is the issue's own title, linked when its URL is known, so a
// notification says what the work is about and not only which number it has.
func (r RunRef) description() string {
	switch {
	case r.Title != "" && r.URL != "":
		return fmt.Sprintf("[%s](%s)", truncate(r.Title, 200), r.URL)
	case r.Title != "":
		return truncate(r.Title, 200)
	default:
		return r.URL
	}
}

func (r RunRef) fields() []embedField {
	return []embedField{
		{Name: "Run ID", Value: r.RunID, Inline: true},
		{Name: "Attempt", Value: fmt.Sprintf("%d", r.Attempt), Inline: true},
	}
}

// RunClaimed reports that an issue was claimed and work is starting. failures
// is how many earlier attempts on this issue ended without a PR, which is what
// the retry back-off is derived from.
func (n *Notifier) RunClaimed(r RunRef, failures int) {
	fields := r.fields()
	if failures > 0 {
		fields = append(fields, embedField{
			Name: "Previous failures", Value: fmt.Sprintf("%d", failures), Inline: true,
		})
	}
	n.post(embed{
		Title:       r.title("Run claimed"),
		Description: r.description(),
		Color:       colorBlurple,
		Fields:      fields,
	})
}

// ClaudeFinished reports that the Claude Code invocation for a run completed.
// The session ID is included so the run can be tied back to the CLI's own
// record of it, and to the sessions table.
func (n *Notifier) ClaudeFinished(r RunRef, res *claude.Result, elapsed time.Duration) {
	if res == nil {
		return
	}
	n.post(embed{
		Title:       r.title("Claude finished"),
		Description: r.description(),
		Color:       colorGray,
		Fields: append(r.fields(),
			embedField{Name: "Model", Value: orNone(res.PrimaryModel()), Inline: true},
			embedField{Name: "Turns", Value: fmt.Sprintf("%d", res.NumTurns), Inline: true},
			embedField{Name: "Cost", Value: money(res.TotalCostUSD), Inline: true},
			embedField{Name: "Tokens", Value: fmt.Sprintf("%d in / %d out", res.TokensIn(), res.TokensOut()), Inline: true},
			embedField{Name: "Duration", Value: humanDuration(elapsed), Inline: true},
			embedField{Name: "Session", Value: orNone(res.SessionID), Inline: true},
		),
	})
}

// VerifyResult reports the outcome of running the repository's test suite. A
// failure carries the tail of the output: the point of running the tests is
// lost if you have to open the PR to find out what broke.
func (n *Notifier) VerifyResult(r RunRef, v verify.Result) {
	what, color := "Verify passed", colorGreen
	switch v.Status {
	case store.VerifyFailed:
		what, color = "Verify failed", colorOrange
	case store.VerifySkipped:
		what, color = "Verify skipped", colorGray
	}
	fields := append(r.fields(), embedField{Name: "Status", Value: v.Status, Inline: true})
	if v.Command != "" {
		fields = append(fields, embedField{Name: "Command", Value: truncate(v.Command, 1000), Inline: false})
	}
	if v.Status == store.VerifyFailed {
		if out := strings.TrimSpace(v.Output); out != "" {
			fields = append(fields, embedField{Name: "Output (tail)", Value: codeBlock(tail(out, 900)), Inline: false})
		}
	}
	n.post(embed{Title: r.title(what), Description: r.description(), Color: color, Fields: fields})
}

// PROpened reports that a draft pull request was opened.
func (n *Notifier) PROpened(r RunRef, prURL string, res *claude.Result, v verify.Result, diffstat string, elapsed time.Duration) {
	model, session, cost := "", "", 0.0
	if res != nil {
		model, session, cost = res.PrimaryModel(), res.SessionID, res.TotalCostUSD
	}
	n.post(embed{
		Title:       r.title("Draft PR opened"),
		Description: prURL,
		Color:       colorGreen,
		Fields: append(r.fields(),
			embedField{Name: "Model", Value: orNone(model), Inline: true},
			embedField{Name: "Cost", Value: money(cost), Inline: true},
			embedField{Name: "Verification", Value: orNone(v.Status), Inline: true},
			embedField{Name: "Duration", Value: humanDuration(elapsed), Inline: true},
			embedField{Name: "Session", Value: orNone(session), Inline: true},
			embedField{Name: "Diffstat", Value: truncate(orNone(diffstat), 1000), Inline: false},
		),
	})
}

// PRAdopted reports that an existing pull request was found for an issue and
// linked to it, so no new work was done.
func (n *Notifier) PRAdopted(r RunRef, prURL, state string) {
	n.post(embed{
		Title:       r.title("Existing PR adopted"),
		Description: prURL,
		Color:       colorBlurple,
		Fields: append(r.fields(),
			embedField{Name: "PR state", Value: orNone(state), Inline: true},
			embedField{Name: "Outcome", Value: "No new work: the issue was already covered.", Inline: false},
		),
	})
}

// PlanPosted reports that a plan was posted for human review and the run is
// now waiting on an "implement" reply.
func (n *Notifier) PlanPosted(r RunRef, res *claude.Result, elapsed time.Duration) {
	model, cost := "", 0.0
	if res != nil {
		model, cost = res.PrimaryModel(), res.TotalCostUSD
	}
	n.post(embed{
		Title:       r.title("Plan posted, awaiting approval"),
		Description: "Reply `implement` on the issue to start the change.",
		Color:       colorBlurple,
		Fields: append(r.fields(),
			embedField{Name: "Model", Value: orNone(model), Inline: true},
			embedField{Name: "Cost", Value: money(cost), Inline: true},
			embedField{Name: "Duration", Value: humanDuration(elapsed), Inline: true},
		),
	})
}

// RunFailed reports a failed run and when it will be tried again. Retries are
// unbounded, so "when" is the useful number, not "how many are left".
func (n *Notifier) RunFailed(r RunRef, cause string, nextAttempt time.Time) {
	n.post(embed{
		Title:       r.title("Run failed"),
		Description: truncate(cause, 500),
		Color:       colorOrange,
		Fields: append(r.fields(),
			embedField{Name: "Next attempt", Value: when(nextAttempt), Inline: true},
		),
	})
}

// RunAbandoned reports a run that was skipped rather than attempted — the
// issue closed, lost its label, or is already covered by a pull request.
func (n *Notifier) RunAbandoned(r RunRef, reason string, nextAttempt time.Time) {
	n.post(embed{
		Title:       r.title("Run abandoned"),
		Description: truncate(reason, 500),
		Color:       colorRed,
		Fields: append(r.fields(),
			embedField{Name: "Next attempt", Value: when(nextAttempt), Inline: true},
		),
	})
}

// RunDeferred reports a run the usage gate stopped. It is neither an attempt
// nor a failure, so the issue keeps its place in the queue.
func (n *Notifier) RunDeferred(r RunRef, reason string) {
	n.post(embed{
		Title:       r.title("Run deferred"),
		Description: truncate(reason, 500),
		Color:       colorYellow,
		Fields:      r.fields(),
	})
}

// RunCanceled reports that an operator cancelled an in-flight run.
func (n *Notifier) RunCanceled(runID string) {
	n.post(embed{
		Title: "Run cancelled by operator",
		Color: colorYellow,
		Fields: []embedField{
			{Name: "Run ID", Value: runID, Inline: true},
		},
	})
}

// LabelUpdateFailed reports that the issue's labels could not be brought in
// line with the run's state, so what GitHub shows now disagrees with the store.
func (n *Notifier) LabelUpdateFailed(repo string, issue int, runID string, add, remove []string, err error) {
	n.post(embed{
		Title:       fmt.Sprintf("Label update failed: %s#%d", repo, issue),
		Description: truncate(err.Error(), 500),
		Color:       colorYellow,
		Fields: []embedField{
			{Name: "Add", Value: labelList(add), Inline: true},
			{Name: "Remove", Value: labelList(remove), Inline: true},
			{Name: "Run ID", Value: runID, Inline: true},
		},
	})
}

// ModelCooledDown reports that a model has been sidelined, so a run served by
// something lower on the ladder than expected is explainable.
func (n *Notifier) ModelCooledDown(model string, until time.Time, reason string) {
	n.post(embed{
		Title:       "Model cooled down: " + model,
		Description: reason,
		Color:       colorYellow,
		Fields: []embedField{
			{Name: "Until", Value: when(until), Inline: true},
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

// DaemonInfo is the configuration worth stating when the daemon comes up: the
// answer to "what is this thing about to do", posted where the run
// notifications will land.
type DaemonInfo struct {
	Worker             string
	Label              string
	Owners             []string
	PollInterval       time.Duration
	MaxConcurrentRepos int
	RetryBackoff       time.Duration
	RetryBackoffMax    time.Duration
	DryRun             bool
}

// DaemonStarted reports that the daemon process started, and how it is set up.
func (n *Notifier) DaemonStarted(info DaemonInfo) {
	fields := []embedField{
		{Name: "Worker", Value: info.Worker, Inline: true},
		{Name: "Trigger label", Value: orNone(info.Label), Inline: true},
		{Name: "Owners", Value: labelList(info.Owners), Inline: true},
		{Name: "Poll interval", Value: humanDuration(info.PollInterval), Inline: true},
		{Name: "Concurrent repos", Value: fmt.Sprintf("%d", info.MaxConcurrentRepos), Inline: true},
		{Name: "Retry back-off", Value: fmt.Sprintf("%s → %s",
			humanDuration(info.RetryBackoff), humanDuration(info.RetryBackoffMax)), Inline: true},
	}
	if info.DryRun {
		fields = append(fields, embedField{Name: "Dry run", Value: "nothing will be pushed", Inline: true})
	}
	n.post(embed{Title: "coding-agent-loop started", Color: colorBlurple, Fields: fields})
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

// humanDuration renders a duration at a granularity a human reads at a glance.
func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "n/a"
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}

// tail keeps the end of s, which is where a failing test says what broke.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

func codeBlock(s string) string {
	return "```\n" + s + "\n```"
}

// when renders a scheduled time, or says that nothing is being waited for.
func when(t time.Time) string {
	if t.IsZero() {
		return "next pass"
	}
	return t.UTC().Format(time.RFC3339)
}

func labelList(labels []string) string {
	if len(labels) == 0 {
		return "(none)"
	}
	return strings.Join(labels, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
