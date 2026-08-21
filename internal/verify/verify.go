// Package verify runs a repository's own test command against a finished
// worktree.
//
// A failure here does not block the pull request: the PR is a draft for human
// review either way, and "the agent tried and the tests fail" is useful
// information to put in front of a reviewer rather than a reason to throw the
// work away.
package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/proc"
	"github.com/ableinc/coding-agent-loop/internal/store"
)

// Result is the outcome of the verification step.
type Result struct {
	// Status is one of the store.Verify* constants.
	Status  string
	Command string
	Output  string
	Err     error
}

// Runner picks and runs the verification command.
type Runner struct {
	Cfg     config.VerifyConfig
	Timeout time.Duration
	Log     func(format string, args ...any)
}

// Command returns the command to run for repo in worktree, or "" when there is
// nothing sensible to run.
func (r *Runner) Command(repo, worktree string) string {
	if cmd, ok := r.Cfg.Commands[repo]; ok && strings.TrimSpace(cmd) != "" {
		return cmd
	}
	if !r.Cfg.AutoDetect {
		return ""
	}
	return detect(worktree)
}

var makeTestTarget = regexp.MustCompile(`(?m)^test:`)

// detect picks a test command from the repo's layout. Order matters: a
// Makefile target is the repo's own opinion about how to test itself, so it
// wins over an ecosystem default.
func detect(worktree string) string {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(worktree, name))
		return err == nil
	}

	if exists("Makefile") {
		if data, err := os.ReadFile(filepath.Join(worktree, "Makefile")); err == nil && makeTestTarget.Match(data) {
			return "make test"
		}
	}
	if exists("go.mod") {
		return "go test ./..."
	}
	if exists("package.json") {
		if data, err := os.ReadFile(filepath.Join(worktree, "package.json")); err == nil {
			var pkg struct {
				Scripts map[string]string `json:"scripts"`
			}
			if json.Unmarshal(data, &pkg) == nil {
				if _, ok := pkg.Scripts["test"]; ok {
					return "npm test"
				}
			}
		}
	}
	if exists("Cargo.toml") {
		return "cargo test"
	}
	if exists("pyproject.toml") || exists("pytest.ini") || exists("tox.ini") {
		return "python -m pytest -q"
	}
	return ""
}

// Run executes the verification command in worktree.
func (r *Runner) Run(ctx context.Context, repo, worktree string) Result {
	cmdline := r.Command(repo, worktree)
	if cmdline == "" {
		return Result{Status: store.VerifySkipped, Output: "no test command configured or detected"}
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Dir = worktree
	cmd.Env = append(os.Environ(), "CI=true")
	// Test suites fork freely (servers, containers, watchers); without this a
	// timed-out suite leaves survivors holding the output pipe open.
	proc.Isolate(cmd)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := tail(buf.String(), 8000)

	switch {
	case ctx.Err() != nil:
		return Result{Status: store.VerifyFailed, Command: cmdline, Output: out, Err: ctx.Err()}
	case err != nil:
		return Result{Status: store.VerifyFailed, Command: cmdline, Output: out, Err: err}
	}
	return Result{Status: store.VerifyPassed, Command: cmdline, Output: out}
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-n:]
}
