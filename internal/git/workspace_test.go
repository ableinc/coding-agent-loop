package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoFromURL(t *testing.T) {
	tests := map[string]string{
		"https://github.com/acme/widgets.git":   "acme/widgets",
		"https://github.com/acme/widgets":       "acme/widgets",
		"https://github.com/acme/widgets/":      "acme/widgets",
		"git@github.com:acme/widgets.git":       "acme/widgets",
		"ssh://git@github.com/acme/widgets.git": "acme/widgets",
		"https://GitHub.com/Acme/Widgets.git":   "Acme/Widgets",
		"https://gitlab.com/acme/widgets.git":   "",
		"/some/local/path":                      "",
		"":                                      "",
	}
	for url, want := range tests {
		if got := RepoFromURL(url); got != want {
			t.Errorf("RepoFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestSplit(t *testing.T) {
	if o, n := Split("acme/widgets"); o != "acme" || n != "widgets" {
		t.Fatalf("Split = %q,%q", o, n)
	}
	if o, n := Split("widgets"); o != "" || n != "widgets" {
		t.Fatalf("Split without owner = %q,%q", o, n)
	}
}

func TestPaths(t *testing.T) {
	m := &Manager{ReposRoot: "/repos", WorkRoot: "/work"}
	if got := m.RepoPath("acme/widgets"); got != filepath.Join("/repos", "acme", "widgets") {
		t.Fatalf("RepoPath = %q", got)
	}
	// Owner and name are flattened so two repos with the same name from
	// different owners cannot collide.
	if got := m.WorktreePath("acme/widgets", 42); got != filepath.Join("/work", "acme__widgets", "issue-42") {
		t.Fatalf("WorktreePath = %q", got)
	}
}

// --- integration against real git ------------------------------------------

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// originRepo builds a bare repo with one commit on main, standing in for GitHub.
func originRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	bare := filepath.Join(root, "origin.git")

	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "init", "--initial-branch=main")
	git(t, seed, "config", "user.email", "seed@example.com")
	git(t, seed, "config", "user.name", "Seed")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-m", "initial")
	git(t, root, "clone", "--bare", seed, bare)
	return bare
}

func TestWorktreeLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	origin := originRepo(t)
	root := t.TempDir()

	m := &Manager{
		ReposRoot:   filepath.Join(root, "repos"),
		WorkRoot:    filepath.Join(root, "work"),
		AuthorName:  "agent",
		AuthorEmail: "agent@example.com",
	}

	repoPath, err := m.EnsureRepo(ctx, "acme/widgets", origin)
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	// Calling it again must fetch rather than fail on an existing clone.
	if _, err := m.EnsureRepo(ctx, "acme/widgets", origin); err != nil {
		t.Fatalf("EnsureRepo second call: %v", err)
	}

	wt := m.WorktreePath("acme/widgets", 42)
	if err := m.AddWorktree(ctx, repoPath, wt, "agent/issue-42", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "README.md")); err != nil {
		t.Fatalf("worktree missing base content: %v", err)
	}

	// A clean worktree has produced nothing.
	if work, err := m.HasWork(ctx, wt, "main"); err != nil || work {
		t.Fatalf("fresh worktree should have no work: work=%v err=%v", work, err)
	}
	if committed, err := m.CommitAll(ctx, wt, "nothing to do"); err != nil || committed {
		t.Fatalf("committing a clean tree should be a no-op: committed=%v err=%v", committed, err)
	}

	// Now simulate the agent editing a file.
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("implemented\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if work, err := m.HasWork(ctx, wt, "main"); err != nil || !work {
		t.Fatalf("edited worktree should report work: work=%v err=%v", work, err)
	}
	committed, err := m.CommitAll(ctx, wt, "add feature")
	if err != nil || !committed {
		t.Fatalf("CommitAll: committed=%v err=%v", committed, err)
	}
	// Committed work still counts as work, via the commit-ahead check.
	if work, err := m.HasWork(ctx, wt, "main"); err != nil || !work {
		t.Fatalf("committed work should still register: work=%v err=%v", work, err)
	}

	stat, err := m.DiffStat(ctx, wt, "main")
	if err != nil {
		t.Fatalf("DiffStat: %v", err)
	}
	if !strings.Contains(stat, "feature.txt") {
		t.Fatalf("diffstat should mention the changed file, got %q", stat)
	}

	// Re-adding resets the branch, so a retry starts from a clean base.
	if err := m.AddWorktree(ctx, repoPath, wt, "agent/issue-42", "main"); err != nil {
		t.Fatalf("AddWorktree retry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "feature.txt")); !os.IsNotExist(err) {
		t.Fatal("retry should start from a clean base branch")
	}

	if err := m.RemoveWorktree(ctx, repoPath, wt); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree directory should be gone")
	}
	// Removing again must be safe.
	if err := m.RemoveWorktree(ctx, repoPath, wt); err != nil {
		t.Fatalf("RemoveWorktree when absent: %v", err)
	}
}

// AssertRemote is the last gate before anything leaves the machine.
func TestAssertRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	git(t, dir, "init", "--initial-branch=main")
	git(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	m := &Manager{}
	if err := m.AssertRemote(ctx, dir, "acme/widgets"); err != nil {
		t.Fatalf("matching remote should pass: %v", err)
	}
	if err := m.AssertRemote(ctx, dir, "ACME/WIDGETS"); err != nil {
		t.Fatalf("comparison should be case-insensitive: %v", err)
	}
	err := m.AssertRemote(ctx, dir, "attacker/evil")
	if err == nil {
		t.Fatal("a mismatched remote must be refused")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("error should say it is refusing to act, got %v", err)
	}
}

func TestPushRefusesMismatchedRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "--initial-branch=main")
	git(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	m := &Manager{}
	if err := m.Push(context.Background(), dir, "agent/issue-1", "someone/else"); err == nil {
		t.Fatal("push must refuse when origin does not match the claimed repo")
	}
}

func TestPushDryRunDoesNotPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "--initial-branch=main")
	// A bogus remote URL: if dry-run were not honoured, the push would fail.
	git(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	m := &Manager{DryRun: true}
	if err := m.Push(context.Background(), dir, "agent/issue-1", "acme/widgets"); err != nil {
		t.Fatalf("dry-run push should be a no-op, got %v", err)
	}
}
