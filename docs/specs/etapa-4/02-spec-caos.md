# Etapa 4 — Spec 02: Cenários de caos

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md) · [decisões da etapa 2](../../decisoes/etapa-2.md) · [decisões da etapa 3](../../decisoes/etapa-3.md) · [decisões da etapa 4](../../decisoes/etapa-4.md) · [etapa 4, spec 01](01-spec-fechamento-e-closerd.md)

## Contexto

A spec 01 entregou o mecanismo. Ela também deixou, espalhadas pelo código, seis frases que afirmam que existe um cenário de caos e o que ele vai cobrar — e nenhuma delas tem uma linha de teste por trás:

- `internal/stream/consumer.go` justifica `claimMinIdle` como *"curto o bastante para que o cenário de caos observe a reivindicação acontecer"*
- o mesmo arquivo distingue `messageGrace` dizendo que *"`docker kill` é o cenário de caos e não ganha nada disso"*
- e apresenta uma mensagem em voo por vez como *"a propriedade que o cenário de caos gasta"*
- `internal/metrics/closing.go` defende as duas gauges morarem no produtor porque, do contrário, *"o gráfico do cenário de caos ganha um buraco no lugar da evidência"*
- `docker-compose.yaml` apresenta o `closerd` como *"o segundo processo de longa duração do projeto, e o que o cenário de caos da spec 02 mata"*
- `internal/db/redis.go` promete, desde a etapa 2, que um Redis que morre depois do boot *"mantém o processo vivo, deixa `/readyz` vermelho e `/healthz` verde"*

Esta spec não instrumenta nada. **Ela não acrescenta uma série, uma coluna nem um `Outcome`** — todas as métricas de que precisa foram escritas pelas etapas 1 a 4 exatamente para que este PR só tivesse de ler. Ela acrescenta um injetor de falhas que roda por fora dos processos, e a mudança no verificador que permite a uma célula quebrada de propósito continuar sendo lida.

É também a primeira vez que três coisas acontecem no projeto:

- **I4 deixa de ser vácuo.** Toda célula das etapas 1 a 4 roda com `ENDS_IN` folgado, então nenhum lance jamais encontrou um `ends_at` no passado e *"nenhum lance após o fechamento"* passou verde sem nunca ter sido testado. O cenário do `closerd` precisa de leilões vencendo no meio da carga, e com isso coloca a guarda das três engines na borda pela primeira vez
- **Erro passa a ser o resultado esperado.** Até aqui, I6 reprova a célula acima de 1% de erro porque acima disso o que quebrou foi a medição. Numa célula de caos, o erro **é** a injeção, e uma célula de caos sem erro nenhum é a que não prova nada
- **A assimetria da etapa 1 volta, só na direção segura.** A decisão 46 igualou `db.Bids` e `client.Accepted` porque a recuperação idempotente fechou a diferença. Ela não fecha quando o processo que responderia está morto — e um `201` cujo commit venceu a resposta é o desfecho certo, não um defeito

Sustentam esta spec as decisões **4** (o verificador nunca pergunta ao servidor), **12** (fechamento é propriedade do tempo), **13** (toda célula parte do mesmo estado), **22** e **50** (o relógio do banco é a autoridade, e o shard mede o desvio dele), **36** (a idempotência falha fechada), **46** (a durabilidade é exata desde a etapa 2), **54** (o `201` do shard sai depois do commit), **59** (zero é uma afirmação diferente de silêncio), **68**, **73**, **77**, **78** e **79** (o `closerd` não responde por corretude, a fila é medida no produtor, uma mensagem em voo por vez, `XAUTOCLAIM` no mesmo laço, e convergência é medida e não afirmada), mais as decisões **81** a **93**, tomadas aqui.

## Objetivo

Provar sob carga, com falha injetada de fora, que as promessas que o projeto vem fazendo desde a etapa 1 sobrevivem a um processo morto no pior instante — e produzir disso um artefato que um leitor consegue conferir sem repetir a execução.

O sistema deve:

- Injetar quatro falhas — `closerd` morto, `auctiond` morto no modo shard, Redis pausado, pool saturado por lock de linha — **durante** uma célula de carga real, e curar cada uma
- Manter I1 a I8 verdes em todas elas, **sem relaxar um único invariante de corretude**
- Registrar em `chaos.json` o que foi injetado, quando, e a evidência de fora do processo de que a injeção aterrissou
- Ensinar ao `cmd/checker` a diferença entre uma célula quebrada de propósito e uma célula que não vale: I5 solta na direção segura, I6 para de reprovar por taxa de erro, e ambos passam a reprovar a célula de caos cuja injeção **não** aterrissou
- Medir a convergência do fechamento em vez de afirmá-la, honrando a decisão 79 com um número
- Fazer isso sem tocar em engine, handler, migration, métrica, gerador de carga ou compose

## Fora de Escopo

- Qualquer série nova, label novo ou bucket novo. Se um cenário parecer precisar de instrumentação, **pare e reporte**: significa que a spec 01 deixou o mecanismo sem evidência, e o achado é sobre ela
- Matriz de 36 células, sweep de pool e dashboards Grafana — etapa 5. Nada em `deploy/grafana/` muda, e **nenhum número de célula de caos entra em gráfico nenhum** (decisão 92)
- Célula da borda do fechamento como medida de desempenho — etapa 5. Aqui o `ENDS_IN` curto existe para tornar I4 exigível, não para medir throughput na borda
- Reassunção de shard **entre processos**. O `auctiond` é um processo só, e o que o cenário 2 prova é que a decisão em memória nunca ficou à frente do que foi prometido a alguém — não que outro nó assume os leilões
- Falha de rede parcial, partição, latência injetada, disco cheio, relógio deslocado. Todos são falhas que a arquitetura deste projeto não distingue das quatro que já estão aqui, e nenhum muda o gráfico
- Reinício automático, `restart: unless-stopped`, supervisão. Curar é trabalho do injetor, e um contêiner que ressuscita sozinho apagaria a janela que o cenário existe para observar (decisão 86)
- WebSocket, tick agregado e painel — etapa 5b
- Correção de qualquer defeito que os cenários encontrem. Achado é achado: **pare e reporte**

## Fluxo

```text
chaos/run-all.sh                       (make chaos aponta para cá)
  └── para cada cenário, em sequência, parando no primeiro código != 0:
        ├── make run STRATEGY=<a que o cenário exige>   e espera /readyz 200
        ├── CHAOS=<cenário> RUN=chaos-<cenário> bench/run-cell.sh
        └── confere a cura: auctiond 200, closerd 200, redis não pausado,
            nenhum lock pendurado — ou aborta antes do cenário seguinte

bench/run-cell.sh                      (o mesmo de sempre, mais um gancho)
  reset → warmup → reset
    ├── chaos/inject.sh <cenário> <results-dir> &        ← só quando CHAOS está setado
    ├── k6 run                                            (byte a byte o mesmo)
    │     └── código 99 (threshold estourado) é ESPERADO sob caos, fatal fora dele
    ├── wait pelo injetor                                 (ele ainda mede a convergência)
    ├── bench/env.sh                → env.json
    └── ./bin/checker -run=... -json → checker.txt / checker.json

chaos/inject.sh <cenário> <dir>        (o único que sabe quebrar coisa)
  ├── trap EXIT: CURA INCONDICIONAL — unpause, start, rollback
  ├── espera o tempo do cenário e executa os passos, um log por passo
  ├── colhe a evidência DE FORA: docker inspect, redis-cli, psql, curl /metrics
  ├── mede a convergência depois que a carga acaba
  └── escreve <dir>/chaos.json  { scenario, steps[], landed, evidence{} }

cmd/checker                            (lê mais um arquivo, muda dois vereditos)
  ├── readChaos(dir)  ausente → célula normal · malformado → exit 2
  ├── I5  db < cliente → FALHA sempre   ·   db > cliente → AVISO só sob caos
  ├── I6  erro ≥ 1% → AVISO sob caos    ·   landed=false → FALHA sob caos
  └── I1 I2 I3 I4 I7 I8 → inalterados, e é esse o ponto
```

