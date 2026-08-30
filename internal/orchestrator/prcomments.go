package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ableinc/coding-agent-loop/internal/claude"
	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/gate"
	"github.com/ableinc/coding-agent-loop/internal/gh"
	"github.com/ableinc/coding-agent-loop/internal/models"
	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/verify"
)

// mentionsAgent reports whether body addresses the agent by handle. The match
// is case-insensitive and must sit on a word boundary on its trailing edge, so
// "@coding-agent-loop" is not read as a mention of "@coding-agent". Quoted
// lines (leading ">") and fenced code blocks are ignored, so a comment that
// merely quotes a previous mention, or shows one in an example, cannot
// re-trigger the agent.
func mentionsAgent(body, handle string) bool {
	handle = strings.ToLower(strings.TrimSpace(handle))
	if handle == "" {
		return false
	}
	inFence := false
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence || strings.HasPrefix(trimmed, ">") {
			continue
		}
		if lineMentions(strings.ToLower(line), handle) {
			return true
		}
	}
	return false
}

func lineMentions(lowerLine, lowerHandle string) bool {
	from := 0
	for {
		i := strings.Index(lowerLine[from:], lowerHandle)
		if i < 0 {
			return false
		}
		pos := from + i
		end := pos + len(lowerHandle)
		if end >= len(lowerLine) || !isMentionChar(lowerLine[end]) {
			return true
		}
		from = pos + 1
	}
}

