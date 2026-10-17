---
name: delamain-commit-gate
description: Prepara e executa o commit de uma fatia no modo autônomo do delamain — stage explícito, revisão do staged diff e mensagem Conventional Commits. Push, merge e troca de branch continuam proibidos. Use ao fechar cada fatia.
---

<role>
Você está executando a skill `delamain-commit-gate`. No delamain, o commit **local** de uma fatia verde é autorizado; tudo que publica ou reescreve histórico pertence ao orquestrador.
</role>

<rules>
| Operação | No delamain |
|---|---|
| `git add <arquivo>` explícito | Permitido |
| `git commit` de fatia verde | Permitido |
| `git add .`, `git add -A`, `git add -u` | **Proibido** — arrasta arquivo desconhecido, artefato local ou segredo |
| `git push`, `merge`, `rebase`, `reset --hard`, `checkout`/`switch`, `stash`, `commit --amend` de commit alheio | **Proibido** |
| `--no-verify` | **Proibido** — hooks existem por um motivo |
</rules>

<workflow>

### 1. Stage explícito
Adicione **um a um** os caminhos que você tocou nesta fatia:
```bash
git add <caminho> <caminho>
```

### 2. Revise o que foi preparado
```bash
git diff --cached --name-only
git diff --cached
```
Procure e remova do stage antes de commitar:

| Procure | Por quê |
|---|---|
| Credencial, token, `.env`, chave, dado pessoal | Commit publica; remoção posterior não desfaz |
| `console.log`, `print`, `debugger`, `fmt.Println` de depuração | Sobra de investigação |
| Código comentado, `TODO` novo | Resolve ou vira `pending` |
| Arquivo que você não tocou nesta fatia | Entrou por engano |
| `skip`, `only`, asserção comentada | Verificação desligada em silêncio |
| Mudança fora do plano | Refatoração oportunista |
| Artefatos do delamain (`plan.json`, `impl.json`, `review.json`) | Vivem fora do repositório; nunca entram no commit |

### 3. Mensagem
Conventional Commits, assunto em inglês, minúsculo, abaixo de 70 caracteres, imperativo, sem ponto final:
```text
<tipo>: <o que muda>
```
| Tipo | Quando |
|---|---|
| `feat` | Comportamento novo |
| `fix` | Correção de comportamento errado |
| `test` | Só testes (ex.: teste de regressão isolado) |
| `refactor` | Estrutura muda, comportamento não |
| `docs` / `chore` | Documentação / build, dependência, configuração |

Corpo opcional, para o **porquê**. Inclua a referência da issue em uma linha (`Refs: #<n>`). Um commit por ideia: se a mensagem precisa de "e", são dois commits.

</workflow>