O que **não** aparece no fluxo: nenhuma variável de ambiente lida por binário nenhum, nenhum `if chaos` dentro de `internal/`, nenhum `INSERT`, `UPDATE` ou `DELETE` partindo do injetor, e nenhuma linha nova em `bench/bid-storm.js`.

## Decisoes Tecnicas

### O caos é injetado de fora, e o binário sob teste não sabe que ele existe

Decisão 81. O injetor é um script que fala com `docker`, `redis-cli`, `psql` e `curl`. Nenhum processo do projeto lê uma variável de caos, ganha um endpoint de falha ou um `if` de injeção.

A alternativa — um `FAULT_INJECT=...` lido pelo `auctiond` — é sedutora porque dá controle fino: dá para falhar exatamente o terceiro commit do shard. E é errada pelo motivo mais simples possível: **o binário com o `if` dentro não é o binário que roda a matriz**. Um ramo de código que só existe para o caos é um ramo que ninguém exercita nas 36 células, e o dia em que ele ficar preso ligado a matriz inteira mede outra coisa em silêncio.

Matar de fora tem a propriedade que interessa: `SIGKILL` num contêiner é a falha que **não tem código nenhum por trás**. É a única forma de testar o caminho que ninguém escreveu.

### `SIGKILL`, nunca `SIGTERM`

Decisão 86. Todos os cenários que derrubam processo usam `docker kill -s KILL`.

O caminho gracioso já tem dono: `messageGrace` no `closerd` e o `ctx` do sinal no `auctiond` existem para `docker compose stop`, e a spec 01 já os demonstrou. O que nunca foi exercitado é o outro: o processo que some entre o commit e a resposta, entre o `XACK` e o próximo laço, entre a decisão em memória e o commit em lote. `SIGTERM` testaria o código do desligamento; `SIGKILL` testa o desenho.

Pela mesma razão nenhum serviço ganha `restart:`: um contêiner que volta sozinho fecha a janela que o cenário existe para observar, e faz a hora da volta ser do Docker em vez de do injetor.

### A célula de caos é uma célula

Decisão 82. `chaos/inject.sh` não tem harness próprio: ele entra em `bench/run-cell.sh` por um gancho, e a célula é a mesma — mesmo reset, mesmo warmup, mesmo segundo reset, mesmo `k6`, mesmo `env.json`, mesmo `checker`.

Um harness de caos separado provaria propriedades de um sistema que não é o medido: outro estado inicial, outro aquecimento, outro gerador. E o custo de manter dois caminhos que precisam continuar idênticos é pago para sempre, na etapa em que menos se pode pagar — a seguinte roda 36 células por cima disso.

O gancho é uma linha: com `CHAOS` setado, o injetor sobe em segundo plano junto com a carga e é esperado depois dela. Sem `CHAOS`, `run-cell.sh` se comporta exatamente como hoje, e é isso que as células da etapa 5 continuam executando.

### O gerador de carga não muda, e o `threshold` que estoura é evidência

Decisão 83. `bench/bid-storm.js` fica byte a byte igual, inclusive o `http_req_failed: ['rate<0.01']`.

O caminho fácil seria um `-e CHAOS=1` desligando o threshold, do mesmo jeito que `WARMUP` já desliga. Ele custaria a propriedade mais valiosa que o harness tem: **o gerador que produziu a célula de caos é o mesmo binário, com a mesma configuração, que produz as 36 células da matriz**. Um instrumento com um modo a mais é um instrumento a menos de comparação.

E o threshold estourando não é um problema a contornar — é a primeira confirmação, vinda do cliente e não do injetor, de que a falha aterrissou. `run-cell.sh` aceita o código 99 do k6 **apenas** sob caos, e continua fatal fora dele. Qualquer outro código do k6 continua abortando: crash do gerador não é injeção.

### `chaos.json` é o artefato, e "não aterrissou" reprova a célula

Decisão 84, que é a decisão 59 outra vez: uma célula de caos que passou verde porque a falha nunca chegou a acontecer é o pior resultado possível — parece prova e não é.

O injetor é quem sabe, e é o único que consegue saber, porque a evidência morre com a janela: um contêiner que já reiniciou não conta que reiniciou, e um Redis que já foi despausado não lembra que ficou parado. Então ele colhe a evidência **enquanto** a falha está de pé, de fora do processo:

| Cenário | Como o injetor prova, de fora, que aterrissou |
| --- | --- |
| `closerd-kill` | `docker inspect -f '{{.State.StartedAt}}'` mudou entre antes e depois |
| `auctiond-kill` | idem, e `bid_confirm_duration_seconds_count` **regrediu**: contador só anda para trás em processo novo |
| `redis-pause` | `docker inspect -f '{{.State.Paused}}'` respondeu `true` dentro da janela |
| `pool-saturation` | `pg_locks` mostrou o lock de linha, e `db_pool_empty_acquire_total` cresceu |

`landed: false`, ou `chaos.json` ausente numa execução com `CHAOS` setado, reprova a célula em I6.

**Limite registrado, dito em voz alta:** `landed` é uma afirmação do injetor sobre si mesmo, e o verificador confia nela. Não dá para fazer diferente depois do fato — o `checker` roda quando a janela já fechou, e dar Docker a ele para reconferir trocaria uma dependência barata por uma cara em cima do componente que precisa ser o mais simples do projeto. O que a spec compra em troca é que a evidência bruta fica gravada ao lado da afirmação, e os checkpoints a conferem à mão uma vez.

### Nenhum invariante de corretude é relaxado

Decisão 85, e é a linha que separa esta spec de um teatro de resiliência.

I1, I2, I3, I4, I7 e I8 valem sob caos exatamente como valem fora dele, com o mesmo veredito e a mesma severidade. Eles não descrevem uma célula saudável: descrevem o que o banco tem de conter depois de qualquer sequência de eventos, e "o processo morreu" é uma sequência de eventos.

O que muda são só dois vereditos, e os dois são sobre a célula ser **legível**, nunca sobre a engine estar certa:

