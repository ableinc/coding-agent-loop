// Package git manages the on-disk workspaces the agent edits.
//
// Layout:
//
//	<ReposRoot>/<owner>/<name>      shared checkout-less clone, fetched before each run
//	<WorkRoot>/<owner>__<name>/<n>  per-issue worktree the agent actually edits
//
// The shared clone is created with `clone --no-checkout` rather than
// `--mirror`: a mirror sets remote.origin.mirror=true, under which a bare
// `git push origin` pushes every ref. A normal clone keeps push semantics
// boring, which is what you want behind an autonomous agent.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Manager creates and tears down repo clones and worktrees.
type Manager struct {
	ReposRoot string
	WorkRoot  string
	// AuthorName/AuthorEmail identify the commits the harness makes. Set
	// explicitly so behaviour does not depend on the host's global gitconfig.
	AuthorName  string
	AuthorEmail string
	// GHBinary is the absolute path to gh, used as git's credential helper
	// for every command. This is set explicitly rather than left to
	// whatever `git config credential.helper` happens to say, because that
	// helper is normally invoked by the bare name "gh" — resolved on
	// $PATH — and a systemd service's PATH does not necessarily include
	// wherever gh was installed, even though gh itself is authenticated.
	// Left empty, git falls back to its own configuration.
	GHBinary string
	DryRun   bool
	Log      func(format string, args ...any)
}

func (m *Manager) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log(format, args...)
	}
}

// CmdError carries the failing git command's stderr.
type CmdError struct {
	Args   []string
	Dir    string
	Stderr string
	Err    error
}

func (e *CmdError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s (in %s): %s", strings.Join(e.Args, " "), e.Dir, msg)
}

func (e *CmdError) Unwrap() error { return e.Err }

// credentialArgs returns the `-c` flags that make gh, by its known absolute
// path, the sole git credential helper for one invocation. The first clears
// whatever helper(s) the host's own gitconfig may or may not have set up;
// the second installs gh by path rather than by name on $PATH, since gh
// resolves credentials from its own auth state independent of git entirely —
// this works regardless of whether `gh auth login` happened to also
// configure git integration on this host. Returns nil when ghBinary is
// unset, leaving git's own configuration untouched.
func credentialArgs(ghBinary string) []string {
	if ghBinary == "" {
		return nil
	}
	return []string{
		"-c", "credential.helper=",
		"-c", "credential.helper=!'" + ghBinary + "' auth git-credential",
	}
}

func (m *Manager) run(ctx context.Context, dir string, args ...string) (string, error) {
	fullArgs := append(credentialArgs(m.GHBinary), args...)

	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Dir = dir
	// Never let git stop for credentials or an editor: this runs unattended.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &CmdError{Args: args, Dir: dir, Stderr: stderr.String(), Err: err}
	}
	return stdout.String(), nil
}

// RepoPath is where the shared clone for "owner/name" lives.
func (m *Manager) RepoPath(repo string) string {
	owner, name := Split(repo)
	return filepath.Join(m.ReposRoot, owner, name)
}

// WorktreePath is where the per-issue worktree for an issue lives.
func (m *Manager) WorktreePath(repo string, issue int) string {
	owner, name := Split(repo)
	return filepath.Join(m.WorkRoot, owner+"__"+name, fmt.Sprintf("issue-%d", issue))
}

// Split breaks "owner/name" into its parts.
func Split(repo string) (owner, name string) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return "", repo
	}
	return parts[0], parts[1]
}

// EnsureRepo clones the repo if it is not present and fetches it either way.
// It returns the path to the shared clone.
func (m *Manager) EnsureRepo(ctx context.Context, repo, cloneURL string) (string, error) {
	path := m.RepoPath(repo)
	gitDir := filepath.Join(path, ".git")

	if _, err := os.Stat(gitDir); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat %s: %w", gitDir, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", fmt.Errorf("create repos dir: %w", err)
		}
		if _, err := m.run(ctx, "", "clone", "--no-checkout", cloneURL, path); err != nil {
			return "", fmt.Errorf("clone %s: %w", repo, err)
		}
	}

	if _, err := m.run(ctx, path, "fetch", "--prune", "--quiet", "origin"); err != nil {
		return "", fmt.Errorf("fetch %s: %w", repo, err)
	}
	if err := m.applyIdentity(ctx, path); err != nil {
		return "", err
	}
	return path, nil
}

// AssertRemote checks the clone's origin actually points at the repo we think
// we are working on, before anything is pushed to it.
func (m *Manager) AssertRemote(ctx context.Context, dir, wantRepo string) error {
	out, err := m.run(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("read origin url: %w", err)
	}
	got := RepoFromURL(strings.TrimSpace(out))
	if got == "" {
		return fmt.Errorf("could not parse a repo out of origin url %q", strings.TrimSpace(out))
	}
	if !strings.EqualFold(got, wantRepo) {
		return fmt.Errorf("refusing to act on %s: origin points at %s", wantRepo, got)
	}
	return nil
}

var remoteRe = regexp.MustCompile(`(?i)(?:github\.com[:/])([^/]+/[^/]+?)(?:\.git)?/?$`)

