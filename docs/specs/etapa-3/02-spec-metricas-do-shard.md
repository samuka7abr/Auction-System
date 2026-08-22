# Etapa 3 — Spec 02: Métricas do mecanismo do shard

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md) · [decisões da etapa 2](../../decisoes/etapa-2.md) · [decisões da etapa 3](../../decisoes/etapa-3.md) · [etapa 3, spec 01](01-spec-engine-single-writer.md)

## Contexto

A [spec 01](01-spec-engine-single-writer.md) entregou a terceira engine inteira — roteamento, propriedade exclusiva, hidratação, lote, commit e recuperação — e a entregou **medida como as outras duas e não mais que isso**: `bid_confirm_duration_seconds{strategy="shard"}` e `bid_outcomes_total{strategy="shard"}`, vindos do decorator, na mesma fronteira das outras.

A decisão 58 registrou o que ficou faltando e por quê. O argumento central do desenho desta engine é *"durabilidade custa isto aqui, e está exposto em vez de escondido no contrato"*, e ele está **afirmado e não medido**. O que a spec 01 conseguiu provar sem série nova foi o outro lado — que o lote existe e amortiza — contando transações em `pg_stat_database` antes e depois de duas células. É prova de mecanismo feita com régua emprestada, e ela não sobrevive à etapa 5: um gráfico com 36 células não se explica com dois deltas colados num PR.

Esta spec fecha a etapa 3 publicando as quatro séries que faltam:

| Série | Tipo | Pergunta |
| --- | --- | --- |
| `bid_accept_duration_seconds` | Histogram | Quanto custa decidir, quando decidir não toca o banco |
| `journal_lag_seconds` | Histogram | Quanto custa a durabilidade, por lance aceito |
| `shard_batch_size` | Histogram | Quantos lances por commit — o ganho, medido em vez de contado à mão |
| `shard_inbox_depth{shard}` | Gauge | Quão perto do teto de 1024 a fila chegou, e se algum shard virou gargalo |

Duas delas chegam diferentes do que [observabilidade.md](../../projeto/observabilidade.md) publicou, e as decisões 59 e 60 dizem exatamente onde e por quê.

Sustentam esta spec as decisões **16** (métrica entra junto da engine que a alimenta), **23** (as três engines são medidas de uma fronteira só), **26** (séries que se leem juntas compartilham fronteiras de bucket), **28** (todo nome de série mora em `internal/metrics`, e a engine recebe uma interface de um método), **48** (o lote vale 1 na célula de 1 leilão), **52** (as três condições de fechamento do lote), **53** (o lote que aborta), **55** (a janela entre decidido e durável é exatamente um lote) e **58** (por que estas quatro séries não nasceram na spec 01), mais as decisões **59** a **67**, tomadas aqui.

## Objetivo

Publicar as quatro séries do mecanismo single-writer, de forma que o custo da durabilidade seja lido **direto**, sem subtrair série de série, e que o tamanho do lote previsto pela decisão 48 e contado à mão na spec 01 reapareça como distribuição.

O sistema deve:

- Registrar as quatro séries em `internal/metrics`, com os nomes de `observabilidade.md` e as duas emendas das decisões 59 e 60
- Medir o aceite da entrada de `PlaceBid` até a decisão em memória, e o lag da decisão até o retorno do commit
- Dar aos dois histogramas novos os buckets do `confirm` estendidos para baixo, para que a leitura contra ele continue exata e a resolução chegue a microssegundos
- Publicar a profundidade do inbox sem pagar nada no caminho quente do lance
- Fazer isso sem que `internal/bid/shard` aprenda um único nome de série, e sem tocar nas outras duas engines, no contrato HTTP, no harness ou no checker

## Fora de Escopo

- Qualquer mudança de comportamento da engine. Esta spec **não** altera decisão, lote, commit, despejo ou recuperação — se um checkpoint exigir mexer em `decide` ou em `commit` além de gravar um instante e chamar `Observe`, pare e reporte
- Série para custo de rejeição — decisão 66
- Painel Grafana, `deploy/grafana/` e o cruzamento `shard_batch_size` × `confirm`: é etapa 5, e o dashboard nasce lá com as três estratégias juntas
- `stream_pending_entries`, Redis Streams, `closerd` e caos — etapa 4
- Matriz de 36 células e sweep de `DB_POOL_SIZE` — etapa 5. Esta spec roda células avulsas e **não conclui nada sobre qual estratégia vence**
- Pipeline de commit (decisão 55) e bissecção de lote (decisão 53), que continuam registrados como não feitos
- Qualquer variável de ambiente nova, inclusive para buckets. Shards, lote, inbox e fronteiras de histograma são constantes (decisão 56)
- Alerta, regra de gravação e SLO: nada em `deploy/prometheus/` muda