| Checagem | Fora de caos | Sob caos | Por quê |
| --- | --- | --- | --- |
| I5 · `db.Bids < cliente` | FALHA | **FALHA** | Lance confirmado que sumiu. É a manchete da etapa, e não afrouxa |
| I5 · `db.Bids > cliente` | FALHA | AVISO | O `201` foi escrito e a resposta morreu com o processo. Direção segura |
| I5 · watermark `db < cliente` | FALHA | **FALHA** | Mesma coisa que a primeira linha, vista pela posição |
| I5 · watermark `db > cliente` | FALHA | AVISO | Mesma causa da segunda linha |
| I6 · erro ≥ 1% | FALHA | AVISO | Sob caos o erro é a injeção; reprovar por ele seria reprovar o experimento |
| I6 · `closed > 0` | AVISO | evidência | No cenário do `closerd` os leilões vencendo são o ponto |
| I6 · `landed = false` | — | **FALHA** | Célula de caos que não quebrou nada não prova nada (decisão 84) |

Repare no que **não** está na tabela: `invalid > 0`, contador negativo, duplicata não selecionada, replay não observado. Essas quatro continuam reprovando sob caos, porque são sobre o gerador estar configurado certo, e o gerador não é o que está sendo derrubado.

### O cenário do `closerd` é o primeiro em que I4 é exigível

Decisão 87. `chaos-closerd-kill` roda com `BENCH_ENDS_IN=45s` e leilões que vencem no meio da rampa.

Toda célula das etapas 1 a 4 rodou com `ENDS_IN` folgado, de propósito e por bons motivos (decisão 13 e o comentário no `run-cell.sh`): um leilão morrendo no meio da carga mistura contenção com a borda do fechamento num número só. A consequência silenciosa é que `WHERE ... now() < ends_at`, a guarda que sustenta a promessa central do projeto, **nunca encontrou um leilão vencido sob carga** — e I4 passou verde por vacuidade em toda célula já rodada.

Aqui ele deixa de ser vácuo, e o que se cobra é a conjunção que a decisão 12 desenhou: **o `closerd` estar morto não pode deixar entrar lance atrasado**, porque quem recusa não é a coluna `status`, é o `ends_at` comparado ao relógio do banco. Com o worker fora do ar, a coluna continua dizendo `open` por um minuto inteiro — e nenhum lance entra.

O cenário roda duas vezes, e a segunda existe por um motivo específico: sob `shard`, quem decide o fechamento é uma goroutine em memória usando o desvio de relógio medido na hidratação (decisão 50), e `created_at` é escrito a partir do instante da **decisão**, não do commit. Esse desenho nunca encontrou um `ends_at` de verdade. `chaos-closerd-kill-shard` é o mesmo cenário, com a mesma injeção, contra a engine que é a aposta do projeto.

Se I4 reprovar sob `shard` e passar sob `optimistic`, isso é um achado sobre a engine e **não** se conserta aqui: pare e reporte.

### O cenário do Redis prova que o sistema falha fechado, e que a fila fica em silêncio

Decisão 89. `docker pause redis` por 5 segundos, no meio da rampa.

Três promessas escritas em etapas diferentes são cobradas de uma vez:

- **A decisão 36**, que fez a idempotência falhar fechada: Redis fora do ar não vira lance sem guarda, vira `503 unavailable` com `retryable: true`. O `ReadTimeout` padrão do cliente transforma o congelamento em erro em três segundos em vez de pendurar a requisição
- **A promessa de `internal/db/redis.go`**, de que um Redis que morre depois do boot deixa `/readyz` vermelho e `/healthz` verde — o processo continua vivo e diagnosticável em vez de entrar em crashloop
- **As decisões 59 e 73**, e é a mais bonita: com o Redis congelado, o `XINFO GROUPS` do scrape falha, e as duas gauges de fila **não são emitidas**. Não saem zeradas. Um painel que mostrasse zero de fila durante um apagão do Redis estaria mentindo exatamente no incidente, e é isso que o injetor grava: o `/metrics` colhido dentro da janela, sem as duas linhas

O que este cenário **também** mostra, e a spec diz sem enfeitar: com a idempotência acima do switch de estratégia, um Redis indisponível é uma indisponibilidade de escrita completa nas três engines. Não é um defeito, é o preço da decisão 36 — e ele agora tem um número em vez de um parágrafo.

### O cenário do pool usa lock de linha, e roda no pessimista

Decisão 90. Uma transação de fora segura `SELECT ... FOR UPDATE` sobre a linha do leilão por oito segundos, e some.

`pg_sleep` em N conexões saturaria o **Postgres**, que não é o recurso sob teste: o que se quer encher é o pool do `auctiond`, e a única forma honesta de enchê-lo é fazer o trabalho que ele já faz demorar. Um lock de linha faz exatamente isso, sem tocar em configuração e sem escrever nada: a transação do injetor termina em `ROLLBACK`, sempre.

Roda no `pessimistic` porque é a engine cuja tese é o pool: ela segura conexão durante a espera de lock, e é a única das três em que "saturou o pool" e "esperou lock" são o mesmo evento. É também a engine que a etapa 5 vai varrer por tamanho de pool, e este cenário é o piso daquela varredura.

O que se cobra é contrapressão, não ausência de erro: as requisições enfileiram, `db_pool_empty_acquire_total` cresce, `db_pool_conns{state="acquired"}` bate no teto, e quando o lock some a fila drena. Nada é escrito pela metade, porque uma transação bloqueada é uma transação que não escreveu.

**Um resultado possível vale como achado e não como falha:** `/readyz` pode responder 503 durante a janela, porque a sonda do Postgres também pega conexão do mesmo pool. Se acontecer, o injetor grava o código e a spec 02 termina com isso registrado — acoplar prontidão a um pool saturado é uma propriedade real do desenho, e descobri-la é o que o cenário serve para fazer.

### Cada cenário declara a estratégia que exige

Decisão 91. Nem todos os cenários são independentes de engine, e fingir que são produziria uma prova mais fraca de graça.

| Cenário | Estratégia | Por quê |
| --- | --- | --- |
| `closerd-kill` | `optimistic` | O `closerd` não está no caminho da requisição; a mais barata serve |
| `closerd-kill-shard` | `shard` | A engine que decide em memória encontrando um `ends_at` real (decisão 87) |
| `auctiond-kill` | `shard` | A única com estado que morre junto com o processo |
| `redis-pause` | `optimistic` | O middleware fica acima do switch: as três se comportam igual |
| `pool-saturation` | `pessimistic` | A engine cujo ponto de sincronização é o pool (decisão 90) |

### Idempotencia

Esta spec não acrescenta nenhuma camada, e as três que existem são **postas à prova** em vez de estendidas:

- **No efeito do fechamento, por construção.** A guarda `status = 'open'` da decisão 68 é o que faz a mensagem reentregue depois de um `XAUTOCLAIM` não mover `closed_at`. O cenário 1 é o teste dela sob carga, e o contador `already_closed` é onde a segunda entrega aparece
- **Na origem, por supressão com prazo.** Com o `closerd` morto por trinta segundos, o varredor republica; a supressão da decisão 70 é o que impede que trinta segundos virem trezentas cópias. O injetor grava o pico de `stream_backlog_entries`, e ele é a medida direta dessa supressão funcionando
- **No lance, pelo `X-Idempotency-Key`.** O cenário 3 é o único em que a camada da etapa 2 é atacada, e o resultado esperado é ela recusar trabalho em vez de deixar passar sem guarda (decisão 36)

