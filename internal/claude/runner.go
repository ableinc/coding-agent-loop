// Package claude drives the Claude Code CLI in headless mode.
//
// The struct tags below mirror a real `--output-format json` payload captured
// from claude 2.1.232 rather than an assumed schema. Fields the CLI does not
// send simply stay zero, and unknown fields are ignored, so a CLI upgrade that
// adds keys will not break parsing.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/proc"
)

// Usage is the token accounting on a result event.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// ModelUsage is one entry of the result's modelUsage map. The map key is the
// dated model ID; CanonicalModel is the undated one.
type ModelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
	CanonicalModel           string  `json:"canonicalModel"`
	Provider                 string  `json:"provider"`
}

// Result is the terminal `type: "result"` event of a headless run.
type Result struct {
	Type           string                `json:"type"`
	Subtype        string                `json:"subtype"`
	IsError        bool                  `json:"is_error"`
	Result         string                `json:"result"`
	SessionID      string                `json:"session_id"`
	TotalCostUSD   float64               `json:"total_cost_usd"`
	NumTurns       int                   `json:"num_turns"`
	DurationMS     int64                 `json:"duration_ms"`
	StopReason     string                `json:"stop_reason"`
	TerminalReason string                `json:"terminal_reason"`
	APIErrorStatus *int                  `json:"api_error_status"`
	Usage          Usage                 `json:"usage"`
	ModelUsage     map[string]ModelUsage `json:"modelUsage"`

	// Stderr is filled in by the runner, not the CLI.
	Stderr string `json:"-"`
	// ExitCode is filled in by the runner.
	ExitCode int `json:"-"`
}

