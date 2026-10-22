package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
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
	return fmt.Errorf("invalid format %q: use %s", format, strings.Join(outputFormats, "|"))
}

// RunView is a run as `ls` and `status` show it.
type RunView struct {
	ID            int64       `json:"id" yaml:"id"`
	Kind          string      `json:"kind" yaml:"kind"`
	Repo          string      `json:"repo" yaml:"repo"`
	Issue         string      `json:"issue" yaml:"issue"`
	Title         string      `json:"title" yaml:"title"`
	State         string      `json:"state" yaml:"state"`
	Iteration     int         `json:"iteration" yaml:"iteration"`
	Harness       string      `json:"harness" yaml:"harness"`
	MaxIterations int         `json:"max_iterations" yaml:"max_iterations"` // as recorded: 0 for runs from before it was
	Complexity    string      `json:"complexity,omitempty" yaml:"complexity,omitempty"`
	Branch        string      `json:"branch,omitempty" yaml:"branch,omitempty"`
	Base          string      `json:"base,omitempty" yaml:"base,omitempty"`
	Worktree      string      `json:"worktree,omitempty" yaml:"worktree,omitempty"`
	Workspace     string      `json:"herdr_workspace,omitempty" yaml:"herdr_workspace,omitempty"`
	PR            string      `json:"pr,omitempty" yaml:"pr,omitempty"`
	Result        string      `json:"result,omitempty" yaml:"result,omitempty"`
	Owner         Process     `json:"owner" yaml:"owner"`
	Agents        []AgentView `json:"agents" yaml:"agents"`
	RunDir        string      `json:"run_dir" yaml:"run_dir"`
	CreatedAt     time.Time   `json:"created_at" yaml:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at" yaml:"updated_at"`
	terminal      bool
	iterations    int // effective maximum: the recorded one, or the config's for older runs
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
		ID: rec.ID, Kind: string(rec.Kind), Repo: rec.Repo, Issue: rec.IssueRef, Title: rec.Title, State: string(rec.State),
		Iteration: rec.Iteration, Branch: rec.Branch, Base: rec.Base, Worktree: rec.Worktree, Workspace: rec.Workspace,
		PR: rec.MRURL, Owner: Process{PID: rec.OwnerPID, Alive: in.alive(rec.OwnerPID)},
		RunDir:    pipeline.RunDir(in.cfg.Paths.StateDir, rec.Repo, rec.ID),
		CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt, terminal: rec.State.Terminal(),
		Harness: rec.Harness, MaxIterations: rec.MaxIterations, Complexity: rec.Complexity, Agents: []AgentView{},
	}
	v.iterations, _ = rec.Limits(in.cfg.Pipeline.MaxIterations, in.cfg.Budget.PerIssue)
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
	_, perIssue := rec.Limits(in.cfg.Pipeline.MaxIterations, in.cfg.Budget.PerIssue)
	b := budget.Status{Mode: in.cfg.Budget.Mode, Usage: total, PerIssue: perIssue, Run: float64(total.BudgetTokens())}
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
		Short:   "List active runs (--all includes finished ones)",
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
	return renderList(w, views, all, newStyles(lipgloss.NewRenderer(w)))
}

func renderList(w io.Writer, views []RunView, all bool, st styles) error {
	if len(views) == 0 {
		msg := "no active run"
		if all {
			msg = "no run"
		}
		_, err := fmt.Fprintln(w, msg)
		return err
	}
	rows := [][]cell{{}}
	for _, h := range []string{"RUN", "KIND", "REPO", "ISSUE", "STATE", "ITER", "COMPLEXITY", "BRANCH", "AGENTS", "OWNER", "UPDATED"} {
		rows[0] = append(rows[0], styled(st.header, h))
	}
	for _, v := range views {
		rows = append(rows, []cell{plain(strconv.FormatInt(v.ID, 10)), plain(v.Kind), plain(filepath.Base(v.Repo)), plain(v.Issue), plain(v.State),
			plain(v.iter()), plain(v.complexity()), plain(orDash(v.Branch)), plain(runningAgents(v.Agents)), plain(v.owner()), plain(ago(v.UpdatedAt))})
	}
	return writeTable(w, rows)
}

// complexity is the run's level, "wait" until the plan classifies it, or "-" for a run that
// ended unclassified.
func (v RunView) complexity() string {
	switch {
	case v.Complexity != "":
		return v.Complexity
	case v.terminal:
		return "-"
	}
	return "wait"
}

