package forge

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

type gitlab struct {
	repo Repo
	run  Runner
}

var glMRURL = regexp.MustCompile(`(https?://\S+/-/merge_requests/(\d+))`)

func (g gitlab) Repo() Repo { return g.repo }

// rflag is a full URL so self-hosted instances resolve without glab's host config.
func (g gitlab) rflag() string { return "https://" + g.repo.String() }

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

func (g gitlab) Issue(ctx context.Context, n int) (Issue, error) {
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
func (g gitlab) ListReady(ctx context.Context, label string) ([]Issue, error) {
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

func (g gitlab) OpenPR(ctx context.Context, head string) (*PR, error) {
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
func (g gitlab) CreatePR(ctx context.Context, o PROptions) (PR, error) {
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

func (g gitlab) Comment(ctx context.Context, n int, body string) error {
	_, err := g.run.Run(ctx, "", "issue", "note", strconv.Itoa(n), "-R", g.rflag(), "--message", body)
	return err
}

// Relabel relies on GitLab creating unknown labels when they are assigned.
func (g gitlab) Relabel(ctx context.Context, n int, add, remove []string) error {
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
