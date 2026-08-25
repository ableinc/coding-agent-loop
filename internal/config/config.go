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

	embedded "github.com/ableinc/coding-agent-loop"
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
	Git       GitConfig       `json:"git"`

	// ModelsPath points at models.json, resolved by the caller (cmd/agent.go),
	// not expanded here: "models.json" (the default) is looked up next to the
	// running binary, falling back to the embedded models.json when absent;
	// any other value is an explicit path that must exist.
	ModelsPath string `json:"models_path"`
}

type GitHubConfig struct {
	// Label is the opt-in label an issue must carry to be picked up.
	Label string `json:"label"`
	// WorkingLabel/DoneLabel/FailedLabel/PlanLabel mirror run state onto the issue.
	WorkingLabel string `json:"working_label"`
	DoneLabel    string `json:"done_label"`
	FailedLabel  string `json:"failed_label"`
	// PlanLabel marks an issue that has a plan comment awaiting human approval.
	PlanLabel string `json:"plan_label"`
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
	// PRComments controls responding to @-mentions on the daemon's own pull requests.
	PRComments PRCommentsConfig `json:"pr_comments"`
}

// PRCommentsConfig controls watching the daemon's own pull requests for
// review comments that @-mention it, and acting on them.
type PRCommentsConfig struct {
	Enabled bool `json:"enabled"`
	// Mention is the handle a comment must contain to trigger a response,
	// e.g. "@coding-agent".
	Mention string `json:"mention"`
	// SearchLimit caps how many of the daemon's own open PRs one pass checks.
	SearchLimit int `json:"search_limit"`
	// MaxAge bounds how old a comment may be and still trigger a response. 0
	// means no limit.
	MaxAge Duration `json:"max_age"`
	// AckReaction/DoneReaction are GitHub reaction content values (one of
	// "+1 -1 laugh confused heart hooray rocket eyes") applied to a triggering
	// comment when it is picked up and when it has been addressed.
	AckReaction  string `json:"ack_reaction"`
	DoneReaction string `json:"done_reaction"`
	// AllowedAuthors is an explicit login allowlist of commenters who may
	// trigger the agent. Empty falls back to AllowedAssociations.
	AllowedAuthors []string `json:"allowed_authors"`
	// AllowedAssociations lists the author_association values (OWNER, MEMBER,
	// COLLABORATOR, ...) permitted to trigger the agent when AllowedAuthors is
	// empty.
	AllowedAssociations []string `json:"allowed_associations"`
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
	// PlanPermissionMode governs the read-only planning run, distinct from the
	// implement run's PermissionMode.
	PlanPermissionMode string `json:"plan_permission_mode"`
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
	// Env is added to the environment of every verification command. It exists
	// mainly for PATH: a daemon started by systemd does not inherit a login
	// shell's PATH, so language toolchains installed outside /usr/bin (Go under
	// /usr/local/go/bin, anything under ~/go/bin or a version manager) are
	// invisible to it and the repository's own test command cannot run.
	Env map[string]string `json:"env"`
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

// GitConfig identifies the author of commits the loop produces, so they are
// visibly distinct from the human repo owner's own commits. Empty means "use
// the built-in default" — see git.Manager's author()/email().
type GitConfig struct {
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
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
			PlanLabel:    "agent-planned",
			SearchLimit:  50,
			PollInterval: Duration(5 * time.Minute),
			Binary:       "gh",
			PRComments: PRCommentsConfig{
				Enabled:             true,
				Mention:             "@coding-agent",
				SearchLimit:         30,
				MaxAge:              Duration(168 * time.Hour),
				AckReaction:         "eyes",
				DoneReaction:        "+1",
				AllowedAssociations: []string{"OWNER", "MEMBER", "COLLABORATOR"},
			},
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
			Binary:             "claude",
			PermissionMode:     "bypassPermissions",
			PlanPermissionMode: "plan",
			UsagePollInterval:  Duration(15 * time.Minute),
			UsageBackoff:       Duration(15 * time.Minute),
			CredentialsPath:    "~/.claude/.credentials.json",
			UsageCachePath:     "~/.agent-loop/usage-cache.json",
		},
		Verify:  VerifyConfig{AutoDetect: true, Commands: map[string]string{}},
		Server:  ServerConfig{Addr: "127.0.0.1:8787"},
		Store:   StoreConfig{Path: "~/.agent-loop/state.db"},
		Discord: DiscordConfig{Enabled: false},
		Git: GitConfig{
			AuthorName:  "coding-agent-loop[bot]",
			AuthorEmail: "coding-agent-loop@users.noreply.github.com",
		},
	}
}

