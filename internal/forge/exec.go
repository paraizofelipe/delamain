package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Runner executes the forge CLI; stdin carries bodies so they never appear in args or errors.
type Runner interface {
	Run(ctx context.Context, stdin string, args ...string) (string, error)
}

// Binary is the CLI that talks to a forge kind.
func Binary(k Kind) string {
	if k == GitLab {
		return "glab"
	}
	return "gh"
}

// NewExec returns the forge client for repo that drives the gh/glab CLI.
func NewExec(repo Repo, run Runner) Forge {
	if repo.Kind == GitLab {
		return gitlabExec{repo: repo, run: run}
	}
	return githubExec{repo: repo, run: run}
}

// ExecRunner runs a forge CLI non-interactively. Auth comes from the CLI's own login or env.
type ExecRunner struct {
	Binary string
	Dir    string
	// Backoff is the wait before the first retry of a transient failure; it doubles after.
	Backoff time.Duration
}

// transient matches network blips and gateway errors worth retrying; API rejections are not.
var execTransient = regexp.MustCompile(`(?i)timeout|timed out|connection (reset|refused)|unexpected EOF|no such host|temporary failure|HTTP 50[234]`)

func (e ExecRunner) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	wait := e.Backoff
	if wait <= 0 {
		wait = 2 * time.Second
	}
	for i := 1; ; i++ {
		out, err := e.run(ctx, stdin, args)
		if err == nil || i == attempts || ctx.Err() != nil || !execTransient.MatchString(err.Error()) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return "", err
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func (e ExecRunner) run(ctx context.Context, stdin string, args []string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Binary, args...)
	cmd.Dir = e.Dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "NO_PROMPT=1", "NO_COLOR=1", "GH_NO_UPDATE_NOTIFIER=1", "GLAB_CHECK_UPDATE=false")
	if err := cmd.Run(); err != nil {
		label := strings.Join(args[:min(2, len(args))], " ")
		return "", fmt.Errorf("%s %s: %w: %s", e.Binary, label, err, truncate(strings.TrimSpace(errb.String()), 500))
	}
	return out.String(), nil
}

// prFromOutput extracts the PR/MR URL the CLIs print on creation.
func prFromOutput(out string, re *regexp.Regexp, draft bool) (PR, error) {
	m := re.FindStringSubmatch(out)
	if m == nil {
		return PR{}, errors.New("a saída não contém a URL do PR/MR criado: " + truncate(strings.TrimSpace(out), 200))
	}
	n, _ := strconv.Atoi(m[2])
	return PR{Number: n, URL: m[1], Draft: draft}, nil
}

type githubExec struct {
	repo Repo
	run  Runner
}

var ghPRURL = regexp.MustCompile(`(https?://\S+/pull/(\d+))`)

func (g githubExec) Repo() Repo { return g.repo }

// rflag is the -R value: OWNER/REPO on github.com, HOST/OWNER/REPO on Enterprise.
func (g githubExec) rflag() string {
	if g.repo.Host == "github.com" {
		return g.repo.Path
	}
	return g.repo.String()
}

type ghIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (v ghIssue) issue() Issue {
	is := Issue{Number: v.Number, Title: v.Title, Body: v.Body, URL: v.URL, Open: v.State == "OPEN"}
	for _, l := range v.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	return is
}

const ghIssueFields = "number,title,body,url,state,labels"

func (g githubExec) Issue(ctx context.Context, n int) (Issue, error) {
	out, err := g.run.Run(ctx, "", "issue", "view", strconv.Itoa(n), "-R", g.rflag(), "--json", ghIssueFields)
	if err != nil {
		return Issue{}, err
	}
	var v ghIssue
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return Issue{}, err
	}
	return v.issue(), nil
}

func (g githubExec) ListReady(ctx context.Context, label string) ([]Issue, error) {
	out, err := g.run.Run(ctx, "", "issue", "list", "-R", g.rflag(), "--label", label, "--state", "open", "--limit", "100", "--json", ghIssueFields)
	if err != nil {
		return nil, err
	}
	var vs []ghIssue
	if err := json.Unmarshal([]byte(out), &vs); err != nil {
		return nil, err
	}
	is := make([]Issue, 0, len(vs))
	for _, v := range vs {
		is = append(is, v.issue())
	}
	return oldestFirst(is), nil
}

func (g githubExec) OpenPR(ctx context.Context, head string) (*PR, error) {
	out, err := g.run.Run(ctx, "", "pr", "list", "-R", g.rflag(), "--head", head, "--state", "open", "--json", "number,url,isDraft")
	if err != nil {
		return nil, err
	}
	var prs []struct {
		Number  int    `json:"number"`
		URL     string `json:"url"`
		IsDraft bool   `json:"isDraft"`
	}
	if err := json.Unmarshal([]byte(out), &prs); err != nil || len(prs) == 0 {
		return nil, err
	}
	return &PR{Number: prs[0].Number, URL: prs[0].URL, Draft: prs[0].IsDraft}, nil
}