## Fluxo

```text
boot (BID_STRATEGY=shard)
  └── internal/app.NewEngine
        ├── metrics.NewShard(reg)  → registra as quatro séries, todas visíveis no /metrics
        │                            ANTES do primeiro lance (nada é criado sob demanda)
        ├── shard.New(pool, shard.Observers{Accept, Lag, Batch})
        └── metrics.RegisterShardInbox(reg, engine)  → collector: lê len(inbox) no scrape

POST /auctions/:id/bids
  └── instrumented (decorator)      ── t0: começa bid_confirm_duration_seconds
        └── shard.Engine.PlaceBid   ── t1: começa bid_accept_duration_seconds (decisão 62)
              └── inbox  ─────────────── a espera daqui entra no aceite, de propósito
                    │
      goroutine do shard
        ├── rejeita  → responde na hora, NADA é observado aqui (decisão 66)
        └── aceita   ── t2: Observe(t2 - t1) em bid_accept_duration_seconds
              │              grava decidedAt = t2 local, NUNCA o created_at (decisão 62)
              └── entra no lote
                    │
                    └── fecha o lote (256 · inbox vazio · linger de 1ms)
                          ├── Observe(len(batch)) em shard_batch_size  ← sempre, decisão 65
                          └── 1 statement
                                ├── ok   ── t3: Observe(t3 - decidedAt) por lance,
                                │            em journal_lag_seconds
                                │            e só então responde Accepted
                                └── erro → despejo + 503, e NENHUM lag observado

GET /metrics  (a cada 5s)
  └── collector do inbox → shard_inbox_depth{shard="0".."7"} = len(inbox) naquele instante
```

O que **não** aparece no fluxo: nenhum `Inc`/`Dec` no envio e no recebimento do canal (decisão 63), e nenhuma série alimentada pelo decorator além das duas que ele já alimentava.

## Decisoes Tecnicas

### O custo da durabilidade é medido, não subtraído

A leitura óbvia seria `confirm − accept`. A decisão 58 já a descartou e vale repetir aqui, porque é o motivo de esta spec existir: as duas séries observam **populações diferentes**. `confirm` agrega todos os desfechos; o custo da durabilidade só existe nos aceites. Na célula de 1 leilão, onde a rejeição é a esmagadora maioria das requisições, os dois p95 caem na mesma população de rejeições, a subtração dá perto de zero e o gráfico anunciaria **durabilidade de graça** — errando a favor da tese do projeto, que é a única direção de erro que este repositório não pode se permitir.

`journal_lag_seconds` resolve isso sendo medido onde a pergunta mora: dentro do shard, por lance aceito, da decisão até o retorno do commit. C4 mede as duas leituras lado a lado e mostra a subtração errando — o checkpoint prova o erro que a série nova evita, em vez de só afirmar que ele existiria.

### `bid_accept_duration_seconds` perde o label, e `journal_lag_seconds` deixa de ser gauge

Duas emendas a `observabilidade.md`, decisões 59 e 60.

O label `strategy` sai porque nas outras duas engines a decisão e a durabilidade acontecem no mesmo statement: alimentá-las nesta série publicaria uma cópia de `confirm` com outro nome, e um leitor concluiria que o custo de durabilidade delas é zero, quando ele é **indivisível**. O precedente é o de `lock_wait_duration_seconds` (decisão 28): um label com um valor só sugere que os outros dois reportam zero, e zero é uma afirmação diferente de silêncio.

`journal_lag_seconds` deixa de ser `Gauge` porque, com o commit síncrono da decisão 55, a janela entre decidido e durável é exatamente um lote — centenas de microssegundos a poucos milissegundos. Um gauge lido a cada 5 segundos amostraria essa janela quase sempre fora dela. O nome fica: renomear custaria um `grep` em três documentos e não muda nenhuma curva.

### Os buckets são os do `confirm`, estendidos para baixo

```go
// internal/metrics/shard.go
var shardBuckets = append(
    []float64{0.000_01, 0.000_025, 0.000_05, 0.000_1, 0.000_25, 0.000_5},
    confirmBuckets...,
)
```

