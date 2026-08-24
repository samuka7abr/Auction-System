# Etapa 5 — Spec 02: A fatia do gráfico principal

[← índice](../../README.md) · [decisões da etapa 5](../../decisoes/etapa-5.md) · [etapa 5, spec 01](01-spec-matriz.md) · [benchmark.md](../../projeto/benchmark.md)

## Contexto

A spec 01 entregou o instrumento e provou que ele funciona: o plano das 36 células, o pré-voo, a troca de engine provada antes de medir, as nove recusas e o agregador. O que ela não entregou foi **um número**.

`docs/projeto/benchmark.md` continua com a tabela de *Resultados* vazia, e o projeto inteiro existe para preenchê-la. Quatro etapas de construção, três engines, oito invariantes e cinco cenários de caos estão de pé para responder uma pergunta que ainda não foi respondida.

Duas coisas impedem a matriz completa de fechar isso hoje:

1. **Custo.** As 36 células levam ~1h20 de carga nominal, e o C3 da spec 01 mediu ~4h30 reais
2. **Uma célula que não converge.** `pessimistic` sobre 1 leilão rodou **43 minutos** em vez de 3. Não é defeito do harness — está escrito em `internal/bid/pessimistic/pessimistic.go`:

> *"there is no lock_timeout: the queue in front of the lock is the phenomenon this engine exists to expose, so it is measured rather than cut short"*

Com 500 clientes sobre uma linha e pool de 25, a fila na frente do lock não tem teto. A célula estourou o próprio `ENDS_IN=30m`, colheu `closed=204354`, e publicaria `1.63 aceitos/s` — um número que **parece throughput e não é**: é o throughput diluído por quarenta minutos de drenagem.

Esta spec não constrói engine, não constrói série, não constrói painel. Ela corta o experimento no menor recorte que ainda desenha a curva do projeto, e dá um nome ao que acontece quando uma célula não converge.

## Objetivo

Produzir o **gráfico principal** — throughput por nível de contenção, uma linha por estratégia, com o ponto de cruzamento — a partir de uma execução que cabe em uma sessão de trabalho.

O sistema deve:

- Rodar **9 células** (`3 estratégias × 3 contenções`) mais a célula-controle, num cenário e numa política fixos
- Limitar cada célula por um orçamento de relógio, e tratar o estouro como **resultado nomeado** e não como falha do harness
- Publicar `matrix.json` e `matrix.md` da fatia, com a mesma autoridade e as mesmas recusas que a matriz completa
- Não inventar número nenhum para a célula que não convergiu

## Fora de Escopo

- **As outras 27 células.** Cenário (`last_second_spike`) e política (`jitter`) continuam sendo eixos legítimos, e continuam medindo perguntas reais — o gume do sniping e a crítica do backoff. Elas ficam para quando houver as quatro horas, e a spec 01 já as roda sem alteração nenhuma
- **Sweep de pool do pessimista.** A célula que não converge é um convite óbvio a essa pergunta. Ela continua sendo pergunta, e esta spec não a responde
- **Dashboards do Grafana e o painel React.** Fora da etapa
- **Consertar a engine pessimista.** Um `lock_timeout` mudaria o que a engine é, e a decisão 102 já disse por quê: afrouxar para o experimento terminar é escolher terminar em vez de medir. O teto desta spec é do **harness**, não da engine — ele limita quanto tempo se observa, nunca quanto tempo a engine espera
- **Série nova, engine nova, migration, rota.** Nada disso muda o gráfico

## Fluxo

```text
PLAN=slice bench/run-matrix.sh
  │
  ├── pré-voo: idêntico ao da spec 01, sem uma linha nova
  │
  ├── para cada uma das 9 células, na ordem da contenção
  │     │   (a1 → a10 → a1000, estratégia por dentro)
  │     │
  │     ├── recreate do auctiond · wait_ready · wait_strategy
  │     └── run-cell.sh, agora com CELL_BUDGET no laço medido
  │              └─ estourou? SIGINT no k6, que ainda escreve o summary
  │                 → a célula termina, o checker roda, e o artefato
  │                   diz que ela foi interrompida
  │
  ├── célula 37: a 01 outra vez
  │
  └── bin/matrix -dir ... -plan slice
        ├── as nove recusas, sobre o plano de 10 e não o de 37
        ├── a linha da célula interrompida sai sem taxa
        └── publica matrix.json + matrix.md + o gráfico
```