O que a etapa 2 **não** cobre, e esta spec registra: a recuperação idempotente do transporte precisa que alguém esteja vivo para replicar o `201` guardado. Matando o `auctiond`, a marca em Redis expira sem dono e a resposta se perde de vez. É exatamente por isso que I5 solta na direção `db > cliente` sob caos (decisão 85), e por isso que ele **não** solta na outra.

### Observabilidade

**Zero séries novas.** É o requisito mais forte desta spec e o mais fácil de violar sem perceber.

O que o injetor lê, e de onde:

| Evidência | Fonte | Cenário |
| --- | --- | --- |
| `stream_backlog_entries`, `stream_pending_entries` | `/metrics` do `auctiond` | 1 e 3 |
| `auction_closings_total{result}`, `stream_claimed_total` | `/metrics` do `closerd` | 1 |
| `bid_confirm_duration_seconds_count{strategy}` | `/metrics` do `auctiond` | 2 |
| `bid_outcomes_total{outcome="accepted"}` | `/metrics` do `auctiond` | 3 |
| `db_pool_empty_acquire_total`, `db_pool_conns{state}` | `/metrics` do `auctiond` | 4 |
| `up{job="closerd"}` | não é lido | — |

A última linha é deliberada: o Prometheus registra a queda do alvo como `up{job="closerd"} 0` (decisão 74), e isso é um dado excelente **para um painel**. O injetor não o consome, pelo mesmo motivo da decisão 4 — o artefato de uma prova não pode depender de um terceiro processo que também pode estar fora do ar durante o cenário.

Nada muda em `bid_confirm_duration_seconds`, `bid_outcomes_total`, nas séries do pool, nas de idempotência, nas quatro do shard nem nas sete do fechamento.

## Requisitos Funcionais

### RF01 - `chaos/inject.sh`: o injetor

```text
chaos/inject.sh <cenário> <results-dir>
```

Um cenário desconhecido, ou argumentos faltando, sai com código diferente de zero e imprime os cinco nomes válidos. Roda em segundo plano a partir de `run-cell.sh`, com a carga já iniciada.

Obrigações, todas elas:

1. **Curar sempre.** Um `trap` de saída que despausa o Redis, sobe o que estiver parado e desfaz a transação do lock, incondicionalmente e sem depender de qual passo estava em andamento. Rodar a cura duas vezes é inofensivo, e ela roda também quando o injetor recebe `SIGTERM`
2. **Nunca escrever no banco.** O único SQL do injetor é `SELECT`, e o único `SELECT ... FOR UPDATE` vive numa transação que termina em `ROLLBACK`. Um `INSERT` vindo daqui quebraria I5 e faria a evidência falsificar a prova
3. **Um log por passo**, com carimbo de tempo, em `stdout`: a saída do injetor é a linha do tempo do cenário e é o que aparece no PR
4. **`docker compose start`, nunca `up -d`**: `up` recria o contêiner e pode ressemeá-lo com um `STRATEGY` diferente do que a célula verificou antes da carga
5. **Colher a evidência dentro da janela** (decisão 84) e escrever `chaos.json` no diretório recebido, mesmo quando um passo falhar — um cenário que abortou no meio precisa deixar dito que abortou

### RF02 - Cenário `closerd-kill`

Exige `STRATEGY=optimistic`, `AUCTIONS=10`, `BENCH_ENDS_IN=45s`, `SCENARIO=ramp`, `POLICY=immediate`.

| t | Passo |
| --- | --- |
| 35s | `docker kill -s KILL closerd` — dez segundos antes do primeiro `ends_at` |
| 45s | os leilões vencem sem consumidor: o varredor publica, `stream_backlog_entries` cresce, nada fecha, **e todo lance passa a receber 410** |
| 75s | `docker compose start closerd`; a fila começa a drenar |
| 78s | `docker kill -s KILL closerd` de novo, agora com alta probabilidade de mensagem na mão |
| 80s | `docker compose start closerd` |
| pós-carga | espera até 120s por `pending` e `backlog` zerados, e grava quanto levou |

Evidência gravada: `startedAtBefore`/`startedAtAfter` das duas mortes, pico de `backlog` e de `pending`, `stream_claimed_total` final, `auction_closings_total` por `result`, contagem de linhas `closed` no banco, e `convergedAfterSeconds`.

O que precisa continuar valendo: **I1 a I8 verdes**, com I4 finalmente não vazio; nenhum leilão com `closed_at < ends_at`; `applied` igual ao número de linhas fechadas; `backlog` e `pending` de volta a zero.

### RF03 - Cenário `closerd-kill-shard`

Idêntico ao RF02 em passos e evidência, com `STRATEGY=shard`. Existe pela decisão 87: é a única execução do projeto em que a engine que decide em memória encontra um `ends_at` no passado sob carga.

Não é um cenário novo no injetor — é o mesmo, e `chaos/run-all.sh` o executa uma segunda vez com outra estratégia e outro `RUN`.

### RF04 - Cenário `auctiond-kill`

Exige `STRATEGY=shard`, `AUCTIONS=10`, `BENCH_ENDS_IN=30m`, `SCENARIO=ramp`, `POLICY=immediate`.

| t | Passo |
| --- | --- |
| 50s | lê `bid_confirm_duration_seconds_count{strategy="shard"}` e o `StartedAt` do contêiner |
| 50s | `docker kill -s KILL auctiond`, com a rampa em 500 VUs |
| 53s | `docker compose start auctiond`, e espera `/readyz` 200, gravando quanto demorou |
| 55s | relê o contador: um valor **menor** que o de antes prova processo novo |

O que precisa continuar valendo, e é a manchete da etapa: **nenhum lance confirmado sumiu**. I5 na direção `db.Bids < client.Accepted` continua sendo FALHA, e é o que este cenário existe para não disparar — o `201` do shard só sai depois do commit em lote (decisão 54), então não existe janela de aceite não persistido para a morte encontrar.

I1 é a segunda metade da prova: a sequência densa mostra que o `seq` decidido em memória e perdido com o processo **nunca foi prometido a ninguém**, e que a hidratação depois do reinício retomou de onde o banco estava.

`db.Bids > client.Accepted` é esperado aqui e sai como AVISO com o número: são os commits cuja resposta morreu no caminho.

### RF05 - Cenário `redis-pause`

Exige `STRATEGY=optimistic`, `AUCTIONS=10`, `BENCH_ENDS_IN=30m`, `SCENARIO=ramp`, `POLICY=immediate`.

| t | Passo |
| --- | --- |
| 50s | lê `bid_outcomes_total{outcome="accepted"}` |
| 50s | `docker pause redis` |
| dentro | `docker inspect -f '{{.State.Paused}}' redis`, `/readyz` do `auctiond` com corpo, e o `/metrics` inteiro salvo em `chaos.json` como as duas gauges **ausentes** |
| 55s | `docker unpause redis`, e espera `/readyz` 200 |
| 56s | relê o contador de aceitos: a diferença são as requisições que já tinham passado do middleware quando o Redis congelou |

