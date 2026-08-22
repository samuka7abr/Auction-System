# Etapa 3 — Spec 01: Engine single-writer

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md) · [decisões da etapa 2](../../decisoes/etapa-2.md) · [decisões da etapa 3](../../decisoes/etapa-3.md) · [etapa 2, spec 01](../etapa-2/01-spec-engine-pessimista.md)

## Contexto

A etapa 2 fechou com duas curvas medidas sob a mesma régua: duas engines passando na mesma suíte de conformidade, idempotência demonstrada sob carga com duplicatas injetadas, e `cmd/checker` reprovando qualquer ambiguidade entre o que o cliente confirmou e o que o banco gravou (I1 a I7).

Falta a terceira, e ela é **a aposta do projeto**. As duas primeiras compartilham a premissa de que o banco é o ponto de sincronização: uma detecta a colisão depois de tentar, a outra impede a colisão trancando a linha. A terceira remove a colisão — se cada leilão tem exatamente um escritor, não existe corrida a resolver, e o Postgres deixa de ser o árbitro e vira apenas durabilidade.

O código já reserva o lugar dela em cinco pontos, e todos serão cobrados aqui:

- `internal/app/engine.go` responde a `BID_STRATEGY=shard` com *"the single-writer engine arrives in etapa 3"*, e falha no boot em vez de subir sem engine
- `internal/bid/engine.go` documenta que o pacote fica livre das dependências que o shard traz
- `internal/bid/enginetest/conformance.go` diz, em três comentários, que o alvo real do replay e do caso da chave de idempotência é o shard, que decide em memória e não tem `WHERE` nenhum para lembrá-lo da regra
- `internal/metrics/bid.go` registra a exceção da decisão 23 para a série que só existe dentro do shard
- `cmd/checker/invariants.go` explica, em I2, que a regra de incremento é o que uma engine que decide em memória pode violar sem deixar buraco na sequência

A promessa da decisão 11 foi paga uma vez, na pessimista, que custou 8 arquivos. Esta é a cobrança difícil: a segunda engine ainda era SQL contra o mesmo banco; esta troca o mecanismo inteiro por goroutines, canais e estado em memória, e ainda assim tem que passar na mesma suíte, servir o mesmo contrato HTTP e não pedir uma linha nova a `internal/httpapi`.

Sustentam esta spec as decisões **7** (pool idêntico nas três, ambiente fixado), **8** (`201` significa durável nas três), **9** (`409` e `422` são o mesmo evento), **11** (conformidade contra a interface), **12** (fechamento é propriedade do tempo), **15** (a chave sai `NULL` quando não existe), **16** (métrica entra junto da engine), **21** (`Current` só existe quando o leilão existe), **22** e **27** (o relógio do Postgres é a autoridade), **23** (as três são medidas de uma fronteira só), **24** (o replay da suíte existe por causa desta engine) e **39** (a engine grava a chave de idempotência), mais as decisões **48** a **58**, tomadas aqui.

## Objetivo

Entregar a terceira estratégia como engine completa — decidindo em memória, comitando em lote e respondendo só depois de durável — sem que o contrato HTTP, a suíte de conformidade, o harness de carga ou o checker mudem uma linha para acomodá-la.

O sistema deve:

- Rotear cada leilão deterministicamente para um shard, e fazer daquela goroutine a única que escreve o estado do leilão
- Hidratar o estado do Postgres sob demanda e decidir `Accepted`, `TooLow`, `Closed` e `NotFound` sem round-trip no caminho quente
- Agrupar aceites em um commit, e responder `201` **somente depois** dele
- Fechar o lote por lotação, por inbox vazio ou por linger de 1ms, para que nenhum aceite fique preso atrás de uma enxurrada de rejeições
- Tratar falha de commit como perda de autoridade: erro para todo o lote e despejo do estado em memória, para que a memória nunca fique à frente do banco
- Passar na suíte de conformidade **sem alterá-la**, e fechar uma célula de benchmark com I1 a I7 verdes
- Fazer isso sem tocar em `internal/httpapi`, `internal/store`, `internal/idem`, `cmd/`, `bench/`, `migrations/`, `docker-compose.yaml` ou `Makefile`

## Fora de Escopo

- `bid_accept_duration_seconds`, `shard_inbox_depth`, `shard_batch_size` e `journal_lag_seconds` — spec 02 desta etapa (decisão 58). A engine entra medida pelo decorator, como as outras duas
- Pipeline de commit: decidir o lote seguinte enquanto o anterior comita — registrado como não feito na decisão 55
- Bissecção de lote em violação de unicidade — registrado como não feito na decisão 53
- Redis Streams, `closerd`, materialização de `status` e caos — etapa 4. A coluna `status` continua sendo escrita por ninguém, e o shard apenas a lê na hidratação
- Reassunção de shard entre processos, particionamento entre réplicas, rebalanceamento — o cenário de caos da etapa 4 mata um processo, e é lá que essa pergunta nasce
- Matriz de 36 células, sweep de `DB_POOL_SIZE` e dashboards — etapa 5. Esta spec roda células avulsas e **não conclui nada sobre qual estratégia vence**
- Despejo por `ends_at` vencido no mapa de estado — registrado como limite na decisão 49
- Qualquer variável de ambiente nova. Shards, lote e inbox são constantes (decisão 56)

