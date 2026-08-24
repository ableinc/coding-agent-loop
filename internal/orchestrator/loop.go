// Package orchestrator is the loop itself: discover labelled issues, claim one
// per repository, drive Claude over it, and open a draft pull request.
//
// Concurrency model: several repositories are worked in parallel, but never
// two issues in the same repository at once. That is enforced twice — in
// memory for this process, and through the store's claim table so it also
// holds across a restart or a second daemon.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ableinc/coding-agent-loop/internal/claude"
	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/discord"
	"github.com/ableinc/coding-agent-loop/internal/gate"
	"github.com/ableinc/coding-agent-loop/internal/gh"
	gitpkg "github.com/ableinc/coding-agent-loop/internal/git"
	"github.com/ableinc/coding-agent-loop/internal/models"
	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/verify"
)

// modelCooldown is how long a model is sidelined after it fails a run.
const modelCooldown = 30 * time.Minute

// Options are the orchestrator's dependencies.
type Options struct {
	Config   config.Config
	Store    *store.Store
	Registry *models.Registry
	GH       *gh.Client
	Git      *gitpkg.Manager
	Runner   *claude.Runner
	Gate     *gate.Gate
	Verify   *verify.Runner
	Logger   *slog.Logger
	DryRun   bool
	WorkerID string
	Discord  *discord.Notifier
}

// Orchestrator runs the loop.
type Orchestrator struct {
	opts Options
	log  *slog.Logger

	wg sync.WaitGroup

	mu          sync.Mutex
	activeRepos map[string]bool
	cancels     map[string]context.CancelFunc
	repoInfo    map[string]repoMeta
}

type repoMeta struct {
	defaultBranch string
	cloneURL      string
}

// New builds an Orchestrator.
func New(opts Options) *Orchestrator {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.WorkerID == "" {
		opts.WorkerID = uuid.NewString()[:8]
	}
	return &Orchestrator{
		opts:        opts,
		log:         opts.Logger,
		activeRepos: map[string]bool{},
		cancels:     map[string]context.CancelFunc{},
		repoInfo:    map[string]repoMeta{},
	}
}

// Run polls until ctx is cancelled, then waits for in-flight work to finish.
func (o *Orchestrator) Run(ctx context.Context) error {
	interval := o.opts.Config.GitHub.PollInterval.D()
	o.log.Info("orchestrator started",
		"worker", o.opts.WorkerID,
		"poll_interval", interval.String(),
		"max_concurrent_repos", o.opts.Config.Run.MaxConcurrentRepos,
		"retry_backoff", o.opts.Config.Run.RetryBackoff.D().String(),
		"retry_backoff_max", o.opts.Config.Run.RetryBackoffMax.D().String(),
		"dry_run", o.opts.DryRun)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	o.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			o.log.Info("orchestrator draining in-flight runs")
			o.wg.Wait()
			o.log.Info("orchestrator stopped")
			return nil
		case <-ticker.C:
			o.tick(ctx)
		}
	}
}

// RunOnce does a single discovery pass and waits for whatever it started.
func (o *Orchestrator) RunOnce(ctx context.Context) error {
	o.tick(ctx)
	o.wg.Wait()
	return nil
}

// Cancel stops an in-flight run. It reports whether the run was found.
func (o *Orchestrator) Cancel(runID string) bool {
	o.mu.Lock()
	cancel, ok := o.cancels[runID]
	o.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// ActiveRepos lists repositories currently being worked by this process.
func (o *Orchestrator) ActiveRepos() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, 0, len(o.activeRepos))
	for r := range o.activeRepos {
		out = append(out, r)
	}
	return out
}

// tick is one discovery pass.
func (o *Orchestrator) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	// Advisory only: this refreshes /status and never gates the loop.
	o.opts.Gate.PollUsage(ctx)

	status, err := o.opts.Gate.Check(ctx)
	if err != nil {
		o.log.Error("gate check failed", "error", err)
		return
	}
	if !status.Allowed {
		for _, g := range status.Gates {
			o.log.Info("not claiming work", "gate", g.Kind, "until", g.BlockedUntil.Format(time.RFC3339), "reason", g.Reason)
		}
		return
	}

	capacity := o.capacity()
	if capacity <= 0 {
		return
	}

	results, err := o.opts.GH.SearchIssues(ctx, o.opts.Config.GitHub.Label, o.opts.Config.GitHub.Owners, o.opts.Config.GitHub.SearchLimit)
	if err != nil {
		o.log.Error("issue discovery failed", "error", err)
		return
	}
	o.log.Debug("discovery pass", "candidates", len(results), "capacity", capacity)

	for _, r := range results {
		if ctx.Err() != nil || capacity <= 0 {
			return
		}
		repo := r.Repository.NameWithOwner
		if repo == "" || r.Number == 0 {
			continue
		}
		// Defense in depth: gh is asked to scope search to Owners, but a search
		// result naming any other repo must never be touched, regardless of why
		// it slipped through (a gh bug, a flag regression, an API change).
		if !o.opts.Config.GitHub.Owned(repo) {
			o.log.Error("discovery returned a repo outside github.owners; refusing to touch it",
				"repo", repo, "issue", r.Number, "owners", strings.Join(o.opts.Config.GitHub.Owners, ","))
			continue
		}
		ok, reason := o.eligible(ctx, repo, r.Number)
		if !ok {
			if reason != "" {
				o.log.Debug("skipping issue", "repo", repo, "issue", r.Number, "reason", reason)
			}
			continue
		}
		if !o.reserveRepo(repo) {
			continue
		}
		capacity--

		cand := candidate{repo: repo, number: r.Number, title: r.Title, url: r.URL}
		o.wg.Add(1)
		go func() {
			defer o.wg.Done()
			defer o.releaseRepo(cand.repo)
			o.work(ctx, cand)
		}()
	}
}

