// Command agent runs the coding-agent-loop daemon: it discovers labelled
// GitHub issues, has Claude Code implement them in isolated git worktrees, and
// opens draft pull requests for human review.
package main

import (
	"context"
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
	configPath string
	once       bool
	dryRun     bool
	logLevel   string
	noServer   bool
	checkOnly  bool
	install    bool
	printUnit  bool
}

const usageHeader = `coding-agent-loop — autonomous GitHub issue to draft PR agent

Discovers issues labelled "agent-ready" (configurable) across your GitHub
repositories, has Claude Code implement each one in an isolated git worktree,
runs the repository's own tests, and opens a draft pull request for human
review. It never merges anything.

Usage:
  agent-loop [flags]              start the daemon in the foreground
  agent-loop --once               run one discovery pass, then exit
  agent-loop --once --dry-run     rehearse a pass without pushing/PRs/labels
  agent-loop --check              verify prerequisites (git, claude, gh, config) and exit
  agent-loop --print-service      print the embedded systemd unit
  sudo agent-loop --install       install, enable, and start the systemd unit

See README.md for configuration (config.json, models.json) and the control API.

Flags:
`

func main() {
	var f flags
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usageHeader)
		flag.PrintDefaults()
	}
	flag.StringVar(&f.configPath, "config", "config.json", "path to the configuration file")
	flag.BoolVar(&f.once, "once", false, "run a single discovery pass, then exit")
	flag.BoolVar(&f.dryRun, "dry-run", false, "do everything except push branches, open PRs, or edit issues")
	flag.StringVar(&f.logLevel, "log-level", "info", "log level: debug, info, warn, error")
	flag.BoolVar(&f.noServer, "no-server", false, "do not start the control API")
	flag.BoolVar(&f.checkOnly, "check", false, "run start-up checks and exit")
	flag.BoolVar(&f.install, "install", false, "install the systemd unit (embedded in this binary), enable it, and start it; must run as root")
	flag.BoolVar(&f.printUnit, "print-service", false, "print the embedded systemd unit file and exit")
	flag.Parse()

	if f.printUnit {
		os.Stdout.Write(install.ServiceUnit)
		return
	}

	if err := run(f); err != nil {
		fmt.Fprintf(os.Stderr, "coding-agent-loop: %v\n", err)
		os.Exit(1)
	}
}

func run(f flags) error {
	log := newLogger(f.logLevel)

	if f.install {
		return install.Run(install.Options{
			ConfigPath: f.configPath,
			Log:        func(format string, args ...any) { log.Info(format, args...) },
		})
	}

	cfg, err := config.Load(f.configPath)
	if err != nil {
		return err
	}
	registry, err := models.Load(cfg.ModelsPath)
	if err != nil {
		return err
	}

	ghClient := gh.New(cfg.GitHub.Binary, f.dryRun)
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

	gitMgr := &gitpkg.Manager{
		ReposRoot: cfg.Workspace.ReposRoot,
		WorkRoot:  cfg.Workspace.Root,
		DryRun:    f.dryRun,
		Log:       func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) },
	}
	runner := &claude.Runner{Log: func(format string, args ...any) { log.Debug(fmt.Sprintf(format, args...)) }}
	gateway := gate.New(st, cfg.Claude, func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) })
	verifier := &verify.Runner{Cfg: cfg.Verify, Timeout: cfg.Run.VerifyTimeout.D()}

	orch := orchestrator.New(orchestrator.Options{
		Config: cfg, Store: st, Registry: registry,
		GH: ghClient, Git: gitMgr, Runner: runner, Gate: gateway, Verify: verifier,
		Logger: log, DryRun: f.dryRun,
	})

	// SIGINT/SIGTERM cancels the root context; the loop then drains in-flight
	// runs rather than abandoning a half-finished worktree.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if f.dryRun {
		log.Warn("dry-run: no branch will be pushed, no PR opened, no issue edited")
	}

	if f.once {
		log.Info("single discovery pass")
		return orch.RunOnce(ctx)
	}

	errCh := make(chan error, 2)
	if !f.noServer {
		srv := server.New(cfg.Server.Addr, st, gateway, orch, log)
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
	return err
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

	if len(cfg.GitHub.Owners) == 0 {
		log.Warn("github.owners is empty: discovery will scan every repository this token can see",
			"label", cfg.GitHub.Label)
	}

	if len(problems) > 0 {
		return fmt.Errorf("start-up checks failed:\n  - %s", strings.Join(problems, "\n  - "))
	}

	ladder := registry.Ladder(models.RoleImplement, nil)
	refs := make([]string, 0, len(ladder))
	for _, m := range ladder {
		refs = append(refs, m.ID)
	}
	log.Info("start-up checks passed",
		"label", cfg.GitHub.Label,
		"owners", strings.Join(cfg.GitHub.Owners, ","),
		"implement_ladder", strings.Join(refs, " > "),
		"store", cfg.Store.Path)
	return nil
}