## Fluxo

```text
POST /auctions/:id/bids            ← nenhum handler muda: nenhum sabe qual engine responde
  └── idempotência (middleware) → carrier → instrumented (decorator, mesma fronteira das três)
        └── shard.Engine.PlaceBid
              ├── shardFor(auctionID) → um dos 8 shards
              └── enfileira no inbox
                    ├── ctx cancelado antes de entrar → erro (503), nada foi decidido
                    └── entrou: a partir daqui o comando é história (decisão 54)

goroutine do shard (dona exclusiva de auctions[id])
  ├── recebe comando
  │     ├── miss no mapa → SELECT ..., clock_timestamp()   (1 rt, síncrono)
  │     │                    └── skew = dbNow - localMid   (decisão 50)
  │     │                    └── sem linha → NotFound, e a ausência não é cacheada
  │     ├── IsClosed(now+skew)      → Closed   responde na hora
  │     ├── Amount < MinNextBid()   → TooLow   responde na hora  (estado decidido, decisão 57)
  │     └── aceita: version++, seq = version, highest = amount
  │           └── entra no lote, SEM responder
  │
  └── fecha o lote quando (decisão 52)
        len(batch) == 256  ·  inbox vazio  ·  lote aberto há mais de 1ms
              └── 1 statement: INSERT unnest(bids) + UPDATE unnest(auctions)   (1 rt)
                    ├── ok    → responde Accepted a cada comando do lote
                    └── erro  → responde erro (503) a todos
                                 e DESPEJA do mapa os leilões do lote (decisão 53)

depois da célula
  └── cmd/checker, sem alteração: I1..I7
        I1 sequência densa · I2 incremento · I3 versão e vencedor · I4 fechamento
        I5 db == cliente   · I6 célula válida · I7 chaves presentes e únicas
```

`ExpectedVersion` não aparece no fluxo porque a engine **nunca lê o campo**, como na pessimista: o escritor único já garante que o estado em memória é o vigente. `Conflict` e `Invalid` não são produzidos.

## Decisoes Tecnicas

### O lote só cresce entre leilões, e a célula de manchete não amortiza `fsync` nenhum

`estrategias.md` descreve o ganho como *"agrupar centenas de lances em um commit"*. Isso vale entre leilões e não vale dentro de um.

Dentro de um leilão, dois aceites consecutivos estão separados por um round-trip de cliente: o shard aceita um, e todo comando que já estava no inbox para aquele leilão passa a estar abaixo de `minNextBid` e é rejeitado em memória. O próximo aceite depende de alguém ler a resposta e re-mirar — e a resposta do aceite só sai depois do commit. Logo, **na célula de 1 leilão o lote vale 1** por construção.

O ganho ali é outro, e é grande: perder é de graça. Uma rejeição do shard não toca o banco, enquanto o otimista paga um `SELECT` de classificação por rejeição e o pessimista paga `Begin` + `FOR UPDATE` + `Rollback`. Sob contenção alta, a rejeição é a esmagadora maioria das requisições, e é aí que o custo por requisição das três diverge (decisão 48).

C3 mede exatamente isso, contando transações em `pg_stat_database` nas duas contenções. É o número que a spec 02 vai reencontrar como `shard_batch_size`, e ele é previsto **antes** de ser medido de propósito.

### A hidratação é preguiçosa, síncrona e não cacheia ausência

O mapa nasce vazio. Miss dispara um `SELECT` feito pela própria goroutine do shard.

Pré-carregar no boot estaria errado na primeira célula: `run-cell.sh` trunca e re-semeia com o `auctiond` de pé, e `POST /auctions` cria leilão em tempo de execução. Hidratar em outra goroutine devolveria o comando ao inbox fora de ordem, ou exigiria uma fila de espera por leilão — nos dois casos a ordem total por leilão, que hoje é consequência da estrutura, passaria a depender de código.

O custo é um round-trip bloqueando aquele shard, **uma vez por leilão**; o warmup do harness já o paga antes da célula medida. Leilão inexistente devolve `NotFound` e não vira entrada: cachear ausência tornaria invisível um leilão criado logo depois (decisão 49).

### O relógio do banco chega como offset, e `created_at` é escrito à mão

O shard não pode perguntar as horas ao Postgres no caminho quente — seria o round-trip que a engine existe para não pagar. Também não pode decidir com o relógio do contêiner, porque aí a terceira engine teria uma guarda de fechamento diferente das outras duas, e a diferença apareceria na borda do `ends_at`, que é o cenário inteiro do projeto (decisões 22 e 27).

A hidratação resolve com o round-trip que já acontece: ela devolve `clock_timestamp()`, o shard guarda `skew = dbNow - localMid` — `localMid` sendo a média dos instantes anterior e posterior à consulta — e toda decisão de fechamento usa `time.Now().Add(skew)`. O erro do offset é no máximo meio round-trip (decisão 50).