type candidate struct {
	repo   string
	number int
	title  string
	url    string
}

// ref describes a run to the notifier.
func (c candidate) ref(runID string, attempt int) discord.RunRef {
	return discord.RunRef{
		Repo: c.repo, Issue: c.number, Title: c.title, URL: c.url,
		RunID: runID, Attempt: attempt,
	}
}

func (o *Orchestrator) capacity() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opts.Config.Run.MaxConcurrentRepos - len(o.activeRepos)
}

func (o *Orchestrator) reserveRepo(repo string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.activeRepos[repo] {
		return false
	}
	if len(o.activeRepos) >= o.opts.Config.Run.MaxConcurrentRepos {
		return false
	}
	o.activeRepos[repo] = true
	return true
}

func (o *Orchestrator) releaseRepo(repo string) {
	o.mu.Lock()
	delete(o.activeRepos, repo)
	o.mu.Unlock()
}

// eligible applies the cheap filters before anything is claimed, then decides
// the phase from the issue's current comments. An issue waiting on a human
// (phaseWait) is reported as not eligible, so it costs a fetch but never a
// claim, a worktree, or a Claude run. execute decides the phase again for
// itself from a fresh fetch before acting, so the phase decided here is not
// carried any further.
func (o *Orchestrator) eligible(ctx context.Context, repo string, issue int) (bool, string) {
	if !o.opts.Config.GitHub.Owned(repo) {
		return false, "repo is not owned by github.owners"
	}
	if o.opts.Config.GitHub.Excluded(repo) {
		return false, "repo excluded by config"
	}

	o.mu.Lock()
	busy := o.activeRepos[repo]
	o.mu.Unlock()
	if busy {
		return false, ""
	}

	// Cross-process/cross-restart version of the same check.
	if busy, err := o.opts.Store.RepoBusy(ctx, repo); err != nil {
		o.log.Error("repo busy check failed", "repo", repo, "error", err)
		return false, "busy check failed"
	} else if busy {
		return false, ""
	}

	hist, err := o.opts.Store.IssueHistory(ctx, repo, issue)
	if err != nil {
		o.log.Error("issue history lookup failed", "repo", repo, "issue", issue, "error", err)
		return false, "history lookup failed"
	}
	if hist.Succeeded {
		o.reconcileDelivered(ctx, repo, issue)
		return false, "already delivered a PR"
	}
	if next := o.nextAttemptAt(hist); time.Now().Before(next) {
		return false, fmt.Sprintf("backing off after %d failed attempt(s), retrying at %s",
			hist.Failures, next.Format(time.RFC3339))
	}

	view, err := o.opts.GH.ViewIssue(ctx, repo, issue)
	if err != nil {
		o.log.Error("issue view failed", "repo", repo, "issue", issue, "error", err)
		return false, "issue fetch failed"
	}
	if phase, reason := decidePhase(view); phase == phaseWait || phase == phaseDone {
		if phase == phaseDone {
			o.reconcileDelivered(ctx, repo, issue)
		}
		return false, reason
	}
	return true, ""
}

// reconcileDelivered takes the trigger label off an issue whose work is already
// done. Without it a delivered issue whose label edit failed — or one a human
// re-labelled hoping for another attempt — is rediscovered on every single
// poll and rejected in silence, forever.
//
// This is self-limiting rather than repeated work: the trigger label is what
// puts an issue in the search results, so removing it is the last time the
// issue is ever seen. EditLabels reduces the edit to what actually changes, so
// an issue whose labels are already correct costs one read and no mutation.
func (o *Orchestrator) reconcileDelivered(ctx context.Context, repo string, issue int) {
	cfg := o.opts.Config
	err := o.opts.GH.EditLabels(ctx, repo, issue,
		[]string{cfg.GitHub.DoneLabel},
		[]string{cfg.GitHub.WorkingLabel, cfg.GitHub.Label, cfg.GitHub.FailedLabel, cfg.GitHub.PlanLabel})
	if err != nil {
		o.log.Warn("could not reconcile the labels of a delivered issue",
			"repo", repo, "issue", issue, "error", err)
	}
}

// nextAttemptAt is when a previously failed issue may be claimed again. The
// zero time means "now": there is nothing to wait for.
func (o *Orchestrator) nextAttemptAt(hist store.IssueState) time.Time {
	if hist.Failures == 0 || hist.LastFailureAt.IsZero() {
		return time.Time{}
	}
	cfg := o.opts.Config.Run
	return hist.LastFailureAt.Add(retryDelay(hist.Failures, cfg.RetryBackoff.D(), cfg.RetryBackoffMax.D()))
}