A decisão 26 exige fronteiras compartilhadas entre séries que se leem juntas, ou a comparação passa por interpolação de quantil. Copiar `confirmBuckets` literalmente não serviria: o piso dele é 1ms e a decisão em memória vive três ordens de grandeza abaixo, então **tudo cairia no primeiro bucket** e o histograma diria só "menos de um milissegundo".

O superconjunto atende as duas regras: toda fronteira do `confirm` continua existindo, então a leitura bucket a bucket contra ele é exata em todo ponto onde ele tem o que dizer; abaixo de 1ms, onde ele não tem, as séries novas têm resolução própria. Os dois histogramas usam o mesmo conjunto, porque a soma dos dois é a vida do lance aceito e somar histogramas de fronteiras diferentes não significa nada (decisão 61).

### O relógio do lag é local, e nunca é o `created_at`

O `created_at` que o lote grava é o instante da decisão **corrigido pelo offset do banco** (decisões 50 e 51). Medir o lag a partir dele mediria o commit somado ao offset entre dois relógios — e faria isso em silêncio: em Go, `time.Now().Add(d)` carrega a leitura monotônica junto, então `time.Since` sobre esse valor devolve um número plausível, errado por exatamente `skew`.

`pendingBid` ganha um campo local próprio, gravado na decisão, e o `created_at` continua servindo só ao que a decisão 51 pediu dele (decisão 62).

### A profundidade é lida no scrape, e o teto continua visível de outro jeito

`shard_inbox_depth` vem de um `prometheus.Collector` que lê `len(inbox)` dos oito shards a cada raspagem. A alternativa — `Inc` no envio, `Dec` no recebimento — são duas escritas atômicas por lance no caminho quente da engine cuja tese é que o caminho quente dela é barato: a medição financiaria o resultado.

**Limite registrado:** um pico de backpressure mais curto que os 5 segundos de scrape é invisível nesta série. Ele não some do sistema — o enfileiramento acontece dentro da fronteira do decorator (decisão 62), então esperar por um inbox cheio aparece como latência em `bid_confirm_duration_seconds{strategy="shard"}`, que é onde ela machuca. O gauge existe para responder "quão perto de 1024 isso chegou", e 24 amostras ao longo de uma célula de dois minutos bastam para essa pergunta (decisão 63).

### O lote é observado mesmo quando o commit falha

`shard_batch_size` é observado uma vez por tentativa de commit, antes do statement. O tamanho é propriedade do laço de acumulação, que já terminou; condicionar ao sucesso faria a série desaparecer **exatamente no incidente que alguém usaria a série para explicar** — o lote abortado da decisão 53, onde se quer saber quantos lances caíram junto.

`journal_lag_seconds`, ao contrário, só é observado no commit bem-sucedido: um lote que abortou não tem instante de durabilidade (decisão 65).

### Observabilidade

As quatro séries são **as quatro desta spec**, e não há uma quinta. Todas são registradas no boot, sem criação sob demanda: o `/metrics` de um processo que ainda não recebeu lance nenhum já traz `bid_accept_duration_seconds_count 0`, `journal_lag_seconds_count 0`, `shard_batch_size_count 0` e os oito `shard_inbox_depth{shard="n"} 0`. É a mesma razão pela qual o decorator liga o label da estratégia no boot — um painel vazio precisa ser distinguível de um painel quebrado, e é isso que permite a C2 conferir a instrumentação antes de gastar uma célula.

`bid_confirm_duration_seconds` e `bid_outcomes_total` **não mudam**: nem de bucket, nem de label, nem de fronteira de medição. São o eixo de comparação das três estratégias, e a etapa 5 compara células rodadas antes e depois desta spec.

Nada é publicado sobre custo de rejeição (decisão 66) e nada é publicado por shard além do gauge (decisão 64).

## Requisitos Funcionais

### RF01 - As quatro séries, em `internal/metrics`

`internal/metrics/shard.go` registra e devolve:

| Série | Tipo | Labels | Help |
| --- | --- | --- | --- |
| `bid_accept_duration_seconds` | Histogram | nenhum | tempo até a decisão em memória, só nos aceites |
| `journal_lag_seconds` | Histogram | nenhum | tempo da decisão até o commit durável, por lance aceito |
| `shard_batch_size` | Histogram | nenhum | lances por commit |
| `shard_inbox_depth` | GaugeVec via collector | `shard` | profundidade do inbox no instante do scrape |