E o `INSERT` do lote **passa `created_at` explicitamente**, com o instante da decisão já corrigido pelo offset. Deixar o `DEFAULT now()` da migration `001` carimbaria o instante do commit, que é posterior: um lance decidido legitimamente antes de `ends_at` seria gravado depois dele, e **I4 reprovaria um lance que o servidor aceitou dentro do prazo** (decisão 51).

### O lote fecha por três condições, e o ticker de 2ms sai

`estrategias.md` ilustra um `time.NewTicker(2 * time.Millisecond)`. Ele cobraria o atraso de todo mundo para amortizar o `fsync` de alguns: com o inbox esvaziando entre lances, cada lance esperaria até 2ms por um lote que não vai crescer, e esses 2ms entrariam em `bid_confirm_duration_seconds` como se fossem mecanismo.

As três condições cobrem os três regimes sem parâmetro novo (decisão 52):

| Condição | Regime |
| --- | --- |
| `len(batch) >= 256` | saturação: fecha por tamanho, `fsync` amortizado |
| inbox vazio | carga baixa: comita na hora |
| lote aberto há mais de 1ms | contenção num leilão só: o aceite não fica preso atrás das rejeições |

A terceira só é necessária por causa da primeira decisão técnica desta seção: na célula de 1 leilão o inbox nunca esvazia e o lote nunca chega a 256, porque quase tudo que entra é rejeição. O valor 1ms segue uma regra, não um gosto — **o linger não pode custar mais do que o commit que ele amortiza**.

### Um statement por lote, e o `UNIQUE (auction_id, seq)` é a asserção

```sql
WITH ins AS (
    INSERT INTO bids (id, auction_id, user_id, amount_cents, seq, idempotency_key, created_at)
    SELECT id, auction_id, user_id, amount_cents, seq, NULLIF(key, ''), created_at
      FROM unnest($1::uuid[], $2::uuid[], $3::uuid[], $4::bigint[],
                  $5::bigint[], $6::text[], $7::timestamptz[])
        AS t(id, auction_id, user_id, amount_cents, seq, key, created_at)
)
UPDATE auctions a
   SET highest_bid_cents = v.amount_cents,
       highest_bidder    = v.bidder,
       version           = v.version
  FROM unnest($8::uuid[], $9::bigint[], $10::uuid[], $11::bigint[])
    AS v(id, amount_cents, bidder, version)
 WHERE a.id = v.id
```

Transação explícita seriam quatro round-trips por lote — irrelevante num lote de 256 e **decisivo na célula de 1 leilão**, onde o lote vale 1 e a terceira engine passaria a pagar quatro round-trips por aceite justamente na célula de manchete. Um statement é igualmente atômico e custa um. A CTE que modifica dados executa até o fim mesmo sem ser lida, que é a garantia que permite pôr o `INSERT` acima do `UPDATE`.

`version` é escrita em valor absoluto, não `version + 1`: a memória do shard é a autoridade, e escrever o valor que ela calculou é o que faz o estado publicado convergir com a história gravada, que é o que I3 confere.

**Sem guarda de versão no `UPDATE`, de propósito.** Uma guarda `WHERE version = prev` falharia em silêncio para aquele leilão enquanto o `INSERT` do mesmo lote gravaria a linha — escrita parcial comitada, pior que o problema. Conferir em Go depois não adianta: o statement já comitou. O que pega divergência é o que já existe: se a memória divergir, o `seq` calculado já pertence a outra linha e `UNIQUE (auction_id, seq)` **aborta o statement inteiro**, sem escrita parcial e sem código novo. Pelo mesmo motivo, o número de linhas afetadas não é conferido: a conferência que vale está no schema (decisão 53).

### Falha de commit é perda de autoridade, não um erro qualquer

Erro no commit responde erro a **todos** os comandos do lote — o handler devolve `503 unavailable, retryable: true` — e **despeja do mapa** os leilões daquele lote.

Manter em memória um estado que o banco não tem faria o próximo aceite gravar um `seq` com buraco atrás dele, e I1 reprovaria a célula. O despejo força a re-hidratação a ler a verdade, inclusive no caso ambíguo em que o commit funcionou e a resposta se perdeu (decisão 53).

C5 planta exatamente essa situação — um segundo escritor gravando um lance por fora — e verifica as três consequências: o statement aborta, o cliente recebe `503`, e o lance seguinte volta correto sem reiniciar o processo.

### O comando aceito no inbox é decidido, comitado e respondido

Cancelamento é respeitado **antes** do enfileiramento: `select` entre o envio e `ctx.Done()`, e quem desistiu recebe erro sem custo nenhum. Depois de decidido, o comando já é história — consumiu um `seq` e moveu o topo em memória —, então retirá-lo do lote deixaria um buraco que I1 reprova.

Por isso o commit **não roda sob o contexto de nenhum chamador**: um lote é de muitos, e o VU que desistiu por `BID_DEADLINE` não pode cancelar a durabilidade dos outros 255, cada um com o próprio `201` prometido. Ele roda sob um contexto do processo, com timeout de 5 segundos — maior que qualquer commit de uma célula viva, menor que a paciência de qualquer cliente (decisão 54).

