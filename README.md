# delamain

## Issues e fila

### Dependências entre issues

Antes de rodar uma issue, o delamain confere se ela depende de outra que ainda está aberta. As dependências vêm de três fontes.

**1. `depends_on:` no corpo da issue.** É a forma canônica:

```markdown
- depends_on: #12, #15
- depends_on: #20
```

- A linha começa com `depends_on:`. Antes dele só podem vir espaços, citação (`>`) e um marcador de lista (`-`, `*` ou `+`).
- Uma linha aceita vários `#N`, separados por vírgula ou espaço.
- Linhas dentro de um bloco de código cercado (```` ``` ```` ou `~~~`) são exemplos e não contam.
- Uma linha malformada, como `depends_on: 12` ou `depends_on: #abc`, impede a run: o erro cita a linha e diz "esperado #N separados por vírgula ou espaço".

**2. Formas antigas.** Sem nenhuma linha `depends_on:`, o delamain aceita as menções livres `depende de #N` (e `depende da`, `do`, `das`, `dos`), `depends on #N`, `blocked by #N`, `bloqueado por #N` e `bloqueada por #N`. Elas valem só no início da linha, depois de citação, marcador de lista e checkbox opcionais, e também são ignoradas dentro de bloco de código. Uma frase que só cita uma issue no meio do texto não declara dependência.

**3. Relações nativas do forge.**

- GitHub: as dependências "blocked by" da issue. Um bloqueador de outro repositório aparece como `owner/repo#N`.
- GitLab: os links `is_blocked_by`, do GitLab Premium. No GitLab Free, declare a dependência só pelo corpo da issue.

**Precedência.** Quando o corpo tem uma linha `depends_on:`, as formas antigas são ignoradas. As relações nativas sempre se somam às do corpo. Cada bloqueador aberto aparece uma vez, com a origem: `#12 (corpo, link)`.

**Na run.** `delamain run --issue N` recusa a issue com dependência aberta: termina com `needs_info` (exit 4), aplica a label `needs-info` e comenta na issue o que falta.

**Na fila.** Uma issue da fila com dependências abertas continua nela, esperando, e o próximo item roda no lugar dela. `delamain queue list` mostra na coluna `WAITING` o que ela espera. Quando as dependências fecham, ela roda. Uma issue fechada ou inexistente sai da fila.

**Boas práticas.**

- Declare só as dependências diretas: a espera em cadeia já sai delas.
- Nunca use um épico como dependência: ele só fecha depois das suas issues, então elas esperariam para sempre.
- Evite ciclos: duas issues que dependem uma da outra nunca rodam.

## Decisões (opcional)

Entre um estágio e outro, o pipeline pode fazer perguntas rápidas e tipadas a um modelo de decisão (Jev, pela Decisions API da OpenRouter). Esse modelo não substitui os agentes: ele só responde se uma condição vale ou qual opção escolher. Quando a decisão falha ou não chega a tempo, vale a regra de hoje. A camada vem desligada.

```yaml
decisions:
  enabled: false
  provider:
    base_url: https://openrouter.ai/api
    api_key_env: DELAMAIN_DECISIONS_API_KEY
    model: typesafe/jev-1.14
    timeout: 2s
  triage:       { mode: shadow, threshold: 0.3 }
  checks:       { mode: off,    threshold: 0.8 }
  review_dedup: { mode: off,    threshold: 0.85 }
```

**Modos.** Cada ponto de decisão tem o seu:

- `off`: o ponto não pergunta nada;
- `shadow`: o ponto pergunta e registra a resposta no log, mas a run segue a regra de hoje;
- `on`: a resposta decide, comparada ao `threshold` do ponto.

**Pontos.** Hoje só `triage` está implementado. Antes de a run começar, ele pergunta se a issue tem objetivo concreto e critérios de aceite verificáveis. Em `on`, uma issue com resposta abaixo do `threshold` termina em `needs_info`, com a label `needs-info` e um comentário pedindo o escopo e os critérios. `checks` e `review_dedup` já aceitam configuração, mas ainda não perguntam nada.

**Dados enviados.** Com `triage` em `shadow` ou `on`, cada run de issue envia ao provedor (OpenRouter) o título, o tipo e o corpo da issue, cortado em 24000 bytes. Isso vale também em `shadow`, e o conteúdo sai do seu ambiente: em repositório privado, deixe `triage` em `off` ou mantenha `decisions.enabled: false`. Não há opt-out por repositório: `decisions` é config global.

**Chave.** Use uma chave OpenRouter **dedicada**, com limite de gasto, na variável nomeada em `api_key_env`. A chave nunca vai no YAML e é apagada do ambiente dos agentes. Com `enabled: true` e a variável vazia, a run não inicia: `delamain run`, `delamain daemon`, `delamain review`, `delamain recover` e o `delamain stop` que finaliza uma run órfã falham na partida, citando o nome da variável, mesmo os que não consultam o modelo. No daemon, a variável precisa estar no ambiente do processo.

**Overrides.** `DELAMAIN_DECISIONS_ENABLED` liga ou desliga a camada, e `DELAMAIN_DECISIONS_MODEL` troca o modelo.

**Calibração.** Deixe o ponto em `shadow` e compare os logs `decision point=triage ...` com o desfecho das runs. Ajuste o `threshold` e só então mude o ponto para `on`.

**Doctor.** `delamain doctor` mostra o check `decisions`: `disabled` quando a camada está desligada, falha quando a chave falta, e uma chamada mínima ao provedor quando ela está presente.

**Teste de contrato.** O teste contra a API real é opt-in:

```sh
DELAMAIN_IT_DECISIONS_MODEL=typesafe/jev-1.14 DELAMAIN_DECISIONS_API_KEY=... go test ./internal/decide/ -run Integration -v
```
