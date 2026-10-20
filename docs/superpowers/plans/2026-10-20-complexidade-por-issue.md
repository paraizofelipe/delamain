# Complexidade por issue — Plano de implementação

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** o planner avalia a complexidade da issue em cinco critérios, o orquestrador classifica em `low | medium | high` e usa a classificação para definir o máximo de iterações e o budget por issue da run.

**Architecture:** o `plan.json` ganha um bloco `complexity` com a avaliação do agente. O nível é calculado pelo orquestrador (pacote novo `internal/complexity`), nunca lido do agente, e o número de arquivos vem de `slices[].files`. Perfis opcionais em `complexity.{low,medium,high}` no config substituem `pipeline.max_iterations` e `budget.per_issue` depois do planejamento. Os valores efetivos são gravados no registro da run.

**Tech Stack:** Go, SQLite (migrations em `internal/store/store.go`), YAML config, prompts Go `text/template`.

**Spec:** não há documento separado. Este plano implementa a proposta da conversa de 2026-10-20 (critérios: arquivos afetados, componentes, tipo de mudança, dependências, testes).

## Global Constraints

- Sem perfis no config, o comportamento é idêntico ao atual: `pipeline.max_iterations` e `budget.per_issue` valem para a run inteira.
- O nível nunca vem do agente: o orquestrador o calcula a partir dos critérios, com arquivos contados de `slices[].files`.
- O planejamento sempre roda com os limites globais (ainda não existe classificação).
- `budget.daily` continua sendo o teto global, sem relação com a complexidade.
- Campo de perfil com valor `0` herda o global (`pipeline.max_iterations` / `budget.per_issue`).
- Texto voltado ao usuário e prompts em português; identificadores e comentários de código em inglês, como no resto do repo.

## Regra de classificação

Cada critério gera um sinal `low`, `medium` ou `high`:

| Critério | low | medium | high |
|---|---|---|---|
| Arquivos (únicos em `slices[].files`) | 1–3 | 4–10 | > 10 |
| `components` | `single_module` | `multiple_modules` | `multiple_services` |
| `change` | `localized` | `cross_cutting` | `architectural` |
| `dependencies` (lista de `api`, `database`, `queue`, `integration`) | vazia | 1 item | ≥ 2 itens |
| `tests` (lista de `unit`, `integration`, `migration`, `regression`) | só `unit` | — | qualquer outro tipo |

Nível: **high** se houver ≥ 2 sinais high; **low** se todos forem low; caso contrário **medium**.

`regression` em `tests` significa suíte de regressão ampla. Não é o teste de regressão obrigatório de todo bug, que costuma ser unitário.

## Review Focus

- Perfil com `per_issue` menor que o já gasto no planejamento: a run precisa parar com `halted` e o motivo do budget **antes** de iniciar o implementer, sem quebrar.
- Plano sem bloco `complexity` (agente antigo ou desobediente): o artefato é rejeitado, o planner recebe uma correção e, se insistir, a run para. Nunca segue em silêncio com nível vazio.
- Run recuperada (`Recover`) de antes desta mudança (`complexity = ''`): usa `pipeline.max_iterations`/`budget.per_issue` do config, sem cair em zero.
- `delamain status` de uma run classificada: mostra o budget com o `per_issue` gravado na run, não o do config.
- Mesmo arquivo repetido em várias fatias conta uma vez só.

---

### Task 1: Contrato `complexity` no plano e nos prompts

**Files:**
- Modify: `internal/artifacts/artifacts.go` (tipos + `Plan.Validate`)
- Modify: `prompts/stages/plan.md` (formato JSON + regras)
- Modify: `prompts/roles/planner/body.md` (passo novo no workflow + checklist)
- Modify: `prompts/agents/analyzer/body.md` (seções no relatório)
- Test: `internal/artifacts/artifacts_test.go`, `internal/pipeline/pipeline_test.go` (fixture `planJSON`)

**Interfaces:**
- Produces:
  ```go
  type Complexity struct {
      Components   string   `json:"components"`   // single_module | multiple_modules | multiple_services
      Change       string   `json:"change"`       // localized | cross_cutting | architectural
      Dependencies []string `json:"dependencies"` // subset of api, database, queue, integration
      Tests        []string `json:"tests"`        // non-empty subset of unit, integration, migration, regression
      Rationale    string   `json:"rationale"`
  }
  // Plan gains: Complexity Complexity `json:"complexity"`
  ```

- [ ] **Step 1: Atualizar as fixtures e escrever os testes que falham**

Acrescentar a `validPlan` (artifacts_test) e a `planJSON` (pipeline_test), antes de `"risks"`:
`"complexity":{"components":"single_module","change":"localized","dependencies":[],"tests":["unit"],"rationale":"um arquivo"},`

