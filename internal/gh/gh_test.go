package gh

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// stubGH writes an executable standing in for `gh`. It records the arguments
// and stdin it was given, so tests can assert on the command that was built.
func stubGH(t *testing.T, stdout string) (bin, argsFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "gh-stub.sh")
	argsFile = filepath.Join(dir, "args.txt")
	stdinFile = filepath.Join(dir, "stdin.txt")

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"cat > " + stdinFile + "\n" +
		"cat <<'STUBEOF'\n" + stdout + "\nSTUBEOF\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile, stdinFile
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestSearchIssues(t *testing.T) {
	out := `[
	  {"number":42,"title":"Add retries","url":"https://github.com/acme/widgets/issues/42",
	   "body":"please","repository":{"name":"widgets","nameWithOwner":"acme/widgets"},
	   "labels":[{"name":"agent-ready"}],"assignees":[],"isPullRequest":false,"state":"open"},
	  {"number":43,"title":"A pull request","url":"x","repository":{"nameWithOwner":"acme/widgets"},
	   "isPullRequest":true,"state":"open"}
	]`
	bin, argsFile, _ := stubGH(t, out)
	c := New(bin, false)

	results, err := c.SearchIssues(context.Background(), "agent-ready", []string{"acme", "other"}, 10)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	// Pull requests must never be worked as issues.
	if len(results) != 1 || results[0].Number != 42 {
		t.Fatalf("expected only the issue, got %+v", results)
	}
	if results[0].Repository.NameWithOwner != "acme/widgets" {
		t.Fatalf("repository not parsed: %+v", results[0].Repository)
	}

	args := readFile(t, argsFile)
	for _, want := range []string{"search", "issues", "--label", "agent-ready", "--state", "open", "--owner", "acme", "other"} {
		if !strings.Contains(args, want) {
			t.Errorf("command missing %q:\n%s", want, args)
		}
	}
	// --include-prs must never be passed.
	if strings.Contains(args, "--include-prs") {
		t.Error("search must not opt into pull requests")
	}
}

// gh is asked to scope the search with --owner, but a result naming a repo
// under any other owner must still be dropped before it reaches the caller.
func TestSearchIssuesDropsResultsOutsideOwners(t *testing.T) {
	out := `[
	  {"number":1,"title":"In scope","repository":{"nameWithOwner":"acme/widgets"},
	   "isPullRequest":false,"state":"open"},
	  {"number":2,"title":"Wrong owner","repository":{"nameWithOwner":"someoneelse/other"},
	   "isPullRequest":false,"state":"open"}
	]`
	bin, _, _ := stubGH(t, out)
	c := New(bin, false)

	results, err := c.SearchIssues(context.Background(), "agent-ready", []string{"acme"}, 10)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(results) != 1 || results[0].Repository.NameWithOwner != "acme/widgets" {
		t.Fatalf("expected only the acme/widgets issue, got %+v", results)
	}
}

// An empty label would match every open issue in every visible repo.
func TestSearchIssuesRejectsEmptyLabel(t *testing.T) {
	bin, _, _ := stubGH(t, "[]")
	if _, err := New(bin, false).SearchIssues(context.Background(), "", nil, 10); err == nil {
		t.Fatal("an empty label must be refused")
	}
}

func TestViewIssue(t *testing.T) {
	out := `{"number":42,"title":"Add retries","body":"please","url":"u","state":"OPEN",
	  "labels":[{"name":"agent-ready"}],
	  "comments":[{"author":{"login":"alice"},"body":"only idempotent ones"}]}`
	bin, argsFile, _ := stubGH(t, out)

	issue, err := New(bin, false).ViewIssue(context.Background(), "acme/widgets", 42)
	if err != nil {
		t.Fatalf("ViewIssue: %v", err)
	}
	if issue.Number != 42 || len(issue.Comments) != 1 || issue.Comments[0].Author.Login != "alice" {
		t.Fatalf("issue not parsed: %+v", issue)
	}
	if !issue.HasLabel("AGENT-READY") {
		t.Fatal("HasLabel should be case-insensitive")
	}
	if issue.HasLabel("nope") {
		t.Fatal("HasLabel matched a label that is not there")
	}
	if args := readFile(t, argsFile); !strings.Contains(args, "--repo") {
		t.Fatalf("repo not passed:\n%s", args)
	}
}

