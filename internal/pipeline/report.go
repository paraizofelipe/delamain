package pipeline

import (
	"fmt"
	"strings"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/forge"
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
	fmt.Fprintf(&b, "| Branch | `%s` (base `%s`) |\n| Worktree | `%s` |\n| Iterações | %d |\n", res.Branch, res.Base, res.Worktree, res.Iterations)
	if res.Complexity != "" {
		fmt.Fprintf(&b, "| Complexidade | %s |\n", res.Complexity)
	}
	if res.PRURL != "" {
		fmt.Fprintf(&b, "| PR/MR | %s%s |\n", res.PRURL, map[bool]string{true: " (draft)"}[res.Draft])
	}
	if res.Delivery != "" {
		fmt.Fprintf(&b, "| Entrega | %s |\n", res.Delivery)
	}
	if res.Budget != "" {
		fmt.Fprintf(&b, "| Budget | %s |\n", res.Budget)
	}
	if len(res.CommandSources) > 0 {
		var parts []string
		for _, k := range []string{"format_check", "lint", "typecheck", "test"} {
			if src, ok := res.CommandSources[k]; ok {
				parts = append(parts, k+": "+src)
			}
		}
		fmt.Fprintf(&b, "| Origem dos comandos | %s |\n", strings.Join(parts, " · "))
	}
	b.WriteString("\n")
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "> ⚠️ %s\n", w)
	}

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
			if c.Skipped {
				state = "⏭️ não executado"
			} else if !c.Passed() {
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

func renderReviewReport(res Result, p *prRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# delamain review %d — PR/MR #%d %s\n\n", res.RunID, p.pr.Number, p.pr.Title)
	fmt.Fprintf(&b, "| Campo | Valor |\n|---|---|\n")
	fmt.Fprintf(&b, "| Resultado | **%s** |\n", res.Outcome)
	if res.Reason != "" {
		fmt.Fprintf(&b, "| Motivo | %s |\n", strings.ReplaceAll(res.Reason, "|", `\|`))
	}
	fmt.Fprintf(&b, "| PR/MR | %s |\n| Head | `%s` (base `%s` em `%s`) |\n", p.pr.URL, p.pr.HeadSHA, p.pr.BaseRef, p.pr.BaseSHA)
	var cmds []string
	for _, c := range [][2]string{{"format_check", p.cmds.FormatCheck}, {"lint", p.cmds.Lint}, {"typecheck", p.cmds.Typecheck}, {"test", p.cmds.Test}} {
		if c[1] != "" {
			cmds = append(cmds, fmt.Sprintf("%s: `%s`", c[0], strings.ReplaceAll(c[1], "|", `\|`)))
		}
	}
	if len(cmds) > 0 {
		fmt.Fprintf(&b, "| Comandos da base (não executados) | %s |\n", strings.Join(cmds, " · "))
	}
	if res.Review != nil {
		fmt.Fprintf(&b, "| Postado | %d inline · %d no resumo · %d descartados |\n", res.Review.Inline, res.Review.InSummary, res.Review.Dropped)
	}
	if res.Budget != "" {
		fmt.Fprintf(&b, "| Budget | %s |\n", res.Budget)
	}
	b.WriteString("\n")
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "> ⚠️ %s\n", w)
	}
	if p.review == nil {
		return b.String()
	}
	rv := p.review
	fmt.Fprintf(&b, "## Resumo\n\nAderência: `%s` · veredito do agente: `%s`\n\n%s\n\n", rv.Adherence, rv.Verdict, rv.Summary)
	b.WriteString("## Achados\n\n")
	for _, a := range rv.Axes {
		for _, f := range a.Findings {
			b.WriteString(findingLine(a.Axis, f) + "\n")
		}
	}
	if len(rv.Dropped) > 0 {
		b.WriteString("\n## Descartados\n\n")
		for _, d := range rv.Dropped {
			fmt.Fprintf(&b, "- `%s:%d` — %s (motivo: %s)\n", d.File, d.Line, flat(d.Message), flat(d.Reason))
		}
	}
	return b.String()
}

