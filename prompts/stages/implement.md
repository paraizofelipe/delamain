# Passo: implementação — {{.Issue.Ref}} · iteração {{.Iteration}} de {{.MaxIterations}}

Implemente o plano com a skill `delamain-implement` (e `delamain-tdd` em cada fatia). Worktree: `{{.Repo.Worktree}}`, branch `{{.Repo.Branch}}`, base `{{.Repo.Base}}`.

- Plano: `{{.PlanPath}}` e `{{.PlanJSONPath}}`
- Budget restante: {{.BudgetLeft}}. Priorize fechar fatias verdes a abrir frentes novas.
{{- if .Feedback}}
{{- if .MinorPass}}

## Ajustes não bloqueantes (a iteração já foi aprovada)

A revisão aprovou o trabalho e deixou as observações abaixo. Corrija as que forem válidas com a menor mudança possível, com teste quando mudar comportamento. Não toque em nada fora desta lista. Se discordar de um item, não o aplique: registre em `feedback_addressed` com `resolution: "não aplicado: <motivo com evidência>"`.

<feedback>
{{.Feedback}}
</feedback>
{{- else}}

## Feedback da iteração anterior (prioridade máxima)

O orquestrador reprovou a iteração anterior pelos motivos abaixo. Corrija cada item antes de qualquer outra coisa; não reescreva o que não foi apontado.

Corrija com a **menor** mudança que resolve o item. Se a correção mínima exigiria sair do plano (subsistema novo, pré-condição nova, dependência nova, perda de capacidade, mudança de contrato público), faça o mínimo e registre a decisão maior em `pending` — não a tome sozinho.

<feedback>
{{.Feedback}}
</feedback>
{{- end}}
{{- end}}
{{- if .Hindsight}}

## Memória do projeto

A sessão tem a memória Hindsight do projeto: siga a skill `delamain-memory`. Antes de implementar, consulte `hindsight_search_knowledge_pages` e confira no código o que ela disser; no fim de cada fatia, só se houve decisão não óbvia, registre-a com `hindsight_ingest_document`.
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

O orquestrador vai conferir tudo isso por conta própria. Relate o que de fato aconteceu, inclusive falhas. Ele também roda os gates `testes-tocados`, `matriz-criterios`, `prova-vermelho` (em bug, os seus testes aplicados sobre o base precisam falhar) e `segredos`: um teste que passa sem a correção reprova a iteração.

Responda apenas: `DONE {{.ArtifactPath}}`