// retryDelay is the wait after the nth consecutive failure on one issue:
// base, 2×base, 4×base, ... capped at max.
//
// No issue is ever given up on for having failed too often — the trigger label
// is the only thing that decides whether it is worked at all — so the cap is
// what keeps a permanently broken issue down to a trickle of runs instead of
// letting it monopolise a repository's serial slot.
func retryDelay(failures int, base, max time.Duration) time.Duration {
	if failures <= 0 || base <= 0 {
		return 0
	}
	if max > 0 && max < base {
		max = base
	}
	d := base
	for i := 1; i < failures; i++ {
		// Doubling overflows int64 after ~63 failures; the cap is the answer
		// either way.
		if d > max/2 {
			return max
		}
		d *= 2
	}
	if max > 0 && d > max {
		d = max
	}
	return d
}

// work runs the full lifecycle for one issue.
func (o *Orchestrator) work(ctx context.Context, cand candidate) {
	cfg := o.opts.Config
	runID := uuid.NewString()
	log := o.log.With("run", runID, "repo", cand.repo, "issue", cand.number)

	hist, err := o.opts.Store.IssueHistory(ctx, cand.repo, cand.number)
	if err != nil {
		log.Error("issue history lookup failed", "error", err)
		return
	}
	attempt := hist.Attempts + 1

	claimed, err := o.opts.Store.TryClaim(ctx, cand.repo, cand.number, runID, o.opts.WorkerID, cfg.Run.Lease.D())
	if err != nil {
		log.Error("claim failed", "error", err)
		return
	}
	if !claimed {
		log.Debug("issue claimed by another worker")
		return
	}
	defer func() {
		if err := o.opts.Store.ReleaseClaim(context.WithoutCancel(ctx), cand.repo, cand.number, runID); err != nil {
			log.Error("release claim failed", "error", err)
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	o.mu.Lock()
	o.cancels[runID] = cancel
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.cancels, runID)
		o.mu.Unlock()
	}()

	// Keep the lease alive for as long as the run is genuinely working, so a
	// slow run never has its claim stolen out from under it.
	stopRenew := o.renewLease(runCtx, cand, runID)
	defer stopRenew()

	branch := branchName(cfg.Workspace.BranchPrefix, cand.number, cand.title)
	logPath := filepath.Join(cfg.Workspace.LogsRoot, runID+".jsonl")

	run := store.Run{
		ID: runID, Repo: cand.repo, Issue: cand.number, Attempt: attempt,
		Branch: branch, Status: store.StatusClaimed, StartedAt: time.Now(), LogPath: logPath,
	}
	if err := o.opts.Store.CreateRun(ctx, run); err != nil {
		log.Error("create run failed", "error", err)
		return
	}
	o.event(ctx, runID, "claimed", fmt.Sprintf("attempt %d as worker %s", attempt, o.opts.WorkerID))
	o.opts.Discord.RunClaimed(cand.ref(runID, attempt), hist.Failures)

	if err := o.execute(runCtx, log, cand, runID, branch, logPath, attempt); err != nil {
		o.handleFailure(ctx, log, cand, runID, attempt, err)
		return
	}
}

