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
	"fmt"
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
	cmd.Env = r.env()
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
		// "The tests failed" and "the tests could not be run" are different
		// facts, and only one of them is about the code under review.
		if tool, missing := missingTool(cmd, out); missing {
			return Result{
				Status:  store.VerifyUnavailable,
				Command: cmdline,
				Output:  out,
				Err:     fmt.Errorf("%q is not available to the daemon (PATH=%s): %w", tool, r.pathOf(), err),
			}
		}
		return Result{Status: store.VerifyFailed, Command: cmdline, Output: out, Err: err}
	}
	return Result{Status: store.VerifyPassed, Command: cmdline, Output: out}
}

// notFound matches the way a shell, make, and the usual build tools all say
// that something is not on PATH. Exit status 127 is the POSIX convention for
// it, but the wrapper (make, npm, cargo) often swallows that and exits 1 or 2
// with the message instead, so both are checked.
var notFound = regexp.MustCompile(`(?i)([\w./+-]+): (?:command not found|No such file or directory|not found)`)

// missingTool reports whether a command failed because something it needs is
// not installed or not on the daemon's PATH, and names it when it can.
func missingTool(cmd *exec.Cmd, output string) (string, bool) {
	m := notFound.FindStringSubmatch(output)
	name := ""
	if len(m) > 1 {
		name = filepath.Base(strings.TrimSpace(m[1]))
	}
	if name != "" {
		return name, true
	}
	// No recognisable message, but 127 says it plainly enough on its own.
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 127 {
		return "a required command", true
	}
	return "", false
}

// env is the environment the test command runs in: the daemon's own, plus
// CI=true, plus whatever the operator declared in verify.env.
//
// This matters more than it looks. A daemon started by systemd inherits a
// minimal PATH that excludes every language toolchain installed outside
// /usr/bin — Go under /usr/local/go/bin, anything under ~/go/bin or a version
// manager — so a repository that tests itself perfectly well from a login
// shell cannot be verified at all without saying where its tools live.
func (r *Runner) env() []string {
	env := append(os.Environ(), "CI=true")
	for k, v := range r.Cfg.Env {
		if k = strings.TrimSpace(k); k != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// pathOf reports the PATH the command was given, for error messages.
func (r *Runner) pathOf() string {
	path := os.Getenv("PATH")
	for k, v := range r.Cfg.Env {
		if strings.EqualFold(strings.TrimSpace(k), "PATH") {
			path = v
		}
	}
	return path
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-n:]
}
