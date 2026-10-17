package forge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
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

func TestGitHubShouldCreateDraftPRWithBodyOnStdin(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"pr create": "https://github.com/acme/api/pull/42\n"}}
	f := New(Repo{GitHub, "github.com", "acme/api"}, run)

	pr, err := f.CreatePR(context.Background(), PROptions{Head: "fix/7-add", Base: "main", Title: "fix: add", Body: "Closes #7", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 42 || pr.URL != "https://github.com/acme/api/pull/42" || !pr.Draft {
		t.Fatalf("pr = %+v", pr)
	}
	c := run.find(t, "pr create")
	for _, want := range [][2]string{{"-R", "acme/api"}, {"--head", "fix/7-add"}, {"--base", "main"}, {"--title", "fix: add"}, {"--body-file", "-"}, {"--draft", ""}} {
		if !has(c.args, want[0], want[1]) {
			t.Errorf("missing %s %s in %v", want[0], want[1], c.args)
		}
	}
	if c.stdin != "Closes #7" {
		t.Errorf("body must go through stdin, got %q", c.stdin)
	}
}

func TestGitHubShouldOpenReadyPRWhenNotDraft(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"pr create": "https://github.com/acme/api/pull/1"}}
	if _, err := New(Repo{GitHub, "github.com", "acme/api"}, run).CreatePR(context.Background(), PROptions{Head: "feat/x", Base: "main", Title: "feat: x"}); err != nil {
		t.Fatal(err)
	}
	if has(run.find(t, "pr create").args, "--draft", "") {
		t.Fatal("approved work must not be opened as draft")
	}
}

func TestGitHubShouldFailWhenCreateOutputHasNoPRURL(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"pr create": "something unexpected"}}
	if _, err := New(Repo{GitHub, "github.com", "acme/api"}, run).CreatePR(context.Background(), PROptions{Head: "feat/x", Base: "main", Title: "t"}); err == nil {
		t.Fatal("an unparseable result must not be reported as an opened PR")
	}
}

func TestGitHubShouldUseHostQualifiedRepoForEnterprise(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"pr list": "[]"}}
	if _, err := New(Repo{GitHub, "ghe.corp.example", "team/api"}, run).OpenPR(context.Background(), "feat/x"); err != nil {
		t.Fatal(err)
	}
	if !has(run.find(t, "pr list").args, "-R", "ghe.corp.example/team/api") {
		t.Fatalf("enterprise host must be explicit: %v", run.calls[0].args)
	}
}

func TestGitHubOpenPRShouldReturnNilWhenNoneIsOpen(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"pr list": "[]"}}
	pr, err := New(Repo{GitHub, "github.com", "acme/api"}, run).OpenPR(context.Background(), "feat/x")
	if err != nil || pr != nil {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
	c := run.find(t, "pr list")
	if !has(c.args, "--head", "feat/x") || !has(c.args, "--state", "open") {
		t.Fatalf("must filter open PRs by head branch: %v", c.args)
	}
}

func TestGitHubOpenPRShouldReturnTheExistingPR(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"pr list": `[{"number":9,"url":"https://github.com/acme/api/pull/9","isDraft":true}]`}}
	pr, err := New(Repo{GitHub, "github.com", "acme/api"}, run).OpenPR(context.Background(), "feat/x")
	if err != nil || pr == nil || pr.Number != 9 || !pr.Draft {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
}

func TestGitHubShouldReadIssue(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"issue view": `{"number":7,"title":"Fix add","body":"## Critérios","url":"https://github.com/acme/api/issues/7","state":"OPEN","labels":[{"name":"bug"},{"name":"delamain:ready"}]}`}}
	is, err := New(Repo{GitHub, "github.com", "acme/api"}, run).Issue(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := Issue{Number: 7, Title: "Fix add", Body: "## Critérios", URL: "https://github.com/acme/api/issues/7", Labels: []string{"bug", "delamain:ready"}, Open: true}
	if is.Number != want.Number || is.Title != want.Title || is.Body != want.Body || is.URL != want.URL || !slices.Equal(is.Labels, want.Labels) || !is.Open {
		t.Fatalf("issue = %+v", is)
	}
}

