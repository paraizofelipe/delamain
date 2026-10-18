---
name: delamain-tdd
description: Ciclo vermelho → verde de uma fatia vertical no modo autônomo do delamain. Define o que é um bom teste, como provar o vermelho, cenários alternativos obrigatórios e os anti-padrões que tornam o teste inútil. Use em toda fatia de implementação e em toda correção de bug.
---

<role>
Você está executando a skill `delamain-tdd`. Conduz o ciclo vermelho → verde de **uma** fatia e garante que o teste resultante valha a pena guardar.

Um teste vale a pena quando descreve comportamento que alguém se importa em preservar. Teste que quebra em toda refatoração sem mudança de comportamento é custo, não rede de segurança.
</role>

<context>
**Entrada:** uma fatia do plano (comportamento + seam + testes previstos).
**Saída:** teste e implementação da fatia, nesta ordem, com o vermelho observado e **registrado** antes do verde.

**Prova independente.** O orquestrador vai pegar os arquivos de teste da sua diff, aplicá-los sobre a branch base e rodá-los: eles **precisam falhar** lá. Um teste que passa sem a sua implementação não prova nada e reprova a iteração.

Antes do primeiro teste, alinhe o vocabulário com o domínio (`README`, glossário, `AGENTS.md`) e procure **prior art**: fixtures, helpers e convenção de nome vêm dos testes que já existem, não de você.
</context>

<workflow>

### 1. Fixe a seam e o comportamento
- Nomeie a seam (interface pública onde o teste observa) e o comportamento da fatia.
- Nome do teste como especificação, no padrão do projeto ou `should <comportamento> when <condição>`. Descreve **o quê**, nunca **como**.

### 2. Vermelho
- Escreva **um** teste, na seam, para **um** comportamento.
- Rode só esse teste e observe a falha. Ela precisa ser **pelo motivo esperado** (asserção sobre o comportamento). Erro de import, de compilação ou de fixture é ruído: ajuste até o vermelho ser real.
- O valor esperado vem de fonte independente: literal conhecido, exemplo da issue, caso calculado à mão. Nunca recalculado do jeito que o código calcula.
- **Registre** o comando e o trecho da falha — vão para o artefato do passo.
- Ao decidir o que asserir, carregue `references/test-quality.md`. Ao tocar fronteira externa (rede, relógio, aleatoriedade, filesystem, banco), carregue `references/mocking.md`.

### 3. Verde
- Escreva o **mínimo** que faz o teste passar. Nada de caminho que nenhum teste exercita.
- Rode o mesmo teste (verde), depois o arquivo inteiro, para flagrar quebra por perto.
- Verde obtido afrouxando a asserção não é verde: o ciclo falhou.

### 4. Cenários alternativos
Para a fatia, considere e cubra, cada um com seu próprio ciclo vermelho → verde, os que se aplicam:
- entrada inválida ou malformada
- estado vazio / coleção vazia / ausência
- limite (zero, um, máximo, fronteira de intervalo)
- caminho de erro de dependência externa (timeout, falha, resposta inesperada)

### 5. Regressão (bugs)
Em correção de bug, o **primeiro** teste reproduz o defeito exatamente como relatado e falha no código atual. Só depois se corrige.

</workflow>

<rules>
- **Vermelho antes de verde.** Sem falha observada, não há prova de que o teste testa algo.
- **Uma fatia por ciclo.** Todos os testes primeiro e depois toda a implementação verifica comportamento imaginado.
- **Nada de detalhe de implementação:** sem método privado, sem contagem ou ordem de chamadas, sem conferir pelo banco em vez da interface.
- **Nada de teste tautológico.**
- **Mock só em fronteira de sistema.** Colaborador interno mockado vira o objeto sob teste.
- **Determinismo:** tempo, aleatoriedade e IDs gerados são fixados pela mesma via por onde entram no código.
- **Refatoração fica fora do ciclo**, depois do verde.
- Teste pulado, `only`, removido ou com asserção comentada é falha reportável, nunca detalhe.
</rules>

<checklist>
- [ ] Prior art consultado antes de criar convenção nova.
- [ ] Vermelho observado pelo motivo esperado, com trecho registrado.
- [ ] Valor esperado de fonte independente.
- [ ] Verde com o mínimo necessário, sem asserção afrouxada.
- [ ] Cenários alternativos aplicáveis cobertos.
- [ ] Nenhum colaborador interno mockado.
- [ ] O teste sobrevive a uma refatoração sem mudança de comportamento.
</checklist>
