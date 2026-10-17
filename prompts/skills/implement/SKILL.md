---
name: delamain-implement
description: Executa o plano do delamain em fatias verticais com TDD, verificando cada fatia e a suíte completa ao final, no modo autônomo (sem perguntas ao usuário). Use no passo de implementação e nas iterações de correção.
---

<role>
Você está executando a skill `delamain-implement`. Constrói o que o plano decidiu — nada além, nada aquém. O plano é a fonte de escopo; a issue é a fonte de intenção.
</role>

<context>
**Entrada:** `plan.json`/`plan.md` (fatias, matriz critério → teste, comandos) e, em iterações seguintes, o **feedback** da iteração anterior (falhas de verificação ou achados de revisão).

**Saída:** código, testes e commits na branch de trabalho, mais o artefato de implementação pedido no prompt do passo.

**Skills acionadas:** `delamain-tdd` em cada fatia; `delamain-commit-gate` ao fechar cada fatia.
</context>

<workflow>

### 1. Ancore
- Leia o plano inteiro e o feedback, se houver. Feedback tem precedência sobre fatias já concluídas: corrija o que foi apontado antes de qualquer coisa nova.
- Confira `git status`: a worktree deve estar limpa ou só com o seu trabalho. Mudança alheia é registrada em `pending`, nunca misturada.
- Use os comandos de teste, lint e typecheck do plano. Comando que não existe no projeto não é inventado.
- Confirme a ordem das fatias com `references/vertical-slices.md`. Fatia irreversível (migração, mudança de contrato publicado, exclusão) não é executada no modo autônomo: vai para `pending`.

### 2. Uma fatia por vez
- Rode o ciclo da skill `delamain-tdd` na seam da fatia, incluindo os cenários alternativos aplicáveis.
- Ao fechar a fatia: typecheck + o arquivo de teste da fatia.
- Faça o commit da fatia seguindo `delamain-commit-gate`.
- Não antecipe a fatia seguinte nem adicione capacidade fora do plano.

### 3. Verifique o conjunto
- Rode **uma vez** a suíte completa, lint e typecheck.
- Falha fora das fatias tocadas é **regressão**: corrija antes de encerrar, ou registre em `pending` com a causa se a correção exigir decisão.
- Teste pulado, pendente ou removido conta como falha. Nunca silencie teste para fechar o passo.

### 4. Iterações de correção
Quando o prompt trouxer feedback:
- Trate cada item como uma fatia: reproduza com teste quando for comportamento (vermelho → verde), ou ajuste direto quando for lint/estilo.
- Não reescreva o que não foi apontado.
- Achado de revisão com o qual você discorda tecnicamente: não ignore em silêncio — registre em `pending` com a evidência.

</workflow>

<rules>
- **Não decida o que o plano não decidiu.** Siga o caminho conservador e registre a suposição em `pending`.
- **Não implemente além do plano.** Código especulativo é defeito.
- **Nenhuma fatia vira concluída antes do verde.**
- **Nunca reporte sucesso com verificação pulada.** Falha entra no artefato como está.
- Push, merge, rebase, reset e troca de branch são proibidos; commits por fatia são permitidos.
</rules>
