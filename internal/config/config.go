// Package config loads and validates the daemon's configuration.
//
// Configuration lives in a JSON file (default ./config.json). Durations are
// written as Go duration strings ("5m", "45m") and parsed on load, so an
// invalid duration is a startup error rather than a surprise at runtime.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Duration wraps time.Duration so it can be read from a JSON string.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.D().String()) }

// D returns the wrapped time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the whole daemon configuration.
type Config struct {
	GitHub    GitHubConfig    `json:"github"`
	Workspace WorkspaceConfig `json:"workspace"`
	Run       RunConfig       `json:"run"`
	Claude    ClaudeConfig    `json:"claude"`
	Verify    VerifyConfig    `json:"verify"`
	Server    ServerConfig    `json:"server"`
	Store     StoreConfig     `json:"store"`
	Discord   DiscordConfig   `json:"discord"`

	// ModelsPath points at models.json. Defaults to ./models.json.
	ModelsPath string `json:"models_path"`
}

type GitHubConfig struct {
	// Label is the opt-in label an issue must carry to be picked up.
	Label string `json:"label"`
	// WorkingLabel/DoneLabel/FailedLabel mirror run state onto the issue.
	WorkingLabel string `json:"working_label"`
	DoneLabel    string `json:"done_label"`
	FailedLabel  string `json:"failed_label"`
	// Owners scopes discovery. Empty means "every repo the token can see",
	// which is broad — prefer naming the orgs/users you actually want.
	Owners []string `json:"owners"`
	// ExcludeRepos is the per-repo veto on org-wide discovery, as "owner/name".
	ExcludeRepos []string `json:"exclude_repos"`
	// SearchLimit caps how many issues one discovery pass returns.
	SearchLimit  int      `json:"search_limit"`
	PollInterval Duration `json:"poll_interval"`
	// Binary is the gh executable; overridable for tests.
	Binary string `json:"binary"`
}

type WorkspaceConfig struct {
	// Root holds per-issue worktrees.
	Root string `json:"root"`
	// ReposRoot holds the shared mirror clones.
	ReposRoot string `json:"repos_root"`
	// LogsRoot holds per-run JSONL transcripts.
	LogsRoot string `json:"logs_root"`
	// KeepFailed leaves a failed run's worktree on disk for inspection.
	KeepFailed bool `json:"keep_failed"`
	// BranchPrefix is prepended to generated branch names.
	BranchPrefix string `json:"branch_prefix"`
}

type RunConfig struct {
	// MaxConcurrentRepos bounds how many repos are worked in parallel. Work
	// within a single repo is always serial.
	MaxConcurrentRepos int `json:"max_concurrent_repos"`
	// RetryBackoff is how long a failed issue waits before it may be claimed
	// again. It doubles with every consecutive failure — 15m, 30m, 1h, ... —
	// so a permanently broken issue costs a trickle of runs rather than a
	// worker, but is still eventually retried instead of going stale.
	RetryBackoff Duration `json:"retry_backoff"`
	// RetryBackoffMax caps that doubling.
	RetryBackoffMax Duration `json:"retry_backoff_max"`
	// Timeout bounds one Claude run.
	Timeout Duration `json:"timeout"`
	// Lease is how long a claim is held before another worker may steal it.
	// Must exceed Timeout or a live run can have its claim taken.
	Lease Duration `json:"lease"`
	// VerifyTimeout bounds the repo's test command.
	VerifyTimeout Duration `json:"verify_timeout"`
}

type ClaudeConfig struct {
	Binary         string   `json:"binary"`
	PermissionMode string   `json:"permission_mode"`
	ExtraArgs      []string `json:"extra_args"`
	// UsagePollInterval throttles the OAuth usage endpoint, which rate-limits
	// hard. Do not lower this below a few minutes.
	UsagePollInterval Duration `json:"usage_poll_interval"`
	// UsageBackoff is how long to wait after a 429 from that endpoint.
	UsageBackoff Duration `json:"usage_backoff"`
	// CredentialsPath is where the OAuth token is read from.
	CredentialsPath string `json:"credentials_path"`
	// UsageCachePath is our own cache file. Deliberately not the statusline's
	// ~/.claude/.usage-cache.json, so the two don't clobber each other.
	UsageCachePath string `json:"usage_cache_path"`
}

type VerifyConfig struct {
	// AutoDetect picks a test command from the repo layout when Commands has
	// no entry for it.
	AutoDetect bool `json:"auto_detect"`
	// Commands maps "owner/name" to an explicit shell command.
	Commands map[string]string `json:"commands"`
}

type ServerConfig struct {
	// Addr should stay on loopback: this API can pause and cancel work.
	Addr string `json:"addr"`
}

type StoreConfig struct {
	Path string `json:"path"`
}

// DiscordConfig controls one-way status notifications posted to a Discord
// channel via an incoming webhook. The daemon never reads Discord.
type DiscordConfig struct {
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhook_url"`
}

