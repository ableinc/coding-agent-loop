package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/store"
)

func repoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"go module", map[string]string{"go.mod": "module x\n"}, "go test ./..."},
		{"makefile with a test target", map[string]string{"Makefile": "build:\n\tgo build\n\ntest:\n\tgo test ./...\n"}, "make test"},
		{"makefile without a test target", map[string]string{"Makefile": "build:\n\tgo build\n"}, ""},
		{"npm with a test script", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`}, "npm test"},
		{"npm without a test script", map[string]string{"package.json": `{"scripts":{"build":"tsc"}}`}, ""},
		{"rust", map[string]string{"Cargo.toml": "[package]\n"}, "cargo test"},
		{"python", map[string]string{"pyproject.toml": "[project]\n"}, "python -m pytest -q"},
		{"nothing recognisable", map[string]string{"README.md": "hi"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := detect(repoWith(t, tc.files)); got != tc.want {
				t.Fatalf("detect = %q, want %q", got, tc.want)
			}
		})
	}
}

// The repo's own Makefile target is its opinion about how to test itself, so it
// should beat the ecosystem default.
func TestMakefileBeatsEcosystemDefault(t *testing.T) {
	dir := repoWith(t, map[string]string{
		"go.mod":   "module x\n",
		"Makefile": "test:\n\techo custom\n",
	})
	if got := detect(dir); got != "make test" {
		t.Fatalf("detect = %q, want the Makefile target", got)
	}
}

func TestConfiguredCommandWins(t *testing.T) {
	r := &Runner{Cfg: config.VerifyConfig{
		AutoDetect: true,
		Commands:   map[string]string{"acme/widgets": "./scripts/ci.sh"},
	}}
	dir := repoWith(t, map[string]string{"go.mod": "module x\n"})
	if got := r.Command("acme/widgets", dir); got != "./scripts/ci.sh" {
		t.Fatalf("configured command should win, got %q", got)
	}
	if got := r.Command("acme/other", dir); got != "go test ./..." {
		t.Fatalf("other repos should fall back to detection, got %q", got)
	}
}

func TestAutoDetectCanBeDisabled(t *testing.T) {
	r := &Runner{Cfg: config.VerifyConfig{AutoDetect: false}}
	if got := r.Command("acme/widgets", repoWith(t, map[string]string{"go.mod": "module x\n"})); got != "" {
		t.Fatalf("detection disabled should yield no command, got %q", got)
	}
}

func TestRunOutcomes(t *testing.T) {
	ctx := context.Background()

	t.Run("skipped when there is nothing to run", func(t *testing.T) {
		r := &Runner{Cfg: config.VerifyConfig{AutoDetect: true}}
		res := r.Run(ctx, "acme/widgets", repoWith(t, map[string]string{"README.md": "hi"}))
		if res.Status != store.VerifySkipped {
			t.Fatalf("status = %q, want skipped", res.Status)
		}
	})

	t.Run("passed", func(t *testing.T) {
		r := &Runner{Cfg: config.VerifyConfig{Commands: map[string]string{"acme/widgets": "echo all good"}}}
		res := r.Run(ctx, "acme/widgets", t.TempDir())
		if res.Status != store.VerifyPassed {
			t.Fatalf("status = %q, want passed (output: %s)", res.Status, res.Output)
		}
		if !strings.Contains(res.Output, "all good") {
			t.Fatalf("output not captured: %q", res.Output)
		}
	})

	t.Run("failed captures output", func(t *testing.T) {
		r := &Runner{Cfg: config.VerifyConfig{Commands: map[string]string{"acme/widgets": "echo boom >&2; exit 1"}}}
		res := r.Run(ctx, "acme/widgets", t.TempDir())
		if res.Status != store.VerifyFailed {
			t.Fatalf("status = %q, want failed", res.Status)
		}
		if !strings.Contains(res.Output, "boom") {
			t.Fatalf("stderr should be captured for the PR body, got %q", res.Output)
		}
	})

	t.Run("timeout is a failure, not a hang", func(t *testing.T) {
		r := &Runner{
			Cfg:     config.VerifyConfig{Commands: map[string]string{"acme/widgets": "sleep 5"}},
			Timeout: 200 * time.Millisecond,
		}
		start := time.Now()
		res := r.Run(ctx, "acme/widgets", t.TempDir())
		if res.Status != store.VerifyFailed {
			t.Fatalf("status = %q, want failed", res.Status)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("verify timeout did not fire")
		}
	})

	t.Run("runs in the worktree", func(t *testing.T) {
		dir := repoWith(t, map[string]string{"marker.txt": "x"})
		r := &Runner{Cfg: config.VerifyConfig{Commands: map[string]string{"acme/widgets": "ls marker.txt"}}}
		if res := r.Run(ctx, "acme/widgets", dir); res.Status != store.VerifyPassed {
			t.Fatalf("command should run inside the worktree, got %q (%s)", res.Status, res.Output)
		}
	})
}

// The failure reported from the field: `make test` runs, but the Go toolchain
// is not on the daemon's PATH, so make exits 127 with
// "make: go: No such file or directory". Calling that a test failure blames the
// agent's change for the operator's environment.
func TestMissingToolchainIsNotATestFailure(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Makefile"), "test:\n\t@go test ./...\n")
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/x\n")

	// A PATH with a shell and make, but deliberately no Go.
	r := &Runner{
		Cfg:     config.VerifyConfig{AutoDetect: true, Env: map[string]string{"PATH": "/usr/bin:/bin"}},
		Timeout: 30 * time.Second,
	}
	res := r.Run(context.Background(), "acme/widgets", dir)

	if res.Command != "make test" {
		t.Fatalf("the repo's own Makefile target should be detected, got %q", res.Command)
	}
	if res.Status != store.VerifyUnavailable {
		t.Fatalf("status = %q, want %q — the tests never ran, so they did not fail",
			res.Status, store.VerifyUnavailable)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "go") {
		t.Errorf("the error should name the missing tool, got %v", res.Err)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "PATH=") {
		t.Errorf("the error should show the PATH that was searched, got %v", res.Err)
	}
}

// A genuinely failing test suite must still be reported as a failure — the
// distinction is only worth having if it does not swallow real breakage.
func TestAFailingSuiteIsStillAFailure(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Makefile"), "test:\n\t@echo 'FAIL: TestThing'; exit 1\n")

	r := &Runner{Cfg: config.VerifyConfig{AutoDetect: true}, Timeout: 30 * time.Second}
	res := r.Run(context.Background(), "acme/widgets", dir)

	if res.Status != store.VerifyFailed {
		t.Fatalf("status = %q, want %q", res.Status, store.VerifyFailed)
	}
	if !strings.Contains(res.Output, "FAIL: TestThing") {
		t.Errorf("the failure output should be kept, got %q", res.Output)
	}
}

// verify.env is how an operator tells the daemon where a toolchain lives.
func TestEnvIsAppliedToTheCommand(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Makefile"), "test:\n\t@test \"$$AGENT_LOOP_PROBE\" = \"applied\"\n")

	r := &Runner{
		Cfg:     config.VerifyConfig{AutoDetect: true, Env: map[string]string{"AGENT_LOOP_PROBE": "applied"}},
		Timeout: 30 * time.Second,
	}
	if res := r.Run(context.Background(), "acme/widgets", dir); res.Status != store.VerifyPassed {
		t.Fatalf("verify.env should reach the command: status=%q output=%q err=%v",
			res.Status, res.Output, res.Err)
	}
}

// Detection reads the repository, never a built-in assumption about it.
func TestDetectionFollowsTheRepository(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"a Makefile with a test target wins", map[string]string{
			"Makefile": "build:\n\t@:\ntest:\n\t@:\n", "go.mod": "module x\n",
		}, "make test"},
		{"a Makefile without one does not", map[string]string{
			"Makefile": "build:\n\t@:\n", "go.mod": "module x\n",
		}, "go test ./..."},
		{"node projects use their own script", map[string]string{
			"package.json": `{"scripts":{"test":"jest"}}`,
		}, "npm test"},
		{"a node project with no test script is not guessed at", map[string]string{
			"package.json": `{"scripts":{"build":"tsc"}}`,
		}, ""},
		{"nothing recognisable means nothing is run", map[string]string{
			"README.md": "hi",
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				writeFile(t, filepath.Join(dir, name), body)
			}
			r := &Runner{Cfg: config.VerifyConfig{AutoDetect: true}}
			if got := r.Command("acme/widgets", dir); got != tc.want {
				t.Fatalf("Command() = %q, want %q", got, tc.want)
			}
		})
	}
}

// An explicit per-repo command always beats detection.
func TestConfiguredCommandBeatsDetection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Makefile"), "test:\n\t@:\n")
	r := &Runner{Cfg: config.VerifyConfig{
		AutoDetect: true,
		Commands:   map[string]string{"acme/widgets": "./scripts/ci.sh"},
	}}
	if got := r.Command("acme/widgets", dir); got != "./scripts/ci.sh" {
		t.Fatalf("Command() = %q, want the configured command", got)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