Consequência boa: como não há saída antecipada, `bid_confirm_duration_seconds{strategy="shard"}` mede sempre a decisão completa, nunca o instante em que alguém desistiu. As três engines continuam medindo a mesma coisa no mesmo lugar.

### O shard decide ou comita, nunca os dois ao mesmo tempo

O commit acontece dentro do laço, bloqueando a goroutine. Pipelinar é a otimização óbvia e é onde está o próximo ganho, e ela dobra o número de estados que o shard precisa distinguir: passaria a existir "decidido, no lote em voo" além de "decidido, no lote aberto", e o despejo precisaria saber a qual dos dois a falha pertence. Com o commit síncrono, **a janela entre decidido e durável é exatamente um lote**, e essa frase é verificável em vez de aproximada (decisão 55).

O custo é o shard ocioso durante o round-trip do commit, limitado pelos oito shards independentes: enquanto um comita, os outros sete decidem. O projeto não afirma nada sobre o ganho do pipeline, porque não o mediu.

### Rejeição responde do estado decidido; só o `201` espera durabilidade

Segurar a rejeição até o lote comitar acrescentaria latência de durabilidade à resposta que não tem nada para tornar durável — e na célula de 1 leilão isso é quase toda resposta. O invariante de método é *`201` significa durável*, e ele fica intacto.

A assimetria criada é dita em voz alta: a rejeição do shard carrega o estado mais fresco que existe, enquanto a do otimista carrega o último estado **comitado**. O apostador re-mira melhor contra o shard e desperdiça menos tentativas. Isso é vantagem real do mecanismo — quem tem escritor único sabe a verdade antes de ela ser durável — e não uma promessa mais fraca (decisão 57). `GET /auctions/:id` continua publicando o estado durável, e por isso pode ficar até um lote atrás.

### Observabilidade

**Nenhuma série nova nesta spec.** `bid_confirm_duration_seconds{strategy="shard"}` e `bid_outcomes_total{strategy="shard"}` vêm do decorator, ligados no boot, que é o que faz `run-cell.sh` conseguir confirmar pelo `/metrics` qual engine o processo roda antes de gastar uma célula.

`bid_accept_duration_seconds`, `shard_inbox_depth`, `shard_batch_size` e `journal_lag_seconds` ficam para a spec 02, e a decisão 58 registra por quê: o gap entre aceite e confirmação só é legível se as duas séries observarem a mesma população, e não observam — `confirm` agrega todos os desfechos e o custo de durabilidade só existe nos aceites. Sob contenção alta os dois p95 cairiam na população de rejeições e o gap leria perto de zero, **subestimando o custo da durabilidade a favor da tese do projeto**. O instrumento honesto é um histograma por lance aceito, da decisão até o commit, medido dentro do shard — e ele nasce junto das outras três, onde a pergunta da população se resolve uma vez só.

O efeito colateral disso é que `internal/bid/shard` **não importa Prometheus**, sendo a única engine que teria motivo para querer.

## Requisitos Funcionais

### RF01 - Roteamento e propriedade exclusiva

`internal/bid/shard` cria 8 shards no construtor. Cada shard é uma goroutine com um inbox (`chan`, capacidade 1024) e um `map[uuid.UUID]*auctionState` que **só ela lê e escreve** — nenhum mutex, nenhuma leitura do mapa fora do laço.

`shardFor(auctionID uuid.UUID, n int) int` é determinístico, puro e derivado dos bytes do UUID. O mesmo leilão vai sempre para o mesmo shard, no mesmo processo.

Os três números são constantes do pacote, sem variável de ambiente (decisão 56).

### RF02 - Hidratação sob demanda

Miss no mapa dispara, na própria goroutine:

```sql
SELECT version, highest_bid_cents, min_increment_cents, status, ends_at, clock_timestamp()
  FROM auctions
 WHERE id = $1
```

O estado guardado inclui o `skew` calculado como `dbNow - localMid`. Sem linha, o comando recebe `NotFound` com `Current` zerado (decisão 21) e **nada é gravado no mapa**. Erro de infraestrutura devolve erro ao comando, também sem gravar.

`New` não pode tocar o banco: o teste de `internal/app` constrói as engines com pool `nil` para que o boot não falhe pelo motivo errado quando o Postgres demora a subir.

### RF03 - Decisão em memória

Na ordem `NotFound` → `Closed` → `TooLow` → `Accepted`, usando `AuctionState.IsClosed(time.Now().Add(skew))` e `AuctionState.MinNextBid()` — os métodos do contrato, sem reimplementar a regra.

`req.ExpectedVersion` não é lido em ponto algum. `Conflict` e `Invalid` nunca são produzidos.

No aceite, a goroutine incrementa `version`, usa o valor como `seq`, grava topo e vencedor em memória, e **não responde**: o comando entra no lote. Rejeições respondem na hora, com `Current` montado do estado decidido.

Toda rejeição devolve `error` nil. Falha de infraestrutura devolve `BidResult{}` e erro embrulhado, como nas outras duas engines.

