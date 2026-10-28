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