// ModelsUsed lists the canonical model IDs that actually served the run. With
// --fallback-model this can differ from the model that was requested, which is
// exactly why it is recorded rather than assumed.
func (r *Result) ModelsUsed() []string {
	if r == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for dated, mu := range r.ModelUsage {
		id := mu.CanonicalModel
		if id == "" {
			id = dated
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// PrimaryModel returns the model that did the most output work, or "".
func (r *Result) PrimaryModel() string {
	if r == nil {
		return ""
	}
	best, bestTokens := "", int64(-1)
	for dated, mu := range r.ModelUsage {
		id := mu.CanonicalModel
		if id == "" {
			id = dated
		}
		if mu.OutputTokens > bestTokens {
			best, bestTokens = id, mu.OutputTokens
		}
	}
	return best
}

// TokensIn totals the input side, including cache traffic, so cost reporting
// is not silently understated on cache-heavy runs.
func (r *Result) TokensIn() int64 {
	if r == nil {
		return 0
	}
	u := r.Usage
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// TokensOut totals the output side.
func (r *Result) TokensOut() int64 {
	if r == nil {
		return 0
	}
	return r.Usage.OutputTokens
}

// Options configures one headless invocation.
type Options struct {
	// Binary is the claude executable.
	Binary string
	// Prompt is delivered on stdin, so length and quoting are never an issue.
	Prompt string
	// SystemPrompt is appended to the default system prompt.
	SystemPrompt string
	// Model is the `--model` ref; Fallbacks is the comma-separated
	// `--fallback-model` list and may be empty.
	Model     string
	Fallbacks string
	// PermissionMode maps to `--permission-mode`.
	PermissionMode string
	// WorkDir is the process cwd, i.e. the worktree.
	WorkDir string
	// Env is appended to the subprocess's inherited environment.
	Env []string
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
	// LogPath receives the raw JSONL transcript. Required.
	LogPath string
	// Timeout bounds the run.
	Timeout time.Duration
	// OnEvent is called for each parsed stream event. May be nil.
	OnEvent func(kind string, raw json.RawMessage)
}

// Runner executes headless Claude runs.
type Runner struct {
	Log func(format string, args ...any)
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// ErrNoResult means the CLI exited without emitting a terminal result event,
// which usually indicates it was killed.
var ErrNoResult = errors.New("claude produced no result event")

// ErrCanceled means the run was stopped from outside — the daemon is shutting
// down, or an operator cancelled it. It is not the issue's fault and must not
// be reported as one.
var ErrCanceled = errors.New("claude run canceled")

// ErrTimeout means the run exceeded run.timeout and was killed.
var ErrTimeout = errors.New("claude run timed out")

// Run invokes Claude and returns the terminal result.
//
// A non-nil Result is returned even alongside an error whenever the CLI got far
// enough to emit one — the caller needs its fields (api_error_status, cost) to
// classify the failure and to bill the run.
func (r *Runner) Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Binary == "" {
		opts.Binary = "claude"
	}
	if opts.LogPath == "" {
		return nil, fmt.Errorf("claude: LogPath is required")
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose", // required alongside stream-json in print mode
		"--no-session-persistence",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Fallbacks != "" {
		args = append(args, "--fallback-model", opts.Fallbacks)
	}
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", opts.PermissionMode)
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.SystemPrompt)
	}
	if opts.WorkDir != "" {
		args = append(args, "--add-dir", opts.WorkDir)
	}
	args = append(args, opts.ExtraArgs...)

	if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.Create(opts.LogPath)
	if err != nil {
		return nil, fmt.Errorf("create transcript %s: %w", opts.LogPath, err)
	}
	defer logFile.Close()

	cmd := exec.CommandContext(ctx, opts.Binary, args...)
	cmd.Dir = opts.WorkDir
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	cmd.Stdin = strings.NewReader(opts.Prompt)
	// Claude Code spawns children (the bash tool, language servers); kill the
	// whole group on cancellation or a timed-out run leaves the worker stuck.
	proc.Isolate(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	result, scanErr := r.consume(stdout, logFile, opts.OnEvent)
	waitErr := cmd.Wait()

	if result != nil {
		result.Stderr = tail(stderr.String(), 4000)
		result.ExitCode = cmd.ProcessState.ExitCode()
	}

	// A run that finished cleanly before the context ended is a success, even
	// if the daemon has since begun shutting down: the work was done and paid
	// for, and throwing it away would redo it.
	if waitErr == nil && scanErr == nil && result != nil && !result.IsError {
		return result, nil
	}

	// Whatever went wrong, say what the CLI itself reported. Claude Code writes
	// its errors into the result event and exits non-zero with an empty stderr,
	// so reporting only the exit status — as this used to — produced the
	// uninformative "exit status 1 (stderr: )" on every real failure.
	detail := diagnose(result, stderr.String())

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return result, fmt.Errorf("%w after %s%s", ErrTimeout, opts.Timeout, detail)
	case errors.Is(ctx.Err(), context.Canceled):
		return result, fmt.Errorf("%w%s", ErrCanceled, detail)
	case waitErr != nil:
		return result, fmt.Errorf("claude exited with error: %w%s", waitErr, detail)
	case scanErr != nil:
		return result, fmt.Errorf("%w%s", scanErr, detail)
	case result == nil:
		return nil, fmt.Errorf("%w%s", ErrNoResult, detail)
	}
	return result, fmt.Errorf("claude reported an error%s", detail)
}

// diagnose renders everything known about a failed run into one line, in
// descending order of usefulness. It returns "" when there is genuinely nothing
// to add, so callers can append it unconditionally.
func diagnose(result *Result, stderr string) string {
	var parts []string
	if result != nil {
		if result.Subtype != "" {
			parts = append(parts, fmt.Sprintf("subtype %q", result.Subtype))
		}
		if result.TerminalReason != "" {
			parts = append(parts, fmt.Sprintf("terminal_reason %q", result.TerminalReason))
		}
		if result.APIErrorStatus != nil {
			parts = append(parts, fmt.Sprintf("api_error_status %d", *result.APIErrorStatus))
		}
		if result.NumTurns > 0 {
			parts = append(parts, fmt.Sprintf("after %d turn(s)", result.NumTurns))
		}
		// The CLI's own message, which is where Claude Code actually explains
		// itself: an invalid model, a bad API key, a tool permission refusal.
		if msg := strings.TrimSpace(result.Result); msg != "" {
			parts = append(parts, "claude said: "+tail(msg, 800))
		}
	}
	if se := strings.TrimSpace(stderr); se != "" {
		parts = append(parts, "stderr: "+tail(se, 800))
	}
	if len(parts) == 0 {
		return " (claude reported nothing; see the run transcript)"
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// consume mirrors the stream to the transcript file while pulling out the
// terminal result event.
func (r *Runner) consume(stdout io.Reader, transcript io.Writer, onEvent func(string, json.RawMessage)) (*Result, error) {
	scanner := bufio.NewScanner(stdout)
	// Stream events carry whole assistant messages and tool payloads, which can
	// be far larger than the 64KiB default line budget.
	scanner.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	var result *Result
	for scanner.Scan() {
		line := scanner.Bytes()
		if _, err := transcript.Write(append(bytes.Clone(line), '\n')); err != nil {
			return result, fmt.Errorf("write transcript: %w", err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// Not JSON: the CLI occasionally prints plain diagnostics. It is in
			// the transcript; do not fail the run over it.
			r.logf("claude: skipping non-JSON stream line: %s", tail(string(line), 200))
			continue
		}
		if onEvent != nil {
			onEvent(probe.Type, json.RawMessage(bytes.Clone(line)))
		}
		if probe.Type == "result" {
			var res Result
			if err := json.Unmarshal(line, &res); err != nil {
				return result, fmt.Errorf("decode result event: %w", err)
			}
			result = &res
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read claude stream: %w", err)
	}
	return result, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