func newStatusCmd(load func() (config.Config, error)) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "status RUN_ID",
		Short: "Show a run: repository, worktrees, running agents and usage",
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
	return renderStatus(w, s, newStyles(lipgloss.NewRenderer(w)))
}

func renderStatus(w io.Writer, s RunStatus, st styles) error {
	v := s.Run
	field := func(label string, value cell) []cell { return []cell{styled(st.label, label), value} }
	rows := [][]cell{
		field("RUN", join(plain(fmt.Sprintf("%d · ", v.ID)), styled(st.info, v.Issue), plain(" "+v.Title))),
		field("KIND", plain(v.Kind)),
		field("REPO", plain(v.Repo)),
		field("HARNESS", plain(orDash(v.Harness))),
		field("STATE", styled(st.runStateStyle(v.State), v.State)),
		field("INTERACTIONS", plain(v.iter())),
		field("COMPLEXITY", styled(st.complexityStyle(v.Complexity), orDash(v.Complexity))),
	}
	if v.Branch != "" {
		rows = append(rows,
			field("BRANCH", plain(fmt.Sprintf("%s (base %s)", v.Branch, v.Base))),
			field("WORKTREE", plain(v.Worktree)))
	}
	rows = append(rows, field("OWNER", plain(v.owner())), field("BUDGET", plain(s.Budget.Summary)))
	if v.PR != "" {
		rows = append(rows, field("PR/MR", styled(st.info.Underline(true), v.PR)))
	}
	if v.Workspace != "" {
		rows = append(rows, field("HERDR", plain(v.Workspace)))
	}
	if v.Result != "" {
		outcome, reason, found := strings.Cut(v.Result, ":")
		if found {
			reason = ":" + reason
		}
		rows = append(rows, field("RESULT", join(styled(st.resultStyle(outcome), outcome), plain(reason))))
	}
	rows = append(rows, field("LOGS", join(plain(filepath.Join(v.RunDir, "logs")+" "), styled(st.muted, fmt.Sprintf("(delamain logs %d -f)", v.ID)))))
	if err := writeTable(w, rows); err != nil {
		return err
	}

	fmt.Fprintf(w, "\n%s in %s\n", st.heading.Render("WORKTREES"), s.WorktreesDir)
	if s.WorktreesError != "" {
		fmt.Fprintf(w, "  not listed: %s\n", s.WorktreesError)
	} else if len(s.Worktrees) == 0 {
		fmt.Fprintln(w, "  none")
	} else {
		rows = [][]cell{{plain(""), styled(st.header, "BRANCH"), styled(st.header, "HEAD"), styled(st.header, "PATH")}}
		for _, wt := range s.Worktrees {
			mark, look := "", st.none
			if wt.ThisRun {
				mark, look = "*", st.info.Bold(true)
			}
			branch := wt.Branch
			if wt.Detached {
				branch = "(detached)"
			}
			rows = append(rows, []cell{styled(look, mark), styled(look, branch), plain(short(wt.Head)), plain(wt.Path)})
		}
		if err := writeTable(w, rows); err != nil {
			return err
		}
	}

	fmt.Fprintf(w, "\n%s\n", st.heading.Render("AGENTS"))
	if len(v.Agents) == 0 {
		_, err := fmt.Fprintln(w, "  none started")
		return err
	}
	rows = [][]cell{{}}
	for _, h := range []string{"ROLE", "STATE", "PID", "ALIVE", "DURATION", "TOKENS", "US$"} {
		rows[0] = append(rows[0], styled(st.header, h))
	}
	for _, a := range v.Agents {
		end := time.Now()
		if a.EndedAt != nil {
			end = *a.EndedAt
		}
		alive := styled(st.muted, yesNo(false))
		if a.Process.Alive {
			alive = styled(st.ok, yesNo(true))
		}
		rows = append(rows, []cell{plain(a.Role), styled(st.agentStateStyle(a.State), a.State), plain(strconv.Itoa(a.Process.PID)), alive,
			plain(end.Sub(a.StartedAt).Round(time.Second).String()), plain(budget.Format(float64(a.Tokens), config.BudgetTokens)), plain(fmt.Sprintf("%.2f", a.CostUSD))})
	}
	return writeTable(w, rows)
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
