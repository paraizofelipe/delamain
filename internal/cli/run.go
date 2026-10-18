package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/herdr"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// ExitError carries a process exit code for outcomes that are not crashes.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

// Exit codes of `delamain run`, so scripts can branch on the outcome.
var outcomeExit = map[pipeline.Outcome]int{
	pipeline.OutcomeApproved:  0,
	pipeline.OutcomeHalted:    1,
	pipeline.OutcomeExhausted: 3,
	pipeline.OutcomeNeedsInfo: 4,
}

func newRunCmd(load func() (config.Config, error)) *cobra.Command {
	var taskFile, repo, kind string
	var issue int
	cmd := &cobra.Command{
		Use:   "run (--task <arquivo.md> | --issue <n>) [--repo <caminho>]",
		Short: "Executa uma task ou issue: plano, implementação TDD, verificação, revisão e PR/MR",
		Long: `Executa uma task markdown ("# Título" + descrição com critérios de aceite) ou uma issue do
GitHub/GitLab num repositório git. Cria uma worktree isolada e um workspace herdr com planner,
implementer, reviewer e checks. No fim faz push da branch e abre o PR/MR (draft quando não
aprovado). Nunca faz merge.

Códigos de saída: 0 aprovado · 1 interrompido · 3 iterações esgotadas · 4 task sem informação suficiente.`,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			if (taskFile == "") == (issue == 0) {
				return errors.New("informe exatamente um de --task ou --issue")
			}
			if repo == "" {
				if repo, err = os.Getwd(); err != nil {
					return err
				}
			}
			fg, err := forgeFor(c.Context(), cfg, repo)
			if err != nil {
				return err
			}
			var task pipeline.Task
			if issue > 0 {
				if task, err = issueTask(c.Context(), fg, issue, repo); err != nil {
					return err
				}
			} else if task, err = fileTask(taskFile, repo); err != nil {
				return err
			}
			if kind != "" {
				task.Kind = kind
			}

			res, err := runTask(c.Context(), cfg, task, fg)
			if err != nil {
				return err
			}
			printResult(c, res)
			if code := outcomeExit[res.Outcome]; code != 0 {
				return ExitError{Code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&taskFile, "task", "", "arquivo markdown da task")
	cmd.Flags().IntVar(&issue, "issue", 0, "número da issue no forge do origin")
	cmd.Flags().StringVar(&repo, "repo", "", "repositório alvo (padrão: diretório atual)")
	cmd.Flags().StringVar(&kind, "kind", "", "feature|bug (padrão: inferido do título)")
	return cmd
}

func fileTask(path, repo string) (pipeline.Task, error) {
	title, body, err := pipeline.ParseTaskFile(path)
	if err != nil {
		return pipeline.Task{}, err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return pipeline.Task{Ref: "task:" + gitx.Slug(stem, 24), Title: title, Body: body, Kind: pipeline.InferKind(title, nil), Repo: repo}, nil
}

func issueTask(ctx context.Context, fg forge.Forge, n int, repo string) (pipeline.Task, error) {
	if fg == nil {
		return pipeline.Task{}, errors.New("--issue exige um repositório com origin no GitHub ou GitLab")
	}
	is, err := fg.Issue(ctx, n)
	if err != nil {
		return pipeline.Task{}, err
	}
	if !is.Open {
		return pipeline.Task{}, fmt.Errorf("a issue #%d está fechada", n)
	}
	return pipeline.TaskFromIssue(is, repo), nil
}

func runTask(ctx context.Context, cfg config.Config, task pipeline.Task, fg forge.Forge) (pipeline.Result, error) {
	st, err := openStore(ctx, cfg)
	if err != nil {
		return pipeline.Result{}, err
	}
	defer func() { _ = st.Close() }()
	r, err := newRunner(cfg, st)
	if err != nil {
		return pipeline.Result{}, err
	}
	r.Forge = fg
	return r.Run(ctx, task)
}

// newRunner wires the real herdr, git and prompts; the caller sets Forge per repository.
func newRunner(cfg config.Config, st *store.Store) (*pipeline.Runner, error) {
	set, err := loadPrompts(cfg)
	if err != nil {
		return nil, err
	}
	return &pipeline.Runner{
		Cfg:           cfg,
		Herdr:         herdr.New(herdr.ExecRunner{Binary: cfg.Herdr.Binary, Session: cfg.Herdr.Session}),
		Agents:        pipeline.RPCAgents{Binary: cfg.Omp.Binary},
		Git:           gitx.New(),
		Store:         st,
		Prompts:       set,
		PromptsTarget: promptsTarget(cfg),
		Log:           slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Nonce:         pipeline.RandomNonce,
	}, nil
}

func openStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	return store.Open(ctx, filepath.Join(cfg.Paths.StateDir, "delamain.db"))
}

func printResult(c *cobra.Command, res pipeline.Result) {
	w := c.OutOrStdout()
	icon := map[pipeline.Outcome]string{
		pipeline.OutcomeApproved: "✅", pipeline.OutcomeExhausted: "⚠️ ", pipeline.OutcomeHalted: "❌", pipeline.OutcomeNeedsInfo: "❓",
	}[res.Outcome]
	fmt.Fprintf(w, "%s %s", icon, res.Outcome)
	if res.Reason != "" {
		fmt.Fprintf(w, " — %s", res.Reason)
	}
	fmt.Fprintln(w)
	if res.Branch != "" {
		fmt.Fprintf(w, "   branch:    %s (base %s)\n   worktree:  %s\n   iterações: %d · commits: %d\n", res.Branch, res.Base, res.Worktree, res.Iterations, len(res.Commits))
	}
	if res.PRURL != "" {
		fmt.Fprintf(w, "   PR/MR:     %s%s\n", res.PRURL, map[bool]string{true: " (draft)"}[res.Draft])
	}
	if res.Delivery != "" {
		fmt.Fprintf(w, "   entrega:   %s\n", res.Delivery)
	}
	if res.Budget != "" {
		fmt.Fprintf(w, "   consumo:   %s\n", res.Budget)
	}
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "   ⚠️  %s\n", warn)
	}
	if res.ReportPath != "" {
		fmt.Fprintf(w, "   relatório: %s\n", res.ReportPath)
	}
}

// IsExit unwraps an ExitError code.
func IsExit(err error) (int, bool) {
	var e ExitError
	if errors.As(err, &e) {
		return e.Code, true
	}
	return 0, false
}