### RF04 - Lote e commit

O lote fecha quando `len(batch) == 256`, quando o inbox está vazio, ou quando o lote está aberto há mais de 1ms — o que vier primeiro.

O commit é o statement único da seção de decisões técnicas: um `INSERT` por lance e um `UPDATE` por leilão distinto do lote, com o estado final daquele leilão. `created_at` vai explícito, com o instante da decisão corrigido pelo `skew`. A chave de idempotência entra por `NULLIF(key, '')`, preservando o `NULL` que mantém a linha fora do índice parcial (decisões 15 e 39).

Só depois do commit bem-sucedido cada comando do lote recebe `Accepted`, com `Seq`, `BidID` e `Current` montados do que foi decidido.

### RF05 - Falha de commit

Erro no commit responde erro a todos os comandos do lote e remove do mapa **todos** os leilões que o lote tocou. O próximo comando para qualquer um deles re-hidrata do banco.

O commit roda sob um contexto do processo com timeout de 5 segundos, nunca sob o contexto de um chamador.

Uma linha de log por lote falho, com o tamanho do lote e o erro. Nenhum log por requisição.

### RF06 - Ciclo de vida e cancelamento

`PlaceBid` enfileira com `select` entre o inbox e `ctx.Done()`; cancelado antes de entrar, devolve erro sem decidir nada. Depois de enfileirado, espera a resposta **sem saída antecipada**.

O canal de resposta é próprio de cada comando e tem capacidade 1: o shard nunca pode ficar parado esperando um receptor.

As goroutines vivem enquanto o processo vive. Não há `Close`, porque `srv.Shutdown` só retorna quando nenhum handler está em voo, e nenhum handler retorna antes do commit do seu lance — logo, no instante em que o processo pode morrer, não existe lance decidido e não durável. `cmd/auctiond/main.go` não muda.

### RF07 - `BID_STRATEGY=shard` sobe

`internal/app.NewEngine` passa a construir a engine shard, embrulhada no mesmo decorator e no mesmo carrier de chave das outras duas. Valor desconhecido continua falhando no boot, e nenhum `switch` por nome de estratégia nasce fora de `internal/app`.

Com isso, `run-cell.sh` reconhece a engine pelo `/metrics` sem alteração.

### RF08 - Conformidade, e o que só o shard prova

`internal/bid/shard/shard_test.go` chama `enginetest.RunConformance` **sem alterar uma linha da suíte**, mais os casos específicos desta engine:

- **Durabilidade antes da resposta.** Assim que `PlaceBid` devolve `Accepted`, a linha já é visível a outra conexão. É o invariante de método que esta engine poderia quebrar de graça, e ele merece asserção própria
- **Versão velha com valor suficiente é aceita, e `ExpectedVersion` nulo também.** No otimista os dois são `409` e `400`
- **Sob concorrência, nenhum resultado é `Conflict` nem `Invalid`**
- **O lote comita mais de um lance de uma vez.** Lances aceitos em leilões distintos do mesmo shard, disparados juntos, produzem menos transações do que lances — medido pelo delta de `xact_commit` em `pg_stat_database`
- **Um segundo escritor faz o commit abortar, e o shard se recupera.** Uma linha de `bids` plantada por fora, com o `seq` que o shard usaria, derruba o lote; o comando recebe erro; o comando seguinte, depois da re-hidratação, é aceito com o `seq` correto
- **Leilão criado depois do boot é hidratado sob demanda**

`internal/bid/shard/routing_test.go` cobre `shardFor` sem banco: determinismo e distribuição sobre uma amostra de UUIDs.

Se a suíte de conformidade precisar de qualquer alteração para acomodar esta engine, **pare e reporte**: ela estaria escrita contra o SQL disfarçada de contrato, e isso é um achado maior que esta spec.

### RF09 - O resto do sistema não muda

O diff de implementação não pode conter arquivos em:

```text
internal/httpapi/
internal/store/
internal/idem/
internal/metrics/
internal/bid/engine.go
internal/bid/outcome.go
internal/bid/enginetest/
internal/bid/optimistic/
internal/bid/pessimistic/
cmd/
bench/
migrations/
docker-compose.yaml
Makefile
go.mod
go.sum
```

Este é o teste real das etapas 1 e 2. A terceira engine entra por pontos de extensão que já existem, ou o contrato publicado estava errado — e é isso que precisa ser reportado, em vez de contornado.

## Requisitos Nao Funcionais

- Nenhuma dependência nova: `pgx/v5`, `gin`, `prometheus/client_golang`, `go-redis` e `google/uuid` continuam sendo tudo
- `internal/bid/shard` importa `internal/bid`, `pgx`, `pgxpool` e `uuid` — e **não importa Prometheus**, `internal/metrics`, `internal/app` nem Gin
- Uma goroutine por shard, e nenhuma por lance. O único bloqueio do chamador é a espera pela própria resposta
- Nenhum mutex sobre o estado dos leilões: o mapa é de uma goroutine só, e é isso que a engine tem a provar
- `go test ./... -race` limpo; `gofmt -l .` vazio; `go vet ./...` sem saída
- A suíte de conformidade do shard roda em menos de 30 segundos, contêiner incluído
- Toda rejeição é `nil` no `error` da engine, sem exceção
- Nenhum log por requisição: continuam apenas o de boot e o de lote falho