// renewLease extends the claim periodically until the returned func is called.
func (o *Orchestrator) renewLease(ctx context.Context, cand candidate, runID string) func() {
	lease := o.opts.Config.Run.Lease.D()
	ticker := time.NewTicker(lease / 3)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if err := o.opts.Store.RenewClaim(ctx, cand.repo, cand.number, runID, lease); err != nil {
					o.log.Error("lease renewal failed", "run", runID, "error", err)
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// execute is the happy path; every failure returns an error for handleFailure.
func (o *Orchestrator) execute(ctx context.Context, log *slog.Logger, cand candidate, runID, branch, logPath string, attempt int) error {
	cfg := o.opts.Config
	ref := cand.ref(runID, attempt)
	started := time.Now()

	issue, err := o.opts.GH.ViewIssue(ctx, cand.repo, cand.number)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}
	// Discovery and execution are minutes apart; re-check the preconditions.
	if !strings.EqualFold(issue.State, "OPEN") {
		return errSkip{fmt.Sprintf("issue is %s, not open", strings.ToLower(issue.State))}
	}
	if !issue.HasLabel(cfg.GitHub.Label) {
		return errSkip{fmt.Sprintf("issue no longer carries the %q label", cfg.GitHub.Label)}
	}

	// An existing pull request is the durable proof that this issue was already
	// worked, whatever the store happens to remember. Adopting it — rather than
	// skipping — is what stops the loop rediscovering the issue on every poll.
	existing, found, err := o.opts.GH.FindPRForIssue(ctx, cand.repo, cand.number, branch, cfg.Workspace.BranchPrefix)
	if err != nil {
		log.Warn("existing PR lookup failed, continuing", "error", err)
	} else if found {
		return o.adoptPR(ctx, log, cand, runID, issue, existing, attempt)
	}

	// Discovery's phase decision may be stale by the time execution starts;
	// decide again from the issue fetched just above.
	phase, phaseReason := decidePhase(issue)
	switch phase {
	case phaseWait:
		return errSkip{"waiting for approval: " + phaseReason}
	case phaseDone:
		// decidePhase saw a PR announcement but FindPRForIssue found no PR, so
		// the pull request has since been deleted. Re-doing the work would need
		// a human to ask for it explicitly.
		return errSkip{phaseReason}
	}

	meta, err := o.repoMetadata(ctx, cand.repo)
	if err != nil {
		return fmt.Errorf("repo metadata: %w", err)
	}

	repoPath, err := o.opts.Git.EnsureRepo(ctx, cand.repo, meta.cloneURL)
	if err != nil {
		return fmt.Errorf("prepare clone: %w", err)
	}
	if err := o.opts.Git.AssertRemote(ctx, repoPath, cand.repo); err != nil {
		return err
	}

	worktree := o.opts.Git.WorktreePath(cand.repo, cand.number)
	if err := o.opts.Git.AddWorktree(ctx, repoPath, worktree, branch, meta.defaultBranch); err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}
	o.event(ctx, runID, "worktree", worktree)

	// Pick the model. Plan and implement draw from separate ladders.
	role := models.RoleImplement
	if phase == phasePlan {
		role = models.RolePlan
	}
	cooled, err := o.opts.Store.CooledDownModels(ctx)
	if err != nil {
		log.Warn("cooldown lookup failed, using full ladder", "error", err)
		cooled = nil
	}
	ladder := o.opts.Registry.Ladder(role, cooled)
	// On a retry, start one rung lower: the previous attempt already showed the
	// model above it did not get there. Retries are unbounded, so this wraps
	// rather than pinning the issue to the weakest model forever — by the time
	// the ladder has been walked once, the top of it is worth another try.
	//
	// This keys off failures, not the attempt number: a successful plan run
	// also advances the attempt counter, and demoting the implement run that
	// follows it for that reason would be wrong.
	if len(ladder) > 0 {
		hist, err := o.opts.Store.IssueHistory(ctx, cand.repo, cand.number)
		if err != nil {
			log.Warn("issue history lookup failed, using full ladder", "error", err)
		} else if drop := hist.Failures % len(ladder); drop > 0 {
			ladder = ladder[drop:]
		}
	}
	head, fallbacks, err := models.Head(ladder)
	if err != nil {
		return fmt.Errorf("select model: %w", err)
	}
	if err := o.opts.Store.RecordUsage(ctx, runID, head.ID, "", 0, 0, 0, 0); err != nil {
		log.Warn("could not pre-record model", "error", err)
	}

	if err := o.opts.Store.SetRunStatus(ctx, runID, store.StatusWorking); err != nil {
		log.Warn("status update failed", "error", err)
	}
	// The outcome labels of an earlier attempt are stale the moment this one
	// starts, so they go in the same edit that marks the issue as working. A
	// plan run additionally clears the previous plan's label: the old plan is
	// stale the moment a re-plan starts.
	removeLabels := []string{cfg.GitHub.DoneLabel, cfg.GitHub.FailedLabel}
	if phase == phasePlan {
		removeLabels = append(removeLabels, cfg.GitHub.PlanLabel)
	}
	o.setLabels(ctx, log, cand, runID, []string{cfg.GitHub.WorkingLabel}, removeLabels)
	o.event(ctx, runID, "model", fmt.Sprintf("%s (fallbacks: %s)", head.ID, orNone(fallbacks)))
	log.Info("starting claude", "phase", phase, "model", head.ID, "branch", branch, "attempt", attempt)

	var prompt, sysPrompt, permissionMode string
	plan := o.approvedPlan(ctx, log, cand, runID, issue)
	if phase == phasePlan {
		prompt = planTaskPrompt(cand.repo, issue, plan)
		sysPrompt = planSystemPrompt(cand.repo, worktree)
		permissionMode = cfg.Claude.PlanPermissionMode
	} else {
		if strings.TrimSpace(plan) == "" {
			log.Warn("no approved plan could be found, implementing from the issue alone")
		}
		prompt = implementTaskPrompt(cand.repo, issue, plan)
		sysPrompt = systemPrompt(cand.repo, branch, worktree)
		permissionMode = cfg.Claude.PermissionMode
	}

	// The CLI announces its session ID on the first stream event, long before
	// the run produces a result. Capturing it there means a run that is killed
	// or times out still leaves a session behind to refer back to.
	var sessionOnce sync.Once
	claudeStarted := time.Now()
	result, runErr := o.opts.Runner.Run(ctx, claude.Options{
		Binary:         cfg.Claude.Binary,
		Prompt:         prompt,
		SystemPrompt:   sysPrompt,
		Model:          head.Ref(),
		Fallbacks:      fallbacks,
		PermissionMode: permissionMode,
		WorkDir:        worktree,
		ExtraArgs:      cfg.Claude.ExtraArgs,
		LogPath:        logPath,
		Timeout:        cfg.Run.Timeout.D(),
		OnEvent: func(_ string, raw json.RawMessage) {
			var probe struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil || probe.SessionID == "" {
				return
			}
			sessionOnce.Do(func() {
				o.recordSession(ctx, log, cand, runID, probe.SessionID, head.ID)
				o.event(ctx, runID, "session", probe.SessionID)
				log.Info("claude session started", "session", probe.SessionID)
			})
		},
	})

	// Record spend even on failure: the tokens were burned either way.
	usedModel := head.ID
	if result != nil {
		if m := result.PrimaryModel(); m != "" {
			usedModel = m
		}
		if err := o.opts.Store.RecordUsage(ctx, runID, usedModel, result.SessionID,
			result.TotalCostUSD, result.TokensIn(), result.TokensOut(), result.NumTurns); err != nil {
			log.Warn("usage record failed", "error", err)
		}
		// Re-record now that the model that actually served the run is known.
		o.recordSession(ctx, log, cand, runID, result.SessionID, usedModel)
	}

	if runErr != nil {
		if hit, limited := gate.DetectLimit(result, runErr); limited {
			until, gerr := o.opts.Gate.RecordLimit(ctx, hit)
			if gerr != nil {
				log.Error("could not record usage limit", "error", gerr)
			}
			if err := o.opts.Gate.CoolDownModel(ctx, head.ID, modelCooldown, hit.Reason); err != nil {
				log.Warn("could not cool down model", "error", err)
			} else {
				o.opts.Discord.ModelCooledDown(head.ID, time.Now().Add(modelCooldown), hit.Reason)
			}
			o.event(ctx, runID, "usage_limit", hit.Reason)
			o.opts.Discord.GateClosed(hit.Reason, until)
			return errRetryable{fmt.Errorf("usage limit reached, paused until %s: %s",
				until.Format(time.RFC3339), hit.Reason)}
		}
		// A model that failed for another reason still gets sidelined briefly so
		// the retry does not immediately land on it again.
		if err := o.opts.Gate.CoolDownModel(ctx, head.ID, modelCooldown, "run failed"); err != nil {
			log.Warn("could not cool down model", "error", err)
		} else {
			o.opts.Discord.ModelCooledDown(head.ID, time.Now().Add(modelCooldown), "run failed")
		}
		return fmt.Errorf("claude run failed: %w", runErr)
	}

	if err := o.opts.Gate.RecordSuccess(ctx); err != nil {
		log.Warn("could not clear usage gate", "error", err)
	}
	o.opts.Discord.GateCleared()
	o.event(ctx, runID, "claude_done", fmt.Sprintf("turns=%d cost=$%.4f", result.NumTurns, result.TotalCostUSD))

	if phase == phasePlan {
		if strings.TrimSpace(result.Result) == "" {
			return fmt.Errorf("the plan run produced no plan")
		}
		// The comment is posted before the plan is recorded, and a failure to
		// post it fails the run. decidePhase reads the plan marker off the
		// issue, so a plan that was stored but never posted leaves the issue
		// looking unplanned — and it would be re-planned, at the cost of a
		// Claude run, on every pass from then on.
		if err := o.opts.GH.Comment(ctx, cand.repo, cand.number,
			planComment(result.Result, runID, usedModel, result.TotalCostUSD)); err != nil {
			return fmt.Errorf("post plan comment: %w", err)
		}
		if err := o.opts.Store.SavePlan(ctx, cand.repo, cand.number, runID, result.Result); err != nil {
			log.Warn("could not save plan", "error", err)
		}
		o.setLabels(ctx, log, cand, runID,
			[]string{cfg.GitHub.PlanLabel}, []string{cfg.GitHub.WorkingLabel})
		if err := o.opts.Store.SetRunStatus(ctx, runID, store.StatusPlanned); err != nil {
			log.Warn("status update failed", "error", err)
		}
		o.event(ctx, runID, "planned", fmt.Sprintf("turns=%d cost=$%.4f", result.NumTurns, result.TotalCostUSD))
		o.opts.Discord.PlanPosted(ref, result, time.Since(claudeStarted))
		o.cleanup(ctx, log, repoPath, worktree, true)
		return nil
	}

	o.opts.Discord.ClaudeFinished(ref, result, time.Since(claudeStarted))

	hasWork, err := o.opts.Git.HasWork(ctx, worktree, meta.defaultBranch)
	if err != nil {
		return fmt.Errorf("inspect worktree: %w", err)
	}
	if !hasWork {
		return fmt.Errorf("the agent finished without changing anything; its summary was: %s",
			firstLine(result.Result))
	}

	commitMsg := fmt.Sprintf("%s\n\nCloses #%d\n\nGenerated by coding-agent-loop run %s.",
		commitSubject(issue.Title, cand.number), cand.number, runID)
	if committed, err := o.opts.Git.CommitAll(ctx, worktree, commitMsg); err != nil {
		return fmt.Errorf("commit changes: %w", err)
	} else if committed {
		o.event(ctx, runID, "committed", "harness committed the agent's working tree")
	}

	if err := o.opts.Store.SetRunStatus(ctx, runID, store.StatusVerifying); err != nil {
		log.Warn("status update failed", "error", err)
	}
	vres := o.opts.Verify.Run(ctx, cand.repo, worktree)
	if err := o.opts.Store.SetVerifyStatus(ctx, runID, vres.Status); err != nil {
		log.Warn("verify status update failed", "error", err)
	}
	o.event(ctx, runID, "verify", fmt.Sprintf("%s (%s)", vres.Status, orNone(vres.Command)))
	o.opts.Discord.VerifyResult(ref, vres)
	log.Info("verification finished", "status", vres.Status, "command", vres.Command)

	if err := o.opts.Git.Push(ctx, worktree, branch, cand.repo); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}
	if err := o.opts.Store.SetRunStatus(ctx, runID, store.StatusPushed); err != nil {
		log.Warn("status update failed", "error", err)
	}
	o.event(ctx, runID, "pushed", branch)

	diffstat, err := o.opts.Git.DiffStat(ctx, worktree, meta.defaultBranch)
	if err != nil {
		log.Warn("diffstat failed", "error", err)
	}

	prURL, err := o.opts.GH.CreatePR(ctx, gh.PROptions{
		Repo:  cand.repo,
		Base:  meta.defaultBranch,
		Head:  branch,
		Title: prTitle(issue.Title, cand.number),
		Draft: true,
		Body: prBody(prReport{
			Repo: cand.repo, Issue: cand.number, RunID: runID, SessionID: result.SessionID,
			ModelID: result.PrimaryModel(), CostUSD: result.TotalCostUSD,
			Summary: result.Result, DiffStat: diffstat, Verify: vres, Attempt: attempt,
		}),
	})
	if err != nil {
		return fmt.Errorf("create pull request: %w", err)
	}
	if err := o.opts.Store.SetPRURL(ctx, runID, prURL); err != nil {
		log.Warn("could not record PR url", "error", err)
	}

	if err := o.opts.GH.Comment(ctx, cand.repo, cand.number, issueComment(prURL, runID, vres)); err != nil {
		log.Warn("could not comment on issue", "error", err)
	}
	o.setLabels(ctx, log, cand, runID,
		[]string{cfg.GitHub.DoneLabel},
		[]string{cfg.GitHub.WorkingLabel, cfg.GitHub.Label, cfg.GitHub.FailedLabel, cfg.GitHub.PlanLabel})

	if err := o.opts.Store.SetRunStatus(ctx, runID, store.StatusPROpen); err != nil {
		log.Warn("status update failed", "error", err)
	}
	o.event(ctx, runID, "pr_open", prURL)
	o.opts.Discord.PROpened(ref, prURL, result, vres, diffstat, time.Since(started))
	log.Info("draft pull request opened", "url", prURL, "cost_usd", result.TotalCostUSD)

	o.cleanup(ctx, log, repoPath, worktree, true)
	return nil
}

