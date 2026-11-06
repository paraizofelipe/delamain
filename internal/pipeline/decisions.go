package pipeline

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/decide"
)

// ask consults the decision model for point. ok is false when the point is off, decisions are
// disabled, or the call failed; failures are logged and never stop the run.
func (r *Runner) ask(ctx context.Context, log *slog.Logger, point string, p config.DecisionPoint, req decide.Request, attrs ...any) (decide.Result, bool) {
	if !r.Cfg.Decisions.Enabled || p.Mode == config.DecisionOff || r.Decider == nil {
		return decide.Result{}, false
	}
	res, err := r.Decider.Decide(ctx, req)
	if err != nil {
		return decide.Result{}, false
	}
	return res, true
}

// triage asks whether t is concrete enough for an agent to do; it returns the problem that keeps
// t from starting, or "" when the run may go on.
func (r *Runner) triage(ctx context.Context, t Task) string {
	p := r.Cfg.Decisions.Triage
	req := decide.Request{
		State: map[string]any{"title": t.Title, "kind": t.Kind, "body": t.Body},
		Nouls: map[string]decide.Noul{"actionable": {
			Instructions: "A issue descreve um objetivo concreto e critérios de aceite que um agente consegue cumprir e verificar sem decidir o escopo sozinho?",
			True:         "Objetivo claro, escopo delimitado (arquivos ou comportamento) e critérios de aceite verificáveis.",
			False:        "Ideia vaga, escopo aberto, critérios ausentes ou que dependem de decisão humana.",
		}},
	}
	res, ok := r.ask(ctx, r.Log, "triage", p, req, "repo", t.Repo, "ref", t.Ref)
	if !ok || p.Mode != config.DecisionOn {
		return ""
	}
	a := res.Answers["actionable"]
	if a.Noul >= p.Threshold {
		return ""
	}
	return fmt.Sprintf("triagem automática: a issue parece não ter objetivo e critérios de aceite verificáveis (p=%.2f); detalhe o escopo e os critérios de aceite", a.Noul)
}
