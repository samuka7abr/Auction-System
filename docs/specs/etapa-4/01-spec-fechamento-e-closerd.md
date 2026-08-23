# Etapa 4 — Spec 01: Fechamento via Redis Streams e o `closerd`

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md) · [decisões da etapa 2](../../decisoes/etapa-2.md) · [decisões da etapa 3](../../decisoes/etapa-3.md) · [decisões da etapa 4](../../decisoes/etapa-4.md) · [etapa 3, spec 02](../etapa-3/02-spec-metricas-do-shard.md)

## Contexto

A etapa 3 fechou com as três curvas existindo sob a mesma régua: três engines passando na mesma suíte de conformidade, o mecanismo da terceira medido por dentro, e I1 a I7 verdes em todas as células rodadas. O que falta para a etapa 5 poder rodar a matriz não é uma quarta estratégia — é a parte do sistema que ainda é promessa escrita em comentário.

A promessa está em cinco lugares do código, e todos os cinco são cobrados aqui:

- `internal/db/redis.go` diz, no comentário do construtor, que *"os Streams da etapa 4 precisam deste mesmo cliente"*
- `internal/store/auctions.go` explica que `status` é derivado e nunca devolvido cru **até o `closerd` da etapa 4 começar a escrever a coluna**
- `internal/bid/engine.go` documenta `IsClosed` como a regra única que já cobre `StatusClosed`, para que nenhum handler mude quando a coluna passar a ser escrita
- `cmd/checker/invariants.go` registra, em I4, que aquele é *"o invariante que o `closerd` da etapa 4 não pode ser capaz de quebrar"*
- `migrations/000001_init.up.sql` carrega `status` e `closed_at` desde a primeira etapa, escritas por ninguém

A decisão 12 fixou desde a etapa 1 que fechamento é propriedade do tempo e não evento, e a consequência dela é a frase que esta spec tem de tornar verdadeira em código: **o `closerd` não responde por corretude, responde por performance**. Matá-lo no meio do processamento não pode deixar entrar lance atrasado, não pode fechar leilão cedo e não pode fechar o mesmo leilão duas vezes — no máximo atrasa a materialização.

É também a primeira vez que o projeto tem dois processos de longa duração, e a primeira vez que existe um transporte com entrega *at-least-once* entre duas partes do sistema. As duas coisas existem por causa da etapa 5: sem um segundo processo não há o que matar no cenário de caos, e sem pending list não há mecanismo de retentativa visível para exercitar.

Sustentam esta spec as decisões **4** (o verificador nunca pergunta ao servidor), **12** (fechamento é propriedade do tempo), **13** (toda célula parte do mesmo estado), **16** (métrica entra junto do mecanismo que a alimenta), **22** e **27** (o relógio do Postgres é a autoridade), **26** (fronteiras compartilhadas só entre séries que se leem juntas), **28** (nome de série mora em `internal/metrics`), **56** (constante em vez de variável de ambiente) e **59** (zero é uma afirmação diferente de silêncio), mais as decisões **68** a **80**, tomadas aqui.

## Objetivo

Entregar o caminho de fechamento inteiro — descoberta, transporte e materialização — de forma que a coluna `status` passe a ser escrita sem que engine, handler, envelope, suíte de conformidade ou harness mudem uma linha, e sem que o `closerd` seja capaz de violar um invariante nem quando morre no meio do trabalho.

O sistema deve:

- Descobrir leilões vencidos e ainda abertos por varredura no `auctiond`, e publicar um evento por leilão no Redis Stream
- Materializar `status` e `closed_at` no `closerd`, num statement guardado que é idempotente por construção e incapaz de fechar cedo
- Sobreviver a entrega duplicada, a mensagem perdida e à morte do consumidor no meio do processamento, com `XAUTOCLAIM` sobre a pending list e reemissão pelo produtor
- Publicar a fila **do lado do produtor**, em duas séries, para que a evidência não morra junto com o processo que ela mede
- Provar coerência de fechamento no `cmd/checker` com I8, e deixar registrado por que "todo leilão vencido está fechado" **não** é invariante
- Fazer isso sem migration, sem mudar `ExpectedSchemaVersion`, sem tocar em nenhuma engine e sem que `internal/httpapi` ganhe uma linha

## Fora de Escopo

- Cenários de caos, `chaos/scenarios.sh` e `make chaos` — spec 02 desta etapa. Esta spec entrega o mecanismo que aquela mata; os checkpoints daqui derrubam o `closerd` à mão para provar que o mecanismo existe, e **não concluem nada** sobre resiliência sob carga
- Célula da borda do fechamento — etapa 5. As células desta spec continuam com `ENDS_IN` folgado, e nenhum leilão morre no meio da carga
- Qualquer mudança de comportamento de engine. Decisão, envelope, `Outcome`, guarda de `ends_at` e `IsClosed` ficam exatamente como estão — se um checkpoint parecer exigir mexer em `internal/bid/`, pare e reporte
- WebSocket, tick agregado e painel — etapa 5b. O fechamento não é publicado a cliente nenhum aqui
- Migration, índice novo, coluna nova e bump de `ExpectedSchemaVersion` — decisão 71
- Outbox transacional — decisão 70, registrado como não feito e com o motivo
- DLQ por contagem de entregas — decisão 78, registrado como não feito
- Matriz de 36 células, sweep de pool e dashboards Grafana — etapa 5. Nada em `deploy/grafana/` muda
- Reassunção de shard entre processos: matar o `auctiond` continua sendo pergunta da spec 02
- Notificação de vencedor, e-mail, cobrança e qualquer coisa que um leilão de verdade faria depois de fechar. Não muda o gráfico

## Fluxo

