package orchestrator

import (
	"fmt"
	"strings"

	"github.com/ableinc/coding-agent-loop/internal/gh"
)

// maxCommentChars bounds how much issue discussion goes into the prompt.
// Kept tight because this is paid on every turn of every run: see issue #18.
const (
	maxBodyChars     = 6000
	maxCommentChars  = 1200
	maxCommentsInclu = 6

	// maxPlanChars bounds the approved/previous plan carried into a prompt.
	// truncate appends truncationSuffix, and extractPlan deliberately refuses
	// to recover a plan ending in it, so this only bites on a runaway plan —
	// comfortably above any real one (~5K tokens).
	maxPlanChars = 20000

	// maxPRCommentsInclu, maxDiffHunkChars and maxReviewsInclu bound
	// prCommentTaskPrompt, which otherwise renders every pending comment and
	// review with no cap at all.
	maxPRCommentsInclu = 10
	maxDiffHunkChars   = 800
	maxReviewsInclu    = 3
)

// systemPrompt states the rules of the harness. It is deliberately short and
// declarative: it says what the agent owns and what the harness owns, and it
// establishes that nobody is watching, so the agent neither asks questions nor
// stops early waiting for input.
//
// It intentionally does NOT tell the agent to double-check or verify its work.
// Current models already self-verify, and instructing it again reliably
// produces redundant re-checking.
func systemPrompt(repo, branch, worktree string) string {
	return fmt.Sprintf(`You are running unattended as an automated coding agent.

Working context:
- Repository: %s
- Branch: %s (already created and checked out for you)
- Worktree: %s

A human has already reviewed and approved the plan for this change (see the
"Approved plan" section of the task below). Follow it; do not re-litigate its
scope.

The harness, not you, owns version control and GitHub. Specifically:
- Do NOT run git push, git rebase, git reset --hard, or any force operation.
- Do NOT create branches, tags, pull requests, or issue comments.
- Do NOT amend or rewrite any commit that already exists.
- Do NOT change git's user.name/user.email or pass --author/--reset-author; the harness has
  already set the commit identity for this worktree.
- You MAY commit your work locally. If you do not, the harness commits it for you.

Scope rules:
- Confine all edits to the worktree above. Do not modify files elsewhere on this machine.
- Do not modify CI configuration, deployment manifests, or anything holding credentials.
- Deliver what the issue asks for, at the scope it intends. Do not opportunistically
  refactor, reformat, or "clean up" code the issue did not ask you to touch.
- Follow the conventions already present in the repository.

Autonomy:
- Nobody is watching and nobody can answer a question mid-task. Do not ask for
  confirmation and do not end your turn with a proposal or a plan you did not carry out.
- If the plan turns out to be wrong once you're in the code, deviate only where it is
  demonstrably wrong, and say so plainly in your final summary.
- If you conclude the change should not be made, make no edits and explain why.

Finish with a short summary: what you changed, which files, and anything a reviewer
should look at closely.`, repo, branch, worktree)
}

// planSystemPrompt states the rules for the read-only planning pass: produce
// a plan, do not touch the repository.
func planSystemPrompt(repo, worktree string) string {
	return fmt.Sprintf(`You are running unattended as an automated planning agent.

Working context:
- Repository: %s
- Worktree (read-only checkout for reference): %s

Your only job is to produce a plan for the change described below. Do NOT edit any
files, do NOT run git commands, and do NOT create branches, commits, or pull requests.
Read whatever parts of the repository you need in order to write a concrete plan.

Autonomy:
- Nobody is watching and nobody can answer a question mid-task. Do not ask for
  confirmation.
- If the issue is genuinely ambiguous, choose the most reasonable reading and
  say so in the plan, rather than stalling on it.

Your final message IS the plan. Write it in markdown, and make it concrete:
- Name the actual files you would change and, where relevant, the functions or
  helpers already in the repository that the change should reuse.
- Describe the approach step by step, in enough detail that a reviewer can judge it
  without reading the code themselves.
- Call out anything risky, ambiguous, or that needs a design decision from the
  reviewer before implementation starts.

Do not implement anything. Do not include a preamble like "Here is my plan" — the
final message should be the plan itself, headed by a short one-line summary of the
change.`, repo, worktree)
}