Assinatura:

```go
func NewShard(reg prometheus.Registerer) (accept, lag, batch prometheus.Observer)

// InboxDepths é o que a engine expõe ao collector: uma profundidade por shard,
// lida no scrape.
type InboxDepths interface{ InboxDepths() []int }

func RegisterShardInbox(reg prometheus.Registerer, src InboxDepths)
```

São duas funções e não uma porque a ordem obriga: os observadores existem antes da engine, e a engine existe antes do collector que a lê.

Nenhum nome de série aparece fora deste pacote.

### RF02 - Buckets

Os três histogramas usam `shardBuckets`, definido como `confirmBuckets` precedido de `10µs, 25µs, 50µs, 100µs, 250µs, 500µs` — exceto `shard_batch_size`, que usa `prometheus.ExponentialBuckets(1, 2, 9)`, ou seja `1, 2, 4, ..., 256`.

A fronteira `le="1"` do lote não é decorativa: é ela que faz a afirmação da decisão 48 — *"na célula de 1 leilão o lote vale 1"* — ser lida como razão direta, sem quantil.

`confirmBuckets` **não** é alterado, e nenhuma série existente muda de fronteira.

### RF03 - O aceite é observado

`shard.Engine` passa a receber os observadores:

```go
type Observers struct {
    Accept prometheus.Observer // bid_accept_duration_seconds
    Lag    prometheus.Observer // journal_lag_seconds
    Batch  prometheus.Observer // shard_batch_size
}

func New(pool *pgxpool.Pool, obs Observers) *Engine
```

`PlaceBid` grava o instante de entrada no `command`, **antes** do `select` de enfileiramento. Na decisão de aceite, e só nela, o shard observa `time.Since(startedAt)` em `Accept`.

Rejeição, `NotFound` e erro de infraestrutura não observam nada nesta série.

### RF04 - O lag é observado

`pendingBid` ganha um instante local gravado na decisão, distinto do `created_at` corrigido pelo skew.

No commit **bem-sucedido**, e antes ou depois de responder `Accepted` — mas uma vez por lance —, o shard observa `time.Since(decidedAt)` em `Lag`. No commit que falha, nada é observado nesta série.

### RF05 - O tamanho do lote é observado

`shard_batch_size` recebe `float64(len(batch))` uma vez por chamada de `commit`, antes do statement, independentemente do resultado.

### RF06 - A profundidade é exposta ao collector

`shard.Engine` ganha `InboxDepths() []int`, que devolve `len(inbox)` dos oito shards, na ordem dos shards, e é seguro chamar de qualquer goroutine.

O collector emite as oito séries a **toda** raspagem, inclusive quando todas valem zero.

Nenhuma chamada a `Inc`, `Dec` ou `Set` acontece no envio ou no recebimento do canal.

### RF07 - Wiring em `internal/app`

`NewEngine`, no caso `shard`, constrói os observadores, passa-os à engine, registra o collector do inbox e embrulha o resultado no mesmo decorator e no mesmo carrier de sempre. Os casos `optimistic` e `pessimistic` não mudam.

Construir a engine continua **sem tocar o banco**: o teste de `internal/app` a constrói com pool `nil`, e registrar as quatro séries acontece contra o registry.

### RF08 - Testes

`internal/metrics/shard_test.go`:

- As quatro séries existem no `/metrics` de um registry recém-construído, com contagem zero, antes de qualquer observação
- `bid_accept_duration_seconds` e `journal_lag_seconds` têm **todas** as fronteiras de `bid_confirm_duration_seconds`, conferidas uma a uma contra a série que o decorator publica no mesmo registry — o mesmo teste que a decisão 26 já exige de `lock_wait_duration_seconds`
- As duas têm fronteiras abaixo de 1ms, e a menor delas é 10µs
- Nenhuma das quatro tem label `strategy`; `shard_inbox_depth` tem label `shard` e nenhuma outra tem label
- O collector do inbox emite uma série por shard a cada `Gather`, com o valor devolvido pela fonte naquele instante — provado com uma fonte falsa que muda de valor entre dois `Gather`

`internal/bid/shard/metrics_test.go`, com observadores falsos e sem Prometheus:

