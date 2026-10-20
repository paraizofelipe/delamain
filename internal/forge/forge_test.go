package forge

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// call is one recorded CLI invocation.
type call struct {
	args  []string
	stdin string
}

// fakeRunner answers by the first two args ("pr create", "issue view", ...).
type fakeRunner struct {
	calls []call
	out   map[string]string
	errs  map[string]error
}

func (f *fakeRunner) Run(_ context.Context, stdin string, args ...string) (string, error) {
	f.calls = append(f.calls, call{args: args, stdin: stdin})
	key := strings.Join(args[:min(2, len(args))], " ")
	return f.out[key], f.errs[key]
}

func (f *fakeRunner) find(t *testing.T, sub string) call {
	t.Helper()
	for _, c := range f.calls {
		if strings.Join(c.args[:min(2, len(c.args))], " ") == sub {
			return c
		}
	}
	t.Fatalf("no %q call in %v", sub, f.calls)
	return call{}
}

func has(args []string, flag, value string) bool {
	i := slices.Index(args, flag)
	return i >= 0 && (value == "" || (i+1 < len(args) && args[i+1] == value))
}

func TestDetectShouldRecognizeForgeFromRemoteURL(t *testing.T) {
	cases := []struct {
		url, kind string
		want      Repo
	}{
		{"https://github.com/acme/api.git", "auto", Repo{GitHub, "github.com", "acme/api"}},
		{"git@github.com:acme/api.git", "auto", Repo{GitHub, "github.com", "acme/api"}},
		{"ssh://git@gitlab.com:2222/grp/sub/api.git", "auto", Repo{GitLab, "gitlab.com", "grp/sub/api"}},
		{"git@gitlab.luizalabs.com:team/api.git", "auto", Repo{GitLab, "gitlab.luizalabs.com", "team/api"}},
		{"https://git.corp.example/team/api", "gitlab", Repo{GitLab, "git.corp.example", "team/api"}},
		{"https://ghe.corp.example/team/api.git", "github", Repo{GitHub, "ghe.corp.example", "team/api"}},
	}
	for _, c := range cases {
		got, err := Detect(c.url, c.kind)
		if err != nil || got != c.want {
			t.Errorf("Detect(%q, %q) = %+v, %v; want %+v", c.url, c.kind, got, err, c.want)
		}
	}
}

func TestDetectShouldNeverKeepCredentialsFromTheURL(t *testing.T) {
	got, err := Detect("https://oauth2:glpat-SECRET@gitlab.com/grp/api.git", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.String(), "SECRET") || strings.Contains(got.String(), "oauth2") {
		t.Fatalf("credentials leaked into %q", got.String())
	}
}

func TestDetectShouldRejectUnknownHostsAndMalformedURLs(t *testing.T) {
	for _, u := range []string{"https://bitbucket.org/acme/api.git", "/srv/git/api.git", "", "https://github.com/", "git@github.com:"} {
		if _, err := Detect(u, "auto"); err == nil {
			t.Errorf("Detect(%q) must fail: unknown forge or no repository path", u)
		}
	}
}

func TestGitLabShouldCreateDraftMRNonInteractively(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"mr create": "\nCreating merge request for fix/7-add into main in grp/api\n\nhttps://gitlab.com/grp/api/-/merge_requests/15\n"}}
	f := NewExec(Repo{GitLab, "gitlab.com", "grp/api"}, run)
	pr, err := f.CreatePR(context.Background(), PROptions{Head: "fix/7-add", Base: "main", Title: "fix: add", Body: "Closes #7", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 15 || pr.URL != "https://gitlab.com/grp/api/-/merge_requests/15" {
		t.Fatalf("mr = %+v", pr)
	}
	c := run.find(t, "mr create")
	for _, want := range [][2]string{{"-R", "https://gitlab.com/grp/api"}, {"--source-branch", "fix/7-add"}, {"--target-branch", "main"}, {"--title", "fix: add"}, {"--description-file", "-"}, {"--draft", ""}, {"--yes", ""}, {"--no-editor", ""}} {
		if !has(c.args, want[0], want[1]) {
			t.Errorf("missing %s %s in %v", want[0], want[1], c.args)
		}
	}
	if has(c.args, "--push", "") || has(c.args, "--fill", "") {
		t.Error("glab must never push: the orchestrator pushes after its own guards")
	}
	if c.stdin != "Closes #7" {
		t.Errorf("description must go through stdin, got %q", c.stdin)
	}
}

func TestGitLabShouldReadIssueAndOpenMR(t *testing.T) {
	run := &fakeRunner{out: map[string]string{
		"issue view": `{"iid":3,"title":"Add login","description":"- [ ] ok","web_url":"https://gitlab.com/grp/api/-/issues/3","state":"opened","labels":["delamain:ready"]}`,
		"mr list":    `[{"iid":5,"web_url":"https://gitlab.com/grp/api/-/merge_requests/5","draft":false}]`,
	}}
	f := NewExec(Repo{GitLab, "gitlab.com", "grp/api"}, run)
	is, err := f.Issue(context.Background(), 3)
	if err != nil || is.Number != 3 || is.Body != "- [ ] ok" || !is.Open || !slices.Equal(is.Labels, []string{"delamain:ready"}) {
		t.Fatalf("issue = %+v, err = %v", is, err)
	}
	pr, err := f.OpenPR(context.Background(), "feat/3-login")
	if err != nil || pr == nil || pr.Number != 5 || pr.Draft {
		t.Fatalf("mr = %+v, err = %v", pr, err)
	}
	if !has(run.find(t, "mr list").args, "--source-branch", "feat/3-login") {
		t.Fatal("must filter MRs by source branch")
	}
}

func TestGitLabShouldRelabelAndCommentWithoutEmptyFlags(t *testing.T) {
	run := &fakeRunner{}
	f := NewExec(Repo{GitLab, "gitlab.com", "grp/api"}, run)
	if err := f.Relabel(context.Background(), 3, []string{"a", "b"}, nil); err != nil {
		t.Fatal(err)
	}
	c := run.find(t, "issue update")
	if !has(c.args, "--label", "a,b") || has(c.args, "--unlabel", "") {
		t.Fatalf("update args = %v", c.args)
	}
	if err := f.Comment(context.Background(), 3, "status"); err != nil {
		t.Fatal(err)
	}
	if c := run.find(t, "issue note"); !has(c.args, "--message", "status") {
		t.Fatalf("note args = %v", c.args)
	}
}

// The whole point of delamain's delivery: it opens PRs/MRs and never merges them.
func TestForgeShouldNeverIssueMergeCommands(t *testing.T) {
	for _, repo := range []Repo{{GitHub, "github.com", "acme/api"}, {GitLab, "gitlab.com", "grp/api"}} {
		run := &fakeRunner{out: map[string]string{
			"pr create": "https://github.com/acme/api/pull/1", "mr create": "https://gitlab.com/grp/api/-/merge_requests/1",
			"pr list": "[]", "mr list": "[]", "issue view": "{}",
		}}
		f := NewExec(repo, run)
		ctx := context.Background()
		_, _ = f.Issue(ctx, 1)
		_, _ = f.OpenPR(ctx, "feat/x")
		_, _ = f.CreatePR(ctx, PROptions{Head: "feat/x", Base: "main", Title: "t"})
		_ = f.Comment(ctx, 1, "c")
		_ = f.Relabel(ctx, 1, []string{"a"}, []string{"b"})
		for _, c := range run.calls {
			for _, a := range c.args {
				if strings.Contains(a, "merge") && !strings.HasPrefix(a, "https://") {
					t.Errorf("%s: forbidden arg %q in %v", repo.Kind, a, c.args)
				}
			}
		}
	}
}

func TestExecRunnerErrorShouldNotEchoStdin(t *testing.T) {
	r := ExecRunner{Binary: "sh"}
	_, err := r.Run(context.Background(), "SECRET-BODY", "-c", "cat >/dev/null; echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("non-zero exit must fail")
	}
	if strings.Contains(err.Error(), "SECRET-BODY") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error must carry stderr but never the body: %v", err)
	}
}

