package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// Output formats of the inspection commands; table is for people, yaml/json for scripts.
var outputFormats = []string{"table", "yaml", "json"}

func addOutputFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVarP(dst, "output", "o", "table", "format: "+strings.Join(outputFormats, "|"))
	_ = cmd.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return outputFormats, cobra.ShellCompDirectiveNoFileComp
	})
}

// printAs writes v as yaml or json, or calls table for the human format.
func printAs(w io.Writer, format string, v any, table func(io.Writer) error) error {
	switch format {
	case "table":
		return table(w)
	case "yaml":
		enc := yaml.NewEncoder(w)
		enc.SetIndent(2)
		return enc.Encode(v)
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	return fmt.Errorf("formato %q inválido: use %s", format, strings.Join(outputFormats, "|"))
}

// RunView is a run as `ls` and `status` show it.
type RunView struct {
	ID         int64       `json:"id" yaml:"id"`
	Repo       string      `json:"repo" yaml:"repo"`
	Issue      string      `json:"issue" yaml:"issue"`
	Title      string      `json:"title" yaml:"title"`
	State      string      `json:"state" yaml:"state"`
	Iteration  int         `json:"iteration" yaml:"iteration"`
	Branch     string      `json:"branch,omitempty" yaml:"branch,omitempty"`
	Base       string      `json:"base,omitempty" yaml:"base,omitempty"`
	Worktree   string      `json:"worktree,omitempty" yaml:"worktree,omitempty"`
	Workspace  string      `json:"herdr_workspace,omitempty" yaml:"herdr_workspace,omitempty"`
	PR         string      `json:"pr,omitempty" yaml:"pr,omitempty"`
	Result     string      `json:"result,omitempty" yaml:"result,omitempty"`
	Owner      Process     `json:"owner" yaml:"owner"`
	Agents     []AgentView `json:"agents" yaml:"agents"`
	RunDir     string      `json:"run_dir" yaml:"run_dir"`
	CreatedAt  time.Time   `json:"created_at" yaml:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at" yaml:"updated_at"`
	terminal   bool
	iterations int
}

type Process struct {
	PID   int  `json:"pid" yaml:"pid"`
	Alive bool `json:"alive" yaml:"alive"`
}

type AgentView struct {
	Role       string     `json:"role" yaml:"role"`
	State      string     `json:"state" yaml:"state"`
	Process    Process    `json:"process" yaml:"process"`
	StartedAt  time.Time  `json:"started_at" yaml:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty" yaml:"ended_at,omitempty"`
	Tokens     int64      `json:"tokens" yaml:"tokens"` // budgeted: input+output+cacheWrite
	CostUSD    float64    `json:"cost_usd" yaml:"cost_usd"`
	Log        string     `json:"log" yaml:"log"`
	SessionDir string     `json:"session_dir" yaml:"session_dir"`
}

// RunStatus is everything `status` knows about one run.
type RunStatus struct {
	Run RunView `json:"run" yaml:"run"`
	// Worktrees are the repository's checkouts under paths.worktrees_dir/<repo>.
	Worktrees    []WorktreeView `json:"worktrees" yaml:"worktrees"`
	WorktreesDir string         `json:"worktrees_dir" yaml:"worktrees_dir"`
	// WorktreesError says why the worktrees could not be listed (e.g. the repo moved).
	WorktreesError string     `json:"worktrees_error,omitempty" yaml:"worktrees_error,omitempty"`
	Budget         BudgetView `json:"budget" yaml:"budget"`
}

type WorktreeView struct {
	gitx.Worktree `yaml:",inline"`
	ThisRun       bool `json:"this_run" yaml:"this_run"`
}

type BudgetView struct {
	Tokens   int64   `json:"tokens" yaml:"tokens"`
	CostUSD  float64 `json:"cost_usd" yaml:"cost_usd"`
	Summary  string  `json:"summary" yaml:"summary"`
	PerIssue float64 `json:"per_issue" yaml:"per_issue"`
	Mode     string  `json:"mode" yaml:"mode"`
}

// inspector reads runs from the store; other processes may be writing to it meanwhile.
type inspector struct {
	cfg   config.Config
	st    *store.Store
	alive func(pid int) bool
	git   interface {
		ListWorktrees(ctx context.Context, repo string) ([]gitx.Worktree, error)
	}
}

func openInspector(ctx context.Context, load func() (config.Config, error)) (*inspector, func(), error) {
	cfg, err := load()
	if err != nil {
		return nil, nil, err
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return &inspector{cfg: cfg, st: st, alive: pipeline.ProcessAlive, git: gitx.New()}, func() { _ = st.Close() }, nil
}

func (in *inspector) view(ctx context.Context, rec store.Run) (RunView, error) {
	v := RunView{
		ID: rec.ID, Repo: rec.Repo, Issue: rec.IssueRef, Title: rec.Title, State: string(rec.State),
		Iteration: rec.Iteration, Branch: rec.Branch, Base: rec.Base, Worktree: rec.Worktree, Workspace: rec.Workspace,
		PR: rec.MRURL, Owner: Process{PID: rec.OwnerPID, Alive: in.alive(rec.OwnerPID)},
		RunDir:    pipeline.RunDir(in.cfg.Paths.StateDir, rec.Repo, rec.ID),
		CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt, terminal: rec.State.Terminal(),
		iterations: in.cfg.Pipeline.MaxIterations, Agents: []AgentView{},
	}
	if v.terminal {
		v.Result = rec.Reason
	}
	agents, err := in.st.Agents(ctx, rec.ID)
	if err != nil {
		return v, err
	}
	usage, err := in.st.RoleUsage(ctx, rec.ID)
	if err != nil {
		return v, err
	}
	for _, a := range agents {
		av := AgentView{
			Role: a.Role, State: string(a.State), StartedAt: a.StartedAt, Log: a.LogPath, SessionDir: a.SessionDir,
			// A session only lives while its owner does: omp dies with the delamain process.
			Process: Process{PID: a.PID, Alive: a.State == store.AgentRunning && v.Owner.Alive && in.alive(a.PID)},
			Tokens:  usage[a.Role].BudgetTokens(), CostUSD: usage[a.Role].CostUSD,
		}
		if !a.EndedAt.IsZero() {
			end := a.EndedAt
			av.EndedAt = &end
		}
		v.Agents = append(v.Agents, av)
	}
	return v, nil
}

func (in *inspector) status(ctx context.Context, id int64) (RunStatus, error) {
	rec, err := in.st.GetRun(ctx, id)
	if err != nil {
		return RunStatus{}, fmt.Errorf("run %d: %w", id, err)
	}
	v, err := in.view(ctx, rec)
	if err != nil {
		return RunStatus{}, err
	}
	s := RunStatus{Run: v, WorktreesDir: filepath.Join(in.cfg.Paths.WorktreesDir, filepath.Base(rec.Repo)), Worktrees: []WorktreeView{}}
	wts, err := in.git.ListWorktrees(ctx, rec.Repo)
	if err != nil {
		s.WorktreesError = err.Error()
	}
	for _, w := range wts {
		if within(s.WorktreesDir, w.Path) {
			s.Worktrees = append(s.Worktrees, WorktreeView{Worktree: w, ThisRun: samePath(w.Path, rec.Worktree)})
		}
	}
	total, err := in.st.RunUsage(ctx, rec.ID)
	if err != nil {
		return s, err
	}
	b := budget.Status{Mode: in.cfg.Budget.Mode, Usage: total, PerIssue: in.cfg.Budget.PerIssue, Run: float64(total.BudgetTokens())}
	if b.Mode == config.BudgetUSD {
		b.Run = total.CostUSD
	}
	s.Budget = BudgetView{Tokens: total.BudgetTokens(), CostUSD: total.CostUSD, Summary: b.Summary(), PerIssue: b.PerIssue, Mode: string(b.Mode)}
	return s, nil
}

// within reports whether path is inside dir, following symlinks such as /tmp on macOS.
func within(dir, path string) bool {
	rel, err := filepath.Rel(resolve(dir), resolve(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool { return b != "" && resolve(a) == resolve(b) }

func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func newListCmd(load func() (config.Config, error)) *cobra.Command {
	var output string
	var all bool
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List active runs (--all includes finished runs)",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			in, done, err := openInspector(ctx, load)
			if err != nil {
				return err
			}
			defer done()
			recs, err := in.st.UnfinishedRuns(ctx)
			if all {
				recs, err = in.st.RecentRuns(ctx, limit)
			}
			if err != nil {
				return err
			}
			views := []RunView{}
			for _, rec := range recs {
				v, err := in.view(ctx, rec)
				if err != nil {
					return err
				}
				views = append(views, v)
			}
			return printAs(c.OutOrStdout(), output, views, func(w io.Writer) error { return listTable(w, views, all) })
		},
	}
	addOutputFlag(cmd, &output)
	cmd.Flags().BoolVar(&all, "all", false, "include finished runs")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum runs with --all")
	return cmd
}

func listTable(w io.Writer, views []RunView, all bool) error {
	if len(views) == 0 {
		msg := "no active runs"
		if all {
			msg = "no runs"
		}
		_, err := fmt.Fprintln(w, msg)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tREPO\tISSUE\tSTATE\tITER\tBRANCH\tAGENTS\tOWNER\tUPDATED")
	for _, v := range views {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			v.ID, filepath.Base(v.Repo), v.Issue, v.State, v.iter(), orDash(v.Branch), runningAgents(v.Agents), v.owner(), ago(v.UpdatedAt))
	}
	return tw.Flush()
}

func newStatusCmd(load func() (config.Config, error)) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "status RUN_ID",
		Short: "Show a run: repository, worktrees, active agents and usage",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			id, err := runID(args[0])
			if err != nil {
				return err
			}
			ctx := c.Context()
			in, done, err := openInspector(ctx, load)
			if err != nil {
				return err
			}
			defer done()
			s, err := in.status(ctx, id)
			if err != nil {
				return err
			}
			return printAs(c.OutOrStdout(), output, s, func(w io.Writer) error { return statusTable(w, s) })
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func runID(arg string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid RUN_ID %q: use the number shown by `delamain ls`", arg)
	}
	return id, nil
}

func statusTable(w io.Writer, s RunStatus) error {
	v := s.Run
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "RUN\t%d · %s %s\n", v.ID, v.Issue, v.Title)
	fmt.Fprintf(tw, "REPO\t%s\n", v.Repo)
	fmt.Fprintf(tw, "STATE\t%s (iteration %s)\n", v.State, v.iter())
	if v.Branch != "" {
		fmt.Fprintf(tw, "BRANCH\t%s (base %s)\n", v.Branch, v.Base)
		fmt.Fprintf(tw, "WORKTREE\t%s\n", v.Worktree)
	}
	fmt.Fprintf(tw, "OWNER\t%s\n", v.owner())
	fmt.Fprintf(tw, "BUDGET\t%s\n", s.Budget.Summary)
	if v.PR != "" {
		fmt.Fprintf(tw, "PR/MR\t%s\n", v.PR)
	}
	if v.Workspace != "" {
		fmt.Fprintf(tw, "HERDR\t%s\n", v.Workspace)
	}
	if v.Result != "" {
		fmt.Fprintf(tw, "RESULT\t%s\n", v.Result)
	}
	fmt.Fprintf(tw, "LOGS\t%s (delamain logs %d -f)\n", filepath.Join(v.RunDir, "logs"), v.ID)
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\nWORKTREES in %s\n", s.WorktreesDir)
	if s.WorktreesError != "" {
		fmt.Fprintf(w, "  not listed: %s\n", s.WorktreesError)
	} else if len(s.Worktrees) == 0 {
		fmt.Fprintln(w, "  none")
	} else {
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "\tBRANCH\tHEAD\tPATH")
		for _, wt := range s.Worktrees {
			mark := ""
			if wt.ThisRun {
				mark = "*"
			}
			branch := wt.Branch
			if wt.Detached {
				branch = "(detached)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mark, branch, short(wt.Head), wt.Path)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	fmt.Fprintln(w, "\nAGENTS")
	if len(v.Agents) == 0 {
		_, err := fmt.Fprintln(w, "  none started")
		return err
	}
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tSTATE\tPID\tALIVE\tDURATION\tTOKENS\tUS$")
	for _, a := range v.Agents {
		end := time.Now()
		if a.EndedAt != nil {
			end = *a.EndedAt
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%.2f\n", a.Role, a.State, a.Process.PID, yesNo(a.Process.Alive),
			end.Sub(a.StartedAt).Round(time.Second), budget.Format(float64(a.Tokens), config.BudgetTokens), a.CostUSD)
	}
	return tw.Flush()
}

func (v RunView) iter() string {
	if v.Iteration == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", v.Iteration, v.iterations)
}

// owner tells whether the process driving the run is alive; a dead owner of an active run
// means the run waits for `delamain recover`.
func (v RunView) owner() string {
	switch {
	case v.Owner.PID == 0:
		return "-"
	case v.terminal:
		return fmt.Sprintf("pid %d (finished)", v.Owner.PID)
	case v.Owner.Alive:
		return fmt.Sprintf("pid %d (alive)", v.Owner.PID)
	}
	return fmt.Sprintf("pid %d (dead: run delamain recover)", v.Owner.PID)
}

func runningAgents(as []AgentView) string {
	var roles []string
	for _, a := range as {
		if a.Process.Alive {
			roles = append(roles, a.Role)
		}
	}
	if len(roles) == 0 {
		return "-"
	}
	return strings.Join(roles, ",")
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
