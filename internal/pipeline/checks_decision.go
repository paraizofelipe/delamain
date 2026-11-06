package pipeline

import (
	"context"
	"fmt"
	"slices"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/decide"
)

// checkCommands are the checks commandChecks runs: the only ones whose failure may be classified.
// A failed git gate, matriz-criterios, segredos or prova-vermelho never asks nor retries.
var checkCommands = []string{"format", "lint", "typecheck", "test"}

// checkState is one failed check as the checks decision reads it.
type checkState struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Tail     string `json:"tail"`
}

// checkDecisionState is the state of the checks decision: the failed checks, in order.
func checkDecisionState(failed []CheckResult) map[string]any {
	items := make([]checkState, len(failed))
	for i, c := range failed {
		items[i] = checkState{Name: c.Name, Command: c.Command, ExitCode: c.ExitCode, Tail: c.Tail}
	}
	return map[string]any{"checks": items}
}

// checkCause asks the cause of the failure of the check name.
func checkCause(name string) decide.Choice {
	return decide.Choice{
		Instructions: "Qual a causa mais provável desta falha do check " + name + "?",
		Options: map[string]string{
			"teste_real":  "O código mudou e um teste ou asserção falhou por causa da mudança.",
			"lint_format": "Falha de lint, formatação, typecheck ou compilação.",
			"flaky":       "Falha intermitente: timeout de teste, corrida, dependência de ordem, porta ocupada; o mesmo código passaria rodando de novo.",
			"infra":       "Problema do ambiente: disco, memória, rede, serviço externo indisponível ou ferramenta ausente.",
		},
	}
}

// retryChecks asks the checks point the cause of each failure in cs; true means verify may run
// once more before the iteration goes back to the implementer. Shadow only logs the decision.
func (r *Runner) retryChecks(ctx context.Context, ru *run, iter int, cs []CheckResult) bool {
	var failed []CheckResult
	for _, c := range cs {
		if c.Passed() || c.Skipped {
			continue
		}
		if !slices.Contains(checkCommands, c.Name) {
			return false
		}
		failed = append(failed, c)
	}
	if len(failed) == 0 {
		return false
	}
	req := decide.Request{State: checkDecisionState(failed), Choices: map[string]decide.Choice{}}
	for i, c := range failed {
		req.Choices[fmt.Sprintf("check_%d", i)] = checkCause(c.Name)
	}
	attrs := []any{"run", ru.rec.ID, "iteration", iter, "checks", failedChecks(cs)}
	p := r.Cfg.Decisions.Checks
	res, ok := r.ask(ctx, ru.log, "checks", p, req, attrs...)
	if !ok {
		return false
	}
	retry := true
	for i := range failed {
		key := fmt.Sprintf("check_%d", i)
		a, answered := res.Answers[key]
		if !answered {
			ru.log.Warn("decision skipped", append([]any{"point", "checks", "reason", "no answer to " + key}, attrs...)...)
			return false
		}
		if a.Probabilities["flaky"]+a.Probabilities["infra"] < p.Threshold {
			retry = false
		}
	}
	return retry && p.Mode == config.DecisionOn
}