```text
boot do auctiond
  ├── db.NewRedis (já existe)
  ├── stream.NewProducer(rdb, metrics)
  │     └── XGROUP CREATE auctions.expired closerd $ MKSTREAM   (BUSYGROUP ignorado)
  │                                       ─ o produtor cria, decisão 72
  ├── metrics.RegisterStreamGroup(reg, producer)  → collector: XINFO GROUPS no scrape
  └── closing.NewExpirer(pool, producer, metrics).Start(ctx)

varredor (goroutine do auctiond, tique de 1s)
  └── SELECT id FROM auctions
       WHERE status = 'open' AND ends_at <= now() ORDER BY ends_at LIMIT 500
        ├── id publicado nos últimos 30s → suprimido (decisão 70)
        └── senão → XADD auctions.expired MAXLEN ~ 100000 * auction_id <uuid>
                    e closing_events_published_total++

                          Redis Stream: auctions.expired
                          grupo: closerd    DLQ: auctions.expired.dead

laço do closerd (um consumidor, um laço, uma mensagem por vez — decisões 77 e 78)
  ├── XAUTOCLAIM min-idle=30s          → stream_claimed_total += n
  ├── XREADGROUP BLOCK 5s COUNT 64
  └── para cada mensagem, em sequência:
        ├── payload não é uuid → XADD na DLQ + XACK, stream_dead_total++
        └── UPDATE auctions SET status='closed', closed_at=clock_timestamp()
             WHERE id=$1 AND status='open' AND ends_at <= clock_timestamp()
             RETURNING closed_at, extract(epoch from (closed_at - ends_at))
              ├── 1 linha → applied  + auction_close_lag_seconds.Observe(lag) + XACK
              ├── 0 linhas → SELECT de classificação (caminho frio, decisão 75)
              │                ├── já fechado    → already_closed + XACK
              │                ├── não existe    → gone           + XACK
              │                └── ainda não venceu → early       + XACK
              └── erro de infra → NADA é observado, NADA é confirmado
                                  a entrada fica pendente para o próximo XAUTOCLAIM

GET /metrics do auctiond  (a cada 5s)
  └── collector → XINFO GROUPS
        ├── stream_pending_entries   entregues e não confirmadas
        └── stream_backlog_entries   publicadas e nunca entregues
        (erro ou lag desconhecido → série NÃO é emitida, decisões 73 e 59)

GET :8081/metrics do closerd  (a cada 5s, job novo no Prometheus)
  └── auction_closings_total{result}, auction_close_lag_seconds,
      stream_claimed_total, stream_dead_total
```

O que **não** aparece no fluxo: nenhuma chamada a Redis dentro de `PlaceBid`, nenhum handler novo em `internal/httpapi`, nenhuma linha de SQL nova nas engines e nenhuma transação no `closerd`.

## Decisoes Tecnicas

### O `closerd` é incapaz de errar, e é o `WHERE` que garante isso

O statement de fechamento tem duas guardas, e nenhuma das duas é redundante (decisão 68):

```sql
UPDATE auctions
   SET status = 'closed', closed_at = clock_timestamp()
 WHERE id = $1 AND status = 'open' AND ends_at <= clock_timestamp()
RETURNING closed_at, extract(epoch from (closed_at - ends_at))
```

`status = 'open'` é o que torna a segunda entrega da mesma mensagem um `UPDATE` de zero linhas — a entrega duplicada, que é o comportamento normal de um transporte *at-least-once*, deixa de ser um problema a resolver e vira um contador. `ends_at <= clock_timestamp()` é o que torna o worker incapaz de fechar cedo, seja qual for o conteúdo da mensagem, seja qual for o relógio de quem a publicou.

A consequência é a que interessa para a etapa 5: **não existe sequência de mortes, replays ou mensagens fabricadas que faça este worker violar um invariante**. O pior que ele consegue fazer é não trabalhar.

E a mensagem carrega um id e nada mais. Todo o resto é relido do banco, que é a autoridade — uma mensagem que carregasse estado seria uma segunda fonte de verdade viajando por um canal que duplica.

### O evento nasce de um varredor, não de um lance

Decisão 69, que emenda o diagrama de [arquitetura.md](../../projeto/arquitetura.md). A engine não publica nada: um `XADD` no caminho quente faria `bid_confirm_duration_seconds` medir Redis exatamente na borda do `ends_at`, que é o cenário inteiro do projeto — e um leilão que vence sem ninguém tentar lance nunca fecharia.

O varredor roda no `auctiond` e não no `closerd` porque produtor e consumidor precisam ser processos diferentes: o que a etapa 4 quer provar é que um fato já publicado sobrevive à morte de quem o consome, e um processo que varre e fecha sozinho transforma o stream numa fila interna.

O custo é uma consulta por segundo no processo sob benchmark, que devolve zero linha em toda célula desta etapa. Ele é pago igualmente pelas três engines, e a matriz da etapa 5 roda inteira depois daqui.

### Idempotencia

Esta spec tem duas camadas de idempotência, e elas não se confundem com a da etapa 2.

**No transporte, nenhuma.** Redis Streams entrega *at-least-once* e é isso que o projeto quer exercitar: a mesma mensagem pode chegar duas vezes, por `XAUTOCLAIM` depois de uma morte, por reemissão do varredor ou por um consumidor lento.

**No efeito, por construção.** A guarda `status = 'open'` faz a segunda aplicação não escrever nada. Nenhuma tabela de deduplicação, nenhuma chave em Redis, nenhum `INSERT ... ON CONFLICT`: o estado que a operação produz é o mesmo estado que a impede de rodar de novo. É o desenho que a decisão 12 tornou possível ao fazer o fechamento ser derivável do tempo em vez de ser um evento com identidade.

**Na origem, por supressão com prazo.** O varredor não tenta publicar uma vez só — ele republica enquanto a coluna não mudar, e suprime repetição do mesmo id por 30 segundos (decisão 70). É o que cura mensagem perdida por trim, por Redis reiniciado ou por grupo recriado, sem uma linha de código de recuperação.

O `X-Idempotency-Key` da etapa 2 continua sendo assunto exclusivo do middleware de lance. Nada aqui o lê, o escreve ou o conhece.

