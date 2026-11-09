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

**Pontos.** São três. `triage` pergunta, antes de a run começar, se a issue tem objetivo concreto e critérios de aceite verificáveis; em `on`, uma issue com resposta abaixo do `threshold` termina em `needs_info`, com a label `needs-info` e um comentário pedindo o escopo e os critérios. `checks` pergunta, quando `format`, `lint`, `typecheck` ou `test` falham no verify, se a falha é flaky ou de infraestrutura; em `on`, repete o verify uma vez (os gates de git, segredos e prova vermelha nunca perguntam). `review_dedup` pergunta, entre iterações de review, se um bloqueante reformulado é o mesmo já visto; em `on`, não o conta como problema novo.

**Dados enviados.** Em `shadow` ou `on`, cada ponto envia ao provedor (OpenRouter) o que precisa para decidir: `triage` manda o título, o tipo e o corpo da issue, cortado em 24000 bytes; `checks` manda o fim da saída dos comandos que falharam (6000 bytes por check); `review_dedup` manda os findings bloqueantes. Isso vale também em `shadow`, e o conteúdo sai do seu ambiente: em repositório privado, deixe o ponto em `off` ou mantenha `decisions.enabled: false`. Não há opt-out por repositório: `decisions` é config global.

**Chave.** Use uma chave OpenRouter **dedicada**, com limite de gasto, na variável nomeada em `api_key_env`. A chave nunca vai no YAML e é apagada do ambiente dos agentes. Com `enabled: true` e a variável vazia, a run não inicia: `delamain run`, `delamain daemon`, `delamain review`, `delamain recover` e o `delamain stop` que finaliza uma run órfã falham na partida, citando o nome da variável, mesmo os que não consultam o modelo. No daemon, a variável precisa estar no ambiente do processo.

**Overrides.** `DELAMAIN_DECISIONS_ENABLED` liga ou desliga a camada, e `DELAMAIN_DECISIONS_MODEL` troca o modelo.

**Calibração.** Deixe o ponto em `shadow` e compare os logs `decision point=triage ...` com o desfecho das runs. Ajuste o `threshold` e só então mude o ponto para `on`.

**Doctor.** `delamain doctor` mostra o check `decisions`: `disabled` quando a camada está desligada, falha quando a chave falta, e uma chamada mínima ao provedor quando ela está presente.

**Teste de contrato.** O teste contra a API real é opt-in:

```sh
DELAMAIN_IT_DECISIONS_MODEL=typesafe/jev-1.14 DELAMAIN_DECISIONS_API_KEY=... go test ./internal/decide/ -run Integration -v
```

## API remota (opcional)

`delamain serve` expõe uma API JSON para um app de celular ver e controlar as runs: status, runs, fila, logs, relatório e um stream de mudanças, mais parar, iniciar, recuperar e apagar runs e mexer na fila. O contrato é um documento OpenAPI 3.1, servido em `GET /api/v1/openapi.yaml`, de onde se gera o cliente do app. A camada vem desligada.

```yaml
api:
  enabled: true
  addr: 127.0.0.1:7778          # só loopback; o túnel encaminha para cá
  token_env: DELAMAIN_API_TOKEN # nome da variável com o token, nunca o token
  repos: []                     # repositórios extras; daemon.repos já valem
  max_streams: 8
  trust_proxy: false
  rate_limit:
    auth_failures: 10
    requests_per_second: 20
  audit_log: ""                 # padrão <state_dir>/api-audit.log
```

**Modelo de segurança.** A API escuta **só em loopback**: `api.addr` fora de loopback é recusado, e não há flag para mudar isso. Quem a leva até o celular é um túnel HTTPS, que termina o TLS:

```sh
cloudflared tunnel --url http://127.0.0.1:7778
# ou
ngrok http 127.0.0.1:7778
```

Toda rota sob `/api/v1` exige `Authorization: Bearer <token>`, inclusive os streams e o OpenAPI; só `GET /healthz` responde sem token, com `{"ok":true}`. O token é comparado por digest em tempo constante. Um cliente que erra o token `auth_failures` vezes em 10 minutos fica bloqueado por 15 minutos, mesmo com o token certo; um balde global limita a `requests_per_second` (com rajada do dobro). O body é limitado a 64 KiB. Não há CORS: não existe cliente de browser.

**Token.** Gere com `delamain serve token`: 32 bytes aleatórios em base64url (43 caracteres). O delamain nunca armazena o token: a config guarda só o **nome da variável** (`api.token_env`), e um valor colado no lugar do nome é rejeitado. Exporte a variável no ambiente do `delamain serve` (em systemd, um `EnvironmentFile`) e no app. Com a variável vazia ou curta, `serve` recusa iniciar citando o nome dela. Só o `serve` usa o token: ele o tira do próprio ambiente assim que o lê, e os demais comandos (daemon, `run`, `review`, `stop`…) o apagam ao carregar a config, exceto o `doctor`, que só confere se ele existe. Assim gates, hooks do git, CLI da forge e agentes nunca o herdam, mesmo com o daemon e o `serve` no mesmo `EnvironmentFile`.

