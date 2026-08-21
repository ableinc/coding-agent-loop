package orchestrator

import (
	"fmt"
	"strings"

	"github.com/ableinc/coding-agent-loop/internal/gh"
)

// maxCommentChars bounds how much issue discussion goes into the prompt.
const (
	maxBodyChars     = 12000
	maxCommentChars  = 2000
	maxCommentsInclu = 12
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

The harness, not you, owns version control and GitHub. Specifically:
- Do NOT run git push, git rebase, git reset --hard, or any force operation.
- Do NOT create branches, tags, pull requests, or issue comments.
- Do NOT amend or rewrite any commit that already exists.
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
- If the issue is genuinely ambiguous, choose the most reasonable reading, implement it,
  and state the assumption in your final message.
- If you conclude the change should not be made, make no edits and explain why.

Finish with a short summary: what you changed, which files, and anything a reviewer
should look at closely.`, repo, branch, worktree)
}

// taskPrompt renders the issue into the actual instruction.
func taskPrompt(repo string, issue gh.Issue) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Implement the change requested by GitHub issue #%d in %s.\n\n", issue.Number, repo)
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

	if len(issue.Comments) > 0 {
		b.WriteString("\n### Discussion\n\n")
		comments := issue.Comments
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n\n...(truncated)"
}
