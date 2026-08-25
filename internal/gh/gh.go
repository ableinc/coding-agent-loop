// Package gh wraps the GitHub CLI.
//
// Everything shells out to `gh` with `--json` and is decoded into typed
// structs; nothing parses human-readable output. Mutating calls honour a
// dry-run flag so the whole pipeline can be exercised against real issues
// without touching GitHub.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Client runs gh commands.
type Client struct {
	// Bin is the gh executable name or path.
	Bin string
	// DryRun suppresses every mutating call. Both --dry-run and --no-mutate
	// set it; they differ in whether any work is done at all, not in whether
	// GitHub is written to.
	DryRun bool
	// Log receives a line for each suppressed mutation. May be nil.
	Log func(format string, args ...any)
}

// New returns a Client using the given binary.
func New(bin string, dryRun bool) *Client {
	if bin == "" {
		bin = "gh"
	}
	return &Client{Bin: bin, DryRun: dryRun}
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

// CmdError carries enough context to debug a failed gh invocation.
type CmdError struct {
	Args     []string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *CmdError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("gh %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *CmdError) Unwrap() error { return e.Err }

// run executes gh and returns stdout. stdin may be nil.
func (c *Client) run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		}
		return nil, &CmdError{Args: args, ExitCode: code, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

func (c *Client) runJSON(ctx context.Context, out any, args ...string) error {
	data, err := c.run(ctx, "", args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("gh %s: decode json: %w", strings.Join(args, " "), err)
	}
	return nil
}

// --- types ------------------------------------------------------------------

// Label is a GitHub label.
type Label struct {
	Name string `json:"name"`
}

// User is a GitHub account.
type User struct {
	Login string `json:"login"`
}

// Comment is one issue comment.
type Comment struct {
	Author    User      `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// SearchResult is one hit from `gh search issues`.
type SearchResult struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Body       string `json:"body"`
	Repository struct {
		Name          string `json:"name"`
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Labels        []Label   `json:"labels"`
	Assignees     []User    `json:"assignees"`
	IsPullRequest bool      `json:"isPullRequest"`
	State         string    `json:"state"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Issue is the detail view of one issue.
type Issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	State     string    `json:"state"`
	Labels    []Label   `json:"labels"`
	Assignees []User    `json:"assignees"`
	Comments  []Comment `json:"comments"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// HasLabel reports whether the issue carries name.
func (i Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// PullRequest is a PR the harness may have opened, used to avoid opening a
// second one for an issue.
type PullRequest struct {
	Number      int        `json:"number"`
	URL         string     `json:"url"`
	Body        string     `json:"body"`
	Title       string     `json:"title"`
	HeadRefName string     `json:"headRefName"`
	State       string     `json:"state"`
	IsDraft     bool       `json:"isDraft"`
	MergedAt    *time.Time `json:"mergedAt"`
}

// Merged reports whether the PR was merged rather than merely closed.
func (p PullRequest) Merged() bool {
	return p.MergedAt != nil && !p.MergedAt.IsZero()
}

// --- read operations --------------------------------------------------------

// AuthStatus returns an error when gh is not authenticated.
func (c *Client) AuthStatus(ctx context.Context) error {
	if _, err := c.run(ctx, "", "auth", "status"); err != nil {
		return fmt.Errorf("gh is not authenticated (run `gh auth login`): %w", err)
	}
	return nil
}

// SearchIssues finds open issues carrying label, restricted to owners when
// given. Pull requests are excluded: --include-prs is deliberately not passed.
func (c *Client) SearchIssues(ctx context.Context, label string, owners []string, limit int) ([]SearchResult, error) {
	if label == "" {
		return nil, fmt.Errorf("search requires a label: an empty label would match every open issue")
	}
	if limit <= 0 {
		limit = 30
	}
	args := []string{"search", "issues",
		"--label", label,
		"--state", "open",
		"--limit", strconv.Itoa(limit),
		"--json", "number,title,url,body,repository,labels,assignees,isPullRequest,state,updatedAt",
	}
	for _, o := range owners {
		if o = strings.TrimSpace(o); o != "" {
			args = append(args, "--owner", o)
		}
	}

	var results []SearchResult
	if err := c.runJSON(ctx, &results, args...); err != nil {
		return nil, err
	}
	// Defensive: drop anything that slipped through as a PR, non-open, or (when
	// owners were given) outside the requested owners. gh is trusted to honour
	// --owner, but a result naming any other repo must never reach the caller.
	filtered := results[:0]
	for _, r := range results {
		if r.IsPullRequest {
			continue
		}
		if len(owners) > 0 && !ownedBy(r.Repository.NameWithOwner, owners) {
			c.logf("dropping search result for %s: repository owner is not in %v", r.Repository.NameWithOwner, owners)
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered, nil
}

// ownedBy reports whether repo ("owner/name") belongs to one of owners.
func ownedBy(repo string, owners []string) bool {
	owner, _, ok := strings.Cut(repo, "/")
	if !ok {
		return false
	}
	for _, o := range owners {
		if o = strings.TrimSpace(o); o != "" && strings.EqualFold(o, owner) {
			return true
		}
	}
	return false
}

// ViewIssue fetches one issue with its comments.
func (c *Client) ViewIssue(ctx context.Context, repo string, number int) (Issue, error) {
	var issue Issue
	err := c.runJSON(ctx, &issue,
		"issue", "view", strconv.Itoa(number),
		"--repo", repo,
		"--json", "number,title,body,url,state,labels,assignees,comments,updatedAt")
	if err != nil {
		return Issue{}, err
	}
	return issue, nil
}

// IssueLabels returns the labels currently on an issue.
func (c *Client) IssueLabels(ctx context.Context, repo string, number int) ([]string, error) {
	var out struct {
		Labels []Label `json:"labels"`
	}
	err := c.runJSON(ctx, &out, "issue", "view", strconv.Itoa(number), "--repo", repo, "--json", "labels")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Labels))
	for _, l := range out.Labels {
		names = append(names, l.Name)
	}
	return names, nil
}

// DefaultBranch returns the repository's default branch name.
func (c *Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var out struct {
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err := c.runJSON(ctx, &out, "repo", "view", repo, "--json", "defaultBranchRef"); err != nil {
		return "", err
	}
	if out.DefaultBranchRef.Name == "" {
		return "", fmt.Errorf("repo %s reported no default branch", repo)
	}
	return out.DefaultBranchRef.Name, nil
}

// CloneURL returns the URL to clone repo from.
func (c *Client) CloneURL(ctx context.Context, repo string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := c.runJSON(ctx, &out, "repo", "view", repo, "--json", "url"); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("repo %s reported no url", repo)
	}
	return out.URL + ".git", nil
}

// ListPRs returns the repo's pull requests in the given state ("open",
// "closed", "merged", or "all").
func (c *Client) ListPRs(ctx context.Context, repo, state string, limit int) ([]PullRequest, error) {
	if limit <= 0 {
		limit = 100
	}
	if state == "" {
		state = "open"
	}
	var prs []PullRequest
	err := c.runJSON(ctx, &prs,
		"pr", "list", "--repo", repo, "--state", state,
		"--limit", strconv.Itoa(limit),
		"--json", "number,url,body,title,headRefName,state,isDraft,mergedAt")
	if err != nil {
		return nil, err
	}
	return prs, nil
}

// FindPRForIssue looks for a pull request the harness has already opened for an
// issue. It searches every state, not just open ones: a merged or closed PR is
// still work that was delivered, and treating it as absent is what would make
// the loop implement the same issue twice.
//
// branch is the branch this run would push to and branchPrefix is the
// configured prefix; both are used because branchName derives its slug from the
// issue title, so editing the title changes the exact branch name while the
// "<prefix><number>" part stays put.
//
// The best match wins: open beats merged beats closed, so an adopting caller is
// pointed at the PR a human would care about.
func (c *Client) FindPRForIssue(ctx context.Context, repo string, issue int, branch, branchPrefix string) (PullRequest, bool, error) {
	// Bounded, so a repository with a very long PR history could in principle
	// hide an old agent PR past the end of the page. gh returns the newest
	// first, and the harness's own PRs are recent by construction.
	prs, err := c.ListPRs(ctx, repo, "all", 200)
	if err != nil {
		return PullRequest{}, false, err
	}
	var best PullRequest
	found := false
	for _, pr := range prs {
		if !prCoversIssue(pr, issue, branch, branchPrefix) {
			continue
		}
		if !found || prRank(pr) > prRank(best) {
			best, found = pr, true
		}
	}
	return best, found, nil
}

// prCoversIssue reports whether a PR is the harness's work on an issue.
func prCoversIssue(pr PullRequest, issue int, branch, branchPrefix string) bool {
	if branch != "" && strings.EqualFold(pr.HeadRefName, branch) {
		return true
	}
	if branchPrefix != "" && matchesIssueBranch(pr.HeadRefName, branchPrefix, issue) {
		return true
	}
	if PRLinksIssue(pr, issue) {
		return true
	}
	// A PR the harness opened whose closing keyword a human edited away is
	// still recognisable from its provenance footer plus the issue number the
	// harness puts in every PR title.
	//
	// The title, not the body: an agent's summary can mention any number of
	// issues in passing, and adopting on that would quietly close out an issue
	// whose work was never done.
	return strings.Contains(strings.ToLower(pr.Body), strings.ToLower(ProvenanceMarker)) &&
		referencesIssue(pr.Title, issue)
}

// ProvenanceMarker is the phrase every harness-written PR body carries, so a PR
// can be recognised as this harness's work even after a human has edited it.
// The orchestrator's prBody writes it; nothing else should.
const ProvenanceMarker = "Opened automatically by coding-agent-loop"

// matchesIssueBranch reports whether head is the branch this harness would use
// for an issue, ignoring the title slug. The boundary check matters: without it
// "agent/issue-42-fix" would be read as covering issue 4.
func matchesIssueBranch(head, prefix string, issue int) bool {
	want := fmt.Sprintf("%s%d", prefix, issue)
	if !strings.HasPrefix(strings.ToLower(head), strings.ToLower(want)) {
		return false
	}
	rest := head[len(want):]
	return rest == "" || strings.HasPrefix(rest, "-")
}

// referencesIssue reports whether s mentions "#<issue>" as a whole number, so
// "#4" does not match text that only refers to "#42".
func referencesIssue(s string, issue int) bool {
	needle := fmt.Sprintf("#%d", issue)
	for i := 0; ; {
		j := strings.Index(s[i:], needle)
		if j < 0 {
			return false
		}
		end := i + j + len(needle)
		if end == len(s) || s[end] < '0' || s[end] > '9' {
			return true
		}
		i = end
	}
}

// prRank orders matches by how much a human would care: open, then merged, then
// closed-unmerged.
func prRank(pr PullRequest) int {
	switch {
	case strings.EqualFold(pr.State, "OPEN"):
		return 3
	case pr.Merged() || strings.EqualFold(pr.State, "MERGED"):
		return 2
	default:
		return 1
	}
}

// --- mutating operations ----------------------------------------------------

// EditLabels reconciles an issue's labels towards add/remove.
//
// It is deliberately more careful than a straight `gh issue edit`: that command
// fails the *whole* call when a label does not exist on the repository or when a
// `--remove-label` is not actually on the issue, which used to take the rest of
// the edit down with it and leave an issue carrying stale state labels. So the
// current labels are read first, the edit is reduced to what genuinely changes,
// missing labels are created, and a rejected combined edit is retried label by
// label so one bad name cannot strand the others.
func (c *Client) EditLabels(ctx context.Context, repo string, number int, add, remove []string) error {
	add, remove = cleanLabels(add), cleanLabels(remove)
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	if c.DryRun {
		c.logf("not mutating: would edit labels on %s#%d (add: %s, remove: %s)",
			repo, number, labelList(add), labelList(remove))
		return nil
	}

	// When the current labels cannot be read, fall back to applying the edit
	// unfiltered: a stale label is worse than a redundant gh call.
	have, err := c.IssueLabels(ctx, repo, number)
	known := err == nil
	if err != nil {
		c.logf("could not read current labels of %s#%d, applying the edit unfiltered: %v", repo, number, err)
	}

	wantAdd := make([]string, 0, len(add))
	for _, l := range add {
		if known && containsLabel(have, l) {
			continue
		}
		wantAdd = append(wantAdd, l)
	}
	wantRemove := make([]string, 0, len(remove))
	for _, l := range remove {
		if known && !containsLabel(have, l) {
			continue
		}
		wantRemove = append(wantRemove, l)
	}
	if len(wantAdd) == 0 && len(wantRemove) == 0 {
		return nil
	}

	for _, l := range wantAdd {
		if err := c.ensureLabel(ctx, repo, l); err != nil {
			c.logf("could not create label %q in %s: %v", l, repo, err)
		}
	}

	if err := c.editLabels(ctx, repo, number, wantAdd, wantRemove); err == nil {
		return nil
	} else if len(wantAdd)+len(wantRemove) == 1 {
		return err
	}

	var errs []error
	for _, l := range wantAdd {
		if err := c.editLabels(ctx, repo, number, []string{l}, nil); err != nil {
			errs = append(errs, err)
		}
	}
	for _, l := range wantRemove {
		if err := c.editLabels(ctx, repo, number, nil, []string{l}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// editLabels is the raw `gh issue edit` call, without any reconciliation.
func (c *Client) editLabels(ctx context.Context, repo string, number int, add, remove []string) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	args := []string{"issue", "edit", strconv.Itoa(number), "--repo", repo}
	for _, l := range add {
		args = append(args, "--add-label", l)
	}
	for _, l := range remove {
		args = append(args, "--remove-label", l)
	}
	_, err := c.run(ctx, "", args...)
	return err
}

// ensureLabel creates a label on the repository, treating "it is already there"
// as success. The daemon's own state labels do not exist in most repositories
// until it puts them there.
func (c *Client) ensureLabel(ctx context.Context, repo, name string) error {
	_, err := c.run(ctx, "", "label", "create", name, "--repo", repo,
		"--description", labelDescription)
	if err == nil {
		return nil
	}
	var cmdErr *CmdError
	if errors.As(err, &cmdErr) && strings.Contains(strings.ToLower(cmdErr.Stderr), "already exists") {
		return nil
	}
	return err
}

// labelDescription is set on labels the daemon has to create itself, so their
// origin is obvious in the repository's label list.
const labelDescription = "Managed by coding-agent-loop"

// cleanLabels trims, drops empties, and removes case-insensitive duplicates.
func cleanLabels(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" || seen[strings.ToLower(l)] {
			continue
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	return out
}

func containsLabel(have []string, name string) bool {
	for _, l := range have {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

func labelList(labels []string) string {
	if len(labels) == 0 {
		return "(none)"
	}
	return strings.Join(labels, ", ")
}

// Comment posts a comment on an issue. The body goes over stdin so it is never
// subject to argument length or quoting limits.
func (c *Client) Comment(ctx context.Context, repo string, number int, body string) error {
	args := []string{"issue", "comment", strconv.Itoa(number), "--repo", repo, "--body-file", "-"}
	if c.DryRun {
		c.logf("not mutating: would comment on %s#%d:\n%s", repo, number, body)
		return nil
	}
	_, err := c.run(ctx, body, args...)
	return err
}

// LinkPRToIssue makes a PR close its issue on merge, by prepending a "Closes
// #<n>" line to its body. It is a no-op when the body already links the issue.
//
// The link matters beyond bookkeeping: without it merging the PR leaves the
// issue open, and nothing downstream — including this harness's own PR
// discovery — can tell that the work was delivered.
func (c *Client) LinkPRToIssue(ctx context.Context, repo string, pr PullRequest, issue int) (bool, error) {
	if PRLinksIssue(pr, issue) {
		return false, nil
	}
	body := fmt.Sprintf("Closes #%d\n\n%s", issue, strings.TrimSpace(pr.Body))
	if c.DryRun {
		c.logf("not mutating: would link %s#%d to issue #%d with body:\n%s", repo, pr.Number, issue, body)
		return true, nil
	}
	_, err := c.run(ctx, body,
		"pr", "edit", strconv.Itoa(pr.Number), "--repo", repo, "--body-file", "-")
	if err != nil {
		return false, err
	}
	return true, nil
}

// PRLinksIssue reports whether a PR body already carries a closing keyword for
// the issue, which is what makes GitHub associate the two.
func PRLinksIssue(pr PullRequest, issue int) bool {
	body := strings.ToLower(pr.Body)
	for _, verb := range []string{"closes", "fixes", "resolves"} {
		if strings.Contains(body, fmt.Sprintf("%s #%d", verb, issue)) {
			return true
		}
	}
	return false
}

// PROptions describes the pull request to open.
type PROptions struct {
	Repo  string
	Base  string
	Head  string
	Title string
	Body  string
	Draft bool
}

// CreatePR opens a pull request and returns its URL.
func (c *Client) CreatePR(ctx context.Context, opts PROptions) (string, error) {
	args := []string{"pr", "create",
		"--repo", opts.Repo,
		"--base", opts.Base,
		"--head", opts.Head,
		"--title", opts.Title,
		"--body-file", "-",
	}
	if opts.Draft {
		args = append(args, "--draft")
	}
	if c.DryRun {
		c.logf("not mutating: would run gh %s\nwith body:\n%s", strings.Join(args, " "), opts.Body)
		return "https://example.invalid/dry-run/pr", nil
	}
	out, err := c.run(ctx, opts.Body, args...)
	if err != nil {
		return "", err
	}
	// gh prints the PR URL on the last non-empty line.
	for _, line := range reverseLines(string(out)) {
		if strings.HasPrefix(line, "http") {
			return line, nil
		}
	}
	return "", fmt.Errorf("gh pr create produced no URL, output was: %s", strings.TrimSpace(string(out)))
}

func reverseLines(s string) []string {
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for i := len(raw) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(raw[i]); line != "" {
			out = append(out, line)
		}
	}
	return out
}