**`trust_proxy`.** Pelo túnel, todo request chega como `127.0.0.1`. Com `trust_proxy: false` (padrão), o bloqueio por tentativas conta todos os clientes como um só: um atacante consegue negar o serviço ao dono por 15 minutos, mas nunca contornar o token. Com `true`, o endereço do cliente vem de `CF-Connecting-IP` ou do primeiro `X-Forwarded-For`, aceitos só quando o peer é loopback (o túnel), e cada celular tem a sua contagem.

**Auditoria.** Toda mutação (`POST`, `PUT`, `DELETE`), todo login recusado e todo request barrado por limite vira uma linha JSON em `api-audit.log`: hora, evento, método, rota, status, IP do cliente, user agent, duração e os ids envolvidos (run, item da fila, operação). Nunca o token nem o body.

**Ambiente.** O `serve` roda na mesma máquina e usuário das runs: a detecção de órfãs e dos slots usa PIDs locais. Ele precisa do mesmo ambiente do daemon (tokens do forge, chave de decisions, token do ntfy), porque recupera órfãs e consulta o forge para enfileirar issues. Rode-o ao lado do daemon, com o mesmo `EnvironmentFile`.

**Rotas.** `GET /status`, `/repos`, `/runs?state=active|finished|all&limit=&before=`, `/runs/{id}`, `/runs/{id}/logs?cursor=&tail=&roles=`, `/queue`, `/operations`, `/report`, `/board` leem. `POST /runs` e `/reviews` iniciam uma run ou review e respondem `202` com uma **operação** para acompanhar; `POST /runs/{id}/stop` pede a parada ao dono (nada é morto; uma órfã é recuperada na hora); `POST /runs/{id}/recover` recupera uma órfã como operação; `DELETE /runs/{id}?force=true` apaga uma run terminada; `POST /queue`, `DELETE /queue/{id}` e `PUT /queue/{id}/position` mexem na fila **por id**. Erros são `application/problem+json` com um `code` estável (`unauthorized`, `rate_limited`, `not_found`, `run_active`, `worktree_dirty`...), listado no OpenAPI. Os repositórios aceitos são `daemon.repos` mais `api.repos`; qualquer outro é `repo_not_allowed`.

**Eventos.** `GET /api/v1/events` é um stream Server-Sent Events: abre com `hello`, `runs`, `queue` e `status` e depois manda só o que mudou, `run` por run, `run_removed` quando uma sai da janela, `queue` e `status` inteiros quando mudam, `operation` a cada passo de uma operação e `problem` quando a releitura falha, com um `: ping` a cada 15 segundos. Numa reconexão, a rajada inicial vem de novo. `GET /runs/{id}/logs/stream` manda `log` conforme as linhas chegam e `end` quando a run termina. Os streams abertos são limitados por `max_streams`.

**Operações.** Iniciar uma run é disparar `delamain run --issue N` como processo próprio; a operação carrega o `run_id` assim que a run aparece, e no fim o `exit_code` (0 aprovada, 1 interrompida, 3 esgotada, 4 needs_info, 5 sem slot), o erro e o fim do log de lançamento. As operações vivem na memória do `serve`: um restart as esquece, mas as runs seguem visíveis.

**Doctor.** `delamain doctor` mostra o check `api`: `off` quando desligada, falha sem token ou com `addr` fora de loopback, e avisa quando nada responde em `/healthz`.

## Notificações push (ntfy)

Para receber no celular as mesmas notificações do desktop (PR aberta, precisa de humano, interrompida, needs_info, orçamento, recover), ligue o backend **ntfy**, que publica num tópico do [ntfy.sh](https://ntfy.sh) ou de um servidor seu. O app ntfy, ou o seu app, assina o tópico. Funciona ao lado do backend de desktop, sem substituí-lo, e respeita `notify.events`.

```yaml
notify:
  ntfy:
    enabled: true
    url: https://ntfy.sh            # https, exceto servidor na própria máquina
    topic: dlm-7f3a9c2e1b          # trate como segredo
    token_env: DELAMAIN_NTFY_TOKEN  # opcional; Bearer quando a variável existe
    priority: { low: 2, normal: 3, critical: 4 }
```

**Privacidade.** No ntfy.sh, qualquer um que saiba o nome do tópico lê o que é publicado: o tópico é o único segredo. Escolha um nome aleatório e longo, ou use um servidor próprio com autenticação (`token_env`). Os títulos levam o título da issue.

**Mapeamento.** O título e o corpo do toast viram o título e a mensagem; a urgência vira a prioridade configurada; a ação padrão (`https`) vira o clique da notificação e até três outras viram botões. Alvos locais, como o `report.md`, não vão para o celular.

**Teste e doctor.** `delamain notify --test --via ntfy` publica um exemplo no tópico (`--via desktop` ou `all` para os outros). `delamain doctor` mostra o check `ntfy`, com o tópico mascarado, e falha quando o servidor não responde.

**Overrides.** `DELAMAIN_NTFY_ENABLED`, `DELAMAIN_NTFY_TOPIC` e `DELAMAIN_NTFY_URL`.