### Observabilidade

Sete séries novas, em dois processos, e a divisão entre eles é a decisão de desenho mais importante deste bloco.

**No `auctiond` (produtor):**

| Série | Tipo | Pergunta |
| --- | --- | --- |
| `closing_events_published_total` | Counter | O varredor viu e publicou? |
| `stream_pending_entries` | Gauge (collector) | Alguma mensagem foi entregue e não confirmada? |
| `stream_backlog_entries` | Gauge (collector) | Alguma mensagem nunca chegou a consumidor nenhum? |

**No `closerd` (consumidor):**

| Série | Tipo | Pergunta |
| --- | --- | --- |
| `auction_closings_total{result}` | Counter | Quantos fecharam, e quantos bateram na guarda |
| `auction_close_lag_seconds` | Histogram | Quanto tempo entre dever fechar e estar fechado |
| `stream_claimed_total` | Counter | A recuperação por pending list disparou? |
| `stream_dead_total` | Counter | Alguma mensagem foi para a DLQ? |

As duas gauges moram no produtor de propósito (decisão 73): a série que mede o consumidor não pode morar dentro dele, ou o alvo para de ser raspado exatamente quando o número começa a interessar. E são **duas** porque um consumidor morto não produz pendência — produz atraso: `pending` congela no punhado que estava em voo, enquanto a fila real cresce invisível. Isso emenda `observabilidade.md`, que declarava só `stream_pending_entries`.

Erro no scrape, ou `lag` que o Redis não sabe calcular, **não vira zero**: a série não é emitida naquela raspagem. Zero é uma afirmação diferente de silêncio (decisão 59).

Todas as sete são registradas no boot, com os quatro valores de `result` pré-ligados: o `/metrics` de um processo que ainda não fechou nada traz `auction_closings_total{result="applied"} 0` e os outros três, para que painel vazio continue distinguível de painel quebrado.

Nada muda em `bid_confirm_duration_seconds`, `bid_outcomes_total`, nas séries do pool, nas de idempotência nem nas quatro do shard.

## Requisitos Funcionais

### RF01 - `internal/stream`: o transporte

Pacote novo, que conhece Redis e não conhece Postgres.

```go
const (
    Key      = "auctions.expired"
    DeadKey  = "auctions.expired.dead"
    Group    = "closerd"
)

// Expired é a mensagem inteira: um id, e nada mais.
type Expired struct{ AuctionID uuid.UUID }

type Producer struct{ /* rdb, metrics */ }

func NewProducer(rdb *redis.Client, m Metrics) *Producer
func (p *Producer) EnsureGroup(ctx context.Context) error          // XGROUP CREATE ... $ MKSTREAM
func (p *Producer) Publish(ctx context.Context, e Expired) error   // XADD MAXLEN ~ 100000
func (p *Producer) GroupInfo(ctx context.Context) (pending, backlog int64, err error)
```

`EnsureGroup` trata `BUSYGROUP` como sucesso e é chamada no boot do `auctiond` (decisão 72). `GroupInfo` lê `XINFO GROUPS` e devolve `pending` e `lag` do grupo `closerd`; grupo inexistente ou `lag` desconhecido é erro, nunca zero.

`Publish` incrementa `closing_events_published_total` **depois** do `XADD` bem-sucedido.

### RF02 - `internal/stream`: o consumidor

```go
type Handler func(ctx context.Context, e Expired) error

type Consumer struct{ /* rdb, consumer name, metrics */ }

func NewConsumer(rdb *redis.Client, name string, m Metrics) *Consumer
func (c *Consumer) Run(ctx context.Context, h Handler) error
```

Cada volta do laço, nesta ordem (decisão 78):

1. `XAUTOCLAIM` com `min-idle` de 30s, `count` 64, cursor persistido entre voltas; soma o que reivindicou em `stream_claimed_total`
2. `XREADGROUP` com `BLOCK` de 5s e `COUNT` 64
3. Processa as mensagens **uma por vez, em sequência** (decisão 77)

Por mensagem:

| Caso | O que acontece |
| --- | --- |
| payload ausente ou não é uuid | `XADD` na DLQ, `XACK`, `stream_dead_total++`, um log com o id da entrada |
| `h` devolve `nil` | `XACK` |
| `h` devolve erro | nada é confirmado; a entrada fica pendente para o próximo `XAUTOCLAIM` |

`Run` devolve quando `ctx` termina, e só depois de terminar a mensagem que estiver na mão — `docker compose stop` é encerramento limpo, `docker kill` é o cenário de caos.

O nome do consumidor é `closerd-<hostname>-<pid>`: dois processos com o mesmo nome dividiriam a mesma pending list e um reivindicaria a mensagem do outro sem que ela estivesse ociosa.

### RF03 - `internal/closing`: o varredor

```go
type Expirer struct{ /* pool, publisher, metrics, suppressed map[uuid.UUID]time.Time */ }

type Publisher interface {
    Publish(ctx context.Context, e stream.Expired) error
}

func NewExpirer(pool *pgxpool.Pool, p Publisher, m ExpirerMetrics) *Expirer
func (e *Expirer) Run(ctx context.Context)   // tique de 1s até ctx terminar
func (e *Expirer) scan(ctx context.Context) error  // uma passada, testável sem relógio
```

A consulta é exatamente:

```sql
SELECT id FROM auctions
 WHERE status = 'open' AND ends_at <= now()
 ORDER BY ends_at
 LIMIT 500
```

Ids publicados há menos de 30s são pulados. O mapa de supressão é podado por idade a cada passada, o que o mantém limitado ao que venceu na última janela.

Erro de consulta ou de publicação é logado e **não** interrompe o laço: a próxima passada tenta de novo, que é o comportamento que a decisão 70 comprou.

`Run` roda numa goroutine do `auctiond` e termina no `ctx` do sinal, junto do servidor HTTP.

### RF04 - `internal/closing`: a materialização