## Decisoes Tecnicas

### Nove células são o gráfico; as outras 27 são outras perguntas

O entregável escrito em `benchmark.md` é *"throughput por nível de contenção, uma linha por estratégia, com o ponto de cruzamento marcado"*. Lido com cuidado, isso é uma função de **duas** variáveis: estratégia e contenção. São 9 pontos.

Cenário e política não entram nesse gráfico — elas geram outros dois, que respondem outras duas perguntas:

| Eixo cortado | Pergunta que ele responde | Por que ela pode esperar |
| --- | --- | --- |
| `last_second_spike` | O gume do sniping: 1000 clientes de uma vez em vez de uma rampa | O cruzamento das curvas não depende dela; ela desloca as curvas, não as inverte |
| `jitter` | *"seu otimista colapsou porque você retentou sem backoff"* | É a crítica mais forte ao projeto, e merece ser respondida com a matriz inteira em vez de com metade dela |

A segunda dói, e a spec registra que dói. Publicar a fatia sem `jitter` significa publicar um gráfico ao qual essa crítica ainda se aplica — e o texto da etapa 6 precisa **dizer isso**, em vez de deixar o leitor descobrir. Um resultado parcial anunciado como parcial é honesto; o mesmo resultado anunciado como completo não é.

O que a fatia preserva inteiro: a contenção como variável independente, as três engines medidas adjacentes no tempo (decisão 94), o pool constante (decisão 105) e o controle (decisão 95).

### O orçamento é do harness, e o estouro é um resultado com nome

`CELL_BUDGET` limita o relógio da **carga medida**, e nada mais. Ele não toca no `BID_DEADLINE` do apostador, não toca no pool, e não põe `lock_timeout` em lugar nenhum: a engine continua esperando o que ela esperaria, e o que muda é por quanto tempo se olha.

Quando o orçamento estoura, três coisas acontecem, nesta ordem:

1. O k6 recebe **SIGINT**, não SIGKILL — ele para graciosamente e ainda executa o `handleSummary`. Um k6 morto não escreve `client.json`, e uma célula sem `client.json` vira exit 2, que é a ausência de resultado. A distinção é a mesma desde a etapa 1
2. O `env.json` grava `cell.interrupted: true`
3. A célula segue normalmente: reset, checker, invariantes. Ela é uma célula **válida** que foi observada por menos tempo

O que o agregador faz com ela é a parte que importa: **a linha sai sem taxa**. `aceitos/s`, `conflitos/s` e `tentativas/aceito` saem vazios, e a coluna de invariantes diz `não convergiu`.

A alternativa — publicar `4238 ÷ 2602s = 1.63` — seria pior do que não publicar. Aquele número tem unidade de throughput, cabe na coluna, e desenha um ponto no gráfico que diz *"o pessimista entrega 1.63 lances por segundo sob contenção máxima"*. O que ele mede de verdade é *"o pessimista aceitou 4238 lances e depois levou quarenta minutos para drenar"*, que é uma frase completamente diferente e muito mais interessante. Uma célula que não converge não tem taxa; ela tem um fato, e o fato vai para a coluna de avisos.

### A célula interrompida não é reprovada, e isso é deliberado

Ela não dispara recusa nenhuma. O `checker.json` dela é verde, os oito invariantes valem, e ela **está no gráfico** — como um ponto com nome em vez de um ponto com número.

Reprovar a matriz por causa dela seria transformar o achado mais forte do projeto na razão de não haver resultado. É o inverso da decisão 102: lá, afrouxar o threshold para terminar seria escolher terminar em vez de medir; aqui, reprovar a fatia por causa da célula que **é** a medição seria escolher não medir para poder reprovar.

O que ela não pode fazer é virar controle. Se a célula 01 ou a 37 for interrompida, o controle não computa e a matriz recusa — pela recusa que já existe.

### O plano vira um parâmetro, e não um segundo script

