package forge

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

type github struct {
	repo Repo
	run  Runner
}

var ghPRURL = regexp.MustCompile(`(https?://\S+/pull/(\d+))`)

func (g github) Repo() Repo { return g.repo }

// rflag is the -R value: OWNER/REPO on github.com, HOST/OWNER/REPO on Enterprise.
func (g github) rflag() string {
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

func (g github) Issue(ctx context.Context, n int) (Issue, error) {
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

func (g github) ListReady(ctx context.Context, label string) ([]Issue, error) {
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

func (g github) OpenPR(ctx context.Context, head string) (*PR, error) {
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

func (g github) CreatePR(ctx context.Context, o PROptions) (PR, error) {
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

func (g github) Comment(ctx context.Context, n int, body string) error {
	_, err := g.run.Run(ctx, body, "issue", "comment", strconv.Itoa(n), "-R", g.rflag(), "--body-file", "-")
	return err
}

// Relabel creates missing labels first: GitHub rejects unknown labels on edit.
func (g github) Relabel(ctx context.Context, n int, add, remove []string) error {
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
