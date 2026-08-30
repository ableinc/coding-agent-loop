// Command agent runs the coding-agent-loop daemon: it discovers labelled
// GitHub issues, has Claude Code implement them in isolated git worktrees, and
// opens draft pull requests for human review.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/claude"
	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/discord"
	"github.com/ableinc/coding-agent-loop/internal/gate"
	"github.com/ableinc/coding-agent-loop/internal/gh"
	gitpkg "github.com/ableinc/coding-agent-loop/internal/git"
	"github.com/ableinc/coding-agent-loop/internal/install"
	"github.com/ableinc/coding-agent-loop/internal/models"
	"github.com/ableinc/coding-agent-loop/internal/orchestrator"
	"github.com/ableinc/coding-agent-loop/internal/server"
	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/verify"
)

type flags struct {
	configPath    string
	once          bool
	dryRun        bool
	noMutate      bool
	logLevel      string
	noServer      bool
	checkOnly     bool
	install       bool
	uninstall     bool
	purge         bool
	printUnit     bool
	migrateConfig bool
}

const usageHeader = `coding-agent-loop — autonomous GitHub issue to draft PR agent

Discovers issues labelled "agent-ready" (configurable) across your GitHub
repositories, has Claude Code implement each one in an isolated git worktree,
runs the repository's own tests, and opens a draft pull request for human
review. It never merges anything.

Usage:
  coding-agent-loop [flags]              start the daemon in the foreground
  coding-agent-loop --once               run one discovery pass, then exit
  coding-agent-loop --once --dry-run     report what one pass would do, free: no Claude run, no cost
  coding-agent-loop --once --no-mutate   really run Claude (costs usage), but push/PR/label nothing
  coding-agent-loop --check              verify prerequisites (git, claude, gh, config) and exit
  coding-agent-loop --print-service      print the embedded systemd unit
  coding-agent-loop --migrate-config     bring config.json up to the current schema, in place
  sudo coding-agent-loop --install       install, enable, and start the systemd unit
  sudo coding-agent-loop --uninstall     stop, disable, and remove the service only
  sudo coding-agent-loop --uninstall --purge   also delete workspace/logs/state data

See README.md for configuration (config.json, models.json) and the control API.

Flags:
`

func main() {
	var f flags
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usageHeader)
		flag.PrintDefaults()
	}
	flag.StringVar(&f.configPath, "config", "", "path to the configuration file (default: config.json next to the binary, falling back to the config.json embedded at build time)")
	flag.BoolVar(&f.once, "once", false, "run a single discovery pass, then exit")
	flag.BoolVar(&f.dryRun, "dry-run", false, "report what each issue would get and do none of it: no Claude run, no clone, no mutation, no cost")
	flag.BoolVar(&f.noMutate, "no-mutate", false, "run Claude for real (this costs usage) but push nothing, open no PR, and edit no issue")
	flag.StringVar(&f.logLevel, "log-level", "info", "log level: debug, info, warn, error")
	flag.BoolVar(&f.noServer, "no-server", false, "do not start the control API")
	flag.BoolVar(&f.checkOnly, "check", false, "run start-up checks and exit")
	flag.BoolVar(&f.install, "install", false, "install the systemd unit (embedded in this binary), enable it, and start it; must run as root")
	flag.BoolVar(&f.uninstall, "uninstall", false, "stop, disable, and remove the systemd unit and /opt/coding-agent-loop; leaves data in place unless --purge is also given; must run as root")
	flag.BoolVar(&f.purge, "purge", false, "with --uninstall: also delete the service's data — workspace.root, workspace.repos_root, workspace.logs_root, store.path, claude.usage_cache_path, and the dedicated service account's home. Without it, --uninstall removes only the service and leaves all data in place")
	flag.BoolVar(&f.printUnit, "print-service", false, "print the systemd unit --install would write and exit")
	flag.BoolVar(&f.migrateConfig, "migrate-config", false, "rewrite -config to the current config.json schema: keep every value already set, add new fields at their default, drop and report fields the schema no longer has; the original is saved as -config.bak first. Combine with -dry-run to preview on stdout instead of writing anything")
	flag.Parse()

	if err := validateFlags(f); err != nil {
		fmt.Fprintf(os.Stderr, "coding-agent-loop: %v\n", err)
		os.Exit(1)
	}

	if f.printUnit {
		unit, err := install.PreviewUnit()
		if err != nil {
			fmt.Fprintf(os.Stderr, "coding-agent-loop: %v\n", err)
			os.Exit(1)
		}
		os.Stdout.Write(unit)
		return
	}

	if err := run(f); err != nil {
		fmt.Fprintf(os.Stderr, "coding-agent-loop: %v\n", err)
		os.Exit(1)
	}
}