`PLAN=full|slice` no laço, `-plan full|slice` no agregador, com `full` como default nos dois. Um segundo `run-matrix-slice.sh` seria uma cópia de 250 linhas cujo pré-voo, cujas esperas e cujas guardas de `rm -rf` sairiam de sincronia no primeiro conserto — o mesmo argumento da decisão 109, que tirou as duas esperas de dentro do `run-all.sh`.

Os nomes das células **não mudam**: a fatia roda `01`, `02`, `03`, `13`, `14`, `15`, `25`, `26`, `27` e `37`, com os números que elas têm no plano completo. Assim uma célula medida na fatia é comparável, byte a byte, com a mesma célula de uma matriz completa futura — e um diretório de fatia pode ser retomado como matriz completa sem renomear nada.

### Idempotencia

Herdada inteira da spec 01, e nada acrescentado. O `run-cell.sh` continua resetando banco e Redis antes do warmup e outra vez antes da carga; `RESUME=1` continua pulando só o que está verde; o agregador continua sendo função pura dos diretórios.

O único ponto novo: uma célula interrompida **conta como pronta** para a retomada, porque o `checker.json` dela sai 0. Isso é correto — refazê-la produziria outra interrupção, no mesmo lugar, custando o mesmo orçamento.

### Observabilidade

**Zero série nova**, de novo, e pelo mesmo motivo da spec 01: tudo já foi instrumentado, e este PR só lê.

## Requisitos Funcionais

### RF01 - `bench/run-matrix.sh`: o plano da fatia

`PLAN=slice` seleciona 10 células do plano de 37, na ordem em que já estão:

```text
01-optimistic-a1-ramp-immediate
02-pessimistic-a1-ramp-immediate
03-shard-a1-ramp-immediate
13-optimistic-a10-ramp-immediate
14-pessimistic-a10-ramp-immediate
15-shard-a10-ramp-immediate
25-optimistic-a1000-ramp-immediate
26-pessimistic-a1000-ramp-immediate
27-shard-a1000-ramp-immediate
37-control-optimistic-a1-ramp-immediate
```

`PLAN=full` é o default e não muda em nada. `--dry-run` e `--only` continuam funcionando, e `--dry-run` com `PLAN=slice` imprime 10 linhas.

A estimativa impressa antes da primeira célula passa a somar `CELL_BUDGET` para as células que podem estourar, em vez de mentir 3 minutos.

### RF02 - `bench/run-cell.sh`: o orçamento da carga medida

`CELL_BUDGET`, em segundos, default vazio — e vazio significa **o comportamento de hoje, sem timeout nenhum**. Toda célula de toda spec anterior continua se comportando exatamente como se comporta.

Quando setado, só a carga medida é envolvida. O warmup não é, os resets não são, o checker não é.

O sinal é `INT`, com um `KILL` de misericórdia depois de uma folga, e o motivo do `INT` é o `handleSummary`: um k6 morto não escreve `client.json`, e a célula viraria exit 2 — ausência de resultado no lugar de um resultado curto.

Estourar o orçamento **não** é erro: o script segue para o `env.json` e para o checker, exatamente como numa célula normal.

### RF03 - `bench/env.sh`: o registro da interrupção

Um campo, dentro do bloco `cell` que já existe:

```json
"cell": { "strategy": "...", "auctions": 1, "...": "...", "interrupted": true }
```

Falso em toda célula que não estourou, e é o único lugar onde a interrupção fica registrada. O `cmd/checker` **não** passa a ler o campo: ele julga corretude, e uma célula curta não é uma célula incorreta — a mesma separação de camadas da decisão 97.

### RF04 - `cmd/matrix`: o plano parametrizado e a linha sem taxa

`-plan full|slice`, default `full`. As nove recusas passam a rodar contra o plano escolhido — R1 conta as células desse plano, e um diretório fora dele continua sendo intruso.

Para a célula com `interrupted: true`:

| Coluna | Valor |
| --- | --- |
| Aceitos/s, Conflitos/s, Tentativas/aceito | **vazias** — no JSON, `null` |
| p95 confirmação, Exauridos | publicadas: são distribuições, não taxas, e a janela não as dilui |
| Invariantes | `não convergiu` |
| Avisos | `interrompida em <CELL_BUDGET>s: a fila do lock não drenou` |