- Aceite observa `Accept` uma vez; rejeição, `NotFound` e leilão fechado não observam nada
- Um commit bem-sucedido de N lances observa `Lag` N vezes e `Batch` uma vez
- Um commit que falha observa `Batch` e **não** observa `Lag` — o caso do segundo escritor da spec 01, reaproveitado
- `InboxDepths()` devolve `numShards` valores

A suíte de conformidade **não muda**, e os testes da spec 01 continuam passando com a assinatura nova de `New`.

### RF09 - O resto do sistema não muda

O diff de implementação não pode conter arquivos em:

```text
internal/httpapi/
internal/store/
internal/idem/
internal/bid/engine.go
internal/bid/outcome.go
internal/bid/enginetest/
internal/bid/optimistic/
internal/bid/pessimistic/
internal/metrics/bid.go
internal/metrics/lock.go
internal/metrics/pool.go
internal/metrics/idempotency.go
cmd/
bench/
migrations/
deploy/
docker-compose.yaml
Makefile
go.mod
go.sum
```

`internal/metrics/bid.go` está nessa lista de propósito, e é a linha mais importante dela: o comentário de `Instrument` já anuncia a exceção desta spec — *"a única exceção registrada é `bid_accept_duration_seconds`, que não existe fora do shard"* —, e o `confirm` que ele publica é o eixo em que a etapa 5 compara células rodadas antes e depois daqui. Se ele precisar mudar, **pare e reporte**.

## Requisitos Nao Funcionais

- Nenhuma dependência nova
- `internal/bid/shard` passa a importar `prometheus` para o tipo `Observer`, e continua **sem** importar `internal/metrics`, `internal/app` e Gin (decisão 67)
- Nenhum nome de série fora de `internal/metrics`
- Nenhuma alocação nova por lance rejeitado; no aceite, apenas o instante já gravado no `command`
- Nenhuma escrita compartilhada nova: os observadores do Prometheus são seguros para concorrência, e o collector só lê `len` de canal
- `go test ./... -race` limpo; `gofmt -l .` vazio; `go vet ./...` sem saída
- `bid_confirm_duration_seconds` e `bid_outcomes_total` byte a byte iguais no `/metrics` de antes: mesmos buckets, mesmos labels, mesma fronteira de medição
- Nenhum log novo, por requisição ou por lote

## Budget do PR

Até 6 arquivos e aproximadamente 300 linhas de código próprio.

É menos da metade da spec 01, e tem que ser: a engine já existe, e esta spec acrescenta um construtor de séries, um collector, três chamadas a `Observe` e um método de leitura. Se o PR passar de 6 arquivos ou 300 linhas, ou se `decide` e `commit` mudarem além de gravar um instante e chamar `Observe`, alguma coisa está sendo reescrita em vez de instrumentada — **pare e reporte**.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: medio
- Referencia permitida: `docs/projeto/observabilidade.md`, `docs/projeto/estrategias.md`, `docs/decisoes/etapa-1.md`, `docs/decisoes/etapa-2.md`, `docs/decisoes/etapa-3.md`, `docs/specs/etapa-3/01-spec-engine-single-writer.md`, `docs/specs/etapa-3/02-spec-metricas-do-shard.md`

Prompt:

