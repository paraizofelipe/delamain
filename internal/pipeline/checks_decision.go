package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"

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

// checkTailBytes keeps the end of each tail sent to the checks decision, where a failure explains itself.
const checkTailBytes = 6000

// checkStateBytes is decide's limit on the encoded state; the checks state must stay under it.
const checkStateBytes = 32000

// clipTail keeps at most the last n bytes of s without splitting a rune.
func clipTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// checkDecisionState is the state of the checks decision: the failed checks, in order. Each tail
// keeps its last checkTailBytes, and all tails shrink proportionally, from their start, until the
// JSON — measured encoded, since json.Marshal escapes <, > and & — fits under checkStateBytes.
func checkDecisionState(failed []CheckResult) map[string]any {
	items := make([]checkState, len(failed))
	for i, c := range failed {
		items[i] = checkState{Name: c.Name, Command: c.Command, ExitCode: c.ExitCode, Tail: clipTail(c.Tail, checkTailBytes)}
	}
	state := map[string]any{"checks": items}
	for {
		b, err := json.Marshal(state)
		if err != nil || len(b) < checkStateBytes {
			return state
		}
		shrunk := false
		for i := range items {
			if n := len(items[i].Tail) * (checkStateBytes - 1) / len(b); n < len(items[i].Tail) {
				items[i].Tail, shrunk = clipTail(items[i].Tail, n), true
			}
		}
		if !shrunk {
			return state
		}
	}
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