// prCommentSystemPrompt states the harness contract for addressing review
// feedback on an already-open pull request: the same rules as systemPrompt,
// reframed around a branch and PR that already exist rather than one about
// to be created.
func prCommentSystemPrompt(repo, branch, worktree string) string {
	return fmt.Sprintf(`You are running unattended as an automated coding agent.

Working context:
- Repository: %s
- Branch: %s (already created and checked out for you; an open pull request already exists from it)
- Worktree: %s

You are addressing reviewer feedback left as comments on that open pull request. Keep
the change to what the feedback below asks for; do not re-litigate the rest of the PR.

The harness, not you, owns version control and GitHub. Specifically:
- Do NOT run git push, git rebase, git reset --hard, or any force operation.
- Do NOT create branches, tags, pull requests, or comments.
- Do NOT amend or rewrite any commit that already exists.
- You MAY commit your work locally. If you do not, the harness commits it for you.

Scope rules:
- Confine all edits to the worktree above. Do not modify files elsewhere on this machine.
- Do not modify CI configuration, deployment manifests, or anything holding credentials.
- Address every comment listed below, at the scope it asks for. Do not opportunistically
  refactor, reformat, or "clean up" code the comments did not ask you to touch.
- Follow the conventions already present in the repository.

Autonomy:
- Nobody is watching and nobody can answer a question mid-task. Do not ask for
  confirmation and do not end your turn with a proposal you did not carry out.
- If a comment is a question rather than a change request, answer it in your final
  summary instead of editing code for it.
- If you conclude a requested change should not be made, make no edits and explain why
  in your final summary.

Finish with a short summary addressing each comment in turn: what you changed (or why
you didn't), and anything a reviewer should look at closely. This summary is posted
back to the pull request.`, repo, branch, worktree)
}

// prCommentTaskPrompt renders the PR and its triggering comments into the
// instruction for the review-feedback pass. reviews are review summary
// bodies, included as context only: they cannot be reacted to, so they never
// appear in comments.
func prCommentTaskPrompt(repo string, pr gh.PullRequest, comments []gh.PRComment, reviews []string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Address reviewer feedback on pull request #%d in %s.\n\n", pr.Number, repo)
	fmt.Fprintf(&b, "## Pull request #%d: %s\n\n", pr.Number, pr.Title)
	if pr.URL != "" {
		fmt.Fprintf(&b, "<%s>\n\n", pr.URL)
	}
	body := strings.TrimSpace(pr.Body)
	if body != "" {
		fmt.Fprintf(&b, "%s\n\n", truncate(body, maxBodyChars))
	}

	b.WriteString("### Comments to address\n\n")
	// Keep the most recent comments: a big review pass can leave dozens
	// pending, and the newest ones are the ones still relevant.
	if len(comments) > maxPRCommentsInclu {
		fmt.Fprintf(&b, "(showing the last %d of %d comments)\n\n", maxPRCommentsInclu, len(comments))
		comments = comments[len(comments)-maxPRCommentsInclu:]
	}
	for _, c := range comments {
		author := c.Author
		if author == "" {
			author = "unknown"
		}
		fmt.Fprintf(&b, "**@%s**: %s\n\n", author, truncate(strings.TrimSpace(c.Body), maxCommentChars))
		if c.Path != "" {
			fmt.Fprintf(&b, "On `%s`", c.Path)
			if c.Line > 0 {
				fmt.Fprintf(&b, " (line %d)", c.Line)
			}
			b.WriteString(":\n\n")
			if c.DiffHunk != "" {
				b.WriteString("```diff\n")
				b.WriteString(truncate(c.DiffHunk, maxDiffHunkChars))
				b.WriteString("\n```\n\n")
			}
		}
	}

	if len(reviews) > 0 {
		b.WriteString("### Review summaries (context only)\n\n")
		if len(reviews) > maxReviewsInclu {
			reviews = reviews[len(reviews)-maxReviewsInclu:]
		}
		for _, r := range reviews {
			fmt.Fprintf(&b, "%s\n\n", truncate(strings.TrimSpace(r), maxCommentChars))
		}
	}

	b.WriteString("\nAddress every comment listed above. If one is a question rather than a change " +
		"request, answer it in your final summary instead of editing code for it. Read the relevant " +
		"parts of the repository before editing, then make the change.\n")
	return b.String()
}