## Budget do PR

Até 8 arquivos e aproximadamente 700 linhas de código próprio.

É o dobro do budget da pessimista, e a razão é que ali a segunda engine era SQL contra o mesmo banco, enquanto aqui o mecanismo inteiro muda: roteamento, propriedade de estado, hidratação, lote, commit e recuperação. Ainda assim o contrato, o envelope, o decorator, a suíte, o harness e o checker já existem — se o PR passar de 8 arquivos ou 700 linhas, alguma coisa que já existe está sendo reescrita. **Pare e reporte** em vez de continuar.

E se a conta estourar por causa de `internal/httpapi` ou de `bench/`, pare mais cedo: significa que a engine vazou para fora do ponto de extensão.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: alto
- Referencia permitida: `docs/projeto/estrategias.md`, `docs/projeto/schema.md`, `docs/projeto/provas.md`, `docs/projeto/observabilidade.md`, `docs/decisoes/etapa-1.md`, `docs/decisoes/etapa-2.md`, `docs/decisoes/etapa-3.md`, `docs/specs/etapa-2/01-spec-engine-pessimista.md`, `docs/specs/etapa-3/01-spec-engine-single-writer.md`

Prompt:

```text
Implemente docs/specs/etapa-3/01-spec-engine-single-writer.md no repositorio bid-storm.

Leia antes de comecar:
  docs/specs/etapa-3/01-spec-engine-single-writer.md  (a spec — a autoridade)
  docs/decisoes/etapa-3.md                            (o porque; decisoes 48 a 58)
  docs/decisoes/etapa-1.md                            (decisoes 8, 11, 12, 15, 21,
                                                       22, 23 e 24)
  internal/bid/pessimistic/                            (a engine irma, ja pronta)
  internal/bid/enginetest/conformance.go               (o contrato executavel)

ATENCAO: a spec emenda estrategias.md em tres pontos, e o codigo ilustrativo
de la NAO deve ser copiado:
  - o ticker de 2ms sai. O lote fecha por 256, por inbox vazio ou por 1ms de
    linger (decisao 52)
  - o commit e UM statement (CTE de INSERT + UPDATE com unnest), sem
    Begin/Commit e sem guarda de version: UNIQUE (auction_id, seq) e a
    assercao que aborta (decisao 53)
  - created_at vai explicito no INSERT, com o instante da decisao corrigido
    pelo offset do relogio do banco. NAO use o DEFAULT now() (decisoes 50 e 51)

Escopo: apenas RF01..RF09. NAO implemente bid_accept_duration_seconds,
shard_inbox_depth, shard_batch_size, journal_lag_seconds, pipeline de commit,
bisseccao de lote, Redis Streams nem closerd — sao da spec 02 e das etapas
seguintes.

Regras:
- Modulo: github.com/samuka7abr/bid-storm
- NAO altere internal/httpapi, internal/store, internal/idem, internal/metrics,
  internal/bid/engine.go, internal/bid/outcome.go, internal/bid/enginetest/,
  internal/bid/optimistic/, internal/bid/pessimistic/, cmd/, bench/,
  migrations/, docker-compose.yaml, Makefile, go.mod nem go.sum. Se algum
  deles parecer precisar de mudanca, pare e reporte: e um achado sobre o
  contrato das etapas 1 e 2, nao uma tarefa desta spec.
- A suite de conformidade NAO pode ser alterada. A engine passa nela como ela
  esta, ou esta errada.
- internal/bid/shard NAO importa Prometheus: esta spec nao publica serie nova.
- Nenhum mutex sobre o estado dos leiloes. O mapa e de uma goroutine so.
- Nenhuma variavel de ambiente nova: shards, lote e inbox sao constantes.
- Rode os checkpoints C1..C5 e cole a saida real de cada um. Checkpoint sem
  saida nao conta como aceito.
- Se estourar o budget de 8 arquivos / ~700 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Criar:

```text
internal/bid/shard/engine.go     Engine, New, PlaceBid, shardFor
internal/bid/shard/shard.go      o laco: recebe, decide, acumula, comita, despeja
internal/bid/shard/state.go      auctionState, hidratacao, skew do relogio
internal/bid/shard/sql.go        os dois statements
```

Editar:

```text
internal/app/engine.go   (construir a engine; nenhuma outra estrategia muda)
```

A divisão em quatro arquivos não é estética: `shard.go` é o único lugar do projeto onde há concorrência escrita à mão, e ele deve caber numa tela sem hidratação e sem SQL ao redor.

## Testes

Adicionar:

```text
internal/bid/shard/shard_test.go     RunConformance + os seis casos de RF08
internal/bid/shard/routing_test.go   shardFor: determinismo e distribuicao, sem banco
```

Editar:

```text
internal/app/engine_test.go   shard agora constroi; so o valor desconhecido erra
```

## Checkpoints Mensuraveis

### C1 - Conformidade sob corrida, e a fronteira do diff

```bash
go test ./internal/bid/shard/... -race -count=1 -v
go test ./... -race -count=1
gofmt -l . && go vet ./...
git diff --name-only -- internal/httpapi internal/store internal/idem internal/metrics \
  internal/bid/engine.go internal/bid/outcome.go internal/bid/enginetest \
  internal/bid/optimistic internal/bid/pessimistic cmd bench migrations \
  docker-compose.yaml Makefile go.mod go.sum
