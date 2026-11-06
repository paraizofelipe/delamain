package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

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
		log.Warn("decision skipped", append([]any{"point", point, "reason", err}, attrs...)...)
		return decide.Result{}, false
	}
	args := append([]any{
		"point", point, "mode", p.Mode, "model", res.Model, "cost_usd", res.CostUSD,
		"latency_ms", res.Latency.Milliseconds(),
	}, attrs...)
	for _, name := range slices.Sorted(maps.Keys(res.Answers)) {
		a := res.Answers[name]
		if _, noul := req.Nouls[name]; noul {
			args = append(args, "noul."+name, a.Noul)
			continue
		}
		args = append(args, "choice."+name, a.Choice, "confidence."+name, a.Confidence)
	}
	log.Info("decision", args...)
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
	attrs := []any{"repo", t.Repo, "ref", t.Ref}
	res, ok := r.ask(ctx, r.Log, "triage", p, req, attrs...)
	if !ok {
		return ""
	}
	a, answered := res.Answers["actionable"]
	if !answered {
		r.Log.Warn("decision skipped", append([]any{"point", "triage", "reason", "no answer to actionable"}, attrs...)...)
		return ""
	}
	if p.Mode != config.DecisionOn || a.Noul >= p.Threshold {
		return ""
	}
	return fmt.Sprintf("triagem automática: a issue parece não ter objetivo e critérios de aceite verificáveis (p=%.2f); detalhe o escopo e os critérios de aceite", a.Noul)
}