// adoptPR reconciles an issue with a pull request that already covers it.
//
// This is the recovery path for two situations that look the same from here: a
// store that was thrown away and rebuilt, and a pull request that was opened
// but never linked back to its issue (because the run died between creating the
// PR and announcing it, or because a human opened it by hand). In both cases
// the work exists and must not be done again, and the issue must be made to say
// so — otherwise every poll rediscovers it.
//
// It returns nil: the run delivered the outcome the issue was claimed for, even
// though it did not produce the change itself. Reporting it as a skip would
// record StatusAbandoned, which does not mark the issue as succeeded, and the
// loop would pick the issue up again on the very next pass.
func (o *Orchestrator) adoptPR(ctx context.Context, log *slog.Logger, cand candidate, runID string, issue gh.Issue, pr gh.PullRequest, attempt int) error {
	cfg := o.opts.Config
	log.Info("adopting an existing pull request for this issue",
		"pr", pr.URL, "state", pr.State, "branch", pr.HeadRefName)

	// Bookkeeping first: this is what IssueHistory reads back as Succeeded, and
	// so what keeps the issue from being reclaimed even if the rest fails.
	if err := o.opts.Store.SetPRURL(ctx, runID, pr.URL); err != nil {
		return fmt.Errorf("record adopted PR url: %w", err)
	}
	if err := o.opts.Store.SetRunStatus(ctx, runID, store.StatusPROpen); err != nil {
		return fmt.Errorf("record adopted PR status: %w", err)
	}

	// Make GitHub itself associate the two, so merging the PR closes the issue.
	if linked, err := o.opts.GH.LinkPRToIssue(ctx, cand.repo, pr, cand.number); err != nil {
		log.Warn("could not link the pull request to the issue", "pr", pr.URL, "error", err)
	} else if linked {
		o.event(ctx, runID, "pr_linked", pr.URL)
	}

	// Announce it on the issue, unless the issue already says so. The marker in
	// this comment is what decidePhase reads on later passes, so posting it is
	// what makes the adoption stick without the store.
	if !issueAnnouncesPR(issue, pr.URL) {
		if err := o.opts.GH.Comment(ctx, cand.repo, cand.number, adoptedComment(pr.URL, runID, pr.State)); err != nil {
			log.Warn("could not announce the adopted pull request", "pr", pr.URL, "error", err)
		}
	}

	o.setLabels(ctx, log, cand, runID,
		[]string{cfg.GitHub.DoneLabel},
		[]string{cfg.GitHub.WorkingLabel, cfg.GitHub.Label, cfg.GitHub.FailedLabel, cfg.GitHub.PlanLabel})

	o.event(ctx, runID, "pr_adopted", pr.URL)
	o.opts.Discord.PRAdopted(cand.ref(runID, attempt), pr.URL, pr.State)
	// No cleanup: adoption happens before anything is cloned or checked out,
	// so there is no worktree, and no clone for `git worktree prune` to run in.
	return nil
}