```

Aceite:

- `RunConformance` passa inteira com `-race`, incluindo o replay do caso de concorrência, que é o caso escrito na etapa 1 **para esta engine** (decisão 24)
- Os seis casos de RF08 passam, inclusive o da durabilidade antes da resposta e o do segundo escritor
- `-race` não acusa nada: nenhuma leitura do mapa acontece fora da goroutine dona
- O último comando não lista arquivo algum

### C2 - Sobe, ignora `expectedVersion`, e o `201` é durável

```bash
make up && sleep 15
make run STRATEGY=shard && sleep 10
curl -s localhost:8080/metrics | grep -c 'strategy="shard"'

make seed AUCTIONS=1 TRUNCATE=1
export AID=$(jq -r '.[0].id' bench/auctions.json)
export UID=$(uuidgen)

# sem expectedVersion: no otimista isto e 400 invalid
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H 'Content-Type: application/json' -d '{"amountCents":500}'
# versao velha com valor suficiente: no otimista isto e 409
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900,"expectedVersion":0}'
# abaixo do minimo
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":100}'
# leilao inexistente, e leilao vencido
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$(uuidgen)/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900}'
make seed AUCTIONS=1 ENDS_IN=-1m OUT=bench/expired.json
curl -s -w '\n%{http_code}\n' -X POST \
  localhost:8080/auctions/$(jq -r '.[0].id' bench/expired.json)/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900}'

docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT seq, amount_cents, idempotency_key IS NULL, created_at <= a.ends_at
     FROM bids b JOIN auctions a ON a.id = b.auction_id WHERE b.auction_id = '$AID' ORDER BY seq;"
curl -s localhost:8080/metrics | grep 'bid_outcomes_total'
```

Aceite:

- `/metrics` traz séries com `strategy="shard"` antes do primeiro lance: o decorator liga o label no boot, e é assim que `run-cell.sh` reconhece a engine
- Os códigos saem `201`, `201`, `422`, `404`, `410`
- As duas linhas de `bids` existem com `seq` 1 e 2, chave `NULL` e `created_at <= ends_at` — a coluna carrega o instante da decisão, não o do commit
- `bid_outcomes_total` **não** tem série com `outcome="conflict"` nem com `outcome="invalid"`
- `GET /auctions/$AID` reporta `version: 2` e `currentHighestBid: 900`

### C3 - O lote existe entre leilões, e vale 1 num leilão só

```bash
make run STRATEGY=shard && sleep 10

commits() {
  docker compose exec -T postgres psql -U auction -d auction -tAc \
    "SELECT xact_commit FROM pg_stat_database WHERE datname = 'auction';"
}

for n in 1000 1; do
  before=$(commits)
  make bench RUN=e3c3-shard-$n STRATEGY=shard AUCTIONS=$n POLICY=immediate SCENARIO=ramp
  after=$(commits)
  accepted=$(jq -r '.accepted' bench/results/e3c3-shard-$n/client.json)
  echo "auctions=$n aceitos=$accepted transacoes=$((after - before))"
done
```

Aceite:

- Na célula de 1000 leilões, o número de transações é **bem menor** que o de lances aceitos: o lote agrupou, e a contagem ainda inclui seed, warmup e checker, ou seja, o limite é conservador
- Na célula de 1 leilão, transações e aceitos ficam na mesma ordem de grandeza: o lote vale ~1, exatamente como a decisão 48 previu **antes** da medição
- Nenhuma conclusão sobre throughput entra no PR. Isto mede o mecanismo do lote, não a estratégia

### C4 - Uma célula por contenção, com o checker verde

```bash
make run STRATEGY=shard && sleep 10
make bench RUN=e3c4-shard-1 STRATEGY=shard AUCTIONS=1 POLICY=immediate SCENARIO=smoke
echo "checker exit: $?"
make bench RUN=e3c4-shard-1000 STRATEGY=shard AUCTIONS=1000 POLICY=jitter SCENARIO=smoke

for run in e3c4-shard-1 e3c4-shard-1000; do
  jq -r --arg r "$run" '[$r,.accepted,.conflict,.outbid,.invalid,.replayed,.exhausted] | @tsv' \
    bench/results/$run/client.json
  tail -1 bench/results/$run/checker.txt