Novos casos em `TestReadPlanShouldRejectContractBreaks` (from → to → erro esperado):
- `"unknown components"`: `"single_module"` → `"monolith"` → `complexity.components`
- `"unknown change"`: `"localized"` → `"tiny"` → `complexity.change`
- `"unknown dependency"`: `"dependencies":[]` → `"dependencies":["cache"]` → `complexity.dependencies`
- `"no test kind"`: `"tests":["unit"],"rationale"` → `"tests":[],"rationale"` → `complexity.tests`
- `"missing complexity"`: remover o bloco inteiro → `complexity.components`

Em `TestReadPlanShouldAcceptContractCompliantPlan`, conferir também `p.Complexity.Change == "localized"`.

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./internal/artifacts/ ./internal/pipeline/`
Expected: FAIL. Em artifacts, `unknown field "complexity"`; no pipeline, o planner fica sem plano válido.

- [ ] **Step 3: Implementar os tipos e a validação em `artifacts.go`**

`Plan.Validate` acrescenta erros com prefixo `complexity.<campo>` para valores fora dos enums acima, tipos repetidos e `tests` vazio. Os conjuntos válidos ficam em `map[string]bool` no pacote.

- [ ] **Step 4: Atualizar os prompts**

- `plan.md`: incluir o bloco `"complexity"` no exemplo JSON e uma regra de formato que lista os enums e explica `regression` (suíte ampla, não o teste do bug) e `dependencies` (só o que a mudança toca: API externa, banco/migração, fila, integração com outro serviço).
- `planner/body.md`: novo passo "### 5. Avalie a complexidade" antes de "Escreva os artefatos", com a tabela de critérios, e o aviso de que o orquestrador conta os arquivos pelas fatias (então `files` precisa estar completo). Item novo no checklist.
- `analyzer/body.md`: duas seções novas em `output_format`: `## Dependências externas tocadas (api | database | queue | integration)` e `## Tipos de teste necessários`.

- [ ] **Step 5: Rodar testes e lint dos prompts**

Run: `go test ./internal/artifacts/ ./internal/pipeline/ ./internal/prompts/ ./prompts/... && go run ./cmd/delamain prompts lint`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/artifacts prompts internal/pipeline/pipeline_test.go
git commit -m "feat(plan): planner reports issue complexity criteria"
```

---

### Task 2: Classificação determinística

**Files:**
- Create: `internal/complexity/complexity.go`
- Test: `internal/complexity/complexity_test.go`

**Interfaces:**
- Consumes: `artifacts.Plan`, `artifacts.Complexity` (Task 1)
- Produces:
  ```go
  type Level string
  const (Low Level = "low"; Medium Level = "medium"; High Level = "high")
  type Assessment struct {
      Level   Level
      Files   int               // unique paths across plan.Slices[].Files
      Signals map[string]Level  // keys: files, components, change, dependencies, tests
  }
  func Classify(p artifacts.Plan) Assessment
  func (a Assessment) Summary() string // e.g. "high (5 arquivos; high: change, dependencies)"
  ```

- [ ] **Step 1: Escrever os testes**

Tabela `TestClassifyShouldApplyTheThresholds` com um plano construído em Go (helper `plan(files []string, c artifacts.Complexity)`):
- 2 arquivos, `single_module`, `localized`, deps `[]`, tests `[unit]` → `Low`
- 5 arquivos, resto low → `Medium` (`Signals["files"] == Medium`)
- 11 arquivos + `architectural`, resto low → `High`
- `architectural` só, resto low → `Medium` (um sinal high não basta)
- deps `[database, queue]` + tests `[unit, migration]` → `High`
- deps `[api]` → `Signals["dependencies"] == Medium`

`TestClassifyShouldCountARepeatedFileOnce`: duas fatias com `calc.go` → `Files == 1`.

`TestSummaryShouldNameTheHighSignals`: o caso high acima → `Summary()` contém `"high"` e `"change"`.

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./internal/complexity/`
Expected: FAIL (pacote não existe)

- [ ] **Step 3: Implementar `Classify` e `Summary`** seguindo a "Regra de classificação" acima. `Summary` lista as chaves high em ordem alfabética.

- [ ] **Step 4: Rodar e ver passar**

Run: `go test ./internal/complexity/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/complexity
git commit -m "feat(complexity): classify a plan as low, medium or high"
```

---

### Task 3: Perfis no config

