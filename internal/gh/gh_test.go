package gh

import (
	"context"
	"os"
	"path/filepath"
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
	out := `[
	  {"number":1,"url":"https://github.com/acme/widgets/pull/1","body":"unrelated","headRefName":"feature-x"},
	  {"number":2,"url":"https://github.com/acme/widgets/pull/2","body":"Closes #42 as discussed","headRefName":"someone-else"}
	]`
	bin, _, _ := stubGH(t, out)
	c := New(bin, false)

	url, err := c.FindPRForIssue(context.Background(), "acme/widgets", 42, "agent/issue-42")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/acme/widgets/pull/2" {
		t.Fatalf("a PR closing the issue should be found, got %q", url)
	}

	// No match for an unrelated issue.
	url, err = c.FindPRForIssue(context.Background(), "acme/widgets", 99, "agent/issue-99")
	if err != nil {
		t.Fatal(err)
	}
	if url != "" {
		t.Fatalf("expected no match, got %q", url)
	}

	// A PR already pushed to our branch also counts.
	bin2, _, _ := stubGH(t, `[{"number":3,"url":"https://github.com/acme/widgets/pull/3","body":"","headRefName":"agent/issue-7"}]`)
	url, err = New(bin2, false).FindPRForIssue(context.Background(), "acme/widgets", 7, "agent/issue-7")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/acme/widgets/pull/3" {
		t.Fatalf("branch match should be found, got %q", url)
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