done
jq -r '.cell.strategy' bench/results/e3c4-shard-1/env.json
```

Aceite:

- As duas células terminam com código 0 e I1 a I7 verdes, sem alteração em `bench/` nem em `cmd/checker`
- `conflict: 0` e `invalid: 0` nas duas: o perdedor apareceu como `422`
- `replayed > 0`: as duplicatas da etapa 2 continuam sendo barradas com a terceira engine atrás do middleware
- I5 exato passa: `count(bids) == accepted`, o que só é verdade se todo `201` do shard for durável
- `env.json` grava `strategy: "shard"`

### C5 - Um segundo escritor aparece, e o shard perde a autoridade em vez de mentir

```bash
make seed AUCTIONS=1 TRUNCATE=1
export AID=$(jq -r '.[0].id' bench/auctions.json)
export UID=$(uuidgen)
curl -s -o /dev/null -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":500}'      # seq 1, e o shard passa a ser dono

# outro escritor grava o lance seq 2 por fora, coerente com a tabela auctions
docker compose exec -T postgres psql -U auction -d auction -c \
  "INSERT INTO bids (id, auction_id, user_id, amount_cents, seq)
   VALUES (gen_random_uuid(), '$AID', '$UID', 5000, 2);
   UPDATE auctions SET highest_bid_cents = 5000, highest_bidder = '$UID', version = 2
    WHERE id = '$AID';"

# o shard ainda acha que o proximo seq e 2
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":9000}'
docker compose logs auctiond | tail -3

# depois do despejo, a proxima tentativa re-hidrata e acerta
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":9000}'
docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT seq, amount_cents FROM bids WHERE auction_id = '$AID' ORDER BY seq;"
docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT version, highest_bid_cents FROM auctions WHERE id = '$AID';"
```

Aceite:

- A primeira tentativa sai `503` com `retryable: true`, e o log traz uma linha do lote falho — a violação de unicidade abortou o statement inteiro, sem escrita parcial
- A segunda sai `201` com `seq: 3`, sem reiniciar o processo: o estado foi despejado e re-hidratado
- A tabela termina com `seq` 1, 2 e 3 densos, e nenhum lance do lote abortado ficou gravado
- `auctions` fecha em `version: 3` e `highest_bid_cents: 9000`, coerente com a história: o shard escreve versão absoluta, então re-hidratar o reconciliou com o que o outro escritor tinha feito
- O banco fica deliberadamente sujo depois deste checkpoint, e **não** se roda `make check` sobre ele: os artefatos de C3 e C4 pertencem a outras corridas, e a próxima célula começa pelo `TRUNCATE` do `run-cell.sh`

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

curl -s -X POST localhost:8080/auctions -H 'Content-Type: application/json' \
  -d '{"title":"Smoke single-writer","startingBidCents":0,"minIncrementCents":100,
       "endsAt":"'$(date -u -d '+5 min' +%Y-%m-%dT%H:%M:%SZ)'"}' | jq

export AID=<id devolvido>
export UID=$(uuidgen)
export KEY=$(uuidgen)

# tres lances sem nunca mandar expectedVersion, num leilao criado depois do boot
for cents in 100 200 300; do
  curl -s -X POST localhost:8080/auctions/$AID/bids -H "X-User-Id: $UID" \
    -d "{\"amountCents\":$cents}" | jq -c
done

# a mesma chave duas vezes: o segundo e replay, e nao vira segunda linha
curl -s -D- -o /dev/null -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $KEY" -d '{"amountCents":400}' | grep -i replayed
curl -s -D- -o /dev/null -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $KEY" -d '{"amountCents":400}' | grep -i replayed

curl -s localhost:8080/auctions/$AID | jq
curl -s localhost:8080/readyz | jq
docker compose exec -T postgres psql -U auction -d auction -c \
  "SELECT count(*) linhas, count(idempotency_key) com_chave FROM bids WHERE auction_id = '$AID';"
make down
```

Aceite manual:

- Os três primeiros lances devolvem `201` com `seq` 1, 2 e 3, sem `expectedVersion` em nenhum: o leilão foi hidratado sob demanda depois do boot
- A primeira requisição com chave sai sem o header de replay; a segunda sai com `X-Idempotency-Replayed: true`, e a tabela fica com 4 linhas, uma delas com chave
- `GET /auctions/:id` reporta `version: 4`, `currentHighestBid: 400`, `minNextBid: 500` e `status: "open"`
- `/readyz` verde nas três condições, e nenhum log por requisição na saída do `auctiond`
- `make down` derruba tudo sem contêiner órfão

## Definicao De Pronto

- RF01 a RF09 implementados
- C1 a C5 executados, com a saída real colada no PR — checkpoint sem saída não conta como aceito
- `RunConformance` passando no shard **sem uma linha alterada** em `internal/bid/enginetest/`
- `go test ./... -race` limpo, sem nenhum aviso de corrida sobre o mapa de estado
- `git diff --name-only` vazio para toda a lista de RF09
- Duas células verdes, uma por contenção, com I1 a I7 e `conflict: 0`, `invalid: 0`
- O lote medido nas duas contenções, com o resultado de 1 leilão batendo a previsão da decisão 48
- Recuperação de commit abortado demonstrada em C5, sem reiniciar o processo
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado pelo PR de implementação
- Nenhuma série nova em `/metrics`: a instrumentação do mecanismo é a spec 02, e a etapa 3 não fecha antes dela