```go
type Result uint8

const (
    Applied Result = iota
    AlreadyClosed
    Gone
    Early
)

type Materializer struct{ /* pool, metrics */ }

func NewMaterializer(pool *pgxpool.Pool, m MaterializerMetrics) *Materializer
func (m *Materializer) Close(ctx context.Context, id uuid.UUID) (Result, error)
```

`Close` roda o `UPDATE` guardado do bloco de decisões técnicas. Uma linha devolvida é `Applied`, e só aí `auction_close_lag_seconds` recebe o valor que o **banco** calculou (decisão 76). Zero linha dispara um `SELECT` de classificação — o único round-trip extra, e só no caminho frio (decisão 75):

```sql
SELECT status = 'closed', ends_at > clock_timestamp() FROM auctions WHERE id = $1
```

Sem linha → `Gone`. Primeira coluna verdadeira → `AlreadyClosed`. Segunda verdadeira → `Early`. O contador do resultado é incrementado em todos os quatro casos.

Erro de infraestrutura devolve `error`, não incrementa nada e não é um `Result`.

### RF05 - `cmd/closerd`

Binário novo, que sobe nesta ordem e falha rápido em qualquer uma:

1. `config.Load()` — o mesmo pacote do `auctiond`; `BID_STRATEGY` é ignorado aqui
2. `db.NewPool` com **4** conexões, constante do processo: um consumidor sequencial usa uma por vez, e as outras três são folga para as sondas
3. `db.NewRedis` — o mesmo construtor, o mesmo `PING` no boot
4. `metrics.NewRegistry()`, próprio, e as quatro séries do consumidor registradas antes da primeira mensagem
5. `http.ServeMux` em `:8081` com `/metrics`, `/healthz` e `/readyz` — sem Gin, sem `internal/httpapi` (decisão 74)
6. `stream.NewConsumer(...).Run(ctx, materializer.Close adaptado a stream.Handler)`

`/readyz` responde 200 quando Postgres e Redis respondem, na mesma forma de envelope do `auctiond` — o worker não verifica versão de schema: ele não tem SQL que dependa de coluna nova, e uma condição a mais aqui seria terceira cópia de uma regra que já mora em dois lugares.

`SIGTERM` cancela o `ctx`, o laço termina a mensagem em voo, e o processo sai com 0 em até 10 segundos.

### RF06 - As sete séries, em `internal/metrics`

`internal/metrics/closing.go` registra e devolve:

| Série | Tipo | Labels | Processo |
| --- | --- | --- | --- |
| `closing_events_published_total` | Counter | nenhum | `auctiond` |
| `stream_pending_entries` | Gauge via collector | nenhum | `auctiond` |
| `stream_backlog_entries` | Gauge via collector | nenhum | `auctiond` |
| `auction_closings_total` | Counter | `result` | `closerd` |
| `auction_close_lag_seconds` | Histogram | nenhum | `closerd` |
| `stream_claimed_total` | Counter | nenhum | `closerd` |
| `stream_dead_total` | Counter | nenhum | `closerd` |

Assinaturas:

```go
func NewExpirer(reg prometheus.Registerer) closing.ExpirerMetrics
func NewMaterializer(reg prometheus.Registerer) closing.MaterializerMetrics
func NewStream(reg prometheus.Registerer) stream.Metrics

// StreamGroup é o que o produtor expõe ao collector, lido no scrape.
type StreamGroup interface {
    GroupInfo(ctx context.Context) (pending, backlog int64, err error)
}

func RegisterStreamGroup(reg prometheus.Registerer, src StreamGroup)
```

As structs de métrica são declaradas em `internal/closing` e `internal/stream`, e construídas aqui — o mesmo arranjo de `idem.Metrics` (decisão 28). Nenhum nome de série aparece fora de `internal/metrics`.

Buckets de `auction_close_lag_seconds`: `0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 300`. Não compartilham fronteira com `bid_confirm_duration_seconds`, e o motivo está na decisão 76 — esta série não é lida contra nenhuma outra.

O collector das duas gauges usa timeout de 200ms por scrape e **não emite série** em erro.

### RF07 - Wiring no `auctiond`

`cmd/auctiond/main.go` ganha, depois do registry e antes do servidor: o produtor, `EnsureGroup`, o collector do grupo e a goroutine do varredor, encerrada pelo mesmo `ctx` do sinal.

`EnsureGroup` que falha **aborta o boot**, pela mesma razão que `db.NewRedis` aborta: um `auctiond` que sobe sem grupo publica no vazio e ninguém percebe até o fim da célula.

Nada mais muda no processo. `httpapi.Deps` não ganha campo, `/readyz` não ganha condição — o Redis já é uma delas —, e nenhuma rota é acrescentada.

### RF08 - Compose, imagem e scrape

`Dockerfile` compila os **dois** binários no mesmo estágio de build e copia os dois para a imagem final. `ENTRYPOINT` continua sendo `auctiond`; o serviço do worker sobrescreve com `entrypoint: /usr/local/bin/closerd`. Uma imagem, um download de módulos, dois processos.

`docker-compose.yaml` ganha o serviço `closerd`, sem `profiles`, com `DATABASE_URL` e `REDIS_URL` iguais aos do `auctiond`, `cpus: 0.5`, `mem_limit: 256M`, `depends_on` de `migrate` e `redis`, e a porta `${CLOSERD_PORT:-8081}:8081` (decisão 80).

`deploy/prometheus/prometheus.yml` ganha o job `closerd` apontando para `closerd:8081`.

`.env.example` ganha `CLOSERD_PORT=8081`.

`bench/env.sh` grava `closerdImage` e `closerdLimits` no `env.json`, do mesmo jeito que já grava os do `auctiond` e do Redis — via `docker inspect`, nunca lendo o YAML.

### RF09 - I8 no `cmd/checker`

Invariante SQL novo, no mesmo formato dos outros:

