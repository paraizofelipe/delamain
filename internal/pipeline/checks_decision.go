package pipeline

import (
	"context"
	"fmt"

	"github.com/paraizo/delamain/internal/decide"
)

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
// once more before the iteration goes back to the implementer.
func (r *Runner) retryChecks(ctx context.Context, ru *run, iter int, cs []CheckResult) bool {
	var failed []CheckResult
	for _, c := range cs {
		if !c.Passed() && !c.Skipped {
			failed = append(failed, c)
		}
	}
	if len(failed) == 0 {
		return false
	}
	req := decide.Request{State: checkDecisionState(failed), Choices: map[string]decide.Choice{}}
	for i, c := range failed {
		req.Choices[fmt.Sprintf("check_%d", i)] = checkCause(c.Name)
	}
	attrs := []any{"run", ru.rec.ID, "iteration", iter, "checks", failedChecks(cs)}
	_, ok := r.ask(ctx, ru.log, "checks", r.Cfg.Decisions.Checks, req, attrs...)
	return ok
}