func TestGitHubShouldEnsureLabelsExistBeforeRelabeling(t *testing.T) {
	run := &fakeRunner{errs: map[string]error{"label create": errors.New(`label with name "delamain:ready" already exists; use --force to update`)}}
	err := New(Repo{GitHub, "github.com", "acme/api"}, run).Relabel(context.Background(), 7, []string{"delamain:in-progress"}, []string{"delamain:ready"})
	if err != nil {
		t.Fatalf("an existing label is not an error: %v", err)
	}
	created := 0
	for _, c := range run.calls {
		if c.args[0] == "label" {
			created++
			if has(c.args, "--force", "") {
				t.Error("must not overwrite the user's label color/description")
			}
		}
	}
	if created != 2 {
		t.Errorf("labels ensured = %d, want 2 (added and removed)", created)
	}
	c := run.find(t, "issue edit")
	if !has(c.args, "--add-label", "delamain:in-progress") || !has(c.args, "--remove-label", "delamain:ready") {
		t.Fatalf("edit args = %v", c.args)
	}
}

func TestGitHubRelabelShouldPropagateRealLabelErrors(t *testing.T) {
	run := &fakeRunner{errs: map[string]error{"label create": errors.New("HTTP 403: Resource not accessible")}}
	if err := New(Repo{GitHub, "github.com", "acme/api"}, run).Relabel(context.Background(), 7, []string{"x"}, nil); err == nil {
		t.Fatal("a permission error must not be swallowed")
	}
}

func TestGitHubCommentShouldSendBodyOnStdin(t *testing.T) {
	run := &fakeRunner{}
	if err := New(Repo{GitHub, "github.com", "acme/api"}, run).Comment(context.Background(), 7, "olá"); err != nil {
		t.Fatal(err)
	}
	c := run.find(t, "issue comment")
	if c.args[2] != "7" || !has(c.args, "--body-file", "-") || c.stdin != "olá" {
		t.Fatalf("comment call = %+v", c)
	}
}

func TestGitLabShouldCreateDraftMRNonInteractively(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"mr create": "\nCreating merge request for fix/7-add into main in grp/api\n\nhttps://gitlab.com/grp/api/-/merge_requests/15\n"}}
	f := New(Repo{GitLab, "gitlab.com", "grp/api"}, run)
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
	f := New(Repo{GitLab, "gitlab.com", "grp/api"}, run)
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
	f := New(Repo{GitLab, "gitlab.com", "grp/api"}, run)
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
		f := New(repo, run)
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

func TestGitHubListReadyShouldReturnOpenLabeledIssuesOldestFirst(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"issue list": `[{"number":9,"title":"b","body":"x","url":"u9","state":"OPEN","labels":[{"name":"delamain:ready"}]},{"number":4,"title":"a","body":"y","url":"u4","state":"OPEN","labels":[]}]`}}
	got, err := New(Repo{GitHub, "github.com", "acme/api"}, run).ListReady(context.Background(), "delamain:ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 4 || got[1].Number != 9 || !got[0].Open || got[1].Labels[0] != "delamain:ready" {
		t.Fatalf("FIFO by issue number, got %+v", got)
	}
	c := run.find(t, "issue list")
	if !has(c.args, "--label", "delamain:ready") || !has(c.args, "--state", "open") {
		t.Fatalf("args = %v", c.args)
	}
}

func TestGitLabListReadyShouldReturnOpenLabeledIssuesOldestFirst(t *testing.T) {
	run := &fakeRunner{out: map[string]string{"issue list": `[{"iid":12,"title":"b","description":"x","web_url":"u12","state":"opened","labels":["delamain:ready"]},{"iid":3,"title":"a","description":"y","web_url":"u3","state":"opened","labels":["delamain:ready"]}]`}}
	got, err := New(Repo{GitLab, "gitlab.com", "grp/api"}, run).ListReady(context.Background(), "delamain:ready")
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