```sql
SELECT format('leilão %s: status %s, closed_at %s, ends_at %s',
              id, status, coalesce(closed_at::text, 'null'), ends_at)
  FROM auctions
 WHERE (status = 'closed') <> (closed_at IS NOT NULL)
    OR (closed_at IS NOT NULL AND closed_at < ends_at)
 ORDER BY id
 LIMIT 5
```

A linha verde reporta quantos leilões estão fechados na célula, e `cellTotals` ganha o campo para isso.

**Não** existe invariante "todo leilão vencido está fechado", e o comentário no código diz por quê: ele é falso enquanto o `closerd` está morto, e matar o `closerd` é cenário de teste da spec 02 (decisão 79).

Também não existe consulta nova para *"nenhum lance depois do fechamento materializado"*: I4 já garante `created_at <= ends_at` e I8 garante `ends_at <= closed_at`. Uma terceira consulta confirmaria as duas primeiras.

### RF10 - Testes

`internal/stream/stream_test.go`, com Redis de `testsupport`:

- `EnsureGroup` cria o grupo, e a segunda chamada devolve `nil` em vez de `BUSYGROUP`
- `Publish` grava uma entrada legível por `XREADGROUP` e incrementa o contador
- `GroupInfo` devolve `backlog` crescente enquanto ninguém lê, e `pending` crescente depois de ler sem confirmar
- `GroupInfo` com grupo inexistente devolve erro, e **não** devolve `(0, 0, nil)`
- O consumidor entrega, confirma, e a mensagem confirmada não volta em nova leitura
- Mensagem não confirmada por um consumidor "morto" é reivindicada por outro depois do `min-idle`, e `stream_claimed_total` conta
- Payload malformado vai para a DLQ, é confirmado e conta em `stream_dead_total`
- Handler que devolve erro deixa a entrada pendente

`internal/closing/expirer_test.go`, com Postgres de `testsupport` e um publisher falso:

- Uma passada publica só os leilões abertos e vencidos, e nunca o que ainda não venceu nem o já fechado
- A segunda passada dentro da janela **não** republica; passada depois da janela republica
- Erro do publisher não interrompe a passada nem impede a próxima

`internal/closing/materializer_test.go`, com Postgres real:

- Leilão vencido e aberto → `Applied`, `status` e `closed_at` escritos, lag observado uma vez
- Segunda chamada no mesmo leilão → `AlreadyClosed`, `closed_at` **não se move**, nada observado no histograma
- Leilão inexistente → `Gone`; leilão que ainda não venceu → `Early`, e a linha continua `open`
- Cem chamadas concorrentes no mesmo leilão → exatamente um `Applied`

`internal/metrics/closing_test.go`:

- As sete séries existem no `/metrics` de um registry recém-construído, com contagem zero, e os quatro valores de `result` presentes
- O collector emite as duas gauges com o valor da fonte a cada `Gather`, e **não emite nada** quando a fonte devolve erro

`cmd/checker/invariants_test.go` ganha os casos de I8: coerente passa; `status='closed'` sem `closed_at`, `closed_at` sem `status='closed'` e `closed_at < ends_at` reprovam.

A suíte de conformidade das engines **não muda**, e nenhum teste existente é reescrito.

### RF11 - O resto do sistema não muda

O diff de implementação não pode conter arquivos em:

```text
internal/bid/
internal/httpapi/
internal/store/
internal/idem/
internal/app/
internal/db/schema.go
internal/metrics/bid.go
internal/metrics/lock.go
internal/metrics/pool.go
internal/metrics/idempotency.go
internal/metrics/shard.go
migrations/
cmd/seed/
bench/bid-storm.js
bench/run-cell.sh
deploy/grafana/
go.mod
go.sum
```

`internal/db/schema.go` está nessa lista e é a linha mais importante dela: `ExpectedSchemaVersion` continua **1** (decisão 71). `go.mod` também: `go-redis`, `pgx`, `uuid` e `prometheus` já são dependências, e esta spec não pede uma sétima.

Se algum deles parecer precisar mudar, **pare e reporte**: é achado sobre o contrato das etapas anteriores, não tarefa desta spec.

## Requisitos Nao Funcionais

- Nenhuma dependência nova em `go.mod`
- Nenhuma variável de ambiente nova lida por processo nenhum. Intervalo de varredura, limite da consulta, janela de supressão, `MAXLEN`, `min-idle`, `BLOCK`, `COUNT`, pool do worker e porta do worker são constantes (decisão 56). `CLOSERD_PORT` é mapeamento de porta do compose, como `POSTGRES_PORT`, e não é lida pelo binário
- `internal/stream` não importa `internal/closing`, `internal/db`, `internal/metrics` nem pgx
- `internal/closing` não importa Redis diretamente: recebe a interface `Publisher` de um método
- `cmd/closerd` não importa `internal/httpapi`, `internal/bid` nem Gin
- Nenhum nome de série fora de `internal/metrics`
- Nenhuma chamada a Redis dentro de `PlaceBid`, em nenhuma engine
- O caminho quente do lance não ganha alocação, mutex nem round-trip
- `go test ./... -race` limpo; `gofmt -l .` vazio; `go vet ./...` sem saída
- `bid_confirm_duration_seconds`, `bid_outcomes_total` e as séries do shard byte a byte iguais no `/metrics` de antes: mesmos buckets, mesmos labels, mesma fronteira
- Um log por evento raro (DLQ, erro de varredura, erro de fechamento) e **nenhum** log por mensagem processada com sucesso: mil leilões vencendo juntos não podem virar mil linhas de log

## Budget do PR

Até 18 arquivos e aproximadamente 800 linhas de código próprio, YAML e Dockerfile incluídos.

É o maior budget desde a etapa 1, e a razão é que esta spec entrega um **processo inteiro** em vez de uma peça ao lado das outras: dois pacotes novos, um binário novo, um transporte novo, sete séries, um serviço no compose e um invariante. Ainda assim nenhuma engine muda, nenhum handler muda, nenhuma migration existe e `go.mod` fica igual.