// issueAnnouncesPR reports whether the issue already carries a harness comment
// naming this pull request, so adoption does not re-announce it every poll.
func issueAnnouncesPR(issue gh.Issue, prURL string) bool {
	for _, c := range issue.Comments {
		if isPRComment(c.Body) && strings.Contains(c.Body, prURL) {
			return true
		}
	}
	return false
}

// approvedPlan returns the plan an implement or re-plan run should work from.
//
// The store is asked first because it is cheap and exact, but the plan the
// human actually approved is the one posted on the issue, and that survives the
// store being deleted. Anything recovered is written back so the next run does
// not have to parse a comment again.
func (o *Orchestrator) approvedPlan(ctx context.Context, log *slog.Logger, cand candidate, runID string, issue gh.Issue) string {
	plan, err := o.opts.Store.LatestPlan(ctx, cand.repo, cand.number)
	if err != nil {
		log.Warn("could not read the stored plan, falling back to the issue", "error", err)
	}
	if strings.TrimSpace(plan) != "" {
		return plan
	}

	recovered := latestPlanBody(issue)
	if recovered == "" {
		return ""
	}
	log.Info("recovered the approved plan from the issue comments")
	if err := o.opts.Store.SavePlan(ctx, cand.repo, cand.number, runID, recovered); err != nil {
		log.Warn("could not save the recovered plan", "error", err)
	}
	o.event(ctx, runID, "plan_recovered", "read the approved plan back off the issue")
	return recovered
}