func (g githubExec) CreatePR(ctx context.Context, o PROptions) (PR, error) {
	args := []string{"pr", "create", "-R", g.rflag(), "--head", o.Head, "--base", o.Base, "--title", o.Title, "--body-file", "-"}
	if o.Draft {
		args = append(args, "--draft")
	}
	out, err := g.run.Run(ctx, o.Body, args...)
	if err != nil {
		return PR{}, err
	}
	return prFromOutput(out, ghPRURL, o.Draft)
}

func (g githubExec) Comment(ctx context.Context, n int, body string) error {
	_, err := g.run.Run(ctx, body, "issue", "comment", strconv.Itoa(n), "-R", g.rflag(), "--body-file", "-")
	return err
}

// Relabel creates missing labels first: GitHub rejects unknown labels on edit.
func (g githubExec) Relabel(ctx context.Context, n int, add, remove []string) error {
	for _, l := range append(append([]string{}, add...), remove...) {
		_, err := g.run.Run(ctx, "", "label", "create", l, "-R", g.rflag(), "--color", "5319e7", "--description", "delamain")
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}
	args := []string{"issue", "edit", strconv.Itoa(n), "-R", g.rflag()}
	for _, l := range add {
		args = append(args, "--add-label", l)
	}
	for _, l := range remove {
		args = append(args, "--remove-label", l)
	}
	_, err := g.run.Run(ctx, "", args...)
	return err
}

type gitlabExec struct {
	repo Repo
	run  Runner
}

var glMRURL = regexp.MustCompile(`(https?://\S+/-/merge_requests/(\d+))`)

func (g gitlabExec) Repo() Repo { return g.repo }

// rflag is a full URL so self-hosted instances resolve without glab's host config.
func (g gitlabExec) rflag() string { return "https://" + g.repo.String() }

type glIssue struct {
	IID         int      `json:"iid"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	WebURL      string   `json:"web_url"`
	State       string   `json:"state"`
	Labels      []string `json:"labels"`
}

func (v glIssue) issue() Issue {
	return Issue{Number: v.IID, Title: v.Title, Body: v.Description, URL: v.WebURL, Labels: v.Labels, Open: v.State == "opened"}
}

func (g gitlabExec) Issue(ctx context.Context, n int) (Issue, error) {
	out, err := g.run.Run(ctx, "", "issue", "view", strconv.Itoa(n), "-R", g.rflag(), "--output", "json")
	if err != nil {
		return Issue{}, err
	}
	var v glIssue
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return Issue{}, err
	}
	return v.issue(), nil
}

// ListReady relies on glab listing only open issues by default.
func (g gitlabExec) ListReady(ctx context.Context, label string) ([]Issue, error) {
	out, err := g.run.Run(ctx, "", "issue", "list", "-R", g.rflag(), "--label", label, "--per-page", "100", "--output", "json")
	if err != nil {
		return nil, err
	}
	var vs []glIssue
	if err := json.Unmarshal([]byte(out), &vs); err != nil {
		return nil, err
	}
	is := make([]Issue, 0, len(vs))
	for _, v := range vs {
		is = append(is, v.issue())
	}
	return oldestFirst(is), nil
}

func (g gitlabExec) OpenPR(ctx context.Context, head string) (*PR, error) {
	out, err := g.run.Run(ctx, "", "mr", "list", "-R", g.rflag(), "--source-branch", head, "--output", "json")
	if err != nil {
		return nil, err
	}
	var mrs []struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
		Draft  bool   `json:"draft"`
	}
	if err := json.Unmarshal([]byte(out), &mrs); err != nil || len(mrs) == 0 {
		return nil, err
	}
	return &PR{Number: mrs[0].IID, URL: mrs[0].WebURL, Draft: mrs[0].Draft}, nil
}

// CreatePR never passes --push/--fill: the branch is already pushed by the orchestrator.
func (g gitlabExec) CreatePR(ctx context.Context, o PROptions) (PR, error) {
	args := []string{"mr", "create", "-R", g.rflag(), "--source-branch", o.Head, "--target-branch", o.Base,
		"--title", o.Title, "--description-file", "-", "--yes", "--no-editor"}
	if o.Draft {
		args = append(args, "--draft")
	}
	out, err := g.run.Run(ctx, o.Body, args...)
	if err != nil {
		return PR{}, err
	}
	return prFromOutput(out, glMRURL, o.Draft)
}

func (g gitlabExec) Comment(ctx context.Context, n int, body string) error {
	_, err := g.run.Run(ctx, "", "issue", "note", strconv.Itoa(n), "-R", g.rflag(), "--message", body)
	return err
}

// Relabel relies on GitLab creating unknown labels when they are assigned.
func (g gitlabExec) Relabel(ctx context.Context, n int, add, remove []string) error {
	args := []string{"issue", "update", strconv.Itoa(n), "-R", g.rflag()}
	if len(add) > 0 {
		args = append(args, "--label", strings.Join(add, ","))
	}
	if len(remove) > 0 {
		args = append(args, "--unlabel", strings.Join(remove, ","))
	}
	_, err := g.run.Run(ctx, "", args...)
	return err
}