// validateFlags rejects flag combinations that parse individually but make
// no sense together.
func validateFlags(f flags) error {
	if f.purge && !f.uninstall {
		return fmt.Errorf("--purge only applies to --uninstall (try: sudo coding-agent-loop --uninstall --purge)")
	}
	return nil
}

func run(f flags) error {
	log := newLogger(f.logLevel)

	if f.install {
		return install.Run(install.Options{
			ConfigPath: f.configPath,
			Log:        func(format string, args ...any) { log.Info(format, args...) },
		})
	}
	if f.uninstall {
		return install.Uninstall(install.UninstallOptions{
			ConfigPath: f.configPath,
			Purge:      f.purge,
			Log:        func(format string, args ...any) { log.Info(format, args...) },
		})
	}

	configPath, allowConfigFallback, err := resolveConfigPath(f.configPath)
	if err != nil {
		return err
	}

	if f.migrateConfig {
		return migrateConfig(configPath, f.dryRun)
	}

	cfg, err := config.Load(configPath, allowConfigFallback)
	if err != nil {
		return err
	}

	modelsPath, allowModelsFallback := cfg.ModelsPath, cfg.ModelsPath == "models.json"
	if allowModelsFallback {
		dir, err := execDir()
		if err != nil {
			return fmt.Errorf("resolve default models.json location: %w", err)
		}
		modelsPath = filepath.Join(dir, cfg.ModelsPath)
	} else {
		expanded, err := config.ExpandPath(modelsPath)
		if err != nil {
			return fmt.Errorf("resolve models_path %q: %w", modelsPath, err)
		}
		modelsPath = expanded
	}
	registry, err := models.Load(modelsPath, allowModelsFallback)
	if err != nil {
		return err
	}

	suppressMutations := f.dryRun || f.noMutate
	ghClient := gh.New(cfg.GitHub.Binary, suppressMutations)
	ghClient.Log = func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) }

	if err := bootCheck(cfg, registry, ghClient, log); err != nil {
		return err
	}
	if f.checkOnly {
		return nil
	}

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return err
	}
	defer st.Close()

	// Resolved to an absolute path so git's credential helper finds gh by
	// that path rather than by name on $PATH — a systemd service's $PATH
	// does not necessarily include wherever gh was installed, even though
	// bootCheck already confirmed gh itself is reachable and authenticated.
	ghBinary := cfg.GitHub.Binary
	if abs, err := exec.LookPath(cfg.GitHub.Binary); err == nil {
		ghBinary = abs
	}
	gitMgr := &gitpkg.Manager{
		ReposRoot:   cfg.Workspace.ReposRoot,
		WorkRoot:    cfg.Workspace.Root,
		AuthorName:  cfg.Git.AuthorName,
		AuthorEmail: cfg.Git.AuthorEmail,
		GHBinary:    ghBinary,
		DryRun:      suppressMutations,
		Log:         func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) },
	}
	runner := &claude.Runner{Log: func(format string, args ...any) { log.Debug(fmt.Sprintf(format, args...)) }}
	gateway := gate.New(st, cfg.Claude, func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) })
	verifier := &verify.Runner{Cfg: cfg.Verify, Timeout: cfg.Run.VerifyTimeout.D()}
	notifier := discord.New(cfg.Discord.Enabled, cfg.Discord.WebhookURL,
		func(format string, args ...any) { log.Warn(fmt.Sprintf(format, args...)) })

	orch := orchestrator.New(orchestrator.Options{
		Config: cfg, Store: st, Registry: registry,
		GH: ghClient, Git: gitMgr, Runner: runner, Gate: gateway, Verify: verifier,
		Logger: log, DryRun: suppressMutations, Rehearse: f.dryRun, Discord: notifier,
	})

	// SIGINT/SIGTERM cancels the root context; the loop then drains in-flight
	// runs rather than abandoning a half-finished worktree.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case f.dryRun:
		log.Warn("dry-run: reporting what each issue would get; " +
			"Claude will not run, nothing will be cloned, and nothing will be mutated")
	case f.noMutate:
		log.Warn("no-mutate: Claude WILL run and this WILL use subscription usage; " +
			"no branch will be pushed, no PR opened, no issue edited")
	}

	if f.once {
		log.Info("single discovery pass")
		return orch.RunOnce(ctx)
	}

	workerID, _ := os.Hostname()
	if workerID == "" {
		workerID = "coding-agent-loop"
	}
	notifier.DaemonStarted(discord.DaemonInfo{
		Worker:             workerID,
		Label:              cfg.GitHub.Label,
		Owners:             cfg.GitHub.Owners,
		PollInterval:       cfg.GitHub.PollInterval.D(),
		MaxConcurrentRepos: cfg.Run.MaxConcurrentRepos,
		RetryBackoff:       cfg.Run.RetryBackoff.D(),
		RetryBackoffMax:    cfg.Run.RetryBackoffMax.D(),
		DryRun:             suppressMutations,
	})

	errCh := make(chan error, 2)
	if !f.noServer {
		srv := server.New(server.Options{
			Addr: cfg.Server.Addr, Store: st, Gate: gateway, Ctrl: orch,
			Log: log, Discord: notifier, Config: cfg, Registry: registry,
		})
		go func() { errCh <- srv.Listen(ctx) }()
	}
	go func() { errCh <- orch.Run(ctx) }()

	// Wait for the first component to stop, then let the other unwind.
	err = <-errCh
	stop()
	select {
	case <-errCh:
	case <-time.After(30 * time.Second):
		log.Warn("shutdown timed out waiting for components")
	}

	if err != nil {
		notifier.DaemonStopped(fmt.Sprintf("crash: %v", err))
	} else {
		notifier.DaemonStopped("graceful shutdown")
	}
	notifier.Close(3 * time.Second)

	return err
}

