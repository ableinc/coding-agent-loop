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

func TestLoadMissingFileRequiresOwners(t *testing.T) {
	// Defaults alone are not loadable: github.owners has no sane default, and an
	// empty list would silently scan every repository the token can see.
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil || !strings.Contains(err.Error(), "owners") {
		t.Fatalf("want an owners validation error, got %v", err)
	}
}

func TestLoadUsesDefaultsWhenOwnersProvided(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"github":{"owners":["acme"]}}`))
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
	_, err := Load(writeConfig(t, `{"github":{"owners":["  ",""]}}`))
	if err == nil || !strings.Contains(err.Error(), "owners") {
		t.Fatalf("want an owners validation error, got %v", err)
	}
}

func TestLoadOverlaysOntoDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"github":{"label":"claude-please","poll_interval":"90s","owners":["acme"]}}`))
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
	_, err := Load(writeConfig(t, `{"github":{"labl":"typo"}}`))
	if err == nil {
		t.Fatal("a misspelled key must fail loudly rather than be silently ignored")
	}
}

func TestBadDurationIsReportedClearly(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"poll_interval":"5 minutes"}}`))
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("want a duration parse error, got %v", err)
	}
}

// A lease shorter than the run timeout means a live run can have its claim
// stolen mid-flight, so it must be rejected at load time.
func TestLeaseMustExceedTimeout(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"owners":["acme"]},"run":{"timeout":"45m","lease":"10m"}}`))
	if err == nil || !strings.Contains(err.Error(), "must exceed") {
		t.Fatalf("want a lease/timeout validation error, got %v", err)
	}
}

func TestEmptyLabelIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, `{"github":{"label":""}}`))
	if err == nil || !strings.Contains(err.Error(), "label") {
		t.Fatalf("an empty label would match every issue; want an error, got %v", err)
	}
}

func TestAggressiveUsagePollIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, `{"claude":{"usage_poll_interval":"5s"}}`))
	if err == nil {
		t.Fatal("the usage endpoint rate-limits hard; a 5s poll must be rejected")
	}
}

func TestPathsAreExpandedAndAbsolute(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"github":{"owners":["acme"]},"store":{"path":"~/.agent-loop/test.db"}}`))
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
	_, err := Load(writeConfig(t, `{"github":{"owners":["acme"]},"run":{"retry_backoff":"1h","retry_backoff_max":"10m"}}`))
	if err == nil || !strings.Contains(err.Error(), "retry_backoff") {
		t.Fatalf("want a backoff validation error, got %v", err)
	}
}