// flaky is a sh script that fails with stderr msg on its first `fails` calls, then prints
// what it read from stdin. It returns the runner, its args and a func reporting the number of calls.
func flaky(t *testing.T, fails int, msg string) (ExecRunner, []string, func() int) {
	t.Helper()
	n := filepath.Join(t.TempDir(), "calls")
	script := `in=$(cat); c=$(($(cat "$1" 2>/dev/null || echo 0)+1)); echo $c >"$1"; ` +
		`if [ $c -le ` + strconv.Itoa(fails) + ` ]; then echo "$2" >&2; exit 1; fi; printf %s "$in"`
	calls := func() int {
		b, _ := os.ReadFile(n)
		c, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return c
	}
	return ExecRunner{Binary: "sh", Backoff: time.Millisecond}, []string{"-c", script, "sh", n, msg}, calls
}

func TestExecRunnerShouldRetryTransientNetworkErrorsWithTheSameStdin(t *testing.T) {
	r, args, calls := flaky(t, 2, `Post "https://api.github.com/repos/a/b/labels": net/http: TLS handshake timeout`)
	out, err := r.Run(context.Background(), "BODY", args...)
	if err != nil || out != "BODY" || calls() != 3 {
		t.Fatalf("a network blip must not leave the issue mislabeled: out=%q err=%v calls=%d", out, err, calls())
	}
}

func TestExecRunnerShouldNotRetryAPIRejections(t *testing.T) {
	r, args, calls := flaky(t, 5, "HTTP 422: Validation Failed")
	if _, err := r.Run(context.Background(), "", args...); err == nil || calls() != 1 {
		t.Fatalf("a rejected request fails at once: err=%v calls=%d", err, calls())
	}
}

func TestExecRunnerShouldGiveUpAfterThreeAttempts(t *testing.T) {
	r, args, calls := flaky(t, 5, "HTTP 502: Bad Gateway")
	_, err := r.Run(context.Background(), "", args...)
	if err == nil || !strings.Contains(err.Error(), "502") || calls() != 3 {
		t.Fatalf("persistent outage fails with its cause after 3 attempts: err=%v calls=%d", err, calls())
	}
}

func TestExecRunnerShouldStopRetryingWhenTheContextEnds(t *testing.T) {
	r, args, calls := flaky(t, 5, "connection reset by peer")
	r.Backoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Run(ctx, "", args...); err == nil || calls() != 1 {
		t.Fatalf("shutdown must not wait out the backoff: err=%v calls=%d", err, calls())
	}
}

func TestGitLabListReadyShouldReturnOpenLabeledIssuesOldestFirst(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"issue list": `[{"iid":12,"title":"b","description":"x","web_url":"u12","state":"opened","labels":["delamain:ready"]},{"iid":3,"title":"a","description":"y","web_url":"u3","state":"opened","labels":["delamain:ready"]}]`}}
	got, err := NewExec(Repo{GitLab, "gitlab.com", "grp/api"}, run).ListReady(context.Background(), "delamain:ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 3 || got[0].Body != "y" || got[1].Number != 12 {
		t.Fatalf("got %+v", got)
	}
	c := run.find(t, "issue list")
	if !has(c.args, "--label", "delamain:ready") || !has(c.args, "--output", "json") || has(c.args, "--all", "") {
		t.Fatalf("only open issues: %v", c.args)
	}
}