```text
Implemente docs/specs/etapa-3/02-spec-metricas-do-shard.md no repositorio bid-storm.

Leia antes de comecar:
  docs/specs/etapa-3/02-spec-metricas-do-shard.md   (a spec — a autoridade)
  docs/decisoes/etapa-3.md                          (o porque; decisoes 59 a 67,
                                                     e a 58, que explica por que
                                                     estas series nao nasceram
                                                     na spec 01)
  internal/metrics/lock.go e internal/metrics/lock_test.go
                                                    (o padrao exato a seguir:
                                                     serie sem label, buckets
                                                     compartilhados, observer de
                                                     um metodo)
  internal/bid/shard/                                (a engine ja pronta)

ATENCAO: a spec emenda observabilidade.md em dois pontos, e o que esta la NAO
deve ser copiado:
  - bid_accept_duration_seconds entra SEM o label {strategy} (decisao 59)
  - journal_lag_seconds e Histogram, nao Gauge, e e observado por lance
    aceito (decisao 60)

E emenda um requisito nao funcional da spec 01: internal/bid/shard AGORA
importa prometheus, para o tipo Observer, exatamente como internal/bid/
pessimistic ja faz (decisao 67). Continua sem importar internal/metrics.

Escopo: apenas RF01..RF09. Esta spec NAO muda comportamento nenhum da engine
— decisao, lote, commit, despejo e recuperacao ficam como estao. Se algum
checkpoint parecer exigir mudar decide() ou commit() alem de gravar um
instante e chamar Observe, pare e reporte.

Regras:
- Modulo: github.com/samuka7abr/bid-storm
- NAO altere internal/httpapi, internal/store, internal/idem,
  internal/metrics/bid.go, internal/metrics/lock.go, internal/metrics/pool.go,
  internal/metrics/idempotency.go, internal/bid/engine.go,
  internal/bid/outcome.go, internal/bid/enginetest/, internal/bid/optimistic/,
  internal/bid/pessimistic/, cmd/, bench/, migrations/, deploy/,
  docker-compose.yaml, Makefile, go.mod nem go.sum.
- confirmBuckets NAO muda, e nenhuma serie existente muda de bucket ou de
  label. A etapa 5 compara celulas rodadas antes e depois deste PR.
- Nenhum Inc/Dec no envio ou no recebimento do canal: a profundidade do inbox
  e lida no scrape, por um collector (decisao 63).
- O lag e medido de um instante LOCAL gravado na decisao, nunca do created_at
  corrigido pelo skew (decisao 62).
- shard_batch_size e observado inclusive quando o commit falha; journal_lag
  so no commit bem-sucedido (decisao 65).
- Nenhuma variavel de ambiente nova, inclusive para buckets.
- Rode os checkpoints C1..C5 e cole a saida real de cada um. Checkpoint sem
  saida nao conta como aceito.
- Se estourar o budget de 6 arquivos / ~300 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Criar:

```text
internal/metrics/shard.go             as quatro series, os buckets e o collector
internal/metrics/shard_test.go        fronteiras, labels e o collector sob Gather
internal/bid/shard/metrics_test.go    quem observa o que, com observadores falsos
```

Editar:

```text
internal/bid/shard/engine.go   Observers, New, o instante de entrada, InboxDepths
internal/bid/shard/shard.go    Observe no aceite, no lote e no commit que deu certo
internal/app/engine.go         construir os observadores e registrar o collector
```

`internal/bid/shard/shard_test.go` e `internal/app/engine_test.go` acompanham a assinatura nova de `New` — e se isso custar mais que uma linha por chamada, a assinatura está errada.

## Testes

Adicionar:

```text
internal/metrics/shard_test.go        RF08, primeira metade
internal/bid/shard/metrics_test.go    RF08, segunda metade
```

Editar:

```text
internal/bid/shard/shard_test.go   so a construcao da engine
internal/app/engine_test.go        so o que a assinatura obrigar
```

## Checkpoints Mensuraveis

### C1 - Unidade sob corrida, e a fronteira do diff

```bash
go test ./internal/metrics/... ./internal/bid/shard/... ./internal/app/... -race -count=1 -v
go test ./... -race -count=1
gofmt -l . && go vet ./...
git diff --name-only -- internal/httpapi internal/store internal/idem \
  internal/metrics/bid.go internal/metrics/lock.go internal/metrics/pool.go \
  internal/metrics/idempotency.go internal/bid/engine.go internal/bid/outcome.go \
  internal/bid/enginetest internal/bid/optimistic internal/bid/pessimistic \
  cmd bench migrations deploy docker-compose.yaml Makefile go.mod go.sum
```

Aceite:

- Os testes de RF08 passam, incluindo o das fronteiras conferidas uma a uma contra `confirm`
- A suíte de conformidade do shard continua passando, sem uma linha alterada em `internal/bid/enginetest/`
- `-race` limpo: o collector lê `len` de canal de outra goroutine, e é isso que ele tem de provar
- O último comando não lista arquivo algum

### C2 - As quatro séries existem antes do primeiro lance

```bash
make up && sleep 15
make run STRATEGY=shard && sleep 10

curl -s localhost:8080/metrics | grep -E '^(bid_accept_duration_seconds_count|journal_lag_seconds_count|shard_batch_size_count) '
curl -s localhost:8080/metrics | grep '^shard_inbox_depth'
curl -s localhost:8080/metrics | grep -c 'bid_accept_duration_seconds_bucket.*strategy'
curl -s localhost:8080/metrics | grep 'bid_accept_duration_seconds_bucket' | head -8
curl -s localhost:8080/metrics | grep 'shard_batch_size_bucket'

