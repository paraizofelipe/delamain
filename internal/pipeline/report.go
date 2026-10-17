package pipeline

import (
	"fmt"
	"strings"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/prompts"
)

func reviewFeedback(r artifacts.Review) string {
	var b strings.Builder
	for _, a := range r.Axes {
		for _, f := range a.Findings {
			if !f.Blocking() {
				continue
			}
			fmt.Fprintf(&b, "- [%s · %s · %s] %s:%d — %s\n  regra: %s\n  evidência: %s\n",
				f.Severity, a.Axis, f.Category, f.File, f.Line, f.Message, f.Rule, f.Evidence)
		}
	}
	return strings.TrimSpace(b.String())
}

func renderReport(res Result, task prompts.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# delamain run %d — %s %s\n\n", res.RunID, task.Ref, task.Title)
	fmt.Fprintf(&b, "| Campo | Valor |\n|---|---|\n")
	fmt.Fprintf(&b, "| Resultado | **%s** |\n", res.Outcome)
	if res.Reason != "" {
		fmt.Fprintf(&b, "| Motivo | %s |\n", strings.ReplaceAll(res.Reason, "|", `\|`))
	}
	fmt.Fprintf(&b, "| Branch | `%s` (base `%s`) |\n| Worktree | `%s` |\n| Iterações | %d |\n\n", res.Branch, res.Base, res.Worktree, res.Iterations)

	if len(res.Commits) > 0 {
		b.WriteString("## Commits\n\n")
		for _, c := range res.Commits {
			fmt.Fprintf(&b, "- `%s`\n", c)
		}
		b.WriteString("\n")
	}
	for _, it := range res.History {
		fmt.Fprintf(&b, "## Iteração %d\n\n", it.N)
		if it.Feedback != "" {
			b.WriteString("Feedback recebido da iteração anterior: sim\n\n")
		}
		b.WriteString("| Gate | Comando | Resultado |\n|---|---|---|\n")
		for _, c := range it.Checks {
			state := "✅"
			if !c.Passed() {
				state = fmt.Sprintf("❌ (%d)", c.ExitCode)
			}
			fmt.Fprintf(&b, "| %s | `%s` | %s |\n", c.Name, c.Command, state)
		}
		if it.Review != nil {
			fmt.Fprintf(&b, "\nRevisão: **%s** — %d achados bloqueantes\n", it.Review.EffectiveVerdict(), len(it.Review.Blocking()))
			for _, a := range it.Review.Axes {
				fmt.Fprintf(&b, "- %s: %d achados\n", a.Axis, len(a.Findings))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