O que precisa continuar valendo: `/healthz` 200 durante a janela inteira, `/readyz` 503 nomeando o check do Redis, nenhuma das duas gauges de fila emitida enquanto pausado, e I1 a I8 verdes — que aqui é quase trivial, e de propósito: falhar fechado significa que quase nada foi escrito, e a prova é essa.

A diferença dos aceitos é gravada como número, **não** afirmada como zero: uma requisição já dentro do handler completa normalmente, e isso é o comportamento certo.

### RF06 - Cenário `pool-saturation`

Exige `STRATEGY=pessimistic`, `AUCTIONS=1`, `BENCH_ENDS_IN=30m`, `SCENARIO=ramp`, `POLICY=immediate`. Um leilão só, para que toda a carga dispute a mesma linha.

| t | Passo |
| --- | --- |
| 50s | lê `db_pool_empty_acquire_total` e `db_pool_conns{state="acquired"}` |
| 50s | abre a transação: `BEGIN; SELECT id FROM auctions WHERE id = $AID FOR UPDATE;` e segura |
| dentro | `pg_locks` confirmando o lock, `/healthz` e `/readyz` com código e corpo, e as duas séries de pool relidas |
| 58s | `ROLLBACK` |
| 60s | relê as séries de pool e espera a fila drenar |

O que precisa continuar valendo: o processo vivo (`/healthz` 200 o tempo todo), `db_pool_empty_acquire_total` maior do que antes, e I1 a I8 verdes. `/readyz` pode ficar vermelho e isso é gravado, não julgado (decisão 90).

### RF07 - `chaos.json`

Escrito pelo injetor, no diretório da célula, ao lado de `client.json` e `env.json`:

```json
{
  "scenario": "closerd-kill",
  "target": "closerd",
  "strategy": "optimistic",
  "landed": true,
  "steps": [
    {"at": "2026-08-23T18:04:35Z", "action": "kill",  "target": "closerd"},
    {"at": "2026-08-23T18:05:15Z", "action": "start", "target": "closerd"}
  ],
  "evidence": {
    "startedAtBefore": "2026-08-23T17:58:02.114Z",
    "startedAtAfter":  "2026-08-23T18:05:16.902Z",
    "backlogPeak": 20,
    "pendingPeak": 1,
    "claimed": 1,
    "closings": {"applied": 10, "already_closed": 1, "gone": 0, "early": 0},
    "closedRows": 10,
    "convergedAfterSeconds": 34.2
  }
}
```

`scenario` e `landed` são os únicos campos que o `checker` lê. `evidence` tem forma livre por cenário e existe para o leitor e para a etapa 6 — impor um esquema comum a quatro falhas que não se parecem produziria campos nulos que ninguém consegue interpretar.

### RF08 - `bench/run-cell.sh`: o gancho

Com `CHAOS` vazio, o comportamento é o de hoje, sem exceção. Com `CHAOS` setado:

- O injetor sobe em segundo plano imediatamente antes da carga medida — nunca antes do warmup, que precisa ser um aquecimento e não uma injeção
- O `trap` de saída que já mata o observador do gerador passa a mandar `SIGTERM` no injetor também, para que o `trap` **dele** cure o sistema
- O código de saída do k6 deixa de abortar a célula quando é exatamente `99` (threshold estourado). Qualquer outro código continua abortando, sob caos ou não (decisão 83)
- O injetor é esperado depois do k6, porque a medição de convergência do cenário 1 acontece depois da carga

Nenhuma outra linha muda: o pré-voo de estratégia, os dois resets, o `FLUSHALL`, o `env.sh` e a chamada do `checker` ficam idênticos.

### RF09 - `cmd/checker`: o modo caos

`cmd/checker/chaos.go`, novo:

```go
type chaosStep struct {
    At     string `json:"at"`
    Action string `json:"action"`
    Target string `json:"target"`
}

// chaosReport is what the injector left behind. Absent means a normal cell;
// malformed means exit 2.
type chaosReport struct {
    Scenario string      `json:"scenario"`
    Target   string      `json:"target"`
    Strategy string      `json:"strategy"`
    Landed   bool        `json:"landed"`
    Steps    []chaosStep `json:"steps"`
}

func readChaos(dir string) (*chaosReport, error)   // (nil, nil) quando ausente
```

`report` ganha `Chaos *chaosReport \`json:"chaos,omitempty"\``, e `render` imprime uma linha antes dos vereditos quando ele existe:

```text
caos: closerd-kill · alvo closerd · estratégia optimistic · 4 passos · aterrissou
```

`checkDurability` e `checkCellValidity` passam a receber o ponteiro. Nulo é a célula normal e nada muda; não-nulo aplica a tabela da decisão 85, e nada além dela.

Um `chaos.json` ilegível é `exit 2` — NÃO VERIFICADO —, pela mesma regra de `client.json`: um arquivo que existe e não decodifica é um artefato quebrado, e uma célula sem artefato íntegro não recebe veredito.

### RF10 - `chaos/run-all.sh` e `make chaos`

`chaos/run-all.sh` roda os cinco em sequência, cada um com sua estratégia e seu `RUN`:

| Ordem | `RUN` | `STRATEGY` | `BENCH_ENDS_IN` | `AUCTIONS` |
| --- | --- | --- | --- | --- |
| 1 | `chaos-closerd-kill` | `optimistic` | `45s` | 10 |
| 2 | `chaos-closerd-kill-shard` | `shard` | `45s` | 10 |
| 3 | `chaos-auctiond-kill` | `shard` | `30m` | 10 |
| 4 | `chaos-redis-pause` | `optimistic` | `30m` | 10 |
| 5 | `chaos-pool-saturation` | `pessimistic` | `30m` | 1 |

Por cenário, e nesta ordem: sobe a estratégia exigida, espera `/readyz` 200 nos **dois** processos, roda a célula, e **confere a cura** antes de seguir — `auctiond` 200, `closerd` 200, Redis não pausado, nenhum lock pendurado. Para no primeiro código diferente de zero e propaga esse código.

O prefixo `chaos-` no `RUN` é o que mantém estas células fora da matriz (decisão 92): a etapa 5 escreve em `bench/results/` com nomes de célula, e a etapa 6 lê a matriz, nunca este prefixo.

`make chaos` é uma linha que chama `chaos/run-all.sh`, e a spec registra o motivo de qualquer automação chamar o script direto: **o GNU make colapsa qualquer código de recipe em 2**, então passar o laço da etapa 5 por dentro do `make` apagaria a diferença entre invariante violado (1) e célula não verificável (2). O alvo existe para gente, o script existe para automação (decisão 93).

### RF11 - O resto do sistema não muda

O diff de implementação não pode conter arquivos em:

```text
internal/
cmd/auctiond/
cmd/closerd/
cmd/seed/
migrations/
deploy/
docker-compose.yaml
Dockerfile
bench/bid-storm.js
bench/env.sh
.env.example
go.mod
go.sum
```

`internal/` inteiro está na lista, e é a linha mais importante dela: esta spec **lê** o sistema e não o altera. `bench/bid-storm.js` está pela decisão 83, e `docker-compose.yaml` porque nenhum serviço ganha `restart:` (decisão 86).