// Default returns a Config with every field populated to a sane value.
func Default() Config {
	return Config{
		ModelsPath: "models.json",
		GitHub: GitHubConfig{
			Label:        "agent-ready",
			WorkingLabel: "agent-working",
			DoneLabel:    "agent-done",
			FailedLabel:  "agent-failed",
			SearchLimit:  50,
			PollInterval: Duration(5 * time.Minute),
			Binary:       "gh",
		},
		Workspace: WorkspaceConfig{
			Root:         "~/.agent-loop/work",
			ReposRoot:    "~/.agent-loop/repos",
			LogsRoot:     "~/.agent-loop/logs",
			KeepFailed:   true,
			BranchPrefix: "agent/issue-",
		},
		Run: RunConfig{
			MaxConcurrentRepos: 3,
			Timeout:            Duration(45 * time.Minute),
			Lease:              Duration(90 * time.Minute),
			VerifyTimeout:      Duration(10 * time.Minute),
			RetryBackoff:       Duration(15 * time.Minute),
			RetryBackoffMax:    Duration(24 * time.Hour),
		},
		Claude: ClaudeConfig{
			Binary:            "claude",
			PermissionMode:    "bypassPermissions",
			UsagePollInterval: Duration(15 * time.Minute),
			UsageBackoff:      Duration(15 * time.Minute),
			CredentialsPath:   "~/.claude/.credentials.json",
			UsageCachePath:    "~/.agent-loop/usage-cache.json",
		},
		Verify:  VerifyConfig{AutoDetect: true, Commands: map[string]string{}},
		Server:  ServerConfig{Addr: "127.0.0.1:8787"},
		Store:   StoreConfig{Path: "~/.agent-loop/state.db"},
		Discord: DiscordConfig{Enabled: false},
	}
}

// Load reads path over the defaults and validates the result. A missing file
// is not an error: the defaults are usable on their own.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		// keep defaults
	case err != nil:
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	default:
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	if err := cfg.expandPaths(); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) expandPaths() error {
	for _, p := range []*string{
		&c.Workspace.Root, &c.Workspace.ReposRoot, &c.Workspace.LogsRoot,
		&c.Store.Path, &c.Claude.CredentialsPath, &c.Claude.UsageCachePath,
		&c.ModelsPath,
	} {
		expanded, err := ExpandPath(*p)
		if err != nil {
			return err
		}
		*p = expanded
	}
	return nil
}

// Validate rejects configurations that would misbehave at runtime.
func (c *Config) Validate() error {
	if c.GitHub.Label == "" {
		return fmt.Errorf("github.label must be set: it is the opt-in trigger and an empty label would match every issue")
	}
	if c.Run.MaxConcurrentRepos < 1 {
		return fmt.Errorf("run.max_concurrent_repos must be >= 1, got %d", c.Run.MaxConcurrentRepos)
	}
	if c.Run.RetryBackoff.D() <= 0 {
		return fmt.Errorf("run.retry_backoff must be positive: it is the delay before a failed issue is retried")
	}
	if c.Run.RetryBackoffMax.D() < c.Run.RetryBackoff.D() {
		return fmt.Errorf("run.retry_backoff_max (%s) must be >= run.retry_backoff (%s)",
			c.Run.RetryBackoffMax.D(), c.Run.RetryBackoff.D())
	}
	if c.Run.Timeout.D() <= 0 {
		return fmt.Errorf("run.timeout must be positive")
	}
	// A lease shorter than the run timeout lets a second worker steal a claim
	// from a run that is still going.
	if c.Run.Lease.D() <= c.Run.Timeout.D() {
		return fmt.Errorf("run.lease (%s) must exceed run.timeout (%s), otherwise a live run's claim can be stolen",
			c.Run.Lease.D(), c.Run.Timeout.D())
	}
	if c.GitHub.PollInterval.D() <= 0 {
		return fmt.Errorf("github.poll_interval must be positive")
	}
	if c.Claude.UsagePollInterval.D() < time.Minute {
		return fmt.Errorf("claude.usage_poll_interval must be >= 1m: the usage endpoint rate-limits aggressively")
	}
	if c.Store.Path == "" {
		return fmt.Errorf("store.path must be set")
	}
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr must be set")
	}
	if c.Discord.Enabled && c.Discord.WebhookURL == "" {
		return fmt.Errorf("discord.webhook_url must be set when discord.enabled is true")
	}
	return nil
}

// Excluded reports whether repo ("owner/name") is vetoed.
func (g GitHubConfig) Excluded(repo string) bool {
	for _, r := range g.ExcludeRepos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

// ExpandPath resolves a leading ~ and makes the path absolute.
func ExpandPath(p string) (string, error) {
	if p == "" {
		return p, nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir for %q: %w", p, err)
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	return abs, nil
}