func forgePROptions(ru *run, draft bool) forge.PROptions {
	return forge.PROptions{
		Head: ru.repo.Branch, Base: strings.TrimPrefix(ru.repo.Base, "origin/"),
		Title: prTitle(ru.task.Kind, ru.task.Title), Body: prBody(ru, draft), Draft: draft,
	}
}

// prBody is what the human reviewer reads: why the change exists, how it was proven, and,
// for drafts, what is still missing.
func prBody(ru *run, draft bool) string {
	res, plan := ru.res, ru.plan
	var b strings.Builder
	b.WriteString("## Resumo\n\n")
	if plan.Summary != "" {
		b.WriteString(plan.Summary + "\n\n")
	}
	if ru.issue > 0 {
		fmt.Fprintf(&b, "Closes #%d\n\n", ru.issue)
	}
	if draft {
		fmt.Fprintf(&b, "> ⚠️ **Draft: precisa de revisão humana.** Resultado `%s`", res.Outcome)
		if res.Reason != "" {
			fmt.Fprintf(&b, ": %s", res.Reason)
		}
		b.WriteString("\n\n")
	}
	if len(plan.Acceptance) > 0 {
		b.WriteString("## Critérios de aceite → testes\n\n")
		for _, a := range plan.Acceptance {
			fmt.Fprintf(&b, "- %s — `%s`\n", a.Criterion, strings.Join(a.Tests, "`, `"))
		}
		b.WriteString("\n")
	}
	if n := len(res.History); n > 0 {
		last := res.History[n-1]
		fmt.Fprintf(&b, "## Gates (iteração %d de %d)\n\n| Gate | Comando | Resultado |\n|---|---|---|\n", last.N, ru.maxIter)
		for _, c := range last.Checks {
			state := "✅"
			if c.Skipped {
				state = "⏭️ não executado"
			} else if !c.Passed() {
				state = fmt.Sprintf("❌ (%d)", c.ExitCode)
			}
			fmt.Fprintf(&b, "| %s | `%s` | %s |\n", c.Name, c.Command, state)
		}
		b.WriteString("\n")
		if last.Review != nil {
			fmt.Fprintf(&b, "Revisão automática (spec, standards, security): **%s**\n\n", last.Review.EffectiveVerdict())
			if fb := reviewFeedback(*last.Review); fb != "" {
				b.WriteString("### Achados bloqueantes\n\n" + fb + "\n\n")
			}
		}
	}
	if len(res.Commits) > 0 {
		b.WriteString("## Commits\n\n")
		for _, c := range res.Commits {
			fmt.Fprintf(&b, "- `%s`\n", c)
		}
		b.WriteString("\n")
	}
	if res.Budget != "" {
		fmt.Fprintf(&b, "Consumo: %s\n\n", res.Budget)
	}
	fmt.Fprintf(&b, "---\n🤖 Aberto pelo delamain (run %d, %d iteração(ões)). O delamain nunca faz merge: a decisão é de quem revisa.\n", res.RunID, res.Iterations)
	return b.String()
}

func issueComment(res Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🤖 **delamain** terminou esta issue: **%s**\n\n", res.Outcome)
	if res.PRURL != "" {
		fmt.Fprintf(&b, "- PR/MR: %s%s\n", res.PRURL, map[bool]string{true: " (draft, precisa de revisão humana)"}[res.Draft])
	}
	fmt.Fprintf(&b, "- Iterações: %d · commits: %d\n", res.Iterations, len(res.Commits))
	if res.Budget != "" {
		fmt.Fprintf(&b, "- Consumo: %s\n", res.Budget)
	}
	if res.Reason != "" {
		fmt.Fprintf(&b, "- Motivo: %s\n", res.Reason)
	}
	if res.Delivery != "" {
		fmt.Fprintf(&b, "- Entrega: %s\n", res.Delivery)
	}
	return b.String()
}