// issueContext renders the issue itself: title, labels, description, and
// recent discussion. Shared by the plan and implement prompts so the two
// phases see the same view of the issue.
func issueContext(repo string, issue gh.Issue) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Issue #%d: %s\n\n", issue.Number, issue.Title)
	if issue.URL != "" {
		fmt.Fprintf(&b, "<%s>\n\n", issue.URL)
	}

	if labels := labelNames(issue.Labels); len(labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n\n", strings.Join(labels, ", "))
	}

	body := strings.TrimSpace(issue.Body)
	if body == "" {
		body = "(The issue has no description. Work from the title and the discussion below.)"
	}
	fmt.Fprintf(&b, "### Description\n\n%s\n", truncate(body, maxBodyChars))

	// The model does not need its own prior narration read back to it: the
	// plan, the PR announcement, and every failure comment the harness itself
	// wrote carry no information the "Approved plan" section doesn't already
	// state, and on a retry they are pure noise. Bare approvals ("implement")
	// carry nothing beyond that either. See issue #18.
	var comments []gh.Comment
	for _, c := range issue.Comments {
		if isAgentComment(c.Body) || isApproval(c.Body) {
			continue
		}
		comments = append(comments, c)
	}

	if len(comments) > 0 {
		b.WriteString("\n### Discussion\n\n")
		// Keep the most recent discussion: that is where the requirements
		// usually get refined.
		if len(comments) > maxCommentsInclu {
			fmt.Fprintf(&b, "(showing the last %d of %d comments)\n\n", maxCommentsInclu, len(comments))
			comments = comments[len(comments)-maxCommentsInclu:]
		}
		for _, c := range comments {
			author := c.Author.Login
			if author == "" {
				author = "unknown"
			}
			fmt.Fprintf(&b, "**@%s**: %s\n\n", author, truncate(strings.TrimSpace(c.Body), maxCommentChars))
		}
	}

	return b.String()
}

// planTaskPrompt renders the issue into a planning instruction. When
// previousPlan is non-empty, this is a re-plan: the model is told to revise
// it against the newest feedback rather than starting from scratch.
func planTaskPrompt(repo string, issue gh.Issue, previousPlan string) string {
	var b strings.Builder

	if previousPlan == "" {
		fmt.Fprintf(&b, "Write a plan for the change requested by GitHub issue #%d in %s.\n\n", issue.Number, repo)
	} else {
		fmt.Fprintf(&b, "Revise the plan for GitHub issue #%d in %s.\n\n", issue.Number, repo)
	}
	b.WriteString(issueContext(repo, issue))

	if previousPlan != "" {
		b.WriteString("\n### Previous plan\n\n")
		b.WriteString(truncate(previousPlan, maxPlanChars))
		b.WriteString("\n\nA reviewer replied with feedback (see the newest comment in the discussion above) " +
			"instead of approving this plan. Revise the plan to address it; keep whatever still holds.\n")
	}

	b.WriteString("\nRead the relevant parts of the repository, then write the plan.\n")
	return b.String()
}

// implementTaskPrompt renders the issue and its approved plan into the
// implementation instruction.
func implementTaskPrompt(repo string, issue gh.Issue, plan string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Implement the change requested by GitHub issue #%d in %s.\n\n", issue.Number, repo)
	b.WriteString(issueContext(repo, issue))

	if plan != "" {
		b.WriteString("\n### Approved plan\n\n")
		b.WriteString("A human reviewed and approved the following plan. Follow it; deviate only where it is " +
			"demonstrably wrong, and say so in your final summary.\n\n")
		b.WriteString(truncate(plan, maxPlanChars))
		b.WriteString("\n")
	}

	b.WriteString("\nRead the relevant parts of the repository before editing, then make the change.\n")
	return b.String()
}

func labelNames(labels []gh.Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != "" {
			out = append(out, l.Name)
		}
	}
	return out
}

// truncationSuffix marks a value that was cut short. extractPlan keys off it to
// refuse to recover a plan that is missing its tail.
const truncationSuffix = "...(truncated)"

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n\n" + truncationSuffix
}
