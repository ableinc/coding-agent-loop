package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// An explicitly requested path (allowEmbeddedFallback=false) that doesn't
// exist is a misconfiguration to report, not something to paper over.
func TestLoadRejectsMissingExplicitPath(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"), false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a config-not-found error, got %v", err)
	}
}

// A binary shipped without the rest of the repo has nothing at the default
// path, so Load must fall back to the config embedded at build time (the
// repo's own, real config.json) rather than failing outright.
func TestLoadFallsBackToEmbeddedConfigWhenAllowed(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"), true)
	if err != nil {
		t.Fatalf("load should fall back to the embedded config, got: %v", err)
	}
	if cfg.GitHub.Label == "" {
		t.Fatal("embedded config should produce a usable label")
	}
	if len(cfg.GitHub.Owners) == 0 {
		t.Fatal("embedded config should produce a usable owners list")
	}
}

func TestLoadUsesDefaultsWhenOwnersProvided(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"github":{"owners":["acme"]}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.Label != "agent-ready" {
		t.Fatalf("unexpected default label %q", cfg.GitHub.Label)
	}
	if cfg.Run.MaxConcurrentRepos != 3 {
		t.Fatalf("unexpected default concurrency %d", cfg.Run.MaxConcurrentRepos)
	}
}

func TestOwnersOfOnlyBlanksIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"owners":["  ",""]}}`), false)
	if err == nil || !strings.Contains(err.Error(), "owners") {
		t.Fatalf("want an owners validation error, got %v", err)
	}
}

func TestLoadOverlaysOntoDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"github":{"label":"claude-please","poll_interval":"90s","owners":["acme"]}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.Label != "claude-please" {
		t.Fatalf("label = %q", cfg.GitHub.Label)
	}
	if cfg.GitHub.PollInterval.D() != 90*time.Second {
		t.Fatalf("poll interval = %v", cfg.GitHub.PollInterval.D())
	}
	// Untouched fields keep their defaults.
	if cfg.GitHub.WorkingLabel != "agent-working" {
		t.Fatalf("working label should keep its default, got %q", cfg.GitHub.WorkingLabel)
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"labl":"typo"}}`), false)
	if err == nil {
		t.Fatal("a misspelled key must fail loudly rather than be silently ignored")
	}
}

func TestBadDurationIsReportedClearly(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"poll_interval":"5 minutes"}}`), false)
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("want a duration parse error, got %v", err)
	}
}

// A lease shorter than the run timeout means a live run can have its claim
// stolen mid-flight, so it must be rejected at load time.
func TestLeaseMustExceedTimeout(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"owners":["acme"]},"run":{"timeout":"45m","lease":"10m"}}`), false)
	if err == nil || !strings.Contains(err.Error(), "must exceed") {
		t.Fatalf("want a lease/timeout validation error, got %v", err)
	}
}

func TestEmptyLabelIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"label":""}}`), false)
	if err == nil || !strings.Contains(err.Error(), "label") {
		t.Fatalf("an empty label would match every issue; want an error, got %v", err)
	}
}

func TestAggressiveUsagePollIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, `{"claude":{"usage_poll_interval":"5s"}}`), false)
	if err == nil {
		t.Fatal("the usage endpoint rate-limits hard; a 5s poll must be rejected")
	}
}

func TestPathsAreExpandedAndAbsolute(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"github":{"owners":["acme"]},"store":{"path":"~/.agent-loop/test.db"}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(cfg.Store.Path, home) {
		t.Fatalf("~ not expanded: %q", cfg.Store.Path)
	}
	if !filepath.IsAbs(cfg.Workspace.Root) {
		t.Fatalf("workspace root should be absolute: %q", cfg.Workspace.Root)
	}
}

func TestExcluded(t *testing.T) {
	g := GitHubConfig{ExcludeRepos: []string{"acme/secrets"}}
	if !g.Excluded("ACME/Secrets") {
		t.Fatal("exclusion should be case-insensitive")
	}
	if g.Excluded("acme/other") {
		t.Fatal("unrelated repo must not be excluded")
	}
}

func TestOwned(t *testing.T) {
	g := GitHubConfig{Owners: []string{"Acme", " ", ""}}
	if !g.Owned("acme/widgets") {
		t.Fatal("owner match should be case-insensitive")
	}
	if g.Owned("someoneelse/widgets") {
		t.Fatal("repo owned by someone else must not be considered owned")
	}
	if g.Owned("not-a-repo-string") {
		t.Fatal("a repo without an owner/name split must never be considered owned")
	}
}

func TestDurationRoundTrip(t *testing.T) {
	d := Duration(90 * time.Second)
	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Duration
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if back.D() != 90*time.Second {
		t.Fatalf("round trip lost value: %v", back.D())
	}
}

func TestRetryBackoffMaxMustNotBeBelowTheBase(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"owners":["acme"]},"run":{"retry_backoff":"1h","retry_backoff_max":"10m"}}`), false)
	if err == nil || !strings.Contains(err.Error(), "retry_backoff") {
		t.Fatalf("want a backoff validation error, got %v", err)
	}
}