# o eixo de comparacao nao pode ter mudado
curl -s localhost:8080/metrics | grep 'bid_confirm_duration_seconds_bucket{strategy="shard"' | wc -l
```

Aceite:

- As três contagens saem `0`, e as oito séries de `shard_inbox_depth` saem com `shard="0"` a `shard="7"`, todas zero: nenhuma série é criada sob demanda, e painel vazio é distinguível de painel quebrado
- O `grep -c` de `strategy` em `bid_accept_duration_seconds` devolve `0` (decisão 59)
- As primeiras fronteiras de `bid_accept_duration_seconds` são `1e-05`, `2.5e-05`, `5e-05`, `0.0001`, `0.00025`, `0.0005`, e depois `0.001` — de onde `confirm` começa
- `shard_batch_size` tem `le="1"` e vai até `le="256"`
- `bid_confirm_duration_seconds{strategy="shard"}` continua com 15 buckets, exatamente como antes deste PR

### C3 - O lote previsto na decisão 48, agora como distribuição

```bash
make run STRATEGY=shard && sleep 10

batch() {
  curl -s localhost:8080/metrics | awk -v OFS='\t' '
    /^shard_batch_size_bucket\{le="1"\}/ { le1 = $2 }
    /^shard_batch_size_sum/              { sum = $2 }
    /^shard_batch_size_count/            { count = $2 }
    END { printf "count=%s sum=%s medio=%.2f fracao_le1=%.3f\n",
                 count, sum, sum/count, le1/count }'
}

for n in 1000 1; do
  make bench RUN=e3c3b-shard-$n STRATEGY=shard AUCTIONS=$n POLICY=immediate SCENARIO=ramp
  echo "auctions=$n  $(batch)"
  jq -r '.accepted' bench/results/e3c3b-shard-$n/client.json
done
```

Aceite:

- Na célula de 1 leilão, `fracao_le1` fica perto de 1 e o tamanho médio fica perto de 1: é o número que a decisão 48 **previu antes de existir série**, e que a spec 01 mediu contando transações
- Na célula de 1000 leilões, o tamanho médio é claramente maior que 1, e `fracao_le1` cai
- O `count` da série é da mesma ordem do número de commits que a spec 01 contou em `pg_stat_database` para a mesma célula — as duas réguas concordam, e é isso que aposenta a contagem à mão
- Nenhuma conclusão sobre throughput entra no PR

### C4 - O custo da durabilidade, medido e não subtraído

```bash
p95() {  # $1 = nome da serie, $2 = filtro de label opcional
  curl -s localhost:8080/metrics \
    | grep "^$1_bucket$2" \
    | sed -E 's/.*le="([^"]+)".* ([0-9.e+-]+)$/\1 \2/' \
    | awk -v n="$(curl -s localhost:8080/metrics | grep "^$1_count$2" | awk '{print $2}')" \
        '$2 >= 0.95*n { print $1; exit }'
}

make run STRATEGY=shard && sleep 10
make bench RUN=e3c4b-shard-1 STRATEGY=shard AUCTIONS=1 POLICY=immediate SCENARIO=smoke

echo "accept  p95 = $(p95 bid_accept_duration_seconds)"
echo "lag     p95 = $(p95 journal_lag_seconds)"
echo "confirm p95 = $(p95 bid_confirm_duration_seconds '{strategy="shard"')"
curl -s localhost:8080/metrics | grep -E '^(bid_accept_duration_seconds_count|journal_lag_seconds_count) '
curl -s localhost:8080/metrics | grep 'bid_outcomes_total{strategy="shard"'
```

Aceite:

- `lag p95` é ordens de grandeza maior que `accept p95`: o custo desta engine é a durabilidade, e ele está **publicado** em vez de escondido no contrato — que é a frase que a spec 01 só pôde afirmar
- `confirm p95 − accept p95` fica perto de `confirm p95`, ou seja, a subtração ingênua **não** reproduz o `lag p95`. É o erro que a decisão 58 previu, demonstrado com números em vez de prosa
- `bid_accept_duration_seconds_count` e `journal_lag_seconds_count` são iguais entre si e iguais a `bid_outcomes_total{outcome="accepted"}` — se `accept` contar mais que `lag`, algum lote abortou, e aí `outcome="error"` explica a diferença
- Na célula de 1 leilão, `accept count` é uma fração pequena de `confirm count`: a população das duas séries é diferente, que é o fato inteiro desta spec

### C5 - A profundidade é lida no scrape, e o teto de 1024 não é a história

```bash
make run STRATEGY=shard && sleep 10
( while true; do
    curl -s localhost:8080/metrics | grep '^shard_inbox_depth' \
      | sed -E 's/shard_inbox_depth\{shard="([0-9])"\} (.*)/\1:\2/' | paste -sd' '
    sleep 5
  done ) > /tmp/depth.txt &