func TestFindPRForIssue(t *testing.T) {
	const prefix = "agent/issue-"

	find := func(t *testing.T, out string, issue int, branch string) (PullRequest, bool) {
		t.Helper()
		bin, _, _ := stubGH(t, out)
		pr, found, err := New(bin, false).FindPRForIssue(
			context.Background(), "acme/widgets", issue, branch, prefix)
		if err != nil {
			t.Fatal(err)
		}
		return pr, found
	}

	t.Run("closing keyword in the body", func(t *testing.T) {
		out := `[
		  {"number":1,"url":"https://github.com/acme/widgets/pull/1","body":"unrelated","headRefName":"feature-x","state":"OPEN"},
		  {"number":2,"url":"https://github.com/acme/widgets/pull/2","body":"Closes #42 as discussed","headRefName":"someone-else","state":"OPEN"}
		]`
		pr, found := find(t, out, 42, "agent/issue-42")
		if !found || pr.Number != 2 {
			t.Fatalf("a PR closing the issue should be found, got %+v (found=%v)", pr, found)
		}
	})

	t.Run("no match for an unrelated issue", func(t *testing.T) {
		out := `[{"number":2,"url":"u","body":"Closes #42","headRefName":"someone-else","state":"OPEN"}]`
		if pr, found := find(t, out, 99, "agent/issue-99"); found {
			t.Fatalf("expected no match, got %+v", pr)
		}
	})

	t.Run("branch match", func(t *testing.T) {
		out := `[{"number":3,"url":"u3","body":"","headRefName":"agent/issue-7","state":"OPEN"}]`
		if pr, found := find(t, out, 7, "agent/issue-7"); !found || pr.Number != 3 {
			t.Fatalf("branch match should be found, got %+v (found=%v)", pr, found)
		}
	})

	// The branch slug comes from the issue title, so editing the title changes
	// the branch this run would use. The PR still has to be found.
	t.Run("branch prefix match after a title change", func(t *testing.T) {
		out := `[{"number":4,"url":"u4","body":"","headRefName":"agent/issue-42-the-old-title","state":"OPEN"}]`
		pr, found := find(t, out, 42, "agent/issue-42-a-brand-new-title")
		if !found || pr.Number != 4 {
			t.Fatalf("prefix match should be found, got %+v (found=%v)", pr, found)
		}
	})

	// A merged or closed PR is delivered work just as much as an open one.
	t.Run("merged PR still counts", func(t *testing.T) {
		out := `[{"number":5,"url":"u5","body":"Closes #42","headRefName":"agent/issue-42","state":"MERGED","mergedAt":"2026-01-02T03:04:05Z"}]`
		pr, found := find(t, out, 42, "agent/issue-42")
		if !found || !pr.Merged() {
			t.Fatalf("a merged PR should be found and reported as merged, got %+v (found=%v)", pr, found)
		}
	})

	t.Run("closed PR still counts", func(t *testing.T) {
		out := `[{"number":6,"url":"u6","body":"Closes #42","headRefName":"agent/issue-42","state":"CLOSED"}]`
		if _, found := find(t, out, 42, "agent/issue-42"); !found {
			t.Fatal("a closed PR should still be found")
		}
	})

	// Issue 4 and issue 42 share a prefix; only exact numbers may match.
	t.Run("issue number boundaries", func(t *testing.T) {
		out := `[{"number":7,"url":"u7","body":"see #42 for context","headRefName":"agent/issue-42-x","state":"OPEN"}]`
		if pr, found := find(t, out, 4, "agent/issue-4"); found {
			t.Fatalf("issue 4 must not match issue 42's PR, got %+v", pr)
		}
	})

	// A human can edit the closing keyword out of a harness PR; the provenance
	// footer plus an issue reference is enough to recognise it.
	t.Run("provenance footer match", func(t *testing.T) {
		body := ProvenanceMarker + " (run abc)."
		out := `[{"number":8,"url":"u8","title":"Change your commit name (#42)","body":` +
			strconv.Quote(body) + `,"headRefName":"whatever","state":"OPEN"}]`
		if _, found := find(t, out, 42, "agent/issue-42"); !found {
			t.Fatal("a harness PR should be recognised by its provenance footer and title")
		}
	})

	// An agent summary can name any number of issues in passing. Adopting on
	// that would close out an issue whose work was never done.
	t.Run("a passing mention in the body is not a match", func(t *testing.T) {
		body := "Closes #7\n\nRelated to #42, but not fixing it.\n\n" + ProvenanceMarker + " (run abc)."
		out := `[{"number":8,"url":"u8","title":"Something else (#7)","body":` +
			strconv.Quote(body) + `,"headRefName":"agent/issue-7-something-else","state":"OPEN"}]`
		if pr, found := find(t, out, 42, "agent/issue-42-x"); found {
			t.Fatalf("issue 42 must not be adopted onto issue 7's PR, got %+v", pr)
		}
	})

	// An adopting caller should be pointed at the PR a human would care about.
	t.Run("open is preferred over closed", func(t *testing.T) {
		out := `[
		  {"number":9,"url":"u9","body":"Closes #42","headRefName":"agent/issue-42-old","state":"CLOSED"},
		  {"number":10,"url":"u10","body":"Closes #42","headRefName":"agent/issue-42-new","state":"OPEN"}
		]`
		if pr, found := find(t, out, 42, ""); !found || pr.Number != 10 {
			t.Fatalf("the open PR should win, got %+v (found=%v)", pr, found)
		}
	})

	t.Run("searches every state", func(t *testing.T) {
		bin, argsFile, _ := stubGH(t, `[]`)
		if _, _, err := New(bin, false).FindPRForIssue(
			context.Background(), "acme/widgets", 42, "agent/issue-42", prefix); err != nil {
			t.Fatal(err)
		}
		if args := readFile(t, argsFile); !strings.Contains(args, "--state all") &&
			!strings.Contains(args, "--state\nall") {
			t.Fatalf("PR discovery must not be limited to open PRs:\n%s", args)
		}
	})
}