As únicas exceções, dentro de `cmd/`, são `cmd/checker/main.go`, `cmd/checker/client.go` e seus testes.

Se algum outro parecer precisar mudar, **pare e reporte**: é achado sobre a spec 01, não tarefa desta.

## Requisitos Nao Funcionais

- Nenhuma série, label ou bucket novo. `curl /metrics | sort` dos dois processos, antes e depois do PR, é idêntico
- Nenhuma dependência nova em `go.mod`, e nenhum import novo no `cmd/checker` além do que ele já usa
- Nenhuma variável de ambiente de caos lida por binário nenhum: `grep -rn 'CHAOS' cmd/ internal/` sai vazio
- Nenhum `if` de caos dentro de `internal/`
- O injetor nunca emite `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE` nem `ALTER`
- O injetor cura sempre, inclusive quando interrompido, e a cura é idempotente
- `bench/bid-storm.js` byte a byte igual: `git diff --stat -- bench/bid-storm.js` vazio
- `bench/run-cell.sh` sem `CHAOS` produz o comportamento de hoje, incluindo abortar em qualquer código do k6
- Os scripts passam em `shellcheck` sem aviso, e usam `set -euo pipefail` como os que já existem
- `go test ./... -race` limpo; `gofmt -l .` vazio; `go vet ./...` sem saída
- Um cenário completo, do `make run` ao `checker`, cabe em cinco minutos; os cinco em trinta
- Nenhuma conclusão sobre throughput, latência ou comparação entre engines entra no PR

## Budget do PR

Até 10 arquivos e aproximadamente 650 linhas, shell incluído.

O peso está no shell: o injetor é o arquivo grande do PR porque ele carrega quatro falhas diferentes e a colheita de evidência de cada uma, e porque não dá para fatorar quatro falhas que não se parecem sem inventar uma abstração pior do que a repetição. O Go é pequeno de propósito — um arquivo novo de sessenta linhas e dois vereditos alterados.

Se o PR passar de 10 arquivos ou 650 linhas, **pare e reporte**. O corte é `pool-saturation`, e nesta ordem: é o cenário que menos compartilha com os outros três — não derruba contêiner, e sua evidência inteira são séries de pool —, é o que a etapa 5 revisita no sweep do pessimista, e os outros quatro continuam provando o que a etapa promete sem ele.

Se a conta estourar por causa de `internal/`, pare bem mais cedo: significa que o caos entrou no processo, que é o que a decisão 81 existe para impedir.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: alto
- Referencia permitida: `docs/projeto/provas.md`, `docs/projeto/benchmark.md`, `docs/projeto/observabilidade.md`, `docs/decisoes/etapa-1.md`, `docs/decisoes/etapa-2.md`, `docs/decisoes/etapa-3.md`, `docs/decisoes/etapa-4.md`, `docs/specs/etapa-4/01-spec-fechamento-e-closerd.md`, `docs/specs/etapa-4/02-spec-caos.md`

Prompt:

```text
Implemente docs/specs/etapa-4/02-spec-caos.md no repositorio bid-storm.

Leia antes de comecar:
  docs/specs/etapa-4/02-spec-caos.md   (a spec — a autoridade)
  docs/decisoes/etapa-4.md             (o porque; decisoes 81 a 93, e 68 a 80)
  docs/projeto/provas.md               (a tabela de caos que esta spec cumpre)
  bench/run-cell.sh                    (a celula inteira; o gancho entra nela)
  cmd/checker/client.go                (I5 e I6 como estao hoje)
  cmd/checker/main.go                  (readClient/readEnv: readChaos imita os dois)
  internal/stream/consumer.go          (claimMinIdle, messageGrace: o que o caos gasta)

Escopo: apenas RF01..RF11. NAO implemente serie nova, migration, mudanca de
engine, dashboard, matriz, painel nem correcao de qualquer defeito que os
cenarios encontrarem.

Regras:
- Modulo: github.com/samuka7abr/bid-storm
- NAO altere internal/, cmd/auctiond/, cmd/closerd/, cmd/seed/, migrations/,
  deploy/, docker-compose.yaml, Dockerfile, bench/bid-storm.js, bench/env.sh,
  .env.example, go.mod nem go.sum. Dentro de cmd/, so cmd/checker/.
- ZERO series novas. Se um cenario parecer precisar de instrumentacao, pare e
  reporte: e achado sobre a spec 01.
- Nenhum binario le variavel de caos. grep -rn 'CHAOS' cmd/ internal/ sai vazio.
- O injetor NUNCA escreve no banco. O unico SELECT ... FOR UPDATE vive numa
  transacao que termina em ROLLBACK.
- O injetor cura SEMPRE, por trap de saida, inclusive sob SIGTERM, e a cura e
  idempotente. Uma execucao que deixa o Redis pausado envenena toda celula
  seguinte.
- docker kill -s KILL, nunca SIGTERM. docker compose start, nunca up -d.
- Nenhum invariante de corretude e relaxado. Sob caos mudam SO: I5 na direcao
  db > cliente (AVISO), I5 watermark na direcao db > cliente (AVISO), I6 taxa
  de erro (AVISO) e I6 landed=false (FALHA). I5 na direcao db < cliente
  continua FALHA — e a manchete da etapa.
- chaos.json ausente = celula normal. chaos.json ilegivel = exit 2.
- Rode os checkpoints C1..C5 e cole a saida real de cada um. Checkpoint sem
  saida nao conta como aceito.
- Se estourar o budget de 10 arquivos / ~650 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Criar:

```text
chaos/inject.sh              os quatro cenarios, a colheita e a cura por trap
chaos/run-all.sh             o laco dos cinco runs, com a cura conferida entre eles
cmd/checker/chaos.go         chaosReport, readChaos, a linha do relatorio
cmd/checker/chaos_test.go    ausente, malformado, landed true e false
```

Editar:

```text
bench/run-cell.sh            o gancho CHAOS, o trap, o codigo 99 do k6
cmd/checker/main.go          readChaos no execute, Chaos no report, a linha em render
cmd/checker/client.go        I5 e I6 sob caos
cmd/checker/client_test.go   as seis direcoes da tabela da decisao 85
Makefile                     make chaos
```

## Testes

Adicionar:

```text
cmd/checker/chaos_test.go    RF09, a leitura do artefato
```

Editar:

```text
cmd/checker/client_test.go   RF09, os vereditos sob caos
```

Os scripts não ganham teste automatizado, e a spec registra por quê: `chaos/inject.sh` só faz sentido contra um compose de pé, e um harness de shell que subisse contêineres para testar o script que sobe contêineres seria mais código do que o script. O aceite deles é C2, que roda os cinco cenários contra um sistema ocioso e confere o `chaos.json` e a cura.

## Checkpoints Mensuraveis

### C1 - Unidade, e a fronteira do diff

```bash
go test ./cmd/checker/... -race -count=1 -v
go test ./... -race -count=1
gofmt -l . && go vet ./...
shellcheck chaos/inject.sh chaos/run-all.sh bench/run-cell.sh

git diff --name-only -- internal cmd/auctiond cmd/closerd cmd/seed migrations \
  deploy docker-compose.yaml Dockerfile bench/bid-storm.js bench/env.sh \
  .env.example go.mod go.sum