**Files:**
- Modify: `internal/config/config.go` (tipos, `Config`, `Validate`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:
  ```go
  // Profile overrides the run's limits once the plan is classified. Zero inherits the global value.
  type Profile struct {
      MaxIterations int     `yaml:"max_iterations"`
      PerIssue      float64 `yaml:"per_issue"`
  }
  type Complexity struct {
      Low    Profile `yaml:"low"`
      Medium Profile `yaml:"medium"`
      High   Profile `yaml:"high"`
  }
  // Config gains: Complexity Complexity `yaml:"complexity"`
  // Limits returns the effective limits for level ("low"|"medium"|"high"); unknown levels and zero fields fall back to global.
  func (c Config) Limits(level string) (maxIterations int, perIssue float64)
  ```

- [ ] **Step 1: Escrever os testes**

- `TestLoadShouldReadComplexityProfiles`: YAML `complexity: {high: {max_iterations: 5, per_issue: 4000000}}` → `cfg.Limits("high") == (5, 4e6)`.
- `TestLimitsShouldInheritGlobalForZeroOrUnknown`: default config → `Limits("low") == (3, 2e6)`. Com `High{MaxIterations: 5}` → `Limits("high") == (5, 2e6)`. `Limits("")` → `(3, 2e6)`.
- Novos casos em `TestValidateShouldRejectUnsafeOrMeaninglessValues`: `"negative profile iterations"` (`c.Complexity.High.MaxIterations = -1`) e `"negative profile budget"` (`c.Complexity.Low.PerIssue = -1`).

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./internal/config/`
Expected: FAIL (campos não existem)

- [ ] **Step 3: Implementar**

Adicionar os tipos, o campo em `Config`, o método `Limits` e, em `Validate`, o erro `complexity.<level>: max_iterations and per_issue must be >= 0`. Os defaults não mudam (perfis vazios).

- [ ] **Step 4: Rodar e ver passar**

Run: `go test ./internal/config/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat(config): complexity profiles for iterations and per-issue budget"
```

---

### Task 4: Gravar complexidade e limites na run

**Files:**
- Modify: `internal/store/store.go` (`Run`, `migrations`, `runCols`/scan, `CreateRun`, `UpdateRun`)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces: `store.Run` ganha `Complexity string` e `PerIssue float64`. `CreateRun(ctx, repo, issueRef, title, harness string, maxIterations int, perIssue float64)` grava `per_issue`. `UpdateRun` passa a persistir também `max_iterations`, `per_issue` e `complexity`.

- [ ] **Step 1: Escrever o teste**

`TestUpdateRunShouldPersistTheClassifiedLimits`: `CreateRun(..., 3, 2e6)`, depois `rec.MaxIterations, rec.PerIssue, rec.Complexity = 5, 4e6, "high"`, `UpdateRun`, `GetRun` → os três valores. Ajustar `TestCreateRunShouldPersistHarnessAndMaxIterations` para também conferir `PerIssue == 2e6` e `Complexity == ""`.

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./internal/store/`
Expected: FAIL de compilação (assinatura / campos)

- [ ] **Step 3: Implementar**

Migração nova no fim de `migrations`:
```sql
ALTER TABLE runs ADD COLUMN complexity TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN per_issue REAL NOT NULL DEFAULT 0;
```
Atualizar o comentário de `Run`: Harness fica fixo na criação; MaxIterations, PerIssue e Complexity podem mudar uma vez, depois do plano. Corrigir o único chamador de `CreateRun` (`pipeline.go:180`) passando `r.Cfg.Budget.PerIssue`.

- [ ] **Step 4: Rodar e ver passar**

Run: `go test ./internal/store/ ./internal/pipeline/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store internal/pipeline/pipeline.go
git commit -m "feat(store): record a run's complexity and per-issue budget"
```

---

### Task 5: O orquestrador aplica a classificação

**Files:**
- Modify: `internal/pipeline/pipeline.go` (`run`, `Result`, `execute`, `stageData`)
- Modify: `internal/pipeline/budget.go` (`meterFor`)
- Modify: `internal/pipeline/recover.go:78`
- Modify: `internal/pipeline/report.go` (linha no relatório)
- Test: `internal/pipeline/pipeline_test.go`, `internal/pipeline/recover_test.go`

**Interfaces:**
- Consumes: `complexity.Classify`/`Assessment.Summary` (Task 2), `config.Config.Limits` (Task 3), `store.Run.{Complexity,PerIssue}` (Task 4)
- Produces: `run.perIssue float64`; `Result.Complexity string` (o `Summary()`); `func (r *Runner) meterFor(ru *run) budget.Meter` (cópia de `meter()` com `Cfg.PerIssue = ru.perIssue`).

- [ ] **Step 1: Escrever os testes**

Usar o harness existente (`newHarness`, `happy`, `finalRun`) e um helper `highPlan` que troca na `planJSON` o bloco `complexity` por `"components":"multiple_services","change":"architectural"`:

- `TestRunShouldUseTheComplexityProfileForIterations`: reviewer sempre `reviewJSON("major")`, planner devolve `highPlan`, `Cfg.Complexity.High.MaxIterations = 5` → `OutcomeExhausted`, `res.Iterations == 5`. O segundo prompt do implementer contém `"iteração 2 de 5"`. `finalRun` tem `MaxIterations == 5` e `Complexity == "high"`.
- `TestRunShouldKeepGlobalLimitsWithoutProfiles`: plano `low` (fixture padrão), sem perfis → exausta em 3 (o teste atual `TestRunShouldStopAfterMaxIterationsWhenReviewKeepsBlocking` já cobre; só conferir também `finalRun.Complexity == "low"`).
- `TestRunShouldHaltBeforeImplementingWhenTheProfileBudgetIsAlreadySpent`: `Cfg.Complexity.Low.PerIssue = 1` (um token) com uso fake > 1 no planner → `OutcomeHalted`, `Reason` contém `"budget da issue esgotado"`, `len(hs.h.prompts["implementer"]) == 0`. Usar o mecanismo de uso fake que `budget_test.go` já usa.
- `TestRecoverShouldFallBackToConfigLimitsForUnclassifiedRuns` (recover_test): run com `MaxIterations 0, Complexity ""` → o relatório mostra `iteração … de 3`, como já faz hoje. Run com `MaxIterations 5` → `de 5`.
- Relatório: em `TestRunShouldDeliverApprovedWorkInOneIteration`, conferir que o relatório tem `| Complexidade | low`.

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./internal/pipeline/`
Expected: FAIL (exausta em 3, sem complexidade no registro)

- [ ] **Step 3: Implementar**

- Em `Run`: `ru.perIssue = r.Cfg.Budget.PerIssue`, ao lado de `maxIter`.
- Em `execute`, logo depois de `ru.plan = plan`, chamar `applyComplexity(ctx, ru)`: roda `Classify`, chama `Limits(level)`, grava `ru.maxIter`, `ru.perIssue`, `ru.rec.{MaxIterations,PerIssue,Complexity}` e `ru.res.Complexity`, registra no log com `ru.log.Info("complexity", ...)`, faz `UpdateRun` e então `return r.checkBudget(ctx, ru)`. Assim um perfil menor que o gasto para a run antes do implementer.
- O loop usa `ru.maxIter` (remover a variável local `maxIter` em `pipeline.go:304`, inclusive no `Reason`). `stageData` usa `ru.maxIter`.
- `checkBudget` e `watchBudget` passam a usar `r.meterFor(ru)`. `Run` continua usando `r.meter().Today` antes de existir run.
- `recover.go`: `maxIter = rec.MaxIterations`, ou `r.Cfg.Pipeline.MaxIterations` se for 0. `perIssue = rec.PerIssue` se `rec.Complexity != ""`, senão `r.Cfg.Budget.PerIssue`. `res.Complexity = rec.Complexity`.
- `report.go`: depois da linha de Iterações, `| Complexidade | <res.Complexity> |` quando não vazio.

- [ ] **Step 4: Rodar e ver passar**

Run: `go test ./internal/pipeline/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline
git commit -m "feat(pipeline): size iterations and per-issue budget by plan complexity"
```

---

### Task 6: `ls`/`status` mostram a complexidade e o budget da run

**Files:**
- Modify: `internal/cli/runs.go` (`RunView`, montagem em `:146`, budget em `:201`, `statusTable`)
- Test: `internal/cli/runs_test.go`

**Interfaces:**
- Consumes: `store.Run.{Complexity,PerIssue}` (Task 4)
- Produces: `RunView.Complexity string` (`json/yaml:"complexity,omitempty"`)

- [ ] **Step 1: Escrever o teste**

Run gravada com `Complexity "high"`, `PerIssue 4e6` e config com `PerIssue 2e6`. A saída de `status` contém `COMPLEXITY  high` e `BUDGET` com `de 4M`. O JSON tem `"complexity":"high"` e `budget.per_issue == 4e6`. Uma run sem complexidade continua com o `per_issue` do config e mostra `COMPLEXITY  -`.

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./internal/cli/`
Expected: FAIL

- [ ] **Step 3: Implementar**

Preencher `Complexity` a partir do registro. No cálculo do budget (`:201`), usar `rec.PerIssue` quando `rec.Complexity != ""`, senão o config (mesma regra do recover). Em `statusTable`, adicionar `COMPLEXITY\t<orDash(v.Complexity)>` logo depois de `INTERACTIONS`.

- [ ] **Step 4: Rodar a suíte inteira**

Run: `go test ./... && go vet ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli
git commit -m "feat(cli): show run complexity and its per-issue budget"
```