// RepoFromURL extracts "owner/name" from an https or ssh GitHub remote.
func RepoFromURL(url string) string {
	m := remoteRe.FindStringSubmatch(strings.TrimSpace(url))
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// AddWorktree creates a fresh worktree with branch reset to origin/base.
// Any worktree already at that path is removed first, so a retry starts clean.
func (m *Manager) AddWorktree(ctx context.Context, repoPath, worktreePath, branch, base string) error {
	if err := m.RemoveWorktree(ctx, repoPath, worktreePath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	// -B resets the branch if a previous attempt left it behind.
	if _, err := m.run(ctx, repoPath, "worktree", "add", "-B", branch, worktreePath, "origin/"+base); err != nil {
		return fmt.Errorf("add worktree for %s: %w", branch, err)
	}
	return nil
}

// RemoveWorktree deletes a worktree and prunes the administrative entry.
// It is safe to call when nothing is there.
func (m *Manager) RemoveWorktree(ctx context.Context, repoPath, worktreePath string) error {
	if _, err := os.Stat(worktreePath); err == nil {
		if _, err := m.run(ctx, repoPath, "worktree", "remove", "--force", worktreePath); err != nil {
			// Fall back to deleting the directory: a half-created worktree
			// should not wedge the loop forever.
			m.logf("worktree remove failed for %s, deleting directory: %v", worktreePath, err)
			if err := os.RemoveAll(worktreePath); err != nil {
				return fmt.Errorf("remove worktree dir %s: %w", worktreePath, err)
			}
		}
	}
	if _, err := m.run(ctx, repoPath, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune worktrees: %w", err)
	}
	return nil
}

// Status returns `git status --porcelain` output for the worktree.
func (m *Manager) Status(ctx context.Context, worktreePath string) (string, error) {
	out, err := m.run(ctx, worktreePath, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	return out, nil
}

// HasWork reports whether the agent produced anything: either uncommitted
// changes, or commits that origin/base does not have.
func (m *Manager) HasWork(ctx context.Context, worktreePath, base string) (bool, error) {
	status, err := m.Status(ctx, worktreePath)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) != "" {
		return true, nil
	}
	out, err := m.run(ctx, worktreePath, "rev-list", "--count", "origin/"+base+"..HEAD")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "0", nil
}

// CommitAll stages everything and commits, if there is anything staged. It
// reports whether a commit was made.
func (m *Manager) CommitAll(ctx context.Context, worktreePath, message string) (bool, error) {
	status, err := m.Status(ctx, worktreePath)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) == "" {
		return false, nil // agent committed its own work, or made none
	}
	if _, err := m.run(ctx, worktreePath, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage changes: %w", err)
	}
	args := []string{
		"-c", "user.name=" + m.author(),
		"-c", "user.email=" + m.email(),
		"commit", "--no-verify", "-m", message,
	}
	if _, err := m.run(ctx, worktreePath, args...); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

func (m *Manager) author() string {
	if m.AuthorName != "" {
		return m.AuthorName
	}
	return "coding-agent-loop[bot]"
}

func (m *Manager) email() string {
	if m.AuthorEmail != "" {
		return m.AuthorEmail
	}
	return "coding-agent-loop@users.noreply.github.com"
}

// IdentityEnv returns the GIT_AUTHOR_*/GIT_COMMITTER_* environment variables
// for the harness's configured identity, so a subprocess that commits on its
// own (e.g. Claude, via `git commit` with no -c flags) picks up the same
// identity as CommitAll, regardless of what its own git config resolves to.
func (m *Manager) IdentityEnv() []string {
	author, email := m.author(), m.email()
	return []string{
		"GIT_AUTHOR_NAME=" + author,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + author,
		"GIT_COMMITTER_EMAIL=" + email,
	}
}

// applyIdentity sets user.name/user.email in repoPath's own git config, so
// every worktree that shares this repo's $GIT_COMMON_DIR/config — including
// ones a subprocess like Claude commits in directly, with no -c flags —
// resolves to the harness's identity rather than the host's global gitconfig.
// Idempotent; safe to call on every EnsureRepo pass.
func (m *Manager) applyIdentity(ctx context.Context, repoPath string) error {
	if _, err := m.run(ctx, repoPath, "config", "user.name", m.author()); err != nil {
		return fmt.Errorf("set commit identity: %w", err)
	}
	if _, err := m.run(ctx, repoPath, "config", "user.email", m.email()); err != nil {
		return fmt.Errorf("set commit identity: %w", err)
	}
	return nil
}

// DiffStat summarises the branch against origin/base, for the PR body.
func (m *Manager) DiffStat(ctx context.Context, worktreePath, base string) (string, error) {
	out, err := m.run(ctx, worktreePath, "diff", "--stat", "origin/"+base+"...HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Push publishes the branch. It re-checks the remote first: this is the last
// gate before anything leaves the machine.
func (m *Manager) Push(ctx context.Context, worktreePath, branch, wantRepo string) error {
	if err := m.AssertRemote(ctx, worktreePath, wantRepo); err != nil {
		return err
	}
	if m.DryRun {
		m.logf("not mutating: would push %s to %s", branch, wantRepo)
		return nil
	}
	// Explicit refspec, never a bare `git push origin`.
	if _, err := m.run(ctx, worktreePath, "push", "--set-upstream", "origin", branch+":"+branch); err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	return nil
}
