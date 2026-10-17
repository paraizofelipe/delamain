# Passo: implementação — {{.Issue.Ref}} · iteração {{.Iteration}} de {{.MaxIterations}}

Implemente o plano com a skill `delamain-implement` (e `delamain-tdd` em cada fatia). Worktree: `{{.Repo.Worktree}}`, branch `{{.Repo.Branch}}`, base `{{.Repo.Base}}`.

- Plano: `{{.PlanPath}}` e `{{.PlanJSONPath}}`
- Budget restante: {{.BudgetLeft}}. Priorize fechar fatias verdes a abrir frentes novas.
{{- if .Feedback}}

## Feedback da iteração anterior (prioridade máxima)

O orquestrador reprovou a iteração anterior pelos motivos abaixo. Corrija cada item antes de qualquer outra coisa; não reescreva o que não foi apontado.

<feedback>
{{.Feedback}}
</feedback>
{{- end}}

## Artefato

Ao terminar, escreva `{{.ArtifactPath}}`, JSON válido neste formato:

```json
{
  "slices": [
    {"id": "S1", "status": "done | pending", "red": {"command": "", "output": "trecho da falha esperada"}, "green": {"command": "", "output": "trecho do sucesso"}, "commits": ["sha curto"]}
  ],
  "feedback_addressed": [{"item": "resumo do item de feedback", "resolution": "o que foi feito"}],
  "full_suite": {"command": "", "passed": 0, "failed": 0, "skipped": 0},
  "lint": {"command": "", "ok": true},
  "typecheck": {"command": "", "ok": true},
  "pending": ["lacuna, suposição ou discordância registrada, com evidência"]
}
```

O orquestrador vai conferir tudo isso por conta própria. Relate o que de fato aconteceu, inclusive falhas.

Responda apenas: `DONE {{.ArtifactPath}}`