// handleFailure records a failed run and schedules the next attempt.
func (o *Orchestrator) handleFailure(ctx context.Context, log *slog.Logger, cand candidate, runID string, attempt int, cause error) {
	cfg := o.opts.Config
	// context.WithoutCancel: the run's context may already be cancelled, but
	// the bookkeeping still has to be written.
	ctx = context.WithoutCancel(ctx)

	var skip errSkip
	if errors.As(cause, &skip) {
		log.Info("run skipped", "reason", skip.reason)
		if err := o.opts.Store.FailRun(ctx, runID, store.StatusAbandoned, "skipped: "+skip.reason); err != nil {
			log.Error("could not record skip", "error", err)
		}
		o.opts.Discord.RunAbandoned(cand.ref(runID, attempt), "skipped: "+skip.reason,
			o.scheduleRetry(ctx, log, cand, runID))
		o.setLabels(ctx, log, cand, runID, nil, []string{cfg.GitHub.WorkingLabel})
		o.finishCleanup(ctx, log, cand)
		return
	}

	// A usage limit is nobody's fault: it is neither an attempt nor a failure,
	// so it neither drops the issue down the model ladder nor extends its
	// back-off. The gate itself was already reported via GateClosed above; this
	// only says which run it caught.
	var retryable errRetryable
	if errors.As(cause, &retryable) {
		log.Warn("run deferred by usage limit", "error", cause)
		if err := o.opts.Store.FailRun(ctx, runID, store.StatusDeferred, cause.Error()); err != nil {
			log.Error("could not record deferral", "error", err)
		}
		o.event(ctx, runID, "deferred", cause.Error())
		o.opts.Discord.RunDeferred(cand.ref(runID, attempt), cause.Error())
		o.setLabels(ctx, log, cand, runID, nil, []string{cfg.GitHub.WorkingLabel})
		o.finishCleanup(ctx, log, cand)
		return
	}

	log.Error("run failed", "error", cause, "attempt", attempt)
	if err := o.opts.Store.FailRun(ctx, runID, store.StatusFailed, cause.Error()); err != nil {
		log.Error("could not record failure", "error", err)
	}
	o.event(ctx, runID, "failed", cause.Error())

	nextAttempt := o.scheduleRetry(ctx, log, cand, runID)
	o.opts.Discord.RunFailed(cand.ref(runID, attempt), cause.Error(), nextAttempt)

	// The trigger label always stays put: it, and only it, decides whether the
	// issue is worked. agent-failed mirrors the outcome until the next attempt
	// clears it.
	o.setLabels(ctx, log, cand, runID,
		[]string{cfg.GitHub.FailedLabel}, []string{cfg.GitHub.WorkingLabel})
	if err := o.opts.GH.Comment(ctx, cand.repo, cand.number,
		failureComment(runID, attempt, cause.Error(), nextAttempt, cfg.GitHub.Label)); err != nil {
		log.Warn("could not comment on issue", "error", err)
	}

	o.finishCleanup(ctx, log, cand)
}

// scheduleRetry reports when this issue becomes claimable again, reading the
// history back after the failure was written so the count includes it. A zero
// time means the next discovery pass may take it.
func (o *Orchestrator) scheduleRetry(ctx context.Context, log *slog.Logger, cand candidate, runID string) time.Time {
	hist, err := o.opts.Store.IssueHistory(ctx, cand.repo, cand.number)
	if err != nil {
		log.Warn("could not compute retry back-off", "error", err)
		return time.Time{}
	}
	next := o.nextAttemptAt(hist)
	if next.IsZero() {
		return next
	}
	o.event(ctx, runID, "retry_scheduled", fmt.Sprintf("failure %d, next attempt at %s",
		hist.Failures, next.Format(time.RFC3339)))
	log.Info("issue backing off", "failures", hist.Failures, "next_attempt", next.Format(time.RFC3339))
	return next
}