Se o PR passar de 18 arquivos ou 800 linhas, **pare e reporte**. O corte provável, nessa ordem: I8 e seus testes saem para um PR próprio — o checker é testável sem carga e independe do worker. Se a conta estourar por causa de `internal/bid/` ou de `internal/httpapi`, pare mais cedo: significa que o fechamento vazou para dentro do caminho do lance, que é o que esta spec inteira existe para não fazer.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: alto
- Referencia permitida: `docs/projeto/arquitetura.md`, `docs/projeto/schema.md`, `docs/projeto/provas.md`, `docs/projeto/observabilidade.md`, `docs/decisoes/etapa-1.md`, `docs/decisoes/etapa-2.md`, `docs/decisoes/etapa-3.md`, `docs/decisoes/etapa-4.md`, `docs/specs/etapa-2/02-spec-idempotencia.md`, `docs/specs/etapa-4/01-spec-fechamento-e-closerd.md`

Prompt:

```text
Implemente docs/specs/etapa-4/01-spec-fechamento-e-closerd.md no repositorio
bid-storm.

Leia antes de comecar:
  docs/specs/etapa-4/01-spec-fechamento-e-closerd.md  (a spec — a autoridade)
  docs/decisoes/etapa-4.md                            (o porque; decisoes 68 a 80)
  docs/decisoes/etapa-1.md                            (decisoes 1, 4, 12, 13, 16 e 22)
  internal/idem/ e internal/metrics/idempotency.go    (o padrao exato do seam de
                                                       metricas: struct declarada
                                                       no pacote que a alimenta,
                                                       construida em metrics)
  internal/metrics/pool.go e internal/metrics/shard.go (os dois collectors que
                                                       ja leem estado no scrape)
  cmd/auctiond/main.go                                 (a ordem de boot a imitar)
  cmd/checker/invariants.go                            (o formato de um invariante)

ATENCAO: a spec emenda dois documentos publicados, e o que esta la NAO deve
ser copiado:
  - arquitetura.md desenha a ESTRATEGIA publicando o evento. Errado: quem
    publica e um varredor do auctiond, por tique de 1s (decisao 69). Nenhuma
    engine ganha uma linha, e nenhum XADD entra no caminho do lance.
  - observabilidade.md declara UMA serie de fila, stream_pending_entries. Sao
    DUAS, as duas publicadas pelo auctiond: pending e backlog (decisao 73).

Escopo: apenas RF01..RF11. NAO implemente caos, chaos/scenarios.sh, make chaos,
WebSocket, painel, dashboards, celula da borda do fechamento, outbox nem DLQ
por contagem de entregas.

Regras:
- Modulo: github.com/samuka7abr/bid-storm
- NAO altere internal/bid/, internal/httpapi/, internal/store/, internal/idem/,
  internal/app/, internal/db/schema.go, internal/metrics/{bid,lock,pool,
  idempotency,shard}.go, migrations/, cmd/seed/, bench/bid-storm.js,
  bench/run-cell.sh, deploy/grafana/, go.mod nem go.sum.
- NAO existe migration nesta etapa, e ExpectedSchemaVersion continua 1
  (decisao 71). Se parecer que falta indice ou coluna, pare e reporte.
- O UPDATE de fechamento tem as DUAS guardas: status='open' E
  ends_at <= clock_timestamp(). Sem elas o worker vira responsavel por
  corretude, que e exatamente o que a decisao 12 impede.
- O lag sai do banco, no RETURNING. NUNCA de time.Since sobre closed_at
  (decisao 76).
- Erro no scrape das gauges NAO vira zero: a serie nao e emitida (decisao 73).
- Nenhuma variavel de ambiente nova lida por binario nenhum.
- Uma mensagem por vez, um laco so, XAUTOCLAIM antes de cada leitura
  (decisoes 77 e 78).
- Rode os checkpoints C1..C5 e cole a saida real de cada um. Checkpoint sem
  saida nao conta como aceito.
- Se estourar o budget de 18 arquivos / ~800 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Criar:

```text
internal/stream/stream.go            chaves, Expired, Producer, EnsureGroup, GroupInfo
internal/stream/consumer.go          o laco: autoclaim, readgroup, ack, DLQ
internal/stream/stream_test.go       transporte contra Redis real
internal/closing/expirer.go          a varredura, a supressao, a publicacao
internal/closing/materializer.go     o UPDATE guardado e a classificacao fria
internal/closing/expirer_test.go     o que publica e o que suprime
internal/closing/materializer_test.go  os quatro desfechos e a concorrencia
internal/metrics/closing.go          as sete series e o collector do grupo
internal/metrics/closing_test.go     nomes, labels, zero no boot, erro sem serie
cmd/closerd/main.go                  o segundo processo
```

Editar:

```text
cmd/auctiond/main.go                 produtor, EnsureGroup, collector, varredor
cmd/checker/invariants.go            I8 e o campo novo de cellTotals
cmd/checker/invariants_test.go       os casos de I8
Dockerfile                           dois binarios, uma imagem
docker-compose.yaml                  o servico closerd
deploy/prometheus/prometheus.yml     o job closerd
bench/env.sh                         imagem e limites do closerd no env.json
.env.example                         CLOSERD_PORT
```

## Testes

Adicionar:

```text
internal/stream/stream_test.go          RF10, primeiro bloco
internal/closing/expirer_test.go        RF10, segundo bloco
internal/closing/materializer_test.go   RF10, terceiro bloco
internal/metrics/closing_test.go        RF10, quarto bloco
```

Editar:

```text
cmd/checker/invariants_test.go   so os casos de I8
```

## Checkpoints Mensuraveis

### C1 - Unidade sob corrida, e a fronteira do diff

```bash
go test ./internal/stream/... ./internal/closing/... ./internal/metrics/... ./cmd/checker/... -race -count=1 -v
go test ./... -race -count=1
gofmt -l . && go vet ./...
git diff --name-only -- internal/bid internal/httpapi internal/store internal/idem \
  internal/app internal/db/schema.go internal/metrics/bid.go internal/metrics/lock.go \
  internal/metrics/pool.go internal/metrics/idempotency.go internal/metrics/shard.go \
  migrations cmd/seed bench/bid-storm.js bench/run-cell.sh deploy/grafana go.mod go.sum