grep -rn 'CHAOS' cmd/ internal/ || echo "nenhum binario le caos"
```

Aceite:

- Os casos de `chaos_test.go` passam: ausente vira célula normal, malformado vira erro, `landed` false e true produzem vereditos opostos em I6
- As seis direções da tabela da decisão 85 passam em `client_test.go`, inclusive as duas que **não** afrouxam sob caos
- A suíte inteira continua verde, com a conformidade das três engines intocada
- `git diff --name-only` não lista arquivo algum
- `grep -rn CHAOS` não encontra nada em `cmd/` nem `internal/`
- `shellcheck` sem aviso nos três scripts

### C2 - O injetor sozinho: aterrissa, grava e cura

Sem carga, contra o sistema ocioso. Prova o injetor antes de confiar nele dentro de uma célula.

```bash
make up && sleep 20
mkdir -p /tmp/chaos-probe

for s in closerd-kill auctiond-kill redis-pause pool-saturation; do
  echo "== $s"
  chaos/inject.sh "$s" /tmp/chaos-probe
  jq -c '{scenario, landed, steps: (.steps | length), evidence: (.evidence | keys)}' /tmp/chaos-probe/chaos.json
  curl -s -o /dev/null -w "auctiond %{http_code}\n" localhost:8080/readyz
  curl -s -o /dev/null -w "closerd  %{http_code}\n" localhost:8081/readyz
  docker inspect -f '{{.State.Paused}}' "$(docker compose ps -q redis)"
  docker compose exec -T postgres psql -U auction -d auction -qtA \
    -c "SELECT count(*) FROM pg_locks WHERE mode = 'ExclusiveLock' AND relation IS NOT NULL"
done

# cenario desconhecido
chaos/inject.sh nao-existe /tmp/chaos-probe; echo "exit=$?"

# e a cura sob interrupcao: matar o injetor no meio nao pode deixar o Redis parado
chaos/inject.sh redis-pause /tmp/chaos-probe & sleep 3; kill %1; wait
docker inspect -f '{{.State.Paused}}' "$(docker compose ps -q redis)"
curl -s -o /dev/null -w "auctiond %{http_code}\n" localhost:8080/readyz
```

Aceite:

- Os quatro escrevem `chaos.json` com `landed: true` e evidência não vazia
- Depois de cada um, os dois `/readyz` respondem 200, o Redis não está pausado e não há lock pendurado
- Cenário desconhecido sai com código diferente de zero e imprime os nomes válidos
- **O injetor interrompido com `SIGTERM` no meio da pausa despausa o Redis mesmo assim** — é o requisito que impede uma execução abortada de envenenar toda célula seguinte

### C3 - `closerd` morto sob carga, e I4 deixando de ser vácuo

```bash
make run STRATEGY=optimistic && sleep 10
CHAOS=closerd-kill RUN=chaos-closerd-kill AUCTIONS=10 BENCH_ENDS_IN=45s \
  SCENARIO=ramp POLICY=immediate STRATEGY=optimistic bench/run-cell.sh
echo "exit=$?"

cat bench/results/chaos-closerd-kill/checker.txt
jq '.evidence' bench/results/chaos-closerd-kill/chaos.json

# I4 e exigivel aqui, e so aqui
docker compose exec -T postgres psql -U auction -d auction -qtA -c "
  SELECT count(*) FILTER (WHERE status='closed'),
         count(*),
         (SELECT count(*) FROM bids b JOIN auctions a ON a.id=b.auction_id
           WHERE b.created_at > a.ends_at),
         (SELECT count(*) FROM auctions WHERE closed_at < ends_at)
    FROM auctions"

curl -s localhost:8081/metrics | grep -E '^(auction_closings_total|stream_claimed_total)'
curl -s localhost:8080/metrics | grep -E '^(stream_backlog_entries|stream_pending_entries) '
jq '.metrics.bids_closed.values.count' bench/results/chaos-closerd-kill/summary.json
```

Aceite:

- I1 a I8 **verdes**, com I5 exato — o `closerd` não está no caminho da requisição e não tem como mover a durabilidade
- A linha `caos:` aparece acima dos vereditos, nomeando o cenário e dizendo que aterrissou
- Os dez leilões terminam `closed`, e as duas contagens de violação — lance depois de `ends_at`, e `closed_at` antes de `ends_at` — saem **zero**: é a primeira vez no projeto que essas duas colunas foram cobradas com leilões vencendo sob carga
- `bids_closed` no `summary.json` é maior que zero: os 410 aconteceram, com o worker morto, e I4 continuou verde
- `already_closed` maior que zero, mostrando a reentrega que o `XAUTOCLAIM` recuperou; `stream_claimed_total` fora do zero
- `backlog` e `pending` de volta a zero, e `convergedAfterSeconds` gravado
- I6 sai como AVISO por taxa de erro, nunca FALHA

### C4 - `auctiond` morto no shard, e a manchete da etapa

```bash
make run STRATEGY=shard && sleep 10
CHAOS=closerd-kill RUN=chaos-closerd-kill-shard AUCTIONS=10 BENCH_ENDS_IN=45s \
  SCENARIO=ramp POLICY=immediate STRATEGY=shard bench/run-cell.sh
echo "exit=$?"
cat bench/results/chaos-closerd-kill-shard/checker.txt

CHAOS=auctiond-kill RUN=chaos-auctiond-kill AUCTIONS=10 \
  SCENARIO=ramp POLICY=immediate STRATEGY=shard bench/run-cell.sh
echo "exit=$?"
cat bench/results/chaos-auctiond-kill/checker.txt
jq '.evidence, .steps' bench/results/chaos-auctiond-kill/chaos.json

jq '{accepted: .metrics.bids_accepted.values.count,
     maxSeq: .metrics.seq_seen.values.max,
     transport: .metrics.transport_retries.values.count}' \
   bench/results/chaos-auctiond-kill/client.json
docker compose exec -T postgres psql -U auction -d auction -qtA \
  -c "SELECT count(*), max(seq) FROM bids"
```

Aceite:

- **I5 nunca reporta `LANCE CONFIRMADO SUMIU`**, em nenhum dos dois runs. É a manchete: o `201` do shard sai depois do commit, e matar o processo não tem como produzir um aceite não persistido
- I1 verde: a sequência continua densa depois do reinício, provando que o `seq` decidido em memória e perdido na morte nunca foi prometido a ninguém, e que a hidratação retomou do banco
- I4 verde no run `-shard`, que é a primeira vez que a engine que decide em memória encontra um `ends_at` real sob carga (decisão 87). Se ele reprovar, **pare e reporte**: é achado sobre a engine
- `transport_retries` maior que zero: o cliente viu a morte
- Se I5 sair AVISO por `db > cliente`, o número aparece no detalhe e a célula continua verde
- O contador `bid_confirm_duration_seconds_count` regrediu entre as duas leituras do injetor, provando processo novo

### C5 - Redis, pool, e a suíte inteira pelo `run-all.sh`

```bash
make run STRATEGY=optimistic && sleep 10
CHAOS=redis-pause RUN=chaos-redis-pause AUCTIONS=10 \
  SCENARIO=ramp POLICY=immediate STRATEGY=optimistic bench/run-cell.sh