// execDir returns the directory containing the running binary — resolved
// through any symlink (e.g. a PATH entry) — not the process's current
// working directory. This is where a config.json/models.json placed "next to
// the binary" is looked up by default, so the answer stays the same
// regardless of where the binary happens to be invoked from.
func execDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve running binary path: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve running binary path: %w", err)
	}
	return filepath.Dir(exe), nil
}

// resolveConfigPath applies --config's default: when unset, config.json next
// to the running binary, falling back to the embedded copy if that's also
// absent. allowFallback reports whether that fallback applies, mirroring
// config.Load's allowEmbeddedFallback parameter.
func resolveConfigPath(configFlag string) (path string, allowFallback bool, err error) {
	if configFlag != "" {
		return configFlag, false, nil
	}
	dir, err := execDir()
	if err != nil {
		return "", false, fmt.Errorf("resolve default config.json location: %w", err)
	}
	return filepath.Join(dir, "config.json"), true, nil
}

// migrateConfig rewrites the config.json at path to the current schema:
// every value it already sets is kept, any field the schema has gained since
// is added at its default, and any field the schema has since dropped is
// reported and left out — unlike config.Load, which uses
// DisallowUnknownFields and would reject exactly that file. dryRun prints the
// migrated config to stdout instead of writing it.
//
// Unlike the rest of run(), this always requires an actual file at path: the
// embedded config is fresh defaults with nothing to bring forward, so
// falling back to it here would silently produce a no-op.
func migrateConfig(path string, dryRun bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s does not exist — nothing to migrate (run `make config` to create one from config.example.json)", path)
		}
		return fmt.Errorf("read %s: %w", path, err)
	}

	result, err := config.Migrate(raw)
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(result.Config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal migrated config: %w", err)
	}
	out = append(out, '\n')

	reportMigration(result)

	if dryRun {
		os.Stdout.Write(out)
		return nil
	}

	backup := path + ".bak"
	if err := os.WriteFile(backup, raw, 0o600); err != nil {
		return fmt.Errorf("back up %s to %s: %w", path, backup, err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "coding-agent-loop: wrote %s (original saved as %s)\n", path, backup)

	// Advisory only: an old file that was already broken stays broken until a
	// human fixes it, but the migration itself — bringing it forward to the
	// current shape — is done and worth keeping either way.
	if verr := result.Config.Validate(); verr != nil {
		fmt.Fprintf(os.Stderr, "coding-agent-loop: warning: migrated config does not pass validation: %v\n", verr)
	}
	return nil
}