// Agent-authored commits should carry their own identity by default, distinct
// from whatever the host's global gitconfig would otherwise resolve to.
func TestGitIdentityDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Git.AuthorName != "coding-agent-loop[bot]" {
		t.Fatalf("unexpected default git author name %q", cfg.Git.AuthorName)
	}
	if cfg.Git.AuthorEmail != "coding-agent-loop@users.noreply.github.com" {
		t.Fatalf("unexpected default git author email %q", cfg.Git.AuthorEmail)
	}
}

func TestGitIdentityOverlaysFromConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"github":{"owners":["acme"]},"git":{"author_name":"Acme Bot","author_email":"bot@acme.example"}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Git.AuthorName != "Acme Bot" {
		t.Fatalf("author_name = %q", cfg.Git.AuthorName)
	}
	if cfg.Git.AuthorEmail != "bot@acme.example" {
		t.Fatalf("author_email = %q", cfg.Git.AuthorEmail)
	}
}

// A name/email containing '<', '>', or a newline would corrupt the commit
// header line git builds ("Name <email>").
func TestGitIdentityWithAngleBracketIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"owners":["acme"]},"git":{"author_name":"evil>name"}}`), false)
	if err == nil || !strings.Contains(err.Error(), "author_name") {
		t.Fatalf("want a git.author_name validation error, got %v", err)
	}
}

func TestGitIdentityEmailWithAngleBracketIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"owners":["acme"]},"git":{"author_email":"<bad@example.com"}}`), false)
	if err == nil || !strings.Contains(err.Error(), "author_email") {
		t.Fatalf("want a git.author_email validation error, got %v", err)
	}
}

func TestPRCommentsDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"github":{"owners":["acme"]}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	pc := cfg.GitHub.PRComments
	if !pc.Enabled {
		t.Fatal("pr_comments should be enabled by default")
	}
	if pc.Mention != "@coding-agent" {
		t.Fatalf("unexpected default mention %q", pc.Mention)
	}
	if pc.AckReaction != "eyes" || pc.DoneReaction != "+1" {
		t.Fatalf("unexpected default reactions: ack=%q done=%q", pc.AckReaction, pc.DoneReaction)
	}
	if len(pc.AllowedAssociations) == 0 {
		t.Fatal("default allowed associations should not be empty")
	}
}

// A config file written before this feature existed has no pr_comments block
// at all; it must still load using the defaults.
func TestConfigWithoutPRCommentsBlockStillLoads(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"github":{"owners":["acme"],"label":"agent-ready"}}`), false)
	if err != nil {
		t.Fatalf("a config predating pr_comments must still load: %v", err)
	}
	if !cfg.GitHub.PRComments.Enabled {
		t.Fatal("defaults should still populate pr_comments")
	}
}

func TestPRCommentsRejectsInvalidReaction(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"owners":["acme"],"pr_comments":{"enabled":true,"mention":"@coding-agent","search_limit":30,"ack_reaction":"nope","done_reaction":"+1"}}}`), false)
	if err == nil || !strings.Contains(err.Error(), "ack_reaction") {
		t.Fatalf("want an invalid reaction error, got %v", err)
	}
}

func TestPRCommentsRejectsMentionWithoutAt(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"owners":["acme"],"pr_comments":{"enabled":true,"mention":"coding-agent","search_limit":30,"ack_reaction":"eyes","done_reaction":"+1"}}}`), false)
	if err == nil || !strings.Contains(err.Error(), "mention") {
		t.Fatalf("want a mention validation error, got %v", err)
	}
}

// The password field must exist on ServerConfig before DisallowUnknownFields
// will let config.example.json carry it.
func TestServerPasswordLoads(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"github":{"owners":["acme"]},"server":{"password":"s3cret"}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Password != "s3cret" {
		t.Fatalf("server.password = %q, want %q", cfg.Server.Password, "s3cret")
	}
}

func TestDefaultServerPasswordIsEmpty(t *testing.T) {
	if got := Default().Server.Password; got != "" {
		t.Fatalf("Default().Server.Password = %q, want empty (auth disabled by default)", got)
	}
}