jq '.evidence' bench/results/chaos-redis-pause/chaos.json
cat bench/results/chaos-redis-pause/checker.txt

make run STRATEGY=pessimistic && sleep 10
CHAOS=pool-saturation RUN=chaos-pool-saturation AUCTIONS=1 \
  SCENARIO=ramp POLICY=immediate STRATEGY=pessimistic bench/run-cell.sh
jq '.evidence' bench/results/chaos-pool-saturation/chaos.json
cat bench/results/chaos-pool-saturation/checker.txt

# e agora os cinco de ponta a ponta, pelo caminho que a automacao usa
chaos/run-all.sh; echo "exit=$?"
for r in chaos-closerd-kill chaos-closerd-kill-shard chaos-auctiond-kill \
         chaos-redis-pause chaos-pool-saturation; do
  printf '%-28s %s\n' "$r" "$(jq -r '"exit=\(.exit) falhas=\(.failures) avisos=\(.warnings) caos=\(.chaos.scenario)"' \
    bench/results/$r/checker.json)"
done

# o make e para gente, e colapsa o codigo: registrado, nao consertado
make chaos > /dev/null 2>&1; echo "make exit=$?"
```

Aceite:

- **Redis:** `landed` com `Paused: true` gravado, `/healthz` 200 e `/readyz` 503 nomeando o check do Redis durante a janela, e o `/metrics` colhido dentro dela **sem** `stream_pending_entries` e **sem** `stream_backlog_entries` — silêncio, não zero (decisões 59 e 73), demonstrado num apagão real pela primeira vez
- **Pool:** `db_pool_empty_acquire_total` cresceu, `db_pool_conns{state="acquired"}` bateu no teto durante a janela, e `/healthz` respondeu 200 o tempo todo. O código do `/readyz` está gravado, seja ele qual for
- Os cinco `checker.json` saem com `exit: 0` e `failures: 0`, cada um carregando o bloco `chaos` que identifica o cenário
- `chaos/run-all.sh` sai com 0, e roda os cinco na ordem, conferindo a cura entre eles
- `make chaos` também sai com 0 — e a saída não-zero dele, quando houver, é 2 seja qual for a causa, que é exatamente o motivo de a automação chamar o script (decisão 93)

## Smoke Manual

Pre-condicoes:

```text
Docker e docker compose v2, jq, uuidgen, shellcheck e make instalados
Portas livres: 5432, 6379, 8080, 8081, 9090, 3000
Repositorio limpo, .env criado a partir de .env.example
```

Passos:

```bash
make up && sleep 20
make seed AUCTIONS=1 ENDS_IN=40s TRUNCATE=1
export AID=$(jq -r '.[0].id' bench/auctions.json)
export UID=$(uuidgen)

# o lance passa com o sistema inteiro de pe
curl -s -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $(uuidgen)" \
  -d '{"amountCents":500,"expectedVersion":0}' | jq '{seq, minNextBid}'

# mata o closerd ANTES do leilao vencer, e observa o que nao acontece
docker kill -s KILL "$(docker compose ps -q closerd)"
sleep 45

# a coluna continua dizendo open, porque ninguem a escreveu...
docker compose exec -T postgres psql -U auction -d auction -qtA \
  -c "SELECT status, closed_at IS NULL FROM auctions WHERE id='$AID'"
# ...e mesmo assim o lance atrasado e recusado. E o ends_at que recusa, nao a coluna
curl -s -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $(uuidgen)" \
  -d '{"amountCents":900,"expectedVersion":1}' | jq .

# a fila cresce onde da para ver, no produtor que continua vivo
curl -s localhost:8080/metrics | grep -E '^(stream_backlog_entries|stream_pending_entries) '

# ressuscita, e a materializacao chega atrasada em vez de nunca
docker compose start closerd && sleep 8
docker compose exec -T postgres psql -U auction -d auction -qtA \
  -c "SELECT status, closed_at >= ends_at FROM auctions WHERE id='$AID'"
curl -s localhost:8081/metrics | grep -E '^(auction_closings_total|auction_close_lag_seconds_count)'
curl -s localhost:8080/metrics | grep -E '^(stream_backlog_entries|stream_pending_entries) '

# e o Redis, para ver o silencio no lugar do zero
docker pause "$(docker compose ps -q redis)"
curl -s localhost:8080/healthz | jq .
curl -s localhost:8080/readyz  | jq .
curl -s localhost:8080/metrics | grep -cE '^(stream_backlog_entries|stream_pending_entries) '
docker unpause "$(docker compose ps -q redis)"

make down
```

Aceite manual:

- O primeiro lance é `201`
- Passados os 40 segundos com o `closerd` morto, o psql responde `open` e `t`: **ninguém escreveu a coluna**
- E ainda assim o lance atrasado recebe `410 auction_closed` com `retryable: false`. Esta é a etapa inteira em duas linhas: a corretude não estava na coluna, estava no `ends_at` comparado ao relógio do banco (decisões 12 e 68)
- Com o worker fora, `stream_backlog_entries` cresce e `stream_pending_entries` fica em zero — as duas séries dizendo coisas diferentes na mesma falha (decisão 73)
- Depois do `start`, o leilão fecha, `closed_at >= ends_at` sai `t`, e `auction_close_lag_seconds_count` sobe: a materialização atrasou, e o atraso virou número
- Com o Redis pausado, `/healthz` é 200 e `/readyz` é 503 nomeando o Redis, e o `grep -c` das duas gauges devolve **0**: elas não saíram zeradas, elas não saíram
- `make down` derruba tudo sem contêiner órfão, com o Redis despausado

## Definicao De Pronto

- RF01 a RF11 implementados
- C1 a C5 executados, com a saída real colada no PR — checkpoint sem saída não conta como aceito
- Cinco execuções de caos com `checker.json` em `exit: 0`, cada uma carregando o bloco `chaos` que a identifica
- I1 a I8 verdes nas cinco, **sem um único invariante de corretude relaxado**
- I5 na direção `db < cliente` demonstradamente intocada: continua sendo FALHA, sob caos e fora dele
- I4 exigível pela primeira vez, e verde nas duas execuções do cenário 1 — inclusive sob `shard`, a engine que decide em memória
- `XAUTOCLAIM` demonstrado recuperando a mensagem de um `closerd` morto com ela na mão, sob carga
- As duas gauges de fila demonstradas **ausentes** durante o apagão do Redis, e não zeradas
- Nenhuma série nova, nenhum `if` de caos dentro de `internal/`, nenhum binário lendo variável de caos
- O injetor demonstrado curando o sistema depois de ser interrompido no meio de uma pausa
- `bench/bid-storm.js` byte a byte igual, e `bench/run-cell.sh` sem `CHAOS` idêntico em comportamento ao de hoje
- `go test ./... -race` limpo, `shellcheck` sem aviso
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado pelo PR de implementação
- Com isto a etapa 4 fecha, e a etapa 5 pode rodar as 36 células sabendo que os dois processos já foram derrubados no pior instante e o verificador continuou dizendo a verdade