func reportMigration(result config.MigrateResult) {
	if len(result.Added) == 0 && len(result.Dropped) == 0 {
		fmt.Fprintln(os.Stderr, "coding-agent-loop: config already matches the current schema, nothing to add or drop")
		return
	}
	if len(result.Added) > 0 {
		fmt.Fprintln(os.Stderr, "coding-agent-loop: added at default:")
		for _, k := range result.Added {
			fmt.Fprintf(os.Stderr, "  + %s\n", k)
		}
	}
	if len(result.Dropped) > 0 {
		fmt.Fprintln(os.Stderr, "coding-agent-loop: dropped (no longer part of the schema):")
		for _, k := range result.Dropped {
			fmt.Fprintf(os.Stderr, "  - %s\n", k)
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

// bootCheck fails fast on the things that would otherwise blow up mid-run, and
// says what to do about each one.
func bootCheck(cfg config.Config, registry *models.Registry, ghClient *gh.Client, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var problems []string

	if _, err := exec.LookPath("git"); err != nil {
		problems = append(problems, "git is not on PATH")
	}
	if _, err := exec.LookPath(cfg.Claude.Binary); err != nil {
		problems = append(problems, fmt.Sprintf("claude binary %q is not on PATH (install Claude Code)", cfg.Claude.Binary))
	}
	if _, err := exec.LookPath(cfg.GitHub.Binary); err != nil {
		problems = append(problems, fmt.Sprintf("gh binary %q is not on PATH (install the GitHub CLI)", cfg.GitHub.Binary))
	} else if err := ghClient.AuthStatus(ctx); err != nil {
		problems = append(problems, "gh is not authenticated — run `gh auth login` (scopes: repo, read:org)")
	}

	if _, err := os.Stat(cfg.Claude.CredentialsPath); err != nil {
		// Not fatal: the CLI may authenticate another way. The usage snapshot
		// is advisory, so losing it does not stop the loop.
		log.Warn("claude credentials not readable; the /status usage snapshot will be unavailable",
			"path", cfg.Claude.CredentialsPath)
	}

	for _, dir := range []string{
		cfg.Workspace.Root, cfg.Workspace.ReposRoot, cfg.Workspace.LogsRoot, filepath.Dir(cfg.Store.Path),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			problems = append(problems, fmt.Sprintf("cannot create %s: %v", dir, err))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("start-up checks failed:\n  - %s", strings.Join(problems, "\n  - "))
	}

	ladderRefs := func(role string) string {
		ladder := registry.Ladder(role, nil)
		refs := make([]string, 0, len(ladder))
		for _, m := range ladder {
			refs = append(refs, m.ID)
		}
		return strings.Join(refs, " > ")
	}
	log.Info("start-up checks passed",
		"label", cfg.GitHub.Label,
		"owners", strings.Join(cfg.GitHub.Owners, ","),
		"plan_ladder", ladderRefs(models.RolePlan),
		"implement_ladder", ladderRefs(models.RoleImplement),
		"store", cfg.Store.Path)
	return nil
}