`accepted` continua publicado como contagem absoluta — é um fato sobre a célula, e o que não existe é a taxa.

R7 (`durationMs` ausente ou ≤ 0) e R8 (`accepted == 0`) continuam valendo para ela: uma célula interrompida ainda precisa ter medido alguma coisa.

Se a célula 01 ou a 37 estiver interrompida, o controle não é computado e a matriz recusa por R9 — um controle medido sobre uma janela truncada não é um controle.

### RF05 - `matrix.md`: o gráfico

Além da tabela que a spec 01 já publica, o rodapé ganha o gráfico principal em texto, uma linha por estratégia sobre os três níveis de contenção:

```text
aceitos/s por contenção

              1 leilão   10 leilões   1000 leilões
Otimista          7.3        142.1          891.4
Pessimista          —        118.7          402.9
Single-writer    49.9        387.2          «««
```

É ASCII e não SVG porque o consumidor é a etapa 6, que escreve markdown, e porque um gráfico que se cola num `benchmark.md` vale mais hoje do que um arquivo de imagem que alguém precisa abrir. O ponto de cruzamento, quando existir, é marcado.

Uma célula sem taxa aparece como `—`, nunca como `0`.

### RF06 - O resto do sistema não muda

`git diff --name-only` vazio para `internal/`, `cmd/auctiond/`, `cmd/closerd/`, `cmd/seed/`, `cmd/checker/`, `migrations/`, `deploy/`, `docker-compose.yaml`, `Dockerfile`, `.env.example`, `go.mod`, `go.sum`, `bench/bid-storm.js`, `chaos/`.

Em particular: **nenhum `lock_timeout`**, nenhuma mudança no apostador, nenhuma engine tocada.

### RF07 - Testes

`cmd/matrix` ganha os casos novos, sobre o helper que já existe:

| Caso | Espera |
| --- | --- |
| 10 células coerentes com `-plan slice` | exit 0, `publishable: true`, 9 linhas, controle OK |
| Célula 14 faltando com `-plan slice` | exit 2, nomeando `14-…` |
| Plano `full` sobre um diretório de fatia | exit 2, nomeando as 27 ausentes |
| Célula 02 com `interrupted: true` | exit 0, linha com taxas `null` e `não convergiu` |
| Célula 01 interrompida | exit 2, recusa do controle |
| `interrupted: true` com `accepted: 0` | exit 2 — R8 continua valendo |

Os scripts não ganham teste automatizado, pela razão da spec 01: o aceite deles é C2 e C3.

## Requisitos Nao Funcionais

- A execução completa cabe em **~35 minutos** numa máquina ociosa
- `shellcheck` sem aviso em `bench/run-matrix.sh`, `bench/run-cell.sh`, `bench/env.sh`, `bench/wait.sh`
- `gofmt -l .` vazio, `go vet ./...` limpo, `go test ./... -race` verde
- Uma célula sem `CELL_BUDGET` se comporta byte a byte como hoje

## Budget do PR

Até 5 arquivos e aproximadamente 250 linhas.

Se estourar, o corte é o gráfico ASCII da RF05: a tabela já carrega os nove números e o gráfico é derivável dela na mão, em três linhas, por quem escreve a etapa 6.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: alto
- Referencia permitida: `docs/specs/etapa-5/01-spec-matriz.md`, `docs/decisoes/etapa-5.md`, `docs/projeto/benchmark.md`

Prompt:

```text
Implemente docs/specs/etapa-5/02-spec-fatia-do-grafico.md no repositorio bid-storm.

Escopo: apenas RF01..RF07. NAO implemente sweep de pool, dashboard, painel,
serie nova, nem lock_timeout em engine nenhuma.

Regras:
- PLAN e CELL_BUDGET sao opcionais. Sem eles, todo script se comporta
  exatamente como hoje: prove isso.
- O sinal do orcamento e SIGINT, nunca SIGKILL: o handleSummary tem de rodar.
- Celula interrompida NAO reprova a matriz, mas tambem NAO ganha taxa.
  null no JSON, travessao na tabela, nunca zero.
- Se a celula 01 ou a 37 for interrompida, o controle recusa.
- Rode C1, C2 e C3 e cole a saida real. C3 leva ~35min: rode.
```