grep -n 'ExpectedSchemaVersion int64' internal/db/schema.go
```

Aceite:

- Os quatro blocos de RF10 passam, incluindo as cem chamadas concorrentes com exatamente um `Applied`
- A suíte de conformidade das três engines continua passando, sem uma linha alterada
- `-race` limpo: o collector lê o grupo de outra goroutine e o varredor roda ao lado do servidor
- O `git diff --name-only` não lista arquivo algum
- `ExpectedSchemaVersion` continua `1`

### C2 - Os dois processos sobem, e as sete séries existem antes do primeiro fechamento

```bash
make up && sleep 20
docker compose ps --format '{{.Service}}\t{{.Status}}'

curl -s localhost:8080/readyz  | jq .
curl -s localhost:8081/readyz  | jq .

curl -s localhost:8080/metrics | grep -E '^(closing_events_published_total|stream_pending_entries|stream_backlog_entries) '
curl -s localhost:8081/metrics | grep -E '^(auction_closings_total|auction_close_lag_seconds_count|stream_claimed_total|stream_dead_total)'

# o eixo de comparacao nao pode ter mudado
curl -s localhost:8080/metrics | grep -c 'bid_confirm_duration_seconds_bucket{strategy="optimistic"'

# o grupo existe antes de qualquer publicacao, e foi o produtor que o criou
docker compose exec -T redis redis-cli XINFO GROUPS auctions.expired

# os dois alvos estao sendo raspados
curl -s 'localhost:9090/api/v1/targets?state=active' | jq -r '.data.activeTargets[] | "\(.labels.job) \(.health)"'
```

Aceite:

- `closerd` aparece em `docker compose ps` e responde `/readyz` 200
- As três séries do produtor saem com `0`, e as quatro do worker também — inclusive `auction_closings_total` com os quatro valores de `result` presentes e zerados
- `bid_confirm_duration_seconds{strategy="optimistic"}` continua com 15 buckets, exatamente como antes deste PR
- `XINFO GROUPS` mostra o grupo `closerd` com `pending` 0
- O Prometheus lista `auctiond` e `closerd`, os dois `up`

### C3 - O leilão fecha, fecha uma vez só, e o replay bate na guarda

```bash
make seed AUCTIONS=3 ENDS_IN=10s TRUNCATE=1
export AID=$(jq -r '.[0].id' bench/auctions.json)

docker compose exec -T postgres psql -U auction -d auction -qtA \
  -c "SELECT id, status, closed_at FROM auctions ORDER BY id"
sleep 15

docker compose exec -T postgres psql -U auction -d auction -qtA \
  -c "SELECT count(*) FILTER (WHERE status='closed'), count(*) FROM auctions"
export CLOSED_AT=$(docker compose exec -T postgres psql -U auction -d auction -qtA \
  -c "SELECT closed_at FROM auctions WHERE id='$AID'")
echo "closed_at = $CLOSED_AT"

curl -s localhost:8081/metrics | grep '^auction_closings_total'
curl -s localhost:8080/metrics | grep -E '^(closing_events_published_total|stream_backlog_entries|stream_pending_entries) '

# replay a mao: a mesma mensagem, de novo
docker compose exec -T redis redis-cli XADD auctions.expired '*' auction_id "$AID"
sleep 3
curl -s localhost:8081/metrics | grep '^auction_closings_total'
docker compose exec -T postgres psql -U auction -d auction -qtA \
  -c "SELECT closed_at = '$CLOSED_AT' FROM auctions WHERE id='$AID'"

# e o lance chega tarde: 410, e o checker continua verde
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $(uuidgen)" -d '{"amountCents":500,"expectedVersion":0}'
```

Aceite:

- Antes dos 10s, os três leilões estão `open` com `closed_at` nulo; depois, os três estão `closed`
- `auction_closings_total{result="applied"}` vale 3, e os outros três resultados valem 0
- `closing_events_published_total` é 3 ou pouco mais — a supressão da decisão 70 impede que ele cresça a cada segundo
- Depois do replay, `already_closed` vale 1 e `applied` continua 3
- `closed_at` **não se moveu**: a comparação devolve `t`
- O lance atrasado recebe `410`, exatamente como recebia antes de a coluna existir escrita

### C4 - Matar o consumidor não perde fechamento, e a fila aparece no produtor

```bash
docker compose stop closerd
make seed AUCTIONS=5 ENDS_IN=10s TRUNCATE=1
sleep 20

# ninguem leu: backlog cresce, pending fica parado
curl -s localhost:8080/metrics | grep -E '^(stream_backlog_entries|stream_pending_entries|closing_events_published_total) '
docker compose exec -T postgres psql -U auction -d auction -qtA \
  -c "SELECT count(*) FILTER (WHERE status='closed') FROM auctions"

docker compose start closerd && sleep 10
curl -s localhost:8080/metrics | grep -E '^(stream_backlog_entries|stream_pending_entries) '
curl -s localhost:8081/metrics | grep -E '^auction_closings_total|^auction_close_lag_seconds_(count|sum)'
docker compose exec -T postgres psql -U auction -d auction -qtA \
  -c "SELECT count(*) FILTER (WHERE status='closed'), count(*) FROM auctions"