// Load reads path over the defaults and validates the result. When
// allowEmbeddedFallback is true and path does not exist, the repo-root
// config.json embedded in the binary at build time (embedded.Config) is used
// instead, so a binary shipped without the rest of the repo still boots with
// a complete, working configuration. allowEmbeddedFallback should be false
// whenever path was explicitly requested (e.g. --config passed on the
// command line): an explicit path that does not exist is a misconfiguration
// to report, not something to silently paper over.
func Load(path string, allowEmbeddedFallback bool) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err) && allowEmbeddedFallback:
		data = embedded.Config
	case os.IsNotExist(err):
		return cfg, fmt.Errorf("config file %s not found", path)
	case err != nil:
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}

	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
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
	if len(validOwners(c.GitHub.Owners)) == 0 {
		return fmt.Errorf("github.owners must list at least one owner: an empty list would scan every repository the token can see")
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
	if containsCommitHeaderBreakingChars(c.Git.AuthorName) {
		return fmt.Errorf("git.author_name must not contain '<', '>', or a newline")
	}
	if containsCommitHeaderBreakingChars(c.Git.AuthorEmail) {
		return fmt.Errorf("git.author_email must not contain '<', '>', or a newline")
	}
	if c.GitHub.PRComments.Enabled {
		pc := c.GitHub.PRComments
		if !strings.HasPrefix(pc.Mention, "@") {
			return fmt.Errorf("github.pr_comments.mention must start with '@', got %q", pc.Mention)
		}
		if pc.SearchLimit < 1 {
			return fmt.Errorf("github.pr_comments.search_limit must be >= 1, got %d", pc.SearchLimit)
		}
		if !validReaction(pc.AckReaction) {
			return fmt.Errorf("github.pr_comments.ack_reaction %q is not a valid GitHub reaction", pc.AckReaction)
		}
		if !validReaction(pc.DoneReaction) {
			return fmt.Errorf("github.pr_comments.done_reaction %q is not a valid GitHub reaction", pc.DoneReaction)
		}
	}
	return nil
}

// containsCommitHeaderBreakingChars reports whether s has a character that
// would corrupt the "Name <email>" line git writes into a commit header.
func containsCommitHeaderBreakingChars(s string) bool {
	return strings.ContainsAny(s, "<>\n")
}

// validReactions are the only content values GitHub's reactions API accepts.
var validReactions = map[string]bool{
	"+1": true, "-1": true, "laugh": true, "confused": true,
	"heart": true, "hooray": true, "rocket": true, "eyes": true,
}

func validReaction(r string) bool { return validReactions[r] }

// validOwners returns the entries of owners that are non-blank after trimming.
func validOwners(owners []string) []string {
	out := make([]string, 0, len(owners))
	for _, o := range owners {
		if strings.TrimSpace(o) != "" {
			out = append(out, o)
		}
	}
	return out
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

// Owned reports whether repo ("owner/name") belongs to one of the configured
// owners. Validate rejects a config with no owners, so this is always a real
// allowlist check, never "everything passes" by default. Callers that touch a
// repository — discovery, cloning, commenting, labeling, opening a PR — must
// check this before doing anything with a repo string that did not originate
// from this allowlist, as a second line of defense against a search result,
// stale store entry, or future code path smuggling in a repo we don't own.
func (g GitHubConfig) Owned(repo string) bool {
	owner, _, ok := strings.Cut(repo, "/")
	if !ok {
		return false
	}
	for _, o := range validOwners(g.Owners) {
		if strings.EqualFold(o, owner) {
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