SAMPLER=$!
make bench RUN=e3c5b-shard-1000 STRATEGY=shard AUCTIONS=1000 POLICY=immediate SCENARIO=ramp
kill $SAMPLER
sort -u /tmp/depth.txt | tail -20
```

Aceite:

- As oito séries aparecem em toda amostra, inclusive nas que valem zero
- O máximo lido em qualquer shard fica muito abaixo de 1024: o teto existe e não foi alcançado, que é a afirmação da decisão 56 sendo verificada em vez de suposta
- Nenhuma amostra some nem repete valor de amostra anterior por congelamento — o collector lê no `Gather`, não guarda estado
- O limite é dito no PR: um pico mais curto que 5 segundos é invisível aqui, e apareceria como latência no `confirm` (decisão 63)

## Smoke Manual

Pre-condicoes:

```text
Docker e docker compose v2, jq, uuidgen e make instalados
Portas livres: 5432, 6379, 8080, 9090, 3000
Repositorio limpo, .env criado a partir de .env.example
```

Passos:

```bash
make up && sleep 15
make run STRATEGY=shard && sleep 10
make seed AUCTIONS=1 TRUNCATE=1
export AID=$(jq -r '.[0].id' bench/auctions.json)
export UID=$(uuidgen)

curl -s localhost:8080/metrics | grep -E '_count |^shard_inbox_depth' | grep -E 'accept|journal|batch|inbox'

for cents in 500 900 1300; do
  curl -s -o /dev/null -X POST localhost:8080/auctions/$AID/bids \
    -H "X-User-Id: $UID" -d "{\"amountCents\":$cents}"
done
curl -s -o /dev/null -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":100}'   # rejeitado: nao observa aceite

curl -s localhost:8080/metrics | grep -E '^(bid_accept_duration_seconds_count|journal_lag_seconds_count|shard_batch_size_count|shard_batch_size_sum) '
curl -s localhost:8080/metrics | grep 'bid_outcomes_total{strategy="shard"'
curl -s localhost:8080/metrics | grep '^shard_inbox_depth' | paste -sd' '

# Prometheus enxerga as quatro
curl -s 'localhost:9090/api/v1/label/__name__/values' | jq -r '.data[]' \
  | grep -E 'accept_duration|journal_lag|shard_batch|shard_inbox'
make down
```

Aceite manual:

- Antes dos lances, as três contagens são zero e as oito profundidades existem
- Depois, `bid_accept_duration_seconds_count` e `journal_lag_seconds_count` valem 3 — a rejeição não entrou em nenhuma das duas
- `shard_batch_size_count` vale 3 e `shard_batch_size_sum` vale 3: três lances no mesmo leilão são três lotes de tamanho 1, exatamente como a decisão 48 descreve
- `bid_outcomes_total{outcome="accepted"}` vale 3 e `outcome="too_low"` vale 1
- As oito profundidades voltam a zero depois da carga
- O Prometheus lista os quatro nomes: elas não só existem no `/metrics`, elas estão sendo raspadas
- `make down` derruba tudo sem contêiner órfão

## Definicao De Pronto

- RF01 a RF09 implementados
- C1 a C5 executados, com a saída real colada no PR — checkpoint sem saída não conta como aceito
- As quatro séries no `/metrics` desde o boot, com contagem zero, e nenhuma delas com label `strategy`
- Fronteiras de bucket conferidas uma a uma contra `bid_confirm_duration_seconds`, em teste e não a olho
- `bid_confirm_duration_seconds` e `bid_outcomes_total` inalterados: mesmos buckets, mesmos labels, mesma fronteira de medição
- `go test ./... -race` limpo, e a suíte de conformidade sem uma linha alterada
- `git diff --name-only` vazio para toda a lista de RF09
- O tamanho do lote medido nas duas contenções, concordando com a contagem de transações da spec 01
- O custo da durabilidade lido direto, e a subtração ingênua demonstrada errando em C4
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado pelo PR de implementação
- Com isto, a etapa 3 fecha: a terceira curva existe, passa na mesma suíte e tem o mecanismo dela medido