func TestLinkPRToIssue(t *testing.T) {
	// Already linked: nothing is edited.
	bin, argsFile, _ := stubGH(t, "")
	pr := PullRequest{Number: 3, Body: "Closes #42\n\nwork"}
	linked, err := New(bin, false).LinkPRToIssue(context.Background(), "acme/widgets", pr, 42)
	if err != nil {
		t.Fatal(err)
	}
	if linked {
		t.Fatal("a PR that already closes the issue must not be edited")
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatalf("no gh call should have been made, got:\n%s", readFile(t, argsFile))
	}

	// Not linked: the body is rewritten with a closing keyword in front.
	bin2, argsFile2, stdinFile := stubGH(t, "")
	pr2 := PullRequest{Number: 3, Body: "just some work"}
	linked, err = New(bin2, false).LinkPRToIssue(context.Background(), "acme/widgets", pr2, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !linked {
		t.Fatal("an unlinked PR should be edited")
	}
	if args := readFile(t, argsFile2); !strings.Contains(args, "pr") || !strings.Contains(args, "edit") {
		t.Fatalf("expected a gh pr edit call, got:\n%s", args)
	}
	body := readFile(t, stdinFile)
	if !strings.HasPrefix(body, "Closes #42") || !strings.Contains(body, "just some work") {
		t.Fatalf("body should gain a closing keyword and keep its content, got:\n%s", body)
	}
}

func TestCreatePRExtractsURLAndSendsBodyOnStdin(t *testing.T) {
	bin, argsFile, stdinFile := stubGH(t, "Warning: 3 uncommitted changes\nhttps://github.com/acme/widgets/pull/9")
	c := New(bin, false)

	url, err := c.CreatePR(context.Background(), PROptions{
		Repo: "acme/widgets", Base: "main", Head: "agent/issue-42",
		Title: "Add retries (#42)", Body: "Closes #42\n\nlong body", Draft: true,
	})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if url != "https://github.com/acme/widgets/pull/9" {
		t.Fatalf("URL should be taken from the last line, got %q", url)
	}

	args := readFile(t, argsFile)
	if !strings.Contains(args, "--draft") {
		t.Errorf("PRs must be opened as drafts:\n%s", args)
	}
	if !strings.Contains(args, "--body-file") {
		t.Errorf("body should be passed by file/stdin, not as an argument:\n%s", args)
	}
	if body := readFile(t, stdinFile); !strings.Contains(body, "Closes #42") {
		t.Errorf("body did not reach stdin: %q", body)
	}
}

func TestCreatePRWithoutURL(t *testing.T) {
	bin, _, _ := stubGH(t, "something went sideways")
	_, err := New(bin, false).CreatePR(context.Background(), PROptions{Repo: "acme/widgets"})
	if err == nil || !strings.Contains(err.Error(), "no URL") {
		t.Fatalf("want a clear no-URL error, got %v", err)
	}
}

// Dry run is the safety net for exercising the pipeline against real issues.
func TestDryRunSuppressesMutations(t *testing.T) {
	bin, argsFile, _ := stubGH(t, "https://github.com/acme/widgets/pull/9")
	c := New(bin, true)
	ctx := context.Background()

	if err := c.EditLabels(ctx, "acme/widgets", 42, []string{"agent-working"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Comment(ctx, "acme/widgets", 42, "hello"); err != nil {
		t.Fatal(err)
	}
	url, err := c.CreatePR(ctx, PROptions{Repo: "acme/widgets", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if url == "" {
		t.Fatal("dry-run should still return a placeholder URL so the caller can continue")
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("dry-run must not invoke gh at all")
	}
}

func TestEditLabelsNoopWhenNothingToDo(t *testing.T) {
	bin, argsFile, _ := stubGH(t, "")
	if err := New(bin, false).EditLabels(context.Background(), "acme/widgets", 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("an empty label edit should not shell out")
	}
}

func TestCmdErrorIncludesStderr(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh-fail.sh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'HTTP 404: Not Found' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := New(bin, false).ViewIssue(context.Background(), "acme/widgets", 42)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("stderr should be surfaced, got %v", err)
	}
	var cmdErr *CmdError
	if !asCmdError(err, &cmdErr) || cmdErr.ExitCode != 1 {
		t.Fatalf("want a typed CmdError with the exit code, got %#v", err)
	}
}

func asCmdError(err error, target **CmdError) bool {
	e, ok := err.(*CmdError)
	if ok {
		*target = e
	}
	return ok
}

func TestDefaultBranchAndCloneURL(t *testing.T) {
	bin, _, _ := stubGH(t, `{"defaultBranchRef":{"name":"trunk"}}`)
	branch, err := New(bin, false).DefaultBranch(context.Background(), "acme/widgets")
	if err != nil || branch != "trunk" {
		t.Fatalf("DefaultBranch = %q, %v", branch, err)
	}

	bin2, _, _ := stubGH(t, `{"url":"https://github.com/acme/widgets"}`)
	url, err := New(bin2, false).CloneURL(context.Background(), "acme/widgets")
	if err != nil || url != "https://github.com/acme/widgets.git" {
		t.Fatalf("CloneURL = %q, %v", url, err)
	}

	bin3, _, _ := stubGH(t, `{"defaultBranchRef":{"name":""}}`)
	if _, err := New(bin3, false).DefaultBranch(context.Background(), "acme/widgets"); err == nil {
		t.Fatal("an empty default branch must be an error, not an empty string")
	}
}

// labelStub stands in for gh across several invocations: it records every
// command line and answers `issue view` with a fixed set of current labels.
// `issue edit` fails whenever it mentions failLabel, which is how the
// per-label retry path is exercised.
func labelStub(t *testing.T, current []string, failLabel string) (bin string, calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "gh-stub.sh")
	callsFile := filepath.Join(dir, "calls.txt")

	labels := make([]string, 0, len(current))
	for _, l := range current {
		labels = append(labels, `{"name":"`+l+`"}`)
	}
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + callsFile + "\n" +
		"case \"$1 $2\" in\n" +
		"  'issue view') echo '{\"labels\":[" + strings.Join(labels, ",") + "]}' ;;\n" +
		"  'issue edit')\n" +
		"    if [ -n \"" + failLabel + "\" ]; then\n" +
		"      case \"$*\" in *" + failLabel + "*) echo \"could not update: '" + failLabel + "' not found\" >&2; exit 1 ;; esac\n" +
		"    fi ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() []string {
		data, err := os.ReadFile(callsFile)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatalf("read calls: %v", err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// gh rejects the whole edit when asked to remove a label the issue does not
// carry, which used to take the rest of the edit down with it.
func TestEditLabelsReducesToTheActualChange(t *testing.T) {
	bin, calls := labelStub(t, []string{"agent-ready", "agent-working"}, "")

	err := New(bin, false).EditLabels(context.Background(), "acme/widgets", 42,
		[]string{"agent-ready"}, []string{"agent-working", "agent-failed"})
	if err != nil {
		t.Fatalf("EditLabels: %v", err)
	}

	var edit string
	for _, c := range calls() {
		if strings.HasPrefix(c, "issue edit") {
			edit = c
		}
	}
	if edit == "" {
		t.Fatalf("expected an issue edit, got calls %v", calls())
	}
	if strings.Contains(edit, "--add-label") {
		t.Errorf("a label the issue already has must not be re-added: %q", edit)
	}
	if !strings.Contains(edit, "--remove-label agent-working") {
		t.Errorf("a label the issue carries must be removed: %q", edit)
	}
	if strings.Contains(edit, "agent-failed") {
		t.Errorf("a label the issue does not carry must not be removed: %q", edit)
	}
}

func TestEditLabelsSkipsGHEntirelyWhenNothingChanges(t *testing.T) {
	bin, calls := labelStub(t, []string{"agent-done"}, "")

	if err := New(bin, false).EditLabels(context.Background(), "acme/widgets", 42,
		[]string{"agent-done"}, []string{"agent-working"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls() {
		if strings.HasPrefix(c, "issue edit") {
			t.Fatalf("no-op edit should not shell out to `issue edit`: %v", calls())
		}
	}
}

// The daemon's own state labels do not exist in a fresh repository, and
// `gh issue edit --add-label` fails outright on an unknown label.
func TestEditLabelsCreatesMissingLabels(t *testing.T) {
	bin, calls := labelStub(t, []string{"agent-ready"}, "")

	if err := New(bin, false).EditLabels(context.Background(), "acme/widgets", 42,
		[]string{"agent-working"}, nil); err != nil {
		t.Fatal(err)
	}
	created := false
	for _, c := range calls() {
		if strings.HasPrefix(c, "label create agent-working") {
			created = true
		}
	}
	if !created {
		t.Fatalf("expected the label to be created first, got calls %v", calls())
	}
}

// One label gh refuses must not strand the others.
func TestEditLabelsRetriesIndividuallyAfterARejection(t *testing.T) {
	bin, calls := labelStub(t, []string{"agent-ready", "agent-working"}, "agent-done")

	err := New(bin, false).EditLabels(context.Background(), "acme/widgets", 42,
		[]string{"agent-done"}, []string{"agent-working"})
	if err == nil {
		t.Fatal("the rejected label should still be reported as an error")
	}
	removed := false
	for _, c := range calls() {
		if strings.HasPrefix(c, "issue edit") && strings.Contains(c, "--remove-label agent-working") &&
			!strings.Contains(c, "agent-done") {
			removed = true
		}
	}
	if !removed {
		t.Fatalf("the removal should have been retried on its own, got calls %v", calls())
	}
}