// recordSession persists a Claude session ID against its run and issue. It is
// bookkeeping, not part of the run: a failure to write it is logged, never
// propagated.
func (o *Orchestrator) recordSession(ctx context.Context, log *slog.Logger, cand candidate, runID, sessionID, modelID string) {
	if sessionID == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if err := o.opts.Store.SetSessionID(ctx, runID, sessionID); err != nil {
		log.Warn("could not record session id on the run", "session", sessionID, "error", err)
	}
	if err := o.opts.Store.RecordSession(ctx, store.Session{
		SessionID: sessionID, RunID: runID, Repo: cand.repo, Issue: cand.number, ModelID: modelID,
	}); err != nil {
		log.Warn("could not record session", "session", sessionID, "error", err)
	}
}

// setLabels mirrors run state onto the issue. A label edit is never fatal to a
// run, but it is also never silently dropped: a failure that goes unrecorded
// leaves an issue whose labels disagree with the store, which is precisely
// what a human reading the issue would be misled by.
func (o *Orchestrator) setLabels(ctx context.Context, log *slog.Logger, cand candidate, runID string, add, remove []string) {
	ctx = context.WithoutCancel(ctx)
	if err := o.opts.GH.EditLabels(ctx, cand.repo, cand.number, add, remove); err != nil {
		log.Warn("could not update labels", "add", add, "remove", remove, "error", err)
		o.event(ctx, runID, "labels_failed", fmt.Sprintf("add %v remove %v: %v", add, remove, err))
		o.opts.Discord.LabelUpdateFailed(cand.repo, cand.number, runID, add, remove, err)
		return
	}
	o.event(ctx, runID, "labels", fmt.Sprintf("add %v remove %v", add, remove))
}

func (o *Orchestrator) finishCleanup(ctx context.Context, log *slog.Logger, cand candidate) {
	repoPath := o.opts.Git.RepoPath(cand.repo)
	worktree := o.opts.Git.WorktreePath(cand.repo, cand.number)
	o.cleanup(ctx, log, repoPath, worktree, false)
}

// cleanup removes the worktree, unless a failed one is being kept for
// inspection.
func (o *Orchestrator) cleanup(ctx context.Context, log *slog.Logger, repoPath, worktree string, success bool) {
	if !success && o.opts.Config.Workspace.KeepFailed {
		log.Info("keeping worktree for inspection", "path", worktree)
		return
	}
	if err := o.opts.Git.RemoveWorktree(context.WithoutCancel(ctx), repoPath, worktree); err != nil {
		log.Warn("worktree cleanup failed", "path", worktree, "error", err)
	}
}

// repoMetadata caches the default branch and clone URL per repository.
func (o *Orchestrator) repoMetadata(ctx context.Context, repo string) (repoMeta, error) {
	o.mu.Lock()
	meta, ok := o.repoInfo[repo]
	o.mu.Unlock()
	if ok {
		return meta, nil
	}

	branch, err := o.opts.GH.DefaultBranch(ctx, repo)
	if err != nil {
		return repoMeta{}, err
	}
	url, err := o.opts.GH.CloneURL(ctx, repo)
	if err != nil {
		return repoMeta{}, err
	}
	meta = repoMeta{defaultBranch: branch, cloneURL: url}

	o.mu.Lock()
	o.repoInfo[repo] = meta
	o.mu.Unlock()
	return meta, nil
}

func (o *Orchestrator) event(ctx context.Context, runID, kind, detail string) {
	if err := o.opts.Store.AppendEvent(context.WithoutCancel(ctx), runID, kind, detail); err != nil {
		o.log.Warn("could not append event", "run", runID, "kind", kind, "error", err)
	}
}

// --- error kinds ------------------------------------------------------------

// errSkip means the issue should not be worked and should not be retried.
type errSkip struct{ reason string }

func (e errSkip) Error() string { return e.reason }

// errRetryable means the run stopped for a reason that is not the issue's
// fault, so it must not consume one of the issue's attempts.
type errRetryable struct{ err error }

func (e errRetryable) Error() string { return e.err.Error() }
func (e errRetryable) Unwrap() error { return e.err }

// --- naming helpers ---------------------------------------------------------

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// branchName builds a stable, git-safe branch name for an issue.
func branchName(prefix string, issue int, title string) string {
	slug := nonSlug.ReplaceAllString(strings.ToLower(title), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	name := fmt.Sprintf("%s%d", prefix, issue)
	if slug != "" {
		name += "-" + slug
	}
	return name
}

func prTitle(title string, issue int) string {
	t := strings.TrimSpace(title)
	if t == "" {
		t = fmt.Sprintf("Address issue #%d", issue)
	}
	if len(t) > 120 {
		t = t[:117] + "..."
	}
	return fmt.Sprintf("%s (#%d)", t, issue)
}

func commitSubject(title string, issue int) string {
	t := strings.TrimSpace(strings.Split(title, "\n")[0])
	if t == "" {
		t = fmt.Sprintf("address issue #%d", issue)
	}
	if len(t) > 68 {
		t = t[:65] + "..."
	}
	return t
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	if s == "" {
		return "(no summary)"
	}
	return s
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}
