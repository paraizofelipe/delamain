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
	cmd := &cobra.Command{
		Use:   "run --task <arquivo.md> [--repo <caminho>]",
		Short: "Executa uma task: plano, implementação TDD, verificação e revisão em loop",
		Long: `Executa uma task markdown ("# Título" + descrição com critérios de aceite) num repositório git.
Cria uma worktree isolada e um workspace herdr com planner, implementer, reviewer e checks.

Códigos de saída: 0 aprovado · 1 interrompido · 3 iterações esgotadas · 4 task sem informação suficiente.`,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			if repo == "" {
				if repo, err = os.Getwd(); err != nil {
					return err
				}
			}
			title, body, err := pipeline.ParseTaskFile(taskFile)
			if err != nil {
				return err
			}
			if kind == "" {
				kind = pipeline.InferKind(title, nil)
			}
			stem := strings.TrimSuffix(filepath.Base(taskFile), filepath.Ext(taskFile))
			task := pipeline.Task{Ref: "task:" + gitx.Slug(stem, 24), Title: title, Body: body, Kind: kind, Repo: repo}

			res, err := runTask(c.Context(), cfg, task)
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
	cmd.Flags().StringVar(&taskFile, "task", "", "arquivo markdown da task (obrigatório)")
	cmd.Flags().StringVar(&repo, "repo", "", "repositório alvo (padrão: diretório atual)")
	cmd.Flags().StringVar(&kind, "kind", "", "feature|bug (padrão: inferido do título)")
	_ = cmd.MarkFlagRequired("task")
	return cmd
}

func runTask(ctx context.Context, cfg config.Config, task pipeline.Task) (pipeline.Result, error) {
	st, err := store.Open(ctx, filepath.Join(cfg.Paths.StateDir, "delamain.db"))
	if err != nil {
		return pipeline.Result{}, err
	}
	defer func() { _ = st.Close() }()
	set, err := loadPrompts(cfg)
	if err != nil {
		return pipeline.Result{}, err
	}
	r := &pipeline.Runner{
		Cfg:           cfg,
		Herdr:         herdr.New(herdr.ExecRunner{Binary: cfg.Herdr.Binary, Session: cfg.Herdr.Session}),
		Git:           gitx.New(),
		Store:         st,
		Prompts:       set,
		PromptsTarget: promptsTarget(cfg),
		Log:           slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Nonce:         pipeline.RandomNonce,
	}
	return r.Run(ctx, task)
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