# e agora o worker morre COM mensagem na mao
docker compose exec -T redis redis-cli XADD auctions.expired '*' auction_id "$(uuidgen)"
docker kill -s KILL "$(docker compose ps -q closerd)" ; sleep 2
docker compose start closerd
curl -s localhost:8080/metrics | grep '^stream_pending_entries '
sleep 40
curl -s localhost:8081/metrics | grep -E '^(stream_claimed_total|auction_closings_total\{result="gone")'
curl -s localhost:8080/metrics | grep '^stream_pending_entries '
```

Aceite:

- Com o `closerd` parado, `stream_backlog_entries` cresce e `stream_pending_entries` fica em zero: é o fato inteiro da decisão 73, e uma série só teria mostrado linha plana durante a falha
- Nenhum leilão fecha enquanto o worker está fora — e nenhum invariante é violado por isso, que é o motivo de "todo vencido está fechado" não ser invariante (decisão 79)
- Depois do `start`, os cinco fecham, `backlog` volta a zero e `auction_close_lag_seconds_sum` mostra a espera que a queda custou
- Depois do `kill`, a entrada não confirmada aparece em `stream_pending_entries`, e o `XAUTOCLAIM` a recupera passados os 30 segundos de ociosidade: `stream_claimed_total` sai de zero
- O id fabricado termina como `gone`, é confirmado, e `stream_pending_entries` volta a zero — nada fica reciclando para sempre

### C5 - A célula continua sendo a célula, com o worker de pé

```bash
make run STRATEGY=shard && sleep 10
make bench RUN=e4c5-shard-1000 STRATEGY=shard AUCTIONS=1000 POLICY=immediate SCENARIO=ramp

cat bench/results/e4c5-shard-1000/checker.txt
jq '.closerdLimits, .closerdImage' bench/results/e4c5-shard-1000/env.json
curl -s localhost:8081/metrics | grep '^auction_closings_total'
curl -s localhost:8080/metrics | grep -E '^(stream_backlog_entries|stream_pending_entries) '
curl -s localhost:8080/metrics | grep 'bid_outcomes_total{strategy="shard"'
```

Aceite:

- I1 a I8 verdes, e I8 reportando zero leilão fechado: `ENDS_IN` folgado continua garantindo que nada morre no meio da célula
- `auction_closings_total` inteiro em zero, e as duas gauges em zero: com nada vencendo, o mecanismo não faz trabalho nenhum e não aparece no número da célula
- `env.json` traz imagem e limites do `closerd`, como traz os do Redis desde a etapa 2 (decisão 80)
- `bid_outcomes_total{strategy="shard"}` com a mesma forma das células da etapa 3 — nenhum desfecho novo, nenhuma coluna nova de resposta
- Nenhuma conclusão sobre throughput entra no PR

## Smoke Manual

Pre-condicoes:

```text
Docker e docker compose v2, jq, uuidgen e make instalados
Portas livres: 5432, 6379, 8080, 8081, 9090, 3000
Repositorio limpo, .env criado a partir de .env.example (com CLOSERD_PORT)
```

Passos:

```bash
make up && sleep 20
make seed AUCTIONS=1 ENDS_IN=20s TRUNCATE=1
export AID=$(jq -r '.[0].id' bench/auctions.json)
export UID=$(uuidgen)

# vivo: lance passa, e a rota de leitura diz open
curl -s -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":500,"expectedVersion":0}' | jq .
curl -s localhost:8080/auctions/$AID | jq '{status, currentHighestBid, version}'

sleep 25

# morto: a coluna foi escrita, e as duas rotas concordam
docker compose exec -T postgres psql -U auction -d auction -qtA \
  -c "SELECT status, closed_at IS NOT NULL, closed_at >= ends_at FROM auctions WHERE id='$AID'"
curl -s localhost:8080/auctions/$AID | jq '{status, currentHighestBid}'
curl -s -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900,"expectedVersion":1}' | jq .

curl -s localhost:8081/metrics | grep -E '^(auction_closings_total|auction_close_lag_seconds_count)'
curl -s localhost:8080/metrics | grep -E '^(closing_events_published_total|stream_backlog_entries) '

# o Prometheus enxerga as sete
curl -s 'localhost:9090/api/v1/label/__name__/values' | jq -r '.data[]' \
  | grep -E 'closing_events|stream_pending|stream_backlog|auction_closings|auction_close_lag|stream_claimed|stream_dead'

make down
```

Aceite manual:

- O primeiro lance é `201`; o segundo, depois do fechamento, é `410 auction_closed` com `retryable: false`
- `GET /auctions/:id` reporta `status: "open"` antes e `status: "closed"` depois, sem que nenhum handler tenha mudado — `IsClosed` já cobria os dois casos desde a etapa 1
- As três colunas do psql saem `closed`, `t`, `t`: a coluna foi escrita, e nunca antes da hora
- `auction_closings_total{result="applied"}` vale 1 e `auction_close_lag_seconds_count` vale 1
- O Prometheus lista os sete nomes: elas não só existem nos dois `/metrics`, elas estão sendo raspadas
- `make down` derruba os dois processos sem contêiner órfão

## Definicao De Pronto

- RF01 a RF11 implementados
- C1 a C5 executados, com a saída real colada no PR — checkpoint sem saída não conta como aceito
- A coluna `status` é escrita, e escrita por um só processo, com as duas guardas no `WHERE`
- Replay demonstrado não movendo `closed_at`, com `already_closed` contando a tentativa
- `closerd` morto com mensagem na mão, e `XAUTOCLAIM` demonstrado recuperando-a
- As duas gauges demonstradas dizendo coisas diferentes na mesma falha: `backlog` cresce, `pending` não
- Sete séries no `/metrics` desde o boot, todas zeradas, com os quatro valores de `result` pré-ligados
- I8 no checker, com I1 a I7 continuando verdes, e nenhuma linha da suíte de conformidade alterada
- Nenhuma migration, `ExpectedSchemaVersion` ainda `1`, `go.mod` e `go.sum` inalterados
- `bid_confirm_duration_seconds`, `bid_outcomes_total` e as séries do shard inalteradas
- `go test ./... -race` limpo
- `env.json` da célula gravando imagem e limites do `closerd`
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado pelo PR de implementação
- Com isto, o sistema tem os dois processos que a spec 02 desta etapa precisa para matar um deles sob carga