func isMentionChar(b byte) bool {
	return b == '-' || b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// authorAllowed reports whether a comment's author may trigger the agent. An
// explicit login allowlist takes precedence over the association list; when
// neither matches, an arbitrary commenter on a public repo could otherwise
// drive a bypassPermissions Claude run.
func authorAllowed(author, association string, cfg config.PRCommentsConfig) bool {
	if len(cfg.AllowedAuthors) > 0 {
		for _, a := range cfg.AllowedAuthors {
			if strings.EqualFold(strings.TrimSpace(a), author) {
				return true
			}
		}
		return false
	}
	for _, a := range cfg.AllowedAssociations {
		if strings.EqualFold(strings.TrimSpace(a), association) {
			return true
		}
	}
	return false
}

// prCommentTaskKey identifies one triggering comment in the tasks map.
func prCommentTaskKey(kind string, id int64) string {
	return fmt.Sprintf("%s/%d", kind, id)
}

// pendingMentions filters a PR's comments to the ones this run should act on:
// mention present, author permitted, not older than maxAge, not authored by
// the daemon itself, not one of the daemon's own marker comments, and either
// unseen, previously acked (a crash between the ack and the run, since a live
// run holds the repo's claim and is never re-entered), or failed with its
// per-comment back-off elapsed.
func pendingMentions(comments []gh.PRComment, cfg config.PRCommentsConfig, botLogin string,
	tasks []store.PRCommentTask, now time.Time, retryBase, retryMax time.Duration) []gh.PRComment {

	byKey := make(map[string]store.PRCommentTask, len(tasks))
	for _, t := range tasks {
		byKey[prCommentTaskKey(t.CommentKind, t.CommentID)] = t
	}

	maxAge := cfg.MaxAge.D()
	var out []gh.PRComment
	for _, c := range comments {
		if isAgentComment(c.Body) {
			continue
		}
		if botLogin != "" && strings.EqualFold(c.Author, botLogin) {
			continue
		}
		if !mentionsAgent(c.Body, cfg.Mention) {
			continue
		}
		if !authorAllowed(c.Author, c.Association, cfg) {
			continue
		}
		if maxAge > 0 && !c.CreatedAt.IsZero() && now.Sub(c.CreatedAt) > maxAge {
			continue
		}
		if task, ok := byKey[prCommentTaskKey(c.Kind, c.ID)]; ok {
			if task.Status == store.PRCommentDone {
				continue
			}
			if task.Status == store.PRCommentFailed {
				next := task.LastAttemptAt.Add(retryDelay(task.Attempts, retryBase, retryMax))
				if now.Before(next) {
					continue
				}
			}
			// PRCommentAcked: either genuinely in flight (the repo's claim
			// already excludes this PR from being picked up again) or
			// stranded by a crash between the ack and the run, in which case
			// it must be retried rather than left behind forever.
		}
		out = append(out, c)
	}
	return out
}

// tickPRComments is one discovery pass over the daemon's own open pull
// requests, looking for @-mentions to act on. It runs before issue discovery
// in each tick and shares the same capacity budget: a reviewer waiting on a
// reply outranks starting a new issue.
func (o *Orchestrator) tickPRComments(ctx context.Context, capacity int) int {
	cfg := o.opts.Config.GitHub.PRComments
	if !cfg.Enabled || capacity <= 0 || ctx.Err() != nil {
		return capacity
	}

	login, err := o.currentLogin(ctx)
	if err != nil {
		o.log.Error("pr comments: could not resolve the daemon's own github login", "error", err)
		return capacity
	}

	results, err := o.opts.GH.SearchPRs(ctx, login, o.opts.Config.GitHub.Owners, cfg.SearchLimit)
	if err != nil {
		o.log.Error("pr discovery failed", "error", err)
		return capacity
	}
	o.log.Debug("pr comment discovery pass", "candidates", len(results), "capacity", capacity)

	for _, r := range results {
		if ctx.Err() != nil || capacity <= 0 {
			return capacity
		}
		repo := r.Repository.NameWithOwner
		if repo == "" || r.Number == 0 {
			continue
		}
		if !o.opts.Config.GitHub.Owned(repo) {
			o.log.Error("pr discovery returned a repo outside github.owners; refusing to touch it",
				"repo", repo, "pr", r.Number, "owners", strings.Join(o.opts.Config.GitHub.Owners, ","))
			continue
		}
		if o.opts.Config.GitHub.Excluded(repo) {
			continue
		}

		o.mu.Lock()
		busy := o.activeRepos[repo]
		o.mu.Unlock()
		if busy {
			continue
		}
		if busy, err := o.opts.Store.RepoBusy(ctx, repo); err != nil {
			o.log.Error("repo busy check failed", "repo", repo, "error", err)
			continue
		} else if busy {
			continue
		}

		pr, err := o.opts.GH.ViewPR(ctx, repo, r.Number)
		if err != nil {
			o.log.Error("pr view failed", "repo", repo, "pr", r.Number, "error", err)
			continue
		}
		if !strings.EqualFold(pr.State, "OPEN") {
			continue
		}
		// Hard safety rule, not a config knob: only ever push to a branch this
		// daemon created itself.
		if !strings.HasPrefix(pr.HeadRefName, o.opts.Config.Workspace.BranchPrefix) {
			continue
		}

		comments, err := o.opts.GH.PRComments(ctx, repo, r.Number)
		if err != nil {
			o.log.Error("pr comments fetch failed", "repo", repo, "pr", r.Number, "error", err)
			continue
		}
		tasks, err := o.opts.Store.PRCommentTasks(ctx, repo, r.Number)
		if err != nil {
			o.log.Error("pr comment task lookup failed", "repo", repo, "pr", r.Number, "error", err)
			continue
		}
		pending := pendingMentions(comments, cfg, login, tasks, time.Now(),
			o.opts.Config.Run.RetryBackoff.D(), o.opts.Config.Run.RetryBackoffMax.D())
		if len(pending) == 0 {
			continue
		}

		if !o.reserveRepo(repo) {
			continue
		}
		capacity--

		cand := candidate{repo: repo, number: r.Number, title: pr.Title, url: pr.URL}
		o.wg.Go(func() {
			defer o.releaseRepo(cand.repo)
			o.workPRComments(ctx, cand, pr, pending)
		})
	}
	return capacity
}

// currentLogin returns and caches the daemon's own GitHub login, used both to
// scope SearchPRs and to make sure the daemon never reacts to its own
// comments.
func (o *Orchestrator) currentLogin(ctx context.Context) (string, error) {
	o.mu.Lock()
	login := o.botLogin
	o.mu.Unlock()
	if login != "" {
		return login, nil
	}
	login, err := o.opts.GH.CurrentLogin(ctx)
	if err != nil {
		return "", err
	}
	o.mu.Lock()
	o.botLogin = login
	o.mu.Unlock()
	return login, nil
}

// workPRComments is the full lifecycle for one batch of triggering comments on
// one pull request.
func (o *Orchestrator) workPRComments(ctx context.Context, cand candidate, pr gh.PullRequest, pending []gh.PRComment) {
	cfg := o.opts.Config
	pcCfg := cfg.GitHub.PRComments
	runID := uuid.NewString()
	log := o.log.With("run", runID, "repo", cand.repo, "pr", cand.number)

	claimed, err := o.opts.Store.TryClaim(ctx, cand.repo, cand.number, runID, o.opts.WorkerID, cfg.Run.Lease.D())
	if err != nil {
		log.Error("claim failed", "error", err)
		return
	}
	if !claimed {
		log.Debug("pull request claimed by another worker")
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

	stopRenew := o.renewLease(runCtx, cand, runID)
	defer stopRenew()

	logPath := filepath.Join(cfg.Workspace.LogsRoot, runID+".jsonl")

	run := store.Run{
		ID: runID, Repo: cand.repo, Issue: cand.number, Attempt: 1,
		Branch: pr.HeadRefName, Status: store.StatusClaimed, Kind: store.RunKindPRComment,
		StartedAt: time.Now(), LogPath: logPath,
	}
	if err := o.opts.Store.CreateRun(ctx, run); err != nil {
		log.Error("create run failed", "error", err)
		return
	}
	ref := cand.ref(runID, 1)
	o.event(ctx, runID, "claimed", fmt.Sprintf("%d review comment(s) as worker %s", len(pending), o.opts.WorkerID))
	o.opts.Discord.RunClaimed(ref, 0)

	// Ack every comment immediately, before any cloning: the 👀 is the
	// user-visible promise that the comment was seen, and it must not wait on
	// a slow clone or worktree setup.
	for _, c := range pending {
		if err := o.opts.GH.React(runCtx, cand.repo, c, pcCfg.AckReaction); err != nil {
			log.Warn("could not ack comment", "comment", c.ID, "error", err)
		}
		if err := o.opts.Store.MarkPRCommentAcked(runCtx, cand.repo, cand.number, c.Kind, c.ID, runID); err != nil {
			log.Warn("could not record ack", "comment", c.ID, "error", err)
		}
	}

	if err := o.executePRComments(runCtx, log, cand, pr, pending, runID, logPath); err != nil {
		o.handlePRCommentFailure(ctx, log, cand, pending, runID, err)
		return
	}
}

// executePRComments is the happy path for one PR-comment run.
func (o *Orchestrator) executePRComments(ctx context.Context, log *slog.Logger, cand candidate, pr gh.PullRequest,
	pending []gh.PRComment, runID, logPath string) error {
	cfg := o.opts.Config
	ref := cand.ref(runID, 1)
	started := time.Now()

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
	// Both branch and base are the PR's own head branch: this checks out the
	// PR head as-is rather than resetting it against the default branch, and
	// a retry after a human pushed more commits to it picks those up too.
	if err := o.opts.Git.AddWorktree(ctx, repoPath, worktree, pr.HeadRefName, pr.HeadRefName); err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}
	o.event(ctx, runID, "worktree", worktree)

	cooled, err := o.opts.Store.CooledDownModels(ctx)
	if err != nil {
		log.Warn("cooldown lookup failed, using full ladder", "error", err)
		cooled = nil
	}
	ladder := o.opts.Registry.Ladder(models.RoleImplement, cooled)
	if len(ladder) > 0 {
		if drop := maxAttempts(ctx, o.opts.Store, cand.repo, cand.number, pending) % len(ladder); drop > 0 {
			ladder = ladder[drop:]
		}
	}
	head, fallbacks, err := models.Head(ladder)
	if err != nil {
		return fmt.Errorf("select model: %w", err)
	}
	if err := o.opts.Store.RecordUsage(ctx, runID, store.RunUsage{ModelID: head.ID}); err != nil {
		log.Warn("could not pre-record model", "error", err)
	}
	if err := o.opts.Store.SetRunStatus(ctx, runID, store.StatusWorking); err != nil {
		log.Warn("status update failed", "error", err)
	}
	o.event(ctx, runID, "model", fmt.Sprintf("%s (fallbacks: %s)", head.ID, orNone(fallbacks)))
	log.Info("starting claude", "model", head.ID, "branch", pr.HeadRefName)

	reviews, err := o.opts.GH.PRReviewBodies(ctx, cand.repo, cand.number)
	if err != nil {
		log.Warn("could not fetch review summaries, continuing without them", "error", err)
	}

	prompt := prCommentTaskPrompt(cand.repo, pr, pending, reviews)
	sysPrompt := prCommentSystemPrompt(cand.repo, pr.HeadRefName, worktree)

	var sessionOnce sync.Once
	result, runErr := o.opts.Runner.Run(ctx, claude.Options{
		Binary:         cfg.Claude.Binary,
		Prompt:         prompt,
		SystemPrompt:   sysPrompt,
		Model:          head.Ref(),
		Fallbacks:      fallbacks,
		Effort:         head.EffortFor(models.RoleImplement),
		PermissionMode: cfg.Claude.PermissionMode,
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

	usedModel := head.ID
	if result != nil {
		if m := result.PrimaryModel(); m != "" {
			usedModel = m
		}
		if err := o.opts.Store.RecordUsage(ctx, runID, store.RunUsage{
			ModelID:    usedModel,
			SessionID:  result.SessionID,
			CostUSD:    result.TotalCostUSD,
			TokensIn:   result.TokensIn(),
			TokensOut:  result.TokensOut(),
			CacheRead:  result.CacheReadTokens(),
			CacheWrite: result.CacheWriteTokens(),
			Turns:      result.NumTurns,
		}); err != nil {
			log.Warn("usage record failed", "error", err)
		}
		o.recordSession(ctx, log, cand, runID, result.SessionID, usedModel)
	}

	if runErr != nil {
		if hit, expired := gate.DetectAuthExpired(result, runErr); expired {
			until, gerr := o.opts.Gate.RecordAuthExpired(ctx, hit)
			if gerr != nil {
				log.Error("could not record auth expiry", "error", gerr)
			}
			o.event(ctx, runID, "auth_expired", hit.Reason)
			o.opts.Discord.GateClosed(hit.Reason, until)
			return errRetryable{fmt.Errorf("oauth token expired: %s", hit.Reason)}
		}
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
	o.event(ctx, runID, "claude_done", fmt.Sprintf("turns=%d cost=$%.4f fresh_in=%d cached_in=%d out=%d",
		result.NumTurns, result.TotalCostUSD, result.FreshTokensIn(), result.CacheReadTokens(), result.TokensOut()))

	hasWork, err := o.opts.Git.HasWork(ctx, worktree, pr.HeadRefName)
	if err != nil {
		return fmt.Errorf("inspect worktree: %w", err)
	}

	pushed := false
	var vres verify.Result
	if hasWork {
		commitMsg := fmt.Sprintf("Address review feedback on #%d\n\nGenerated by coding-agent-loop run %s.",
			cand.number, runID)
		if committed, err := o.opts.Git.CommitAll(ctx, worktree, commitMsg); err != nil {
			return fmt.Errorf("commit changes: %w", err)
		} else if committed {
			o.event(ctx, runID, "committed", "harness committed the agent's working tree")
		}

		if err := o.opts.Store.SetRunStatus(ctx, runID, store.StatusVerifying); err != nil {
			log.Warn("status update failed", "error", err)
		}
		vres = o.opts.Verify.Run(ctx, cand.repo, worktree)
		if err := o.opts.Store.SetVerifyStatus(ctx, runID, vres.Status); err != nil {
			log.Warn("verify status update failed", "error", err)
		}
		o.event(ctx, runID, "verify", fmt.Sprintf("%s (%s)", vres.Status, orNone(vres.Command)))
		log.Info("verification finished", "status", vres.Status, "command", vres.Command)

		if err := o.opts.Git.Push(ctx, worktree, pr.HeadRefName, cand.repo); err != nil {
			return fmt.Errorf("push branch: %w", err)
		}
		if err := o.opts.Store.SetRunStatus(ctx, runID, store.StatusPushed); err != nil {
			log.Warn("status update failed", "error", err)
		}
		o.event(ctx, runID, "pushed", pr.HeadRefName)
		pushed = true
	} else {
		// No code changes is a legitimate outcome here: a question answered in
		// prose is not a failure.
		o.event(ctx, runID, "no_work", "the agent answered in its summary without changing the worktree")
	}

	if err := o.opts.GH.CommentOnPR(ctx, cand.repo, cand.number,
		prCommentComment(pending, result.Result, runID, vres, pushed)); err != nil {
		log.Warn("could not comment on pull request", "error", err)
	}
	for _, c := range pending {
		if err := o.opts.GH.React(ctx, cand.repo, c, o.opts.Config.GitHub.PRComments.DoneReaction); err != nil {
			log.Warn("could not react to comment as done", "comment", c.ID, "error", err)
		}
		if err := o.opts.Store.MarkPRCommentDone(ctx, c.Kind, c.ID, runID); err != nil {
			log.Warn("could not record comment as done", "comment", c.ID, "error", err)
		}
	}

	if err := o.opts.Store.SetRunStatus(ctx, runID, store.StatusAddressed); err != nil {
		log.Warn("status update failed", "error", err)
	}
	o.event(ctx, runID, "addressed", fmt.Sprintf("%d comment(s)", len(pending)))
	o.opts.Discord.PRCommentsAddressed(ref, len(pending), result, vres, time.Since(started))
	log.Info("pr review feedback addressed", "comments", len(pending), "pushed", pushed, "cost_usd", result.TotalCostUSD)

	o.cleanup(ctx, log, repoPath, worktree, true)
	return nil
}

// maxAttempts is the highest attempt count among the tasks backing pending,
// used to demote the model ladder the same way a repeatedly-failing issue
// does.
func maxAttempts(ctx context.Context, st *store.Store, repo string, pr int, pending []gh.PRComment) int {
	tasks, err := st.PRCommentTasks(ctx, repo, pr)
	if err != nil {
		return 0
	}
	byKey := make(map[string]store.PRCommentTask, len(tasks))
	for _, t := range tasks {
		byKey[prCommentTaskKey(t.CommentKind, t.CommentID)] = t
	}
	max := 0
	for _, c := range pending {
		if t, ok := byKey[prCommentTaskKey(c.Kind, c.ID)]; ok && t.Attempts > max {
			max = t.Attempts
		}
	}
	return max
}

// handlePRCommentFailure records a failed PR-comment run. The ack reaction
// stays: the comment was seen, and the per-comment back-off decides when it
// is retried.
func (o *Orchestrator) handlePRCommentFailure(ctx context.Context, log *slog.Logger, cand candidate,
	pending []gh.PRComment, runID string, cause error) {
	ctx = context.WithoutCancel(ctx)
	cfg := o.opts.Config.Run

	var retryable errRetryable
	if errors.As(cause, &retryable) {
		log.Warn("pr comment run deferred by usage limit", "error", cause)
		if err := o.opts.Store.FailRun(ctx, runID, store.StatusDeferred, cause.Error()); err != nil {
			log.Error("could not record deferral", "error", err)
		}
		o.event(ctx, runID, "deferred", cause.Error())
		o.opts.Discord.RunDeferred(cand.ref(runID, 1), cause.Error())
		return
	}

	log.Error("pr comment run failed", "error", cause)
	if err := o.opts.Store.FailRun(ctx, runID, store.StatusFailed, cause.Error()); err != nil {
		log.Error("could not record failure", "error", err)
	}
	o.event(ctx, runID, "failed", cause.Error())

	var nextAttempt time.Time
	for _, c := range pending {
		if err := o.opts.Store.MarkPRCommentFailed(ctx, c.Kind, c.ID, runID); err != nil {
			log.Warn("could not record comment failure", "comment", c.ID, "error", err)
			continue
		}
		tasks, err := o.opts.Store.PRCommentTasks(ctx, cand.repo, cand.number)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if t.CommentKind != c.Kind || t.CommentID != c.ID {
				continue
			}
			next := t.LastAttemptAt.Add(retryDelay(t.Attempts, cfg.RetryBackoff.D(), cfg.RetryBackoffMax.D()))
			if nextAttempt.IsZero() || next.Before(nextAttempt) {
				nextAttempt = next
			}
		}
	}

	if err := o.opts.GH.CommentOnPR(ctx, cand.repo, cand.number,
		prCommentFailureComment(runID, cause.Error(), nextAttempt)); err != nil {
		log.Warn("could not comment on pull request", "error", err)
	}
	o.opts.Discord.RunFailed(cand.ref(runID, 1), cause.Error(), nextAttempt)

	o.finishCleanup(ctx, log, cand)
}