## Arquivos Esperados

Editar:

```text
bench/run-matrix.sh          PLAN=slice, e a estimativa que soma o orcamento
bench/run-cell.sh            CELL_BUDGET em volta da carga medida, com SIGINT
bench/env.sh                 cell.interrupted
cmd/matrix/main.go           -plan, e a leitura de interrupted
cmd/matrix/report.go         a linha sem taxa, e o grafico do rodape
```

## Testes

Editar:

```text
cmd/matrix/matrix_test.go    os seis casos da RF07
```

## Checkpoints Mensuraveis

### C1 - Unidade, e a compatibilidade para trás

```bash
go test ./cmd/matrix/... -race -count=1 -v
gofmt -l . && go vet ./...
shellcheck bench/run-matrix.sh bench/run-cell.sh bench/env.sh bench/wait.sh

# uma celula sem CELL_BUDGET e sem PLAN continua identica
RUN=smoke-sem-budget AUCTIONS=1 SCENARIO=smoke POLICY=immediate \
  STRATEGY=optimistic bench/run-cell.sh; echo "exit=$?"
jq -r '.cell.interrupted' bench/results/smoke-sem-budget/env.json   # false
```

Aceite: os seis casos da RF07 passam; a célula sem orçamento sai 0 com `interrupted: false`.

### C2 - O plano da fatia, antes de qualquer carga

```bash
PLAN=slice bench/run-matrix.sh --dry-run
PLAN=full  bench/run-matrix.sh --dry-run | grep -c .
```

Aceite: 10 linhas na fatia, exatamente as da RF01; 37 no plano completo, inalterado.

### C3 - A fatia inteira, e o gráfico

```bash
make down && make up && sleep 20
git status --porcelain
time PLAN=slice CELL_BUDGET=240 bench/run-matrix.sh; echo "exit=$?"

M=$(ls -1dt bench/results/m* | head -1)
jq -r '.publishable, (.cells | length)' "$M/matrix.json"
jq -r '.cells[] | "\(.strategy) a\(.auctions): \(.acceptedPerSecond // "não convergiu")"' "$M/matrix.json"
jq -r '.control | "controle: \(.divergence) · \(.verdict)"' "$M/matrix.json"
cat "$M/matrix.md"
```

Aceite:

- As 10 células rodam em ~35 minutos, todas com `checker.json` em `exit: 0`
- `publishable: true`, 9 linhas
- A célula pessimista de 1 leilão sai com `acceptedPerSecond: null` e `não convergiu`, e **nenhuma outra** sai assim
- O gráfico do rodapé tem as três linhas sobre os três níveis de contenção
- A saída do `time` e o `matrix.md` vão colados no PR: é este checkpoint que entrega o resultado

## Smoke Manual

```bash
# o orcamento estoura e a celula sobrevive
CELL_BUDGET=30 RUN=smoke-budget AUCTIONS=1 SCENARIO=ramp POLICY=immediate \
  STRATEGY=pessimistic bench/run-cell.sh; echo "exit=$?"
jq -r '.cell.interrupted' bench/results/smoke-budget/env.json
jq -r '.durationMs, .accepted' bench/results/smoke-budget/client.json
```

Aceite manual: sai 0, `interrupted: true`, `client.json` existe com `durationMs` próximo de 30000 e `accepted` maior que zero. É a prova de que o SIGINT deixou o `handleSummary` rodar.

## Definicao De Pronto

- RF01 a RF07 implementados
- C1, C2 e C3 executados, com a saída real colada
- 9 células mais o controle, num único commit, com `dirty: false`
- O gráfico principal existe como dado, e a tabela de *Resultados* de `benchmark.md` pode ser preenchida a partir de um arquivo
- A célula que não converge está no gráfico com nome, e sem número inventado
- Nenhuma série nova, nenhum `lock_timeout`, nenhuma engine tocada
- Está escrito, no PR e no texto que a etapa 6 vai publicar, que este gráfico é **uma fatia**: sem `last_second_spike` e sem `jitter`, e portanto sem resposta ainda para a crítica do backoff
